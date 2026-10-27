package payprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atalaymudanyali/quickbytes/internal/webhook"
)

const (
	// sendBatch caps how many deliveries one SendDue call takes. They're sent
	// at the same time, so one slow endpoint doesn't hold up the others.
	sendBatch = 20
	// MaxDeliveryAttempts is how many times a webhook is tried before it's
	// marked failed. With the backoff below that's about 2 hours 12 minutes
	// of trying; after that, the merchant's own checking (asking with GET) is
	// what's left.
	MaxDeliveryAttempts = 14
	// Backoff: after the n-th failed try the next one waits 1 s, 2 s, 4 s...
	// doubling, up to an hour.
	deliveryBackoffBase = time.Second
	deliveryBackoffMax  = time.Hour
)

// Event is the body of a webhook: what happened to which payment.
type Event struct {
	// ID is the event's public ID, the same on every try: the merchant uses
	// it to notice a webhook it has already handled.
	ID        uuid.UUID `json:"id"`
	Type      string    `json:"type"` // payment.<status>, like payment.authorized
	CreatedAt time.Time `json:"created_at"`
	Data      EventData `json:"data"`
}

type EventData struct {
	// Payment is the payment as this event left it: its status is the
	// event's, even when the payment has moved on since (a webhook retried
	// after the payment was captured still says authorized). So webhooks can
	// arrive out of order, and the merchant must never move a payment back.
	Payment Payment `json:"payment"`
}

// SenderConfig holds the sender's settings.
type SenderConfig struct {
	// Timeout is how long one try waits for the endpoint's answer.
	Timeout time.Duration
	// SendTwice posts every webhook twice in a row: a test switch, to show
	// the merchant handles a repeated webhook only once.
	SendTwice bool
}

// Sender posts queued webhooks to their endpoints, signed, and tries again
// later (backoff) when an endpoint doesn't answer 2xx.
//
// Delivery is "at least once": a webhook can arrive twice (the endpoint
// handled it, but its answer was lost, so we try again), and there's no
// promise about order (a retried webhook can come after a newer one). The
// merchant handles both: by remembering event IDs, and by never letting a
// payment's status go backwards.
type Sender struct {
	pool   *pgxpool.Pool
	client *http.Client
	tries  int // posts per delivery: 1, or 2 with SendTwice
	lease  time.Duration
	logger *slog.Logger
}

func NewSender(pool *pgxpool.Pool, cfg SenderConfig, logger *slog.Logger) *Sender {
	tries := 1
	if cfg.SendTwice {
		tries = 2
	}
	return &Sender{
		pool: pool,
		client: &http.Client{
			Timeout: cfg.Timeout,
			// Redirects aren't followed: the endpoint's URL must be the real
			// receiver. Following one could carry signed payment data to an
			// address nobody set up. A 3xx counts as a failed try.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		tries: tries,
		// How long a claimed delivery is left to this sender before another
		// may take it: well over the longest a delivery can take here.
		lease:  max(time.Minute, 3*time.Duration(tries)*cfg.Timeout),
		logger: logger,
	}
}

// delivery is one claimed webhook, ready to send.
type delivery struct {
	id       int64
	attempts int // tries made before this one
	url      string
	secret   string
	event    Event
}

// SendDue sends up to sendBatch due webhooks and records how each went. It
// returns how many it took.
func (s *Sender) SendDue(ctx context.Context) (int, error) {
	return s.sendDue(ctx, nil)
}

// SendDueTo is SendDue limited to these endpoints' deliveries. Tests use it
// (here and in the API's tests, which run this sender against a real API), so
// they never send other tests' webhooks in the shared test database.
func (s *Sender) SendDueTo(ctx context.Context, endpoints ...uuid.UUID) (int, error) {
	return s.sendDue(ctx, endpoints)
}

// sendDue is SendDue limited to some endpoints' deliveries; nil means any.
func (s *Sender) sendDue(ctx context.Context, only []uuid.UUID) (int, error) {
	batch, err := s.claim(ctx, only)
	if err != nil {
		return 0, err
	}
	var wg sync.WaitGroup
	for _, d := range batch {
		wg.Go(func() {
			s.record(ctx, d, s.send(ctx, d))
		})
	}
	wg.Wait()
	return len(batch), nil
}

// claim takes due deliveries of enabled endpoints, oldest due first, and
// pushes their next try a lease ahead, so another provider copy's sender
// leaves them alone meanwhile (SKIP LOCKED: it doesn't wait for them either).
// If this copy dies mid-send, the lease runs out and another copy sends them.
// Like the API's reconcile loop, only the claim is in a transaction: nothing
// is locked while waiting on the network.
func (s *Sender) claim(ctx context.Context, only []uuid.UUID) ([]delivery, error) {
	rows, err := s.pool.Query(ctx, `
		WITH claimed AS (
			UPDATE webhook_deliveries SET next_attempt_at = now() + make_interval(secs => $1)
			WHERE id IN (
				SELECT d.id FROM webhook_deliveries d
				JOIN webhook_endpoints e ON e.id = d.endpoint_id AND e.enabled
				WHERE d.status = 'pending' AND d.next_attempt_at <= now()
					AND ($3::uuid[] IS NULL OR d.endpoint_id = ANY($3))
				ORDER BY d.next_attempt_at
				LIMIT $2
				FOR UPDATE OF d SKIP LOCKED)
			RETURNING id, attempts, endpoint_id, payment_event_id
		)
		SELECT c.id, c.attempts, e.url, e.secret, ev.public_id, ev.status, ev.created_at,
			p.id, p.amount_kurus, p.currency, p.decline_code, p.card_last4, p.reference, p.created_at
		FROM claimed c
		JOIN webhook_endpoints e ON e.id = c.endpoint_id
		JOIN payment_events ev ON ev.id = c.payment_event_id
		JOIN payments p ON p.id = ev.payment_id
		ORDER BY c.id`, s.lease.Seconds(), sendBatch, only)
	if err != nil {
		return nil, fmt.Errorf("claim due webhooks: %w", err)
	}
	batch, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (delivery, error) {
		var d delivery
		p := &d.event.Data.Payment
		err := row.Scan(&d.id, &d.attempts, &d.url, &d.secret, &d.event.ID, &p.Status, &d.event.CreatedAt,
			&p.ID, &p.AmountKurus, &p.Currency, &p.DeclineCode, &p.CardLast4, &p.Reference, &p.CreatedAt)
		// The payment as this event left it (see EventData).
		d.event.Type = "payment." + p.Status
		p.UpdatedAt = d.event.CreatedAt
		if p.Status != StatusFailed {
			p.DeclineCode = nil // only a failed payment has a reason
		}
		return d, err
	})
	if err != nil {
		return nil, fmt.Errorf("claim due webhooks: %w", err)
	}
	return batch, nil
}

// send posts one webhook (twice with SendTwice) and returns nil if the
// endpoint answered 2xx.
func (s *Sender) send(ctx context.Context, d delivery) error {
	body, err := json.Marshal(d.event)
	if err != nil {
		return fmt.Errorf("encode webhook: %w", err)
	}
	var errs []error
	for range s.tries {
		err := s.post(ctx, d, body)
		if err == nil {
			errs = nil // one 2xx is enough
		} else {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Sender) post(ctx context.Context, d delivery, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "QuickBytes-PayProvider/1")
	// Signed now, on every try: a retry two hours later must not look like a
	// replay of an old webhook.
	req.Header.Set(webhook.Header, webhook.Sign(d.secret, time.Now(), body))

	resp, err := s.client.Do(req)
	if err != nil {
		return err // a timeout, a refused connection...
	}
	defer resp.Body.Close()
	// Read (a little of) the answer, so the connection can be used again.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("endpoint answered %s", resp.Status)
	}
	return nil
}

// record writes how a try went: delivered, another try after a wait, or
// failed for good after MaxDeliveryAttempts.
func (s *Sender) record(ctx context.Context, d delivery, sendErr error) {
	// Recorded even while the provider shuts down: the try happened.
	ctx = context.WithoutCancel(ctx)
	attempts := d.attempts + 1
	log := s.logger.With("delivery_id", d.id, "event_id", d.event.ID, "type", d.event.Type, "attempts", attempts)

	var err error
	switch {
	case sendErr == nil:
		_, err = s.pool.Exec(ctx, `
			UPDATE webhook_deliveries
			SET status = 'delivered', attempts = $2, delivered_at = now(), last_error = NULL
			WHERE id = $1 AND status = 'pending'`, d.id, attempts)
		log.Info("webhook delivered")
	case attempts >= MaxDeliveryAttempts:
		_, err = s.pool.Exec(ctx, `
			UPDATE webhook_deliveries SET status = 'failed', attempts = $2, last_error = $3
			WHERE id = $1 AND status = 'pending'`, d.id, attempts, sendErr.Error())
		log.Error("webhook failed for good; no more tries", "err", sendErr)
	default:
		wait := deliveryBackoff(attempts)
		_, err = s.pool.Exec(ctx, `
			UPDATE webhook_deliveries
			SET attempts = $2, last_error = $3, next_attempt_at = now() + make_interval(secs => $4)
			WHERE id = $1 AND status = 'pending'`, d.id, attempts, sendErr.Error(), wait.Seconds())
		log.Warn("webhook not delivered; trying again later", "err", sendErr, "retry_in", wait.Round(time.Millisecond).String())
	}
	if err != nil {
		// The lease runs out and the webhook is sent again: a repeat, which
		// the merchant handles anyway.
		log.Error("record webhook try", "err", err)
	}
}

// deliveryBackoff is how long to wait after the attempts-th failed try: 1 s,
// 2 s, 4 s... up to an hour, each with up to 20% added at random (jitter).
// The jitter matters after an outage: every webhook that failed during it
// would otherwise come back at the same moment and knock the endpoint over
// again.
func deliveryBackoff(attempts int) time.Duration {
	d := deliveryBackoffMax
	if attempts < 30 { // beyond that, the shift would only overflow
		d = min(deliveryBackoffBase<<max(attempts-1, 0), deliveryBackoffMax)
	}
	return d + time.Duration(rand.Int64N(int64(d)/5+1))
}

// RunSender sends due webhooks every interval until ctx is cancelled (the
// provider shutting down), going again at once while full batches come back.
func (s *Sender) RunSender(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for {
			n, err := s.SendDue(ctx)
			if err != nil {
				if ctx.Err() == nil {
					s.logger.Error("send webhooks", "err", err)
				}
				break
			}
			if n < sendBatch || ctx.Err() != nil {
				break
			}
		}
	}
}

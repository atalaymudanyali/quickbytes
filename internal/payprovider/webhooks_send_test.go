package payprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atalaymudanyali/quickbytes/internal/webhook"
)

// receiver is a fake merchant endpoint. It checks every webhook's signature
// and its body against the spec, and answers with the next status from
// answers (200 once they run out).
type receiver struct {
	t       *testing.T
	mu      sync.Mutex
	answers []int
	got     []Event
	srv     *httptest.Server
}

func newReceiver(t *testing.T, answers ...int) *receiver {
	r := &receiver{t: t, answers: answers}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		if err := webhook.Verify(testSecret, req.Header.Get(webhook.Header), body, time.Now()); err != nil {
			t.Errorf("webhook signature: %v", err)
		}
		if ct := req.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		checkEventAgainstSpec(t, body)
		var ev Event
		if err := json.Unmarshal(body, &ev); err != nil {
			t.Errorf("webhook body: %v", err)
		}
		r.mu.Lock()
		r.got = append(r.got, ev)
		status := http.StatusOK
		if len(r.answers) > 0 {
			status, r.answers = r.answers[0], r.answers[1:]
		}
		r.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.got...)
}

// checkEventAgainstSpec checks a webhook body against the WebhookEvent schema
// in api/payprovider.yaml.
func checkEventAgainstSpec(t *testing.T, body []byte) {
	t.Helper()
	if err := eventSpecError(body); err != nil {
		t.Errorf("webhook doesn't match WebhookEvent in api/payprovider.yaml: %v\nbody: %s", err, body)
	}
}

func eventSpecError(body []byte) error {
	doc, err := spec()
	if err != nil {
		return err
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return err
	}
	return doc.Components.Schemas["WebhookEvent"].Value.VisitJSON(v)
}

// The schema check must fail webhooks that break it.
func TestWebhookSchemaCatchesMismatches(t *testing.T) {
	payment := `{"id": "01a17880-7c89-7afd-ac33-f3935dc16620", "amount_kurus": 1, "currency": "TRY", "status": "%s",
		"card_last4": "4242", "created_at": "2026-10-26T10:00:00Z", "updated_at": "2026-10-26T10:00:00Z"}`
	event := `{"id": "02c4baf7-d82f-4da5-9f30-6a38c707d649", "type": "%s", "created_at": "2026-10-26T10:00:00Z",
		"data": {"payment": ` + payment + `}}`
	if err := eventSpecError(fmt.Appendf(nil, event, "payment.authorized", "authorized")); err != nil {
		t.Fatalf("a valid webhook was refused: %v", err)
	}
	for name, body := range map[string]string{
		"unknown type":    fmt.Sprintf(event, "payment.processing", "processing"),
		"unknown status":  fmt.Sprintf(event, "payment.authorized", "paid"),
		"no data":         `{"id": "02c4baf7-d82f-4da5-9f30-6a38c707d649", "type": "payment.authorized", "created_at": "2026-10-26T10:00:00Z"}`,
		"amount as text":  strings.Replace(fmt.Sprintf(event, "payment.authorized", "authorized"), `"amount_kurus": 1`, `"amount_kurus": "1"`, 1),
		"extra top field": strings.Replace(fmt.Sprintf(event, "payment.authorized", "authorized"), `"type"`, `"extra": 1, "type"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if eventSpecError([]byte(body)) == nil {
				t.Fatal("a webhook that breaks the schema was accepted")
			}
		})
	}
}

// senderSetup is a merchant whose endpoint is url, and a sender.
func senderSetup(t *testing.T, url string, cfg SenderConfig) (*Payments, *Sender, *pgxpool.Pool, uuid.UUID, Endpoint) {
	t.Helper()
	p, pool, merchant := newPayments(t)
	ep, err := NewEndpoints(pool).Ensure(context.Background(), merchant, url, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 2 * time.Second
	}
	return p, NewSender(pool, cfg, slog.New(slog.DiscardHandler)), pool, merchant, ep
}

// deliveryState is a delivery as stored.
type deliveryState struct {
	Status    string
	Attempts  int
	LastError *string
	WaitsFor  time.Duration // until the next try
}

func deliveryOf(t *testing.T, pool *pgxpool.Pool, paymentID uuid.UUID, status string) deliveryState {
	t.Helper()
	var d deliveryState
	var secs float64
	err := pool.QueryRow(context.Background(), `
		SELECT d.status::text, d.attempts, d.last_error, extract(epoch FROM d.next_attempt_at - now())::float8
		FROM webhook_deliveries d JOIN payment_events ev ON ev.id = d.payment_event_id
		WHERE ev.payment_id = $1 AND ev.status = $2`, paymentID, status).Scan(&d.Status, &d.Attempts, &d.LastError, &secs)
	if err != nil {
		t.Fatal(err)
	}
	d.WaitsFor = time.Duration(secs * float64(time.Second))
	return d
}

// makeDeliveriesDue moves an endpoint's waiting deliveries' next try into the
// past, instead of waiting for the backoff.
func makeDeliveriesDue(t *testing.T, pool *pgxpool.Pool, ep Endpoint) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE webhook_deliveries SET next_attempt_at = now() - interval '1 second' WHERE endpoint_id = $1 AND status = 'pending'`,
		ep.ID); err != nil {
		t.Fatal(err)
	}
}

func sendDue(t *testing.T, s *Sender, ep Endpoint) int {
	t.Helper()
	n, err := s.sendDue(context.Background(), []uuid.UUID{ep.ID})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// A webhook arrives signed, shaped like the spec says, and is then marked
// delivered and not sent again.
func TestWebhookIsDelivered(t *testing.T) {
	ctx := context.Background()
	r := newReceiver(t)
	p, s, pool, merchant, ep := senderSetup(t, r.srv.URL, SenderConfig{})
	ref := "order-" + uuid.NewString()
	pay, err := p.Authorize(ctx, merchant, AuthorizeInput{AmountKurus: 32000, CardNumber: cardApprove, Reference: &ref})
	if err != nil {
		t.Fatal(err)
	}

	if n := sendDue(t, s, ep); n != 1 {
		t.Fatalf("sent %d, want 1", n)
	}
	got := r.events()
	if len(got) != 1 {
		t.Fatalf("received %d webhooks, want 1", len(got))
	}
	ev := got[0]
	if ev.Type != "payment.authorized" || ev.Data.Payment.ID != pay.ID || ev.Data.Payment.Status != StatusAuthorized ||
		ev.Data.Payment.AmountKurus != 32000 || ev.Data.Payment.Reference == nil || *ev.Data.Payment.Reference != ref {
		t.Errorf("webhook = %+v", ev)
	}
	if d := deliveryOf(t, pool, pay.ID, StatusAuthorized); d.Status != "delivered" || d.Attempts != 1 || d.LastError != nil {
		t.Errorf("delivery = %+v", d)
	}
	if n := sendDue(t, s, ep); n != 0 || len(r.events()) != 1 {
		t.Errorf("a delivered webhook was sent again")
	}
}

// A retried webhook keeps its event's status, even after the payment moved
// on: the webhook for "authorized" still says authorized once the payment is
// captured, and both carry their own event ID.
func TestWebhookDescribesItsEvent(t *testing.T) {
	ctx := context.Background()
	r := newReceiver(t)
	p, s, _, merchant, ep := senderSetup(t, r.srv.URL, SenderConfig{})
	pay := authorize(t, p, merchant, cardApprove)
	if _, err := p.Capture(ctx, merchant, pay.ID); err != nil {
		t.Fatal(err)
	}
	declined := authorize(t, p, merchant, cardDecline)

	sendDue(t, s, ep)
	types := map[string]Event{}
	for _, ev := range r.events() {
		types[ev.Type] = ev
	}
	if len(types) != 3 {
		t.Fatalf("received %v", r.events())
	}
	auth, capt := types["payment.authorized"], types["payment.captured"]
	if auth.Data.Payment.Status != StatusAuthorized || capt.Data.Payment.Status != StatusCaptured || auth.ID == capt.ID {
		t.Errorf("authorized: %+v\ncaptured: %+v", auth, capt)
	}
	failed := types["payment.failed"]
	if failed.Data.Payment.ID != declined.ID || failed.Data.Payment.DeclineCode == nil || *failed.Data.Payment.DeclineCode != DeclineCardDeclined {
		t.Errorf("failed: %+v", failed)
	}
}

// The endpoint fails twice (the API is down), then works: the webhook gets
// through on the third try, after growing waits.
func TestWebhookIsRetriedAfterFailures(t *testing.T) {
	r := newReceiver(t, http.StatusInternalServerError, http.StatusServiceUnavailable)
	p, s, pool, merchant, ep := senderSetup(t, r.srv.URL, SenderConfig{})
	pay := authorize(t, p, merchant, cardApprove)

	sendDue(t, s, ep)
	d := deliveryOf(t, pool, pay.ID, StatusAuthorized)
	if d.Status != "pending" || d.Attempts != 1 || d.LastError == nil || *d.LastError != "endpoint answered 500 Internal Server Error" {
		t.Fatalf("after one failure: %+v", d)
	}
	// 1 s (+ up to 20%) until the next try; not due yet.
	if d.WaitsFor < 500*time.Millisecond || d.WaitsFor > 1300*time.Millisecond {
		t.Errorf("waits %v after the first failure, want about 1 s", d.WaitsFor)
	}
	if n := sendDue(t, s, ep); n != 0 {
		t.Errorf("sent before its wait was over")
	}

	makeDeliveriesDue(t, pool, ep)
	sendDue(t, s, ep)
	if d := deliveryOf(t, pool, pay.ID, StatusAuthorized); d.Attempts != 2 || d.WaitsFor < 1500*time.Millisecond || d.WaitsFor > 2500*time.Millisecond {
		t.Errorf("after the second failure: %+v, want a wait of about 2 s", d)
	}

	makeDeliveriesDue(t, pool, ep)
	sendDue(t, s, ep)
	if d := deliveryOf(t, pool, pay.ID, StatusAuthorized); d.Status != "delivered" || d.Attempts != 3 || d.LastError != nil {
		t.Errorf("after the endpoint came back: %+v", d)
	}
	// Three tries of the same event, with the same ID.
	got := r.events()
	if len(got) != 3 || got[0].ID != got[2].ID {
		t.Errorf("received %v", got)
	}
}

// No answer at all counts as a failed try too: a refused connection (nothing
// listening) and an endpoint slower than the timeout.
func TestWebhookWithoutAnswerIsRetried(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	downURL := down.URL
	down.Close() // nothing listens there any more

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(slow.Close)

	for name, url := range map[string]string{"refused": downURL, "timeout": slow.URL} {
		t.Run(name, func(t *testing.T) {
			p, s, pool, merchant, ep := senderSetup(t, url, SenderConfig{Timeout: 200 * time.Millisecond})
			pay := authorize(t, p, merchant, cardApprove)
			sendDue(t, s, ep)
			if d := deliveryOf(t, pool, pay.ID, StatusAuthorized); d.Status != "pending" || d.Attempts != 1 || d.LastError == nil {
				t.Errorf("delivery = %+v", d)
			}
		})
	}
}

// A redirect isn't followed: it's a failed try.
func TestWebhookRedirectIsNotFollowed(t *testing.T) {
	r := newReceiver(t)
	redirect := httptest.NewServer(http.RedirectHandler(r.srv.URL, http.StatusFound))
	t.Cleanup(redirect.Close)
	p, s, pool, merchant, ep := senderSetup(t, redirect.URL, SenderConfig{})
	pay := authorize(t, p, merchant, cardApprove)

	sendDue(t, s, ep)
	if len(r.events()) != 0 {
		t.Error("the redirect was followed")
	}
	if d := deliveryOf(t, pool, pay.ID, StatusAuthorized); d.Status != "pending" || d.LastError == nil || *d.LastError != "endpoint answered 302 Found" {
		t.Errorf("delivery = %+v", d)
	}
}

// After MaxDeliveryAttempts failures the webhook is given up.
func TestWebhookIsGivenUp(t *testing.T) {
	r := newReceiver(t)
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(failing.Close)
	p, s, pool, merchant, ep := senderSetup(t, failing.URL, SenderConfig{})
	pay := authorize(t, p, merchant, cardApprove)

	for range MaxDeliveryAttempts {
		makeDeliveriesDue(t, pool, ep)
		sendDue(t, s, ep)
	}
	if d := deliveryOf(t, pool, pay.ID, StatusAuthorized); d.Status != "failed" || d.Attempts != MaxDeliveryAttempts {
		t.Fatalf("after %d failures: %+v", MaxDeliveryAttempts, d)
	}
	// Given up means given up, even when the endpoint works again.
	if _, err := NewEndpoints(pool).Ensure(context.Background(), merchant, r.srv.URL, testSecret); err != nil {
		t.Fatal(err)
	}
	makeDeliveriesDue(t, pool, ep)
	if n := sendDue(t, s, ep); n != 0 || len(r.events()) != 0 {
		t.Error("a failed webhook was sent again")
	}
}

// The test switch posts every webhook twice: same event, same ID.
func TestSendTwice(t *testing.T) {
	r := newReceiver(t)
	p, s, pool, merchant, ep := senderSetup(t, r.srv.URL, SenderConfig{SendTwice: true})
	pay := authorize(t, p, merchant, cardApprove)

	sendDue(t, s, ep)
	got := r.events()
	if len(got) != 2 || got[0].ID != got[1].ID {
		t.Fatalf("received %v, want the same event twice", got)
	}
	if d := deliveryOf(t, pool, pay.ID, StatusAuthorized); d.Status != "delivered" || d.Attempts != 1 {
		t.Errorf("delivery = %+v", d)
	}
}

// Deliveries of a disabled endpoint wait; it gets them once it's enabled
// again.
func TestDisabledEndpointIsNotSent(t *testing.T) {
	ctx := context.Background()
	r := newReceiver(t)
	p, s, _, merchant, ep := senderSetup(t, r.srv.URL, SenderConfig{})
	authorize(t, p, merchant, cardApprove) // queued while enabled
	if err := NewEndpoints(p.pool).Disable(ctx, merchant); err != nil {
		t.Fatal(err)
	}
	if n := sendDue(t, s, ep); n != 0 {
		t.Errorf("sent %d to a disabled endpoint", n)
	}
	if _, err := NewEndpoints(p.pool).Ensure(ctx, merchant, r.srv.URL, testSecret); err != nil {
		t.Fatal(err)
	}
	if n := sendDue(t, s, ep); n != 1 || len(r.events()) != 1 {
		t.Errorf("after enabling again: sent %d", n)
	}
}

// A delivery another sender claimed is left alone until its lease runs out:
// two provider copies don't send the same webhook at the same time.
func TestClaimedDeliveryIsSkipped(t *testing.T) {
	r := newReceiver(t)
	p, s, pool, merchant, ep := senderSetup(t, r.srv.URL, SenderConfig{})
	authorize(t, p, merchant, cardApprove)

	batch, err := s.claim(context.Background(), []uuid.UUID{ep.ID}) // the other copy
	if err != nil || len(batch) != 1 {
		t.Fatalf("claim: %v, %d", err, len(batch))
	}
	if n := sendDue(t, s, ep); n != 0 {
		t.Errorf("a claimed delivery was taken again")
	}
	// The other copy died: once its lease runs out, the webhook goes out.
	makeDeliveriesDue(t, pool, ep)
	if n := sendDue(t, s, ep); n != 1 || len(r.events()) != 1 {
		t.Errorf("after the lease ran out: sent %d", n)
	}
}

func TestDeliveryBackoff(t *testing.T) {
	for attempts, base := range map[int]time.Duration{
		1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 12: 2048 * time.Second,
		13: time.Hour, 14: time.Hour, 100: time.Hour,
	} {
		for range 50 {
			if d := deliveryBackoff(attempts); d < base || d > base+base/5 {
				t.Fatalf("attempt %d: waited %v, want %v plus up to 20%%", attempts, d, base)
			}
		}
	}
	// Adding up every wait before giving up: about 2 hours of trying.
	var total time.Duration
	for n := 1; n < MaxDeliveryAttempts; n++ {
		total += min(deliveryBackoffBase<<(n-1), deliveryBackoffMax)
	}
	if total < 2*time.Hour || total > 2*time.Hour+15*time.Minute {
		t.Errorf("tries span %v, want about 2 h 10 min", total)
	}
}

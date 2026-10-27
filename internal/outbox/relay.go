package outbox

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// relayBatch caps how many events one round takes.
	relayBatch = 100
	// publishTimeout is how long one round waits for the broker to confirm
	// its batch. The round's transaction (and its row locks) stays open that
	// long at most.
	publishTimeout = 10 * time.Second
	// Backoff while publishing fails: 1 s, 2 s, 4 s... up to 30 s.
	relayBackoffBase = time.Second
	relayBackoffMax  = 30 * time.Second
)

// Publisher sends events to the event stream.
type Publisher interface {
	// Publish sends events in the order given and returns nil only once the
	// broker has stored every one of them.
	Publish(ctx context.Context, events []Event) error
}

// Relay publishes the outbox's waiting events and stamps them published: the
// postman who empties the outbox.
//
// Delivery is "at least once". If the relay dies after the broker stored a
// batch but before its commit, the rows are still waiting and get published
// again: a repeat, never a loss. Consumers drop repeats by event ID.
//
// Several relays (one per API copy) can run at once. Each round locks the rows
// it takes (FOR UPDATE SKIP LOCKED), and the others skip those rows instead of
// waiting, so no row is published by two relays at the same time.
type Relay struct {
	pool   *pgxpool.Pool
	pub    Publisher
	logger *slog.Logger
	// only limits the relay to these aggregates' events (nil: all). Tests set
	// it, so they never publish other tests' events in the shared test
	// database.
	only []uuid.UUID
}

func NewRelay(pool *pgxpool.Pool, pub Publisher, logger *slog.Logger) *Relay {
	return &Relay{pool: pool, pub: pub, logger: logger}
}

// PublishWaiting runs one round: it takes up to relayBatch waiting events,
// publishes them and stamps them published, all in one transaction. It
// returns how many it published.
//
// It takes only each aggregate's OLDEST waiting event. Without that rule, two
// relays could break one order's order: relay A holds "order 42 paid" (locked,
// being published), relay B skips that row and takes "order 42 accepted",
// and B's might reach the broker first. With it, B sees that 42 still has an
// earlier event waiting and leaves "accepted" for a later round. An order
// with several waiting events (after an outage) needs one round per event.
//
// If publishing fails, the transaction is rolled back: the rows stay waiting,
// unchanged, for the next round.
func (r *Relay) PublishWaiting(ctx context.Context) (int, error) {
	n := 0
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT o.id, o.topic, o.aggregate_id, o.type, o.payload
			FROM outbox o
			WHERE o.published_at IS NULL
				AND ($2::uuid[] IS NULL OR o.aggregate_id = ANY($2))
				AND NOT EXISTS (
					SELECT 1 FROM outbox earlier
					WHERE earlier.aggregate_id = o.aggregate_id
						AND earlier.published_at IS NULL
						AND earlier.seq < o.seq)
			ORDER BY o.seq
			LIMIT $1
			FOR UPDATE OF o SKIP LOCKED`, relayBatch, r.only)
		if err != nil {
			return fmt.Errorf("take waiting events: %w", err)
		}
		batch, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Event, error) {
			var e Event
			err := row.Scan(&e.ID, &e.Topic, &e.AggregateID, &e.Type, &e.Payload)
			return e, err
		})
		if err != nil {
			return fmt.Errorf("take waiting events: %w", err)
		}
		if len(batch) == 0 {
			return nil
		}

		pubCtx, cancel := context.WithTimeout(ctx, publishTimeout)
		defer cancel()
		if err := r.pub.Publish(pubCtx, batch); err != nil {
			return fmt.Errorf("publish %d events: %w", len(batch), err)
		}

		ids := make([]uuid.UUID, len(batch))
		for i, e := range batch {
			ids[i] = e.ID
		}
		if _, err := tx.Exec(ctx, `UPDATE outbox SET published_at = now() WHERE id = ANY($1)`, ids); err != nil {
			return fmt.Errorf("mark events published: %w", err)
		}
		n = len(batch)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// Run publishes waiting events until ctx is cancelled (the API shutting down).
// It looks every interval, and again at once while rounds find work (a
// backlog after an outage drains at full speed).
//
// While publishing fails (the broker is down), it waits longer each time,
// up to 30 s. Every event fails for the same reason then, so the whole relay
// backs off, not each event. Orders keep working meanwhile: their events
// pile up in the outbox and go out, in order, once the broker is back.
func (r *Relay) Run(ctx context.Context, every time.Duration) {
	failures := 0
	for {
		n, err := r.PublishWaiting(ctx)
		if ctx.Err() != nil {
			return
		}
		wait := every
		switch {
		case err != nil:
			failures++
			wait = relayBackoff(failures)
			r.logger.Warn("publishing outbox events failed; trying again", "err", err,
				"failures", failures, "in", wait.Round(time.Millisecond).String())
		case failures > 0:
			r.logger.Info("publishing outbox events again", "after_failures", failures)
			failures = 0
			continue
		case n > 0:
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// relayBackoff is how long to wait after the failures-th failed round in a
// row: 1 s, 2 s, 4 s... up to 30 s, each with up to 20% added at random
// (jitter), so several API copies don't all hit the broker at the same
// moment when it comes back.
func relayBackoff(failures int) time.Duration {
	d := relayBackoffMax
	if failures < 20 { // beyond that, the shift would only overflow
		d = min(relayBackoffBase<<max(failures-1, 0), relayBackoffMax)
	}
	return d + time.Duration(rand.Int64N(int64(d)/5+1))
}

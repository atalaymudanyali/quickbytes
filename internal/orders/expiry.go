package orders

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// expiryBatch caps how many orders one ExpireUnpaid call takes, so one
// transaction never holds thousands of locks.
const expiryBatch = 100

// ExpireUnpaid cancels (reason expired) up to expiryBatch placed orders older
// than payWindow (how long an order may wait for its payment: 15 minutes in the
// API), oldest first, and returns how many it cancelled.
//
// FOR UPDATE SKIP LOCKED lets several API copies run this at once. Each locks
// the orders it takes, and the others skip those rows instead of waiting for
// them, like cashiers serving one queue: no two take the same customer. An
// order a payment is moving right now is locked too, so it's skipped, and
// looked at again on the next run (by then it's paid and no longer matches).
func (s *Store) ExpireUnpaid(ctx context.Context, payWindow time.Duration) (int, error) {
	n := 0
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id FROM orders
			WHERE status = 'placed' AND created_at < now() - make_interval(secs => $1)
			ORDER BY created_at
			LIMIT $2
			FOR UPDATE SKIP LOCKED`, payWindow.Seconds(), expiryBatch)
		if err != nil {
			return fmt.Errorf("find unpaid orders: %w", err)
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return fmt.Errorf("find unpaid orders: %w", err)
		}
		// The same move as any other, so each expiry gets its history row.
		for _, id := range ids {
			if _, err := transition(ctx, tx, id, Expire, System); err != nil {
				return fmt.Errorf("expire order %s: %w", id, err)
			}
		}
		n = len(ids)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// RunExpiry checks for unpaid orders every interval until ctx is cancelled (the
// API shutting down). A failed run is logged and tried again next time: the
// orders are still there.
func (s *Store) RunExpiry(ctx context.Context, every, payWindow time.Duration, logger *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		// Keep going while full batches come back: after downtime there may be
		// more than one batch waiting.
		for {
			n, err := s.ExpireUnpaid(ctx, payWindow)
			if err != nil {
				if ctx.Err() == nil {
					logger.Error("expire unpaid orders", "err", err)
				}
				break
			}
			if n > 0 {
				logger.Info("expired unpaid orders", "count", n)
			}
			if n < expiryBatch {
				break
			}
		}
	}
}

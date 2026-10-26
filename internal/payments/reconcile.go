package payments

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// reconcileBatch caps how many payments one ReconcileDue run takes.
const reconcileBatch = 50

// leaseFor is how long a run keeps the payments it took: their next check is
// pushed this far ahead the moment they're taken, so another API copy's
// loop leaves them alone meanwhile. Each Sync then sets the real next check
// (never, or a backoff). If this copy dies mid-run, the lease simply runs
// out and another copy takes the payment: nothing stays stuck.
const leaseFor = time.Minute

// ReconcileDue takes up to reconcileBatch due payments, oldest first, and
// syncs each one with the provider: finding out about unknown, pending and
// processing payments, and retrying money steps that didn't finish. It
// returns how many it took.
//
// "Due" means next_check_at has passed: a move marked it (MarkDueIn), a
// failed try's backoff ran out, or a new payment's grace ended.
func (s *Store) ReconcileDue(ctx context.Context) (int, error) {
	ids, err := s.claimDue(ctx, nil)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := s.sync(ctx, id, ViaReconcile); err != nil {
			s.logger.Warn("reconcile payment", "order_id", id, "err", err)
		}
	}
	return len(ids), nil
}

// claimDue takes due payments and pushes their next check leaseFor ahead, in
// one short transaction, and returns their orders. FOR UPDATE SKIP LOCKED
// lets several API copies claim at once without taking the same payment
// (like the expiry loop). Only the claim is in the transaction: the provider
// calls come after it commits, so no lock is held while waiting on the
// network.
//
// only limits the claim to some orders' payments (tests use it, so they never
// touch other tests' payments in the shared database); nil means any.
func (s *Store) claimDue(ctx context.Context, only []uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE payments SET next_check_at = now() + make_interval(secs => $1)
		WHERE id IN (
			SELECT id FROM payments
			WHERE next_check_at <= now()
				AND status IN ('pending', 'unknown', 'processing', 'authorized', 'captured')
				AND ($3::uuid[] IS NULL OR order_id = ANY($3))
			ORDER BY next_check_at
			LIMIT $2
			FOR UPDATE SKIP LOCKED)
		RETURNING order_id`, leaseFor.Seconds(), reconcileBatch, only)
	if err != nil {
		return nil, fmt.Errorf("claim due payments: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("claim due payments: %w", err)
	}
	return ids, nil
}

// RunReconcile runs ReconcileDue every interval until ctx is cancelled (the
// API shutting down), going again at once while full batches come back.
func (s *Store) RunReconcile(ctx context.Context, every time.Duration, logger *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for {
			n, err := s.ReconcileDue(ctx)
			if err != nil {
				if ctx.Err() == nil {
					logger.Error("reconcile payments", "err", err)
				}
				break
			}
			if n < reconcileBatch || ctx.Err() != nil {
				break
			}
		}
	}
}

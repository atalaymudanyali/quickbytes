package payprovider

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// settleBatch caps how many payments one SettleDue call finishes.
const settleBatch = 100

// SettleDue gives processing payments whose time has come their final answer
// (authorized: the slow card always ends approved), and returns how many it
// finished. It plays the card's bank answering late.
//
// The merchant hears about it through a payment.authorized webhook, queued
// here with the event (or by asking with GET /v1/payments/{id}).
//
// It's one statement: pick the due payments, change them, append their
// events and queue their webhooks. SKIP LOCKED leaves out a payment some
// other transaction is changing right now (a second provider copy settling
// it too), instead of waiting for it; the other copy settles that one.
func (p *Payments) SettleDue(ctx context.Context) (int, error) {
	var n int
	err := p.pool.QueryRow(ctx, `
		WITH due AS (
			SELECT id FROM payments
			WHERE status = 'processing' AND settle_at <= now()
			ORDER BY settle_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		), settled AS (
			UPDATE payments SET status = 'authorized', settle_at = NULL, updated_at = now()
			FROM due WHERE payments.id = due.id
			RETURNING payments.id
		), ev AS (
			INSERT INTO payment_events (payment_id, status)
			SELECT id, 'authorized' FROM settled
			RETURNING id, payment_id, status
		), queued AS (
			`+queueWebhooks+`
		)
		SELECT count(*) FROM ev`,
		settleBatch).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("settle processing payments: %w", err)
	}
	return n, nil
}

// RunSettler finishes due processing payments every interval until ctx is
// cancelled (the provider shutting down). Like the API's order expiry, it keeps
// going while full batches come back, and a failed run is logged and tried
// again next time.
func (p *Payments) RunSettler(ctx context.Context, every time.Duration, logger *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for {
			n, err := p.SettleDue(ctx)
			if err != nil {
				if ctx.Err() == nil {
					logger.Error("settle processing payments", "err", err)
				}
				break
			}
			if n > 0 {
				logger.Info("settled processing payments", "count", n)
			}
			if n < settleBatch {
				break
			}
		}
	}
}

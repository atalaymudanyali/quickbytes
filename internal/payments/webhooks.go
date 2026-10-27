package payments

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// WebhookEvent is a webhook from the provider, already checked (its
// signature) and translated into our words by the provider's plug.
type WebhookEvent struct {
	ID      string // the provider's event ID: the same on every try
	Type    string // the provider's own name for it, kept for the record
	Payment ProviderPayment
	// Reference is our reference on the payment (the order ID): how a
	// payment is found whose provider ID we never learned (its authorize
	// timed out).
	Reference string
}

// Outcome is what ApplyWebhook did with a webhook.
type Outcome string

const (
	Applied   Outcome = "applied"   // it changed the payment (and maybe the order)
	Stale     Outcome = "stale"     // nothing new, out of order, or contradicting what we know
	Unmatched Outcome = "unmatched" // not about any payment of ours
	Mismatch  Outcome = "mismatch"  // our payment, but another amount: not trusted
	Repeat    Outcome = "repeat"    // handled before (same event ID): nothing done
)

// ApplyWebhook acts on what the provider told us. Webhooks are the fast path:
// the reconcile loop would find out the same thing, only later.
//
// All of it is one transaction:
//
//  1. Sign the guest book (webhook_events) with the event ID. Already there:
//     a repeat, nothing more to do.
//  2. Find our payment: by the provider's payment ID, else (we never learned
//     it) by our reference, the order ID.
//  3. Record the news through recordIn, the same code the request and the
//     reconcile loop use: the order and payment locks, the ratchet (older
//     news changes nothing), the ledger, and paying or cancelling the order.
//  4. If the payment changed, mark it due, so the reconcile loop takes any
//     money step that follows (the order was cancelled while the card was
//     processing: void the hold). No provider call is made here, so the
//     webhook's answer stays quick and no lock waits on the network.
//
// The guest-book line is undone with everything else when a step fails: the
// error becomes a 500, the provider tries again later, and that try is
// handled for real, not skipped as a repeat.
func (s *Store) ApplyWebhook(ctx context.Context, ev WebhookEvent) (Outcome, error) {
	var outcome Outcome
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO webhook_events (event_id, type, outcome) VALUES ($1, $2, 'unmatched')
			ON CONFLICT (event_id) DO NOTHING`, ev.ID, ev.Type)
		if err != nil {
			return fmt.Errorf("record webhook: %w", err)
		}
		if tag.RowsAffected() == 0 {
			outcome = Repeat
			return nil
		}

		log := s.logger.With("event_id", ev.ID, "type", ev.Type, "provider_payment_id", ev.Payment.ID)
		p, err := matchIn(ctx, tx, ev)
		if errors.Is(err, ErrNotFound) {
			log.Warn("webhook about no payment of ours", "reference", ev.Reference)
			outcome = Unmatched
			return nil // the row already says unmatched
		}
		if err != nil {
			return err
		}
		outcome = Stale
		switch {
		case ev.Payment.AmountKurus != p.AmountKurus:
			log.Error("webhook for another amount than our payment; ignored",
				"payment_id", p.ID, "theirs", ev.Payment.AmountKurus, "ours", p.AmountKurus)
			outcome = Mismatch
		default:
			out, err := s.recordIn(ctx, tx, p, Change{
				Status: ev.Payment.Status, ProviderPaymentID: &ev.Payment.ID, DeclineCode: ev.Payment.DeclineCode,
			}, ViaWebhook)
			if err != nil {
				return err
			}
			if out.Status != ev.Payment.Status && rank[out.Status] == rank[ev.Payment.Status] {
				// Not old news but a contradiction (authorized vs failed):
				// an honest provider never says that. Someone must look.
				log.Error("webhook contradicts what we know; ignored",
					"payment_id", p.ID, "ours", out.Status, "theirs", ev.Payment.Status)
			}
			if out.Status != p.Status {
				outcome = Applied
				if _, err := tx.Exec(ctx,
					`UPDATE payments SET next_check_at = now(), attempts = 0 WHERE id = $1`, p.ID); err != nil {
					return fmt.Errorf("mark payment due: %w", err)
				}
			}
		}
		_, err = tx.Exec(ctx,
			`UPDATE webhook_events SET outcome = $2, payment_id = $3 WHERE event_id = $1`, ev.ID, outcome, p.ID)
		if err != nil {
			return fmt.Errorf("record webhook outcome: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("apply webhook %s: %w", ev.ID, err)
	}
	return outcome, nil
}

// matchIn finds the payment a webhook is about: by the provider's payment ID
// or, for a payment whose provider ID we never learned, by the reference (our
// order ID). A reference match on a payment that already has another
// provider ID is not ours to change: that's a second hold for the same
// order, which a person must look at.
func matchIn(ctx context.Context, tx pgx.Tx, ev WebhookEvent) (Payment, error) {
	p, err := scanPayment(tx.QueryRow(ctx,
		`SELECT `+paymentColumns+` FROM payments WHERE provider_payment_id = $1`, ev.Payment.ID))
	switch {
	case err == nil:
		return p, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return Payment{}, fmt.Errorf("find webhook's payment: %w", err)
	}
	orderID, err := uuid.Parse(ev.Reference)
	if err != nil {
		return Payment{}, ErrNotFound // not a reference we send
	}
	p, err = scanPayment(tx.QueryRow(ctx,
		`SELECT `+paymentColumns+` FROM payments WHERE order_id = $1 AND provider_payment_id IS NULL`, orderID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Payment{}, ErrNotFound
	}
	if err != nil {
		return Payment{}, fmt.Errorf("find webhook's payment: %w", err)
	}
	return p, nil
}

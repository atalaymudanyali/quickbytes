package payments

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atalaymudanyali/quickbytes/internal/orders"
)

// maxSyncSteps caps one Sync. Following the rules settles in at most two
// provider calls (capture, then refund when the order was cancelled
// meanwhile); more means the provider keeps answering something we didn't
// ask for, and we stop rather than loop.
const maxSyncSteps = 4

// Backoff: after a failed try, the payment waits backoffBase, then twice as
// long each time, up to backoffMax.
const (
	backoffBase = 2 * time.Second
	backoffMax  = 5 * time.Minute
)

// MarkDueIn notes, inside an order move's transaction, that the order's
// payment may need work now: its next check is due at once, and earlier
// failures are forgotten (the situation changed). It's an orders.MoveHook.
// Because it commits with the move, a crash right after the move can't lose
// the note: the reconcile loop finds the payment due.
//
// Locks: the move already holds the order; this takes the payment after it.
func (s *Store) MarkDueIn(ctx context.Context, tx pgx.Tx, c orders.Change) error {
	_, err := tx.Exec(ctx,
		`UPDATE payments SET next_check_at = now(), attempts = 0 WHERE order_id = $1`, c.OrderID)
	if err != nil {
		return fmt.Errorf("mark payment due: %w", err)
	}
	return nil
}

// Sync brings an order's payment in line with the order, the thermostat way:
// look at both, take the one step nextStep names, and look again, until
// there's nothing left to do. It's what runs right after a move (the fast
// path) and what the reconcile loop runs for every due payment.
//
// Every provider call uses the payment's own key for that step, so two Syncs
// at once (a request and the loop), or a retry after a lost answer, still
// move the money once.
//
// It returns an error when a step couldn't be finished (no answer from the
// provider, a refusal). The payment then waits a little longer before the
// next try (backoff), and the order stays as it is: the restaurant did
// accept, whether or not the money has been taken yet.
//
// Like Authorize, it isn't stopped by ctx being cancelled: once a call may
// move money, we want to record its answer.
//
// Sync is the request's quick version: finding out about a payment we have
// no answer for (Lookup, Check) is left to the reconcile loop, which runs
// the same steps with ViaReconcile and does those too.
func (s *Store) Sync(ctx context.Context, orderID uuid.UUID) error {
	return s.sync(ctx, orderID, ViaRequest)
}

func (s *Store) sync(ctx context.Context, orderID uuid.UUID, via Via) error {
	ctx = context.WithoutCancel(ctx)
	for range maxSyncSteps {
		p, step, err := s.decide(ctx, orderID)
		if errors.Is(err, ErrNotFound) {
			return nil // an order from before payments existed
		}
		if err != nil {
			return err
		}
		switch step {
		case Nothing:
			return nil
		case PayOrder, FailOrder:
			continue // decide already moved the order
		case Capture, Void, Refund:
			if err := s.move(ctx, p, step, via); err != nil {
				return err
			}
		case Lookup, Check:
			if via != ViaReconcile {
				return nil // finding out is the reconcile loop's job
			}
			learned, err := s.findOut(ctx, p, step)
			if err != nil || !learned {
				return err
			}
		}
	}
	return s.tryLater(ctx, orderID, fmt.Errorf("payment didn't settle in %d steps", maxSyncSteps))
}

// decide locks the order and its payment and names the next step. Steps that
// only move the order (PayOrder, FailOrder) are taken right here, in the same
// transaction. When there's nothing to do, the payment is taken off the
// loop's list (next_check_at = infinity) until a move marks it due again;
// doing that under the order's lock means a move can't slip in between the
// decision and the note.
func (s *Store) decide(ctx context.Context, orderID uuid.UUID) (Payment, Step, error) {
	var p Payment
	var step Step
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		orderStatus, err := orders.LockIn(ctx, tx, orderID)
		if err != nil {
			return err
		}
		p, err = scanPayment(tx.QueryRow(ctx,
			`SELECT `+paymentColumns+` FROM payments WHERE order_id = $1 FOR UPDATE`, orderID))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock payment: %w", err)
		}
		step = nextStep(orderStatus, p.Status)
		switch step {
		case Nothing:
			_, err = tx.Exec(ctx,
				`UPDATE payments SET next_check_at = 'infinity', attempts = 0 WHERE id = $1`, p.ID)
		case PayOrder:
			_, err = orders.TransitionIn(ctx, tx, orderID, orders.Pay, orders.System)
		case FailOrder:
			_, err = orders.TransitionIn(ctx, tx, orderID, orders.FailPayment, orders.System)
		}
		return err
	})
	return p, step, err
}

// move asks the provider for one money step and records the answer.
func (s *Store) move(ctx context.Context, p Payment, step Step, via Via) error {
	call := map[Step]func(context.Context, string, string) (ProviderPayment, error){
		Capture: s.provider.Capture,
		Void:    s.provider.Void,
		Refund:  s.provider.Refund,
	}[step]
	answer, err := call(ctx, key(p.ID, string(step)), *p.ProviderPaymentID)
	if err != nil {
		return s.tryLater(ctx, p.OrderID, fmt.Errorf("%s payment %s: %w", step, p.ID, err))
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		_, err := s.recordIn(ctx, tx, p, Change{Status: answer.Status, ProviderPaymentID: &answer.ID, DeclineCode: answer.DeclineCode}, via)
		return err
	})
	if err != nil {
		return fmt.Errorf("record %s answer: %w", step, err)
	}
	if answer.Status == p.Status {
		// The provider answered with the payment unmoved: it refused the
		// move (its 409). The rules will ask for the same step again, so
		// wait instead of asking at once.
		return s.tryLater(ctx, p.OrderID, fmt.Errorf("provider didn't %s payment %s: it is still %s", step, p.ID, answer.Status))
	}
	return nil
}

// findOut asks the provider about a payment we have no answer for, and
// records what it says. It reports whether we learned something new: when
// not, the payment waits (backoff) and Sync stops for now.
//
//   - Check (processing, the slow card): GET it by its ID.
//   - Lookup (pending or unknown: we never heard back from authorize): find it
//     by our reference, the order ID. Never by authorizing again: we don't
//     even have the card number any more.
//
// A Lookup that finds nothing means the authorize never reached the
// provider, but only once AuthorizeGrace has passed since we wrote the
// payment: until then the request could still be on its way. After it, the
// payment is failed (no_answer) and the order is cancelled like a decline.
func (s *Store) findOut(ctx context.Context, p Payment, step Step) (bool, error) {
	var answer ProviderPayment
	var err error
	if step == Check {
		answer, err = s.provider.Get(ctx, *p.ProviderPaymentID)
	} else {
		var found []ProviderPayment
		found, err = s.provider.FindByReference(ctx, p.OrderID.String())
		switch {
		case err != nil:
		case len(found) == 0 && time.Since(p.CreatedAt) < AuthorizeGrace:
			return false, s.waitFor(ctx, p, "authorize not seen at the provider yet")
		case len(found) == 0:
			s.logger.Warn("authorize never reached the provider; failing the payment",
				"payment_id", p.ID, "order_id", p.OrderID)
			return true, s.record(ctx, p, Change{Status: Failed, DeclineCode: ptr(NoAnswer)})
		default:
			if len(found) > 1 {
				// One key per payment makes this impossible; if it happens
				// anyway, someone must look at the extra holds.
				s.logger.Error("several provider payments for one order; using the newest",
					"payment_id", p.ID, "order_id", p.OrderID, "count", len(found))
			}
			answer = found[0]
		}
	}
	if err != nil {
		return false, s.tryLater(ctx, p.OrderID, fmt.Errorf("%s payment %s: %w", step, p.ID, err))
	}
	if answer.AmountKurus != p.AmountKurus {
		return false, s.tryLater(ctx, p.OrderID, fmt.Errorf("provider payment %s is for %d kuruş, ours %s is for %d",
			answer.ID, answer.AmountKurus, p.ID, p.AmountKurus))
	}
	if answer.Status == p.Status {
		return false, s.waitFor(ctx, p, "still "+string(p.Status))
	}
	return true, s.record(ctx, p, Change{Status: answer.Status, ProviderPaymentID: &answer.ID, DeclineCode: answer.DeclineCode})
}

// record writes what the reconcile loop learned.
func (s *Store) record(ctx context.Context, p Payment, c Change) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		_, err := s.recordIn(ctx, tx, p, c, ViaReconcile)
		return err
	})
	if err != nil {
		return fmt.Errorf("record what the provider said: %w", err)
	}
	return nil
}

// waitFor is tryLater for a payment that is fine, just not settled yet (the
// slow card still processing): it waits the same way, but it isn't an error.
func (s *Store) waitFor(ctx context.Context, p Payment, why string) error {
	if err := s.later(ctx, p.OrderID); err != nil {
		return fmt.Errorf("%s: %w", why, err)
	}
	return nil
}

// tryLater counts a failed try and pushes the payment's next check back
// (backoff), then returns cause. The order is untouched.
func (s *Store) tryLater(ctx context.Context, orderID uuid.UUID, cause error) error {
	if err := s.later(ctx, orderID); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// later counts one more try and pushes the payment's next check back by the
// backoff for that many tries.
func (s *Store) later(ctx context.Context, orderID uuid.UUID) error {
	var attempts int
	err := s.pool.QueryRow(ctx,
		`UPDATE payments SET attempts = attempts + 1 WHERE order_id = $1 RETURNING attempts`, orderID).Scan(&attempts)
	if err == nil {
		_, err = s.pool.Exec(ctx,
			`UPDATE payments SET next_check_at = now() + make_interval(secs => $2) WHERE order_id = $1`,
			orderID, backoff(attempts).Seconds())
	}
	if err != nil {
		return fmt.Errorf("schedule the next try: %w", err)
	}
	return nil
}

// backoff is how long to wait after the attempts-th failed try in a row:
// 2 s, 4 s, 8 s... up to 5 minutes, each with up to 20% added at random
// (jitter), so many payments that failed together (the provider was down)
// don't all come back at the same moment.
func backoff(attempts int) time.Duration {
	d := backoffMax
	if attempts < 20 { // beyond that, 2^attempts would only overflow
		d = min(backoffBase<<max(attempts-1, 0), backoffMax)
	}
	return d + time.Duration(rand.Int64N(int64(d)/5+1))
}

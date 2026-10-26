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
func (s *Store) Sync(ctx context.Context, orderID uuid.UUID) error {
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
			if err := s.move(ctx, p, step); err != nil {
				return err
			}
		case Lookup, Check:
			return nil // finding out is the reconcile loop's job
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
func (s *Store) move(ctx context.Context, p Payment, step Step) error {
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
		_, err := s.recordIn(ctx, tx, p, Change{Status: answer.Status, ProviderPaymentID: &answer.ID, DeclineCode: answer.DeclineCode}, ViaRequest)
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

// tryLater counts a failed try and pushes the payment's next check back
// (backoff), then returns cause. The order is untouched.
func (s *Store) tryLater(ctx context.Context, orderID uuid.UUID, cause error) error {
	var attempts int
	err := s.pool.QueryRow(ctx,
		`UPDATE payments SET attempts = attempts + 1 WHERE order_id = $1 RETURNING attempts`, orderID).Scan(&attempts)
	if err == nil {
		_, err = s.pool.Exec(ctx,
			`UPDATE payments SET next_check_at = now() + make_interval(secs => $2) WHERE order_id = $1`,
			orderID, backoff(attempts).Seconds())
	}
	if err != nil {
		return errors.Join(cause, fmt.Errorf("schedule the next try: %w", err))
	}
	return cause
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

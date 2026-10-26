package payments

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atalaymudanyali/quickbytes/internal/orders"
)

// AuthorizeGrace is how long a new payment is left to checkout's own call
// before the reconcile loop may look at it: far longer than the call can take
// (its timeout is 5 seconds), so the loop never goes looking for a payment
// while the request is still waiting for the answer. If checkout dies
// mid-call, the loop finds the payment pending after this.
const AuthorizeGrace = time.Minute

// Finish is work that must happen together with recording a payment's
// answer: it runs last inside that transaction. Checkout stores its final
// idempotent answer this way.
type Finish func(ctx context.Context, tx pgx.Tx, p Payment) error

// key is the idempotency key for one step of one payment, the same on every
// try: "<payment_id>:authorize", "<payment_id>:capture"...
func key(paymentID uuid.UUID, step string) string {
	return paymentID.String() + ":" + step
}

// Authorize asks the provider to hold a pending payment's amount on the card,
// then records what came back, and moves the order to match, in one
// transaction:
//
//   - authorized: order paid
//   - failed (declined, or the provider refused the card): order cancelled,
//     payment_failed
//   - processing (the slow card): order stays placed, waiting
//   - no answer (a timeout...): payment unknown, order stays placed. The
//     reconcile loop finds out what happened. Nothing ever authorizes again:
//     the card number is used for this one call and kept nowhere.
//
// The provider call runs outside any transaction: nothing waits on a lock for
// up to 5 seconds. It's also detached from ctx's cancellation (the customer
// closing the app): once the money may move, we want to hear and record the
// answer. The client's own timeout still bounds the wait.
//
// An error means the answer couldn't be recorded (a database problem). The
// payment then stays pending, and the reconcile loop finds it after
// AuthorizeGrace.
func (s *Store) Authorize(ctx context.Context, p Payment, cardNumber string, finish Finish) (Payment, error) {
	ctx = context.WithoutCancel(ctx)
	answer, err := s.provider.Authorize(ctx, AuthorizeRequest{
		Key:         key(p.ID, "authorize"),
		AmountKurus: p.AmountKurus,
		CardNumber:  cardNumber,
		Reference:   p.OrderID.String(),
	})

	var c Change
	switch {
	case err == nil:
		c = Change{Status: answer.Status, ProviderPaymentID: &answer.ID, DeclineCode: answer.DeclineCode}
	case IsRefused(err):
		// Not a card the provider takes: a final no, though no bank said it.
		c = Change{Status: Failed, DeclineCode: ptr(CardRejected)}
	default:
		s.logger.Warn("authorize got no answer; the payment is unknown until reconciled",
			"payment_id", p.ID, "order_id", p.OrderID, "err", err)
		c = Change{Status: Unknown}
	}

	var out Payment
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		if out, err = s.recordIn(ctx, tx, p, c, ViaRequest); err != nil {
			return err
		}
		if finish != nil {
			return finish(ctx, tx, out)
		}
		return nil
	})
	if err != nil {
		return Payment{}, fmt.Errorf("record authorize answer: %w", err)
	}
	return out, nil
}

// recordIn writes what we learned about a payment and, when the payment
// settled while the order still waits on it, moves the order (paid, or
// cancelled with payment_failed). Moves needing the provider (capture, void,
// refund) are not made here.
//
// Locks: the order first, then the payment (see orders.LockIn).
func (s *Store) recordIn(ctx context.Context, tx pgx.Tx, p Payment, c Change, via Via) (Payment, error) {
	orderStatus, err := orders.LockIn(ctx, tx, p.OrderID)
	if err != nil {
		return Payment{}, err
	}
	out, err := updateIn(ctx, tx, p.ID, c, via)
	if err != nil {
		return Payment{}, err
	}
	var move orders.Action
	switch nextStep(orderStatus, out.Status) {
	case PayOrder:
		move = orders.Pay
	case FailOrder:
		move = orders.FailPayment
	default:
		return out, nil
	}
	if _, err := orders.TransitionIn(ctx, tx, p.OrderID, move, orders.System); err != nil {
		return Payment{}, fmt.Errorf("move order after payment: %w", err)
	}
	return out, nil
}

func ptr[T any](v T) *T { return &v }

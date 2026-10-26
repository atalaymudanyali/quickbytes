package payments_test

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atalaymudanyali/quickbytes/internal/orders"
	"github.com/atalaymudanyali/quickbytes/internal/payments"
	"github.com/atalaymudanyali/quickbytes/internal/payments/paymentstest"
	"github.com/atalaymudanyali/quickbytes/internal/testutil"
)

type authFixture struct {
	pool     *pgxpool.Pool
	payments *payments.Store
	provider *paymentstest.Fake
}

func setupAuth(t *testing.T) authFixture {
	t.Helper()
	pool := testutil.DB(t)
	fake := paymentstest.NewFake()
	return authFixture{
		pool:     pool,
		payments: payments.NewStore(pool, fake, slog.New(slog.NewTextHandler(io.Discard, nil))),
		provider: fake,
	}
}

// pending places an order and writes its payment as checkout's first step
// does.
func (f authFixture) pending(t *testing.T) payments.Payment {
	t.Helper()
	order := payments.NewOrderForTest(t, f.pool)
	var p payments.Payment
	err := pgx.BeginFunc(context.Background(), f.pool, func(tx pgx.Tx) error {
		var err error
		p, err = payments.CreateIn(context.Background(), tx, order, 47990, "4242", payments.AuthorizeGrace)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (f authFixture) order(t *testing.T, id uuid.UUID) (orders.Status, *orders.CancelReason) {
	t.Helper()
	var s orders.Status
	var r *orders.CancelReason
	if err := f.pool.QueryRow(context.Background(),
		`SELECT status, cancel_reason FROM orders WHERE id = $1`, id).Scan(&s, &r); err != nil {
		t.Fatal(err)
	}
	return s, r
}

func (f authFixture) ledger(t *testing.T, p payments.Payment) []string {
	t.Helper()
	events, err := f.payments.Events(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = string(e.Status) + " via " + string(e.Via)
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Every test card's answer, recorded with the order moved to match.
func TestAuthorizeEachCard(t *testing.T) {
	f := setupAuth(t)
	ctx := context.Background()
	cases := []struct {
		card        string
		pay         payments.Status
		decline     string
		order       orders.Status
		orderReason orders.CancelReason
	}{
		{paymentstest.CardApprove, payments.Authorized, "", orders.Paid, ""},
		{paymentstest.CardDecline, payments.Failed, payments.CardDeclined, orders.Cancelled, orders.PaymentFailed},
		{paymentstest.CardInsufficient, payments.Failed, payments.InsufficientFunds, orders.Cancelled, orders.PaymentFailed},
		{paymentstest.CardSlow, payments.Processing, "", orders.Placed, ""},
		// The money is held, but we never heard: unknown, and the order waits.
		{paymentstest.CardHang, payments.Unknown, "", orders.Placed, ""},
		// The provider refuses the card itself: a final no, like a decline.
		{"1111222233334444", payments.Failed, payments.CardRejected, orders.Cancelled, orders.PaymentFailed},
	}
	for _, tc := range cases {
		t.Run(tc.card, func(t *testing.T) {
			p := f.pending(t)
			got, err := f.payments.Authorize(ctx, p, tc.card, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != tc.pay || (tc.decline == "") != (got.DeclineCode == nil) ||
				(got.DeclineCode != nil && *got.DeclineCode != tc.decline) {
				t.Fatalf("payment %s (%v), want %s (%q)", got.Status, got.DeclineCode, tc.pay, tc.decline)
			}
			status, reason := f.order(t, p.OrderID)
			if status != tc.order || (tc.orderReason == "") != (reason == nil) || (reason != nil && *reason != tc.orderReason) {
				t.Fatalf("order %s (%v), want %s (%q)", status, reason, tc.order, tc.orderReason)
			}
			if want := []string{"pending via request", string(tc.pay) + " via request"}; !equal(f.ledger(t, got), want) {
				t.Fatalf("ledger %v, want %v", f.ledger(t, got), want)
			}
		})
	}
}

// The provider is asked with our payment's own key and the order as the
// reference, so a timed-out authorize can be found and is never repeated.
func TestAuthorizeKeyAndReference(t *testing.T) {
	f := setupAuth(t)
	p := f.pending(t)
	if _, err := f.payments.Authorize(context.Background(), p, paymentstest.CardHang, nil); err != nil {
		t.Fatal(err)
	}
	held := f.provider.Payments(p.OrderID.String())
	if len(held) != 1 || held[0].Status != payments.Authorized {
		t.Fatalf("provider holds %+v for the order, want one authorized payment", held)
	}
	// Asking again with the same key (what a retry would send) gets the same
	// payment, not a second one.
	again, err := f.provider.Authorize(context.Background(), payments.AuthorizeRequest{
		Key: p.ID.String() + ":authorize", AmountKurus: p.AmountKurus, CardNumber: paymentstest.CardHang, Reference: p.OrderID.String(),
	})
	if err != nil || again.ID != held[0].ID || len(f.provider.Payments(p.OrderID.String())) != 1 {
		t.Fatalf("retry with the key: %+v, %v; want payment %s and still one", again, err, held[0].ID)
	}
}

// The customer cancels while the card is being authorized. The answer is
// still recorded (the money is held!), but the order is not paid: it stays
// cancelled, and the hold is left for the void step to drop.
func TestAuthorizeAfterTheOrderWasCancelled(t *testing.T) {
	f := setupAuth(t)
	ctx := context.Background()
	p := f.pending(t)
	f.provider.BeforeCall = func(string) error {
		if _, err := f.pool.Exec(ctx, `UPDATE orders SET status = 'cancelled', cancel_reason = 'by_customer' WHERE id = $1`, p.OrderID); err != nil {
			t.Error(err)
		}
		return nil
	}
	got, err := f.payments.Authorize(ctx, p, paymentstest.CardApprove, nil)
	if err != nil || got.Status != payments.Authorized {
		t.Fatalf("got %+v, %v; want authorized", got, err)
	}
	if status, _ := f.order(t, p.OrderID); status != orders.Cancelled {
		t.Fatalf("order %s, want still cancelled", status)
	}
}

// The customer closing the app (the request's context ending) doesn't stop
// the answer from being recorded: once money may move, we want to know.
func TestAuthorizeOutlivesTheRequest(t *testing.T) {
	f := setupAuth(t)
	p := f.pending(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := f.payments.Authorize(ctx, p, paymentstest.CardApprove, nil)
	if err != nil || got.Status != payments.Authorized {
		t.Fatalf("got %+v, %v; want authorized despite the cancelled context", got, err)
	}
}

// finish runs in the same transaction as the answer: if it fails, nothing is
// recorded, and the payment stays pending for the reconcile loop.
func TestAuthorizeFinishFailing(t *testing.T) {
	f := setupAuth(t)
	ctx := context.Background()
	p := f.pending(t)
	_, err := f.payments.Authorize(ctx, p, paymentstest.CardApprove, func(context.Context, pgx.Tx, payments.Payment) error {
		return context.DeadlineExceeded
	})
	if err == nil {
		t.Fatal("want an error")
	}
	got, _ := f.payments.ForOrder(ctx, p.OrderID)
	if status, _ := f.order(t, p.OrderID); got.Status != payments.Pending || status != orders.Placed {
		t.Fatalf("payment %s, order %s; want pending and placed", got.Status, status)
	}
}

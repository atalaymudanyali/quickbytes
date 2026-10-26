package payments_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atalaymudanyali/quickbytes/internal/orders"
	"github.com/atalaymudanyali/quickbytes/internal/payments"
	"github.com/atalaymudanyali/quickbytes/internal/payments/paymentstest"
)

// paid makes an order with an authorized payment, through Authorize.
func (f authFixture) paid(t *testing.T) payments.Payment {
	t.Helper()
	p, err := f.payments.Authorize(context.Background(), f.pending(t), paymentstest.CardApprove, nil)
	if err != nil || p.Status != payments.Authorized {
		t.Fatalf("authorize: %+v, %v", p, err)
	}
	return p
}

// setOrder puts the order in a status directly. Moving it through the state
// machine is tested in internal/orders and through HTTP; here only Sync is
// under test.
func (f authFixture) setOrder(t *testing.T, id uuid.UUID, s orders.Status) {
	t.Helper()
	var reason *string
	if s == orders.Cancelled {
		r := "rejected"
		reason = &r
	}
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE orders SET status = $2, cancel_reason = $3 WHERE id = $1`, id, s, reason); err != nil {
		t.Fatal(err)
	}
}

func (f authFixture) payment(t *testing.T, orderID uuid.UUID) payments.Payment {
	t.Helper()
	p, err := f.payments.ForOrder(context.Background(), orderID)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// schedule is the payment's place in the reconcile loop's list.
func (f authFixture) schedule(t *testing.T, orderID uuid.UUID) (attempts int, dueIn time.Duration, never bool) {
	t.Helper()
	var ms float64
	err := f.pool.QueryRow(context.Background(), `
		SELECT attempts,
			CASE WHEN next_check_at = 'infinity' THEN 0 ELSE extract(epoch FROM next_check_at - now()) * 1000 END::float8,
			next_check_at = 'infinity'
		FROM payments WHERE order_id = $1`, orderID).Scan(&attempts, &ms, &never)
	if err != nil {
		t.Fatal(err)
	}
	return attempts, time.Duration(ms) * time.Millisecond, never
}

// Where the order is decides what happens to the money.
func TestSyncFollowsTheOrder(t *testing.T) {
	f := setupAuth(t)
	ctx := context.Background()
	cases := []struct {
		order  orders.Status
		ledger []string
	}{
		{orders.Paid, []string{"pending", "authorized"}},                 // nothing to do yet
		{orders.Accepted, []string{"pending", "authorized", "captured"}}, // take the money
		{orders.Ready, []string{"pending", "authorized", "captured"}},    // a missed capture is caught later too
		{orders.Cancelled, []string{"pending", "authorized", "voided"}},  // drop the hold
	}
	for _, tc := range cases {
		t.Run(string(tc.order), func(t *testing.T) {
			p := f.paid(t)
			f.setOrder(t, p.OrderID, tc.order)
			if err := f.payments.Sync(ctx, p.OrderID); err != nil {
				t.Fatal(err)
			}
			if got := statusesOf(f.ledger(t, p)); !equal(got, tc.ledger) {
				t.Fatalf("ledger %v, want %v", got, tc.ledger)
			}
			if _, _, never := f.schedule(t, p.OrderID); !never {
				t.Fatal("a settled payment is still on the loop's list")
			}
		})
	}
}

// Taken, then the order is cancelled (rejected after accepting): refunded.
// Accepted and cancelled before the capture even happened: captured, then
// refunded in the same Sync. Either way, each step happens once.
func TestSyncRefunds(t *testing.T) {
	f := setupAuth(t)
	ctx := context.Background()

	p := f.paid(t)
	f.setOrder(t, p.OrderID, orders.Accepted)
	if err := f.payments.Sync(ctx, p.OrderID); err != nil {
		t.Fatal(err)
	}
	f.setOrder(t, p.OrderID, orders.Cancelled)
	if err := f.payments.Sync(ctx, p.OrderID); err != nil {
		t.Fatal(err)
	}
	if got, want := statusesOf(f.ledger(t, p)), []string{"pending", "authorized", "captured", "refunded"}; !equal(got, want) {
		t.Fatalf("ledger %v, want %v", got, want)
	}
	// Syncing again changes nothing and calls nobody.
	before := f.provider.Calls("refund")
	if err := f.payments.Sync(ctx, p.OrderID); err != nil || f.provider.Calls("refund") != before {
		t.Fatalf("second sync: %v, %d more refund calls", err, f.provider.Calls("refund")-before)
	}
}

// The provider doesn't answer the capture: Sync says so, the payment stays
// authorized and waits (backoff), each failure waiting longer. When the
// provider is back, the next Sync captures, with the same key: once.
func TestSyncWithoutAnAnswerTriesLater(t *testing.T) {
	f := setupAuth(t)
	ctx := context.Background()
	p := f.paid(t)
	f.setOrder(t, p.OrderID, orders.Accepted)

	f.provider.BeforeCall = func(call string) error {
		if call == "capture" {
			return fmt.Errorf("%w: fake timeout", payments.ErrNoAnswer)
		}
		return nil
	}
	var waits []time.Duration
	for range 2 {
		if err := f.payments.Sync(ctx, p.OrderID); !errors.Is(err, payments.ErrNoAnswer) {
			t.Fatalf("got %v, want ErrNoAnswer", err)
		}
		_, due, _ := f.schedule(t, p.OrderID)
		waits = append(waits, due)
	}
	if got := f.payment(t, p.OrderID).Status; got != payments.Authorized {
		t.Fatalf("payment %s, want still authorized", got)
	}
	if attempts, _, _ := f.schedule(t, p.OrderID); attempts != 2 || waits[0] < time.Second || waits[1] <= waits[0] {
		t.Fatalf("%d attempts, waits %v; want 2, each longer", attempts, waits)
	}

	f.provider.BeforeCall = nil
	if err := f.payments.Sync(ctx, p.OrderID); err != nil {
		t.Fatal(err)
	}
	if got := f.payment(t, p.OrderID).Status; got != payments.Captured {
		t.Fatalf("payment %s, want captured", got)
	}
	if held := f.provider.Payments(p.OrderID.String()); len(held) != 1 || held[0].Status != payments.Captured {
		t.Fatalf("provider holds %+v, want one captured payment", held)
	}
	if attempts, _, never := f.schedule(t, p.OrderID); attempts != 0 || !never {
		t.Fatalf("after capturing: %d attempts, off the list %v; want 0, true", attempts, never)
	}
}

// A move marks the payment due at once and forgets earlier failures.
func TestMarkDueIn(t *testing.T) {
	f := setupAuth(t)
	ctx := context.Background()
	p := f.paid(t)
	if err := f.payments.Sync(ctx, p.OrderID); err != nil { // paid: nothing to do, off the list
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.payments.MarkDueIn(ctx, tx, orders.Change{OrderID: p.OrderID}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if attempts, due, never := f.schedule(t, p.OrderID); attempts != 0 || never || due > 0 {
		t.Fatalf("attempts %d, due in %v, off the list %v; want due now", attempts, due, never)
	}
}

func statusesOf(ledger []string) []string {
	out := make([]string, len(ledger))
	for i, l := range ledger {
		out[i] = l[:len(l)-len(" via request")]
	}
	return out
}

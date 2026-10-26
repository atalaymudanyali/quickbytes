package payments

// End-to-end tests of the reconcile loop: the API's payments in its test
// database, the real provider (its code, its database, real HTTP), and a
// 300 ms client timeout so the hang card really times out.

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atalaymudanyali/quickbytes/internal/orders"
	"github.com/atalaymudanyali/quickbytes/internal/payprovider"
	"github.com/atalaymudanyali/quickbytes/internal/testutil"
)

type e2e struct {
	pool     *pgxpool.Pool
	payments *Store
	provider *PayProvider
}

func setupE2E(t *testing.T, provider *PayProvider) e2e {
	t.Helper()
	pool := testutil.DB(t)
	return e2e{pool: pool, payments: NewStore(pool, provider, slog.New(slog.NewTextHandler(io.Discard, nil))), provider: provider}
}

// checkout is what checkout does: a placed order, its pending payment, and
// the authorize call.
func (e e2e) checkout(t *testing.T, card string) Payment {
	t.Helper()
	ctx := context.Background()
	order := newOrder(t, e.pool)
	var p Payment
	if err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		var err error
		p, err = CreateIn(ctx, tx, order, 47990, card[len(card)-4:], AuthorizeGrace)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	p, err := e.payments.Authorize(ctx, p, card, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// reconcile is one run of the loop, for this test's orders only.
func (e e2e) reconcile(t *testing.T, orderIDs ...uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	// Due now, as if the backoff had run out.
	if _, err := e.pool.Exec(ctx, `UPDATE payments SET next_check_at = now() WHERE order_id = ANY($1) AND next_check_at <> 'infinity'`, orderIDs); err != nil {
		t.Fatal(err)
	}
	ids, err := e.payments.claimDue(ctx, orderIDs)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		_ = e.payments.sync(ctx, id, ViaReconcile)
	}
}

func (e e2e) order(t *testing.T, id uuid.UUID) (orders.Status, *orders.CancelReason) {
	t.Helper()
	var s orders.Status
	var r *orders.CancelReason
	if err := e.pool.QueryRow(context.Background(), `SELECT status, cancel_reason FROM orders WHERE id = $1`, id).Scan(&s, &r); err != nil {
		t.Fatal(err)
	}
	return s, r
}

func (e e2e) ledger(t *testing.T, p Payment) []string {
	t.Helper()
	events, err := e.payments.Events(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = string(ev.Status) + " via " + string(ev.Via)
	}
	return out
}

// settle makes the provider's bank answer a slow card now: its payment is due
// and the provider's settler runs. (The slow delay is an hour, so the
// provider's own tests, settling in the same database, can't do it first.)
func settle(t *testing.T, pp *payprovider.Payments, orderID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	if _, err := testutil.PayProviderDB(t).Exec(ctx,
		`UPDATE payments SET settle_at = now() WHERE reference = $1 AND status = 'processing'`, orderID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pp.SettleDue(ctx); err != nil {
		t.Fatal(err)
	}
}

// atProvider is every payment the provider holds for an order.
func (e e2e) atProvider(t *testing.T, orderID uuid.UUID) []ProviderPayment {
	t.Helper()
	found, err := e.provider.FindByReference(context.Background(), orderID.String())
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// The issue's acceptance test for timeouts. The hang card: the provider holds
// the money but answers too late, so the payment is unknown and the order
// waits. The reconcile loop finds the payment by the order ID and the order
// is paid; accepting it captures. One payment at the provider all along.
func TestHangCardIsReconciled(t *testing.T) {
	e := setupE2E(t, newProvider(t))
	ctx := context.Background()

	start := time.Now()
	p := e.checkout(t, cardHang)
	if p.Status != Unknown || time.Since(start) > testTimeout+time.Second {
		t.Fatalf("checkout: %s after %v, want unknown after about %v", p.Status, time.Since(start), testTimeout)
	}
	if s, _ := e.order(t, p.OrderID); s != orders.Placed {
		t.Fatalf("order %s, want placed while unknown", s)
	}

	e.reconcile(t, p.OrderID)
	if s, _ := e.order(t, p.OrderID); s != orders.Paid {
		t.Fatalf("after reconciling: order %s, want paid", s)
	}
	if got, want := e.ledger(t, p), []string{"pending via request", "unknown via request", "authorized via reconcile"}; !slices.Equal(got, want) {
		t.Fatalf("ledger %v, want %v", got, want)
	}

	// The restaurant accepts; the capture goes to the payment the loop found.
	if _, err := e.pool.Exec(ctx, `UPDATE orders SET status = 'accepted' WHERE id = $1`, p.OrderID); err != nil {
		t.Fatal(err)
	}
	if err := e.payments.Sync(ctx, p.OrderID); err != nil {
		t.Fatal(err)
	}
	held := e.atProvider(t, p.OrderID)
	if len(held) != 1 || held[0].Status != Captured || held[0].AmountKurus != p.AmountKurus {
		t.Fatalf("the provider has %+v, want exactly one payment, captured, for %d", held, p.AmountKurus)
	}
}

// Retried checkouts, each reaching the provider (the worst case: no answer
// each time), still hold the money once: every try sends the same key.
func TestRetriedAuthorizeChargesOnce(t *testing.T) {
	e := setupE2E(t, newProvider(t))
	ctx := context.Background()
	p := e.checkout(t, cardHang)
	for range 3 {
		// Back to pending, as if checkout started over for the same payment.
		if _, err := e.pool.Exec(ctx, `UPDATE payments SET status = 'pending' WHERE id = $1`, p.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := e.payments.Authorize(ctx, p, cardHang, nil); err != nil {
			t.Fatal(err)
		}
	}
	if held := e.atProvider(t, p.OrderID); len(held) != 1 {
		t.Fatalf("the provider has %d payments for the order, want 1", len(held))
	}
}

// The slow card: processing, then the provider's bank answers, and the loop
// sees it on its next GET. Before that, a GET that still says processing
// only makes the payment wait.
func TestSlowCardIsPolled(t *testing.T) {
	provider, pp := newProviderWith(t, time.Hour)
	e := setupE2E(t, provider)
	ctx := context.Background()
	p := e.checkout(t, cardSlow)
	if p.Status != Processing {
		t.Fatalf("checkout: %s, want processing", p.Status)
	}

	e.reconcile(t, p.OrderID) // the provider hasn't settled it yet
	if got, _ := e.payments.ForOrder(ctx, p.OrderID); got.Status != Processing {
		t.Fatalf("payment %s, want still processing", got.Status)
	}
	settle(t, pp, p.OrderID)
	e.reconcile(t, p.OrderID)
	if s, _ := e.order(t, p.OrderID); s != orders.Paid {
		t.Fatalf("order %s, want paid once the bank said yes", s)
	}
	if got, want := e.ledger(t, p), []string{"pending via request", "processing via request", "authorized via reconcile"}; !slices.Equal(got, want) {
		t.Fatalf("ledger %v, want %v", got, want)
	}
}

// The order was cancelled (it expired) while the slow card was processing.
// When the bank says yes, the loop drops the hold at once.
func TestHoldOfACancelledOrderIsDropped(t *testing.T) {
	provider, pp := newProviderWith(t, time.Hour)
	e := setupE2E(t, provider)
	ctx := context.Background()
	p := e.checkout(t, cardSlow)
	if _, err := e.pool.Exec(ctx, `UPDATE orders SET status = 'cancelled', cancel_reason = 'expired' WHERE id = $1`, p.OrderID); err != nil {
		t.Fatal(err)
	}
	settle(t, pp, p.OrderID)
	e.reconcile(t, p.OrderID)
	if held := e.atProvider(t, p.OrderID); len(held) != 1 || held[0].Status != Voided {
		t.Fatalf("the provider has %+v, want one voided payment", held)
	}
}

// A payment whose authorize never reached the provider (checkout died before
// calling, or the request was lost on the way). While it might still arrive,
// the loop waits; after AuthorizeGrace it fails the payment, and the order is
// cancelled like a decline.
func TestAuthorizeThatNeverArrived(t *testing.T) {
	e := setupE2E(t, newProvider(t))
	ctx := context.Background()
	order := newOrder(t, e.pool)
	var p Payment
	if err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		var err error
		p, err = CreateIn(ctx, tx, order, 47990, "4242", AuthorizeGrace)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	e.reconcile(t, order) // too early to tell
	if got, _ := e.payments.ForOrder(ctx, order); got.Status != Pending {
		t.Fatalf("young payment: %s, want still pending", got.Status)
	}

	if _, err := e.pool.Exec(ctx, `UPDATE payments SET created_at = now() - interval '2 minutes' WHERE id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	e.reconcile(t, order)
	got, _ := e.payments.ForOrder(ctx, order)
	if got.Status != Failed || got.DeclineCode == nil || *got.DeclineCode != NoAnswer {
		t.Fatalf("payment %+v, want failed (no_answer)", got)
	}
	if s, r := e.order(t, order); s != orders.Cancelled || r == nil || *r != orders.PaymentFailed {
		t.Fatalf("order %s (%v), want cancelled (payment_failed)", s, r)
	}
}

// Two loops (two API copies) claiming at once never take the same payment.
func TestClaimsDontOverlap(t *testing.T) {
	e := setupE2E(t, newProvider(t))
	ctx := context.Background()
	var ids []uuid.UUID
	for range 10 {
		ids = append(ids, e.checkout(t, cardSlow).OrderID)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE payments SET next_check_at = now() WHERE order_id = ANY($1)`, ids); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	claimed := map[uuid.UUID]int{}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := e.payments.claimDue(ctx, ids)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			for _, id := range got {
				claimed[id]++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(claimed) != len(ids) {
		t.Fatalf("%d of %d payments claimed", len(claimed), len(ids))
	}
	for id, n := range claimed {
		if n != 1 {
			t.Fatalf("payment of order %s claimed %d times", id, n)
		}
	}
	// Claimed means leased: not due again for a while.
	if again, err := e.payments.claimDue(ctx, ids); err != nil || len(again) != 0 {
		t.Fatalf("claimed again: %v, %v; want none", again, err)
	}
}

// The loop stops when the API shuts down.
func TestRunReconcileStops(t *testing.T) {
	e := setupE2E(t, newProvider(t))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		e.payments.RunReconcile(ctx, time.Hour, e.payments.logger)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunReconcile didn't stop")
	}
}

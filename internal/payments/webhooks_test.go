package payments_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atalaymudanyali/quickbytes/internal/orders"
	"github.com/atalaymudanyali/quickbytes/internal/payments"
	"github.com/atalaymudanyali/quickbytes/internal/payments/paymentstest"
)

// webhookFor builds a webhook carrying pp, as the provider's plug would hand
// it over, with a new event ID.
func webhookFor(pp payments.ProviderPayment, orderID uuid.UUID) payments.WebhookEvent {
	return payments.WebhookEvent{
		ID: "evt_" + uuid.NewString(), Type: "payment." + string(pp.Status),
		Payment: pp, Reference: orderID.String(),
	}
}

func (f authFixture) apply(t *testing.T, ev payments.WebhookEvent, want payments.Outcome) {
	t.Helper()
	got, err := f.payments.ApplyWebhook(context.Background(), ev)
	if err != nil {
		t.Fatalf("apply %s: %v", ev.Type, err)
	}
	if got != want {
		t.Fatalf("apply %s: outcome %q, want %q", ev.Type, got, want)
	}
}

// providerPayment is the fake provider's one payment for an order.
func (f authFixture) providerPayment(t *testing.T, orderID uuid.UUID) payments.ProviderPayment {
	t.Helper()
	list := f.provider.Payments(orderID.String())
	if len(list) != 1 {
		t.Fatalf("the provider holds %d payments for the order, want 1", len(list))
	}
	return list[0]
}

// The slow card: the webhook brings the bank's late answer, and the order is
// paid without the reconcile loop asking.
func TestWebhookPaysTheOrder(t *testing.T) {
	f := setupAuth(t)
	ctx := context.Background()
	p, err := f.payments.Authorize(ctx, f.pending(t), paymentstest.CardSlow, nil)
	if err != nil || p.Status != payments.Processing {
		t.Fatalf("authorize: %+v, %v", p, err)
	}
	f.provider.Settle(*p.ProviderPaymentID, true)

	f.apply(t, webhookFor(f.providerPayment(t, p.OrderID), p.OrderID), payments.Applied)

	if s, _ := f.order(t, p.OrderID); s != orders.Paid {
		t.Errorf("order %s, want paid", s)
	}
	want := []string{"pending via request", "processing via request", "authorized via webhook"}
	if got := f.ledger(t, p); !equal(got, want) {
		t.Errorf("ledger = %v, want %v", got, want)
	}
	if f.provider.Calls("get") != 0 {
		t.Error("the provider was asked; the webhook should have been enough")
	}
}

// The same webhook twice: the second is a repeat and changes nothing.
func TestRepeatedWebhookIsIgnored(t *testing.T) {
	f := setupAuth(t)
	ctx := context.Background()
	p, err := f.payments.Authorize(ctx, f.pending(t), paymentstest.CardSlow, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.provider.Settle(*p.ProviderPaymentID, true)
	ev := webhookFor(f.providerPayment(t, p.OrderID), p.OrderID)

	f.apply(t, ev, payments.Applied)
	f.apply(t, ev, payments.Repeat)
	if got := f.ledger(t, p); len(got) != 3 {
		t.Errorf("ledger = %v, want one authorized line", got)
	}
	var paidMoves int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM order_status_history WHERE order_id = $1 AND to_status = 'paid'`, p.OrderID).Scan(&paidMoves); err != nil {
		t.Fatal(err)
	}
	if paidMoves != 1 {
		t.Errorf("the order was moved to paid %d times, want once", paidMoves)
	}
}

// Out of order: "captured" arrives before the older "authorized" (whose
// first try failed and was retried). The older one changes nothing.
func TestOlderWebhookAfterNewerIsStale(t *testing.T) {
	f := setupAuth(t)
	ctx := context.Background()
	p := f.paid(t)
	authorized := webhookFor(f.providerPayment(t, p.OrderID), p.OrderID)
	f.setOrder(t, p.OrderID, orders.Accepted)
	captured, err := f.provider.Capture(ctx, "test-capture", *p.ProviderPaymentID)
	if err != nil {
		t.Fatal(err)
	}

	f.apply(t, webhookFor(captured, p.OrderID), payments.Applied)
	f.apply(t, authorized, payments.Stale)

	if got := f.payment(t, p.OrderID); got.Status != payments.Captured {
		t.Errorf("payment %s, want captured", got.Status)
	}
	want := []string{"pending via request", "authorized via request", "captured via webhook"}
	if got := f.ledger(t, p); !equal(got, want) {
		t.Errorf("ledger = %v, want %v", got, want)
	}
}

// Authorize timed out, so we never learned the provider's ID: the webhook
// finds the payment by our reference (the order ID) and settles it at once.
func TestWebhookSettlesAnUnknownPayment(t *testing.T) {
	f := setupAuth(t)
	p, err := f.payments.Authorize(context.Background(), f.pending(t), paymentstest.CardHang, nil)
	if err != nil || p.Status != payments.Unknown {
		t.Fatalf("authorize: %+v, %v", p, err)
	}
	pp := f.providerPayment(t, p.OrderID)

	f.apply(t, webhookFor(pp, p.OrderID), payments.Applied)

	got := f.payment(t, p.OrderID)
	if got.Status != payments.Authorized || got.ProviderPaymentID == nil || *got.ProviderPaymentID != pp.ID {
		t.Errorf("payment = %+v", got)
	}
	if s, _ := f.order(t, p.OrderID); s != orders.Paid {
		t.Errorf("order %s, want paid", s)
	}
}

// The race the ratchet fixes: the provider holds the money at once and its
// webhook arrives while our own authorize call is still waiting. Then the
// call times out. "No answer" must not wipe out what the webhook told us.
func TestTimeoutAfterWebhookKeepsTheWebhooksNews(t *testing.T) {
	f := setupAuth(t)
	pending := f.pending(t)
	held := payments.ProviderPayment{ID: "pay_" + uuid.NewString(), Status: payments.Authorized, AmountKurus: pending.AmountKurus}
	f.provider.BeforeCall = func(string) error {
		// While "waiting" for the provider, its webhook comes in.
		f.apply(t, webhookFor(held, pending.OrderID), payments.Applied)
		return payments.ErrNoAnswer // and then our call times out
	}

	p, err := f.payments.Authorize(context.Background(), pending, paymentstest.CardApprove, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != payments.Authorized {
		t.Errorf("Authorize returned %s, want authorized (from the webhook)", p.Status)
	}
	if s, _ := f.order(t, p.OrderID); s != orders.Paid {
		t.Errorf("order %s, want paid", s)
	}
	want := []string{"pending via request", "authorized via webhook"}
	if got := f.ledger(t, p); !equal(got, want) {
		t.Errorf("ledger = %v, want %v (no unknown line)", got, want)
	}
}

// A webhook about a payment we don't have is kept in the guest book and
// changes nothing.
func TestWebhookForNoPaymentOfOurs(t *testing.T) {
	f := setupAuth(t)
	stranger := payments.ProviderPayment{ID: "pay_" + uuid.NewString(), Status: payments.Authorized, AmountKurus: 100}
	f.apply(t, webhookFor(stranger, uuid.New()), payments.Unmatched)

	notOurs := webhookFor(stranger, uuid.Nil)
	notOurs.Reference = "someone-elses-label"
	f.apply(t, notOurs, payments.Unmatched)
	f.apply(t, notOurs, payments.Repeat) // remembered all the same
}

// The provider ID matches, the amount doesn't: not trusted.
func TestWebhookForAnotherAmountIsIgnored(t *testing.T) {
	f := setupAuth(t)
	ctx := context.Background()
	p, err := f.payments.Authorize(ctx, f.pending(t), paymentstest.CardSlow, nil)
	if err != nil {
		t.Fatal(err)
	}
	pp := f.providerPayment(t, p.OrderID)
	pp.Status, pp.AmountKurus = payments.Authorized, 1

	f.apply(t, webhookFor(pp, p.OrderID), payments.Mismatch)
	if got := f.payment(t, p.OrderID); got.Status != payments.Processing {
		t.Errorf("payment %s, want still processing", got.Status)
	}
}

// "failed" about a payment we know is authorized contradicts us: we keep what
// we know.
func TestContradictingWebhookIsIgnored(t *testing.T) {
	f := setupAuth(t)
	p := f.paid(t)
	pp := f.providerPayment(t, p.OrderID)
	declined := payments.CardDeclined
	pp.Status, pp.DeclineCode = payments.Failed, &declined

	f.apply(t, webhookFor(pp, p.OrderID), payments.Stale)
	if got := f.payment(t, p.OrderID); got.Status != payments.Authorized {
		t.Errorf("payment %s, want still authorized", got.Status)
	}
	if s, _ := f.order(t, p.OrderID); s != orders.Paid {
		t.Errorf("order %s, want still paid", s)
	}
}

// Break it: applying fails halfway (here, the order stays locked past the
// deadline). Everything is undone, the guest-book line too, so the
// provider's retry is applied for real instead of being taken for a repeat.
func TestFailedApplyIsRetriedForReal(t *testing.T) {
	f := setupAuth(t)
	ctx := context.Background()
	p, err := f.payments.Authorize(ctx, f.pending(t), paymentstest.CardSlow, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.provider.Settle(*p.ProviderPaymentID, true)
	ev := webhookFor(f.providerPayment(t, p.OrderID), p.OrderID)

	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM orders WHERE id = $1 FOR UPDATE`, p.OrderID); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if _, err := f.payments.ApplyWebhook(short, ev); err == nil {
		t.Fatal("apply worked while the order was locked")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	var seen int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM webhook_events WHERE event_id = $1`, ev.ID).Scan(&seen); err != nil {
		t.Fatal(err)
	}
	if seen != 0 {
		t.Fatal("the failed webhook is in the guest book: its retry would be skipped")
	}
	f.apply(t, ev, payments.Applied) // the provider's retry
	if s, _ := f.order(t, p.OrderID); s != orders.Paid {
		t.Errorf("order %s, want paid", s)
	}
}

// The order was cancelled while the card was still processing. The webhook
// says "authorized": the hold must be dropped. The webhook only records it
// and marks the payment due; the reconcile loop voids it.
func TestWebhookLeavesTheMoneyStepToTheLoop(t *testing.T) {
	f := setupAuth(t)
	ctx := context.Background()
	p, err := f.payments.Authorize(ctx, f.pending(t), paymentstest.CardSlow, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.setOrder(t, p.OrderID, orders.Cancelled)
	f.provider.Settle(*p.ProviderPaymentID, true)

	f.apply(t, webhookFor(f.providerPayment(t, p.OrderID), p.OrderID), payments.Applied)
	if f.provider.Calls("void") != 0 {
		t.Error("the webhook called the provider itself")
	}
	if _, dueIn, never := f.schedule(t, p.OrderID); never || dueIn > 0 {
		t.Fatalf("payment not due after the webhook (due in %v)", dueIn)
	}

	if err := f.payments.ReconcileForTest(ctx, p.OrderID); err != nil {
		t.Fatal(err)
	}
	if got := f.payment(t, p.OrderID); got.Status != payments.Voided {
		t.Errorf("payment %s, want voided", got.Status)
	}
}

// Sanity: ApplyWebhook's errors aren't the plug's "bad webhook" ones; a
// database problem is a 500, not a 400.
func TestApplyErrorIsNotABadWebhook(t *testing.T) {
	f := setupAuth(t)
	done, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := f.payments.ApplyWebhook(done, webhookFor(payments.ProviderPayment{ID: "pay_x", Status: payments.Authorized}, uuid.New()))
	if err == nil || errors.Is(err, payments.ErrBadWebhook) {
		t.Errorf("err = %v", err)
	}
}

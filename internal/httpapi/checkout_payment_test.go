package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atalaymudanyali/quickbytes/internal/orders"
	"github.com/atalaymudanyali/quickbytes/internal/payments"
	"github.com/atalaymudanyali/quickbytes/internal/payments/paymentstest"
	"github.com/atalaymudanyali/quickbytes/internal/testutil"
)

func ledgerOf(p *paymentDetail) []payments.Status {
	out := make([]payments.Status, len(p.Events))
	for i, e := range p.Events {
		out[i] = e.Status
	}
	return out
}

// What checkout answers for each kind of card, and that GET shows the same.
func TestCheckoutPays(t *testing.T) {
	f := newOrderFixture(t)
	cases := []struct {
		name    string
		card    string
		order   orders.Status
		reason  orders.CancelReason
		pay     payments.Status
		decline string
	}{
		{"approved", paymentstest.CardApprove, orders.Paid, "", payments.Authorized, ""},
		{"declined", paymentstest.CardDecline, orders.Cancelled, orders.PaymentFailed, payments.Failed, payments.CardDeclined},
		{"slow card", paymentstest.CardSlow, orders.Placed, "", payments.Processing, ""},
		{"no answer", paymentstest.CardHang, orders.Placed, "", payments.Unknown, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			me, home := f.customer(t)
			f.fill(t, me)
			o := decode[orderDetail](t, f.payWith(t, me, home, tc.card), http.StatusCreated)

			if o.Status != tc.order || (tc.reason == "") != (o.CancelReason == nil) || (o.CancelReason != nil && *o.CancelReason != tc.reason) {
				t.Fatalf("order %s (%v), want %s (%q)", o.Status, o.CancelReason, tc.order, tc.reason)
			}
			p := o.Payment
			if p == nil || p.Status != tc.pay || p.AmountKurus != o.TotalKurus || p.CardLast4 != tc.card[len(tc.card)-4:] {
				t.Fatalf("payment %+v, want %s for %d kuruş", p, tc.pay, o.TotalKurus)
			}
			if (tc.decline == "") != (p.DeclineCode == nil) || (p.DeclineCode != nil && *p.DeclineCode != tc.decline) {
				t.Fatalf("decline code %v, want %q", p.DeclineCode, tc.decline)
			}
			if got := ledgerOf(p); len(got) != 2 || got[0] != payments.Pending || got[1] != tc.pay {
				t.Fatalf("ledger %v, want pending then %s", got, tc.pay)
			}
			if tc.order != orders.Placed {
				if last := o.History[len(o.History)-1]; last.To != tc.order || last.By != "system" {
					t.Fatalf("history ends with %+v, want the system moving it to %s", last, tc.order)
				}
			}

			got := decode[orderDetail](t, send(t, f.h, http.MethodGet, "/orders/"+o.ID.String(), me, ""), http.StatusOK)
			if got.Status != o.Status || got.Payment == nil || got.Payment.Status != p.Status || len(got.Payment.Events) != 2 {
				t.Fatalf("GET shows %s / %+v, checkout said %s / %s", got.Status, got.Payment, o.Status, p.Status)
			}
		})
	}
}

// A retry that arrives while the card is still being authorized gets the
// answer stored before the call (placed, payment pending): never "your cart
// is empty", which would be true but misleading. Once the call is done,
// retries get the final answer. The card is authorized once.
func TestCheckoutRetryDuringTheCall(t *testing.T) {
	f := newOrderFixture(t)
	me, home := f.customer(t)
	f.fill(t, me)
	key := uuid.NewString()
	body := checkoutBody(home, paymentstest.CardApprove)

	inCall, release := make(chan struct{}), make(chan struct{})
	f.provider.BeforeCall = func(call string) error {
		if call == "authorize" {
			close(inCall)
			<-release
		}
		return nil
	}
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- sendWithKey(t, f.h, "/orders", me, key, body) }()

	select {
	case <-inCall:
	case <-time.After(5 * time.Second):
		t.Fatal("checkout never called the provider")
	}
	mid := sendWithKey(t, f.h, "/orders", me, key, body)
	m := decode[orderDetail](t, mid, http.StatusCreated)
	if mid.Header().Get(replayedHeader) != "true" || m.Status != orders.Placed || m.Payment == nil || m.Payment.Status != payments.Pending {
		t.Fatalf("retry during the call: %s, payment %+v; want a replay: placed, pending", m.Status, m.Payment)
	}

	close(release)
	done := decode[orderDetail](t, <-first, http.StatusCreated)
	if done.Status != orders.Paid || done.Payment.Status != payments.Authorized {
		t.Fatalf("first attempt: %s / %s, want paid / authorized", done.Status, done.Payment.Status)
	}
	after := sendWithKey(t, f.h, "/orders", me, key, body)
	if a := decode[orderDetail](t, after, http.StatusCreated); a.ID != done.ID || a.Status != orders.Paid {
		t.Fatalf("retry after the call: %s %s, want order %s, paid", a.ID, a.Status, done.ID)
	}
	if n := f.provider.Calls("authorize"); n != 1 {
		t.Fatalf("%d authorize calls, want 1", n)
	}
}

// A missing or malformed card number is refused before anything happens: no
// order, no call to the provider, and the cart is untouched.
func TestCheckoutNeedsACardNumber(t *testing.T) {
	f := newOrderFixture(t)
	me, home := f.customer(t)
	f.fill(t, me)
	for name, body := range map[string]string{
		"missing":   `{"address_id": "` + home + `"}`,
		"too short": checkoutBody(home, "4242"),
		"letters":   checkoutBody(home, "4242-4242-4242-4242"),
	} {
		t.Run(name, func(t *testing.T) {
			rec := sendWithKey(t, f.h, "/orders", me, uuid.NewString(), body)
			if e := decodeError(t, rec); rec.Code != http.StatusUnprocessableEntity || e.Fields["card_number"] == "" {
				t.Fatalf("got %d %+v, want 422 naming card_number", rec.Code, e)
			}
		})
	}
	if n := f.orderCount(t, me); n != 0 || f.cartLines(t, me) == 0 || f.provider.Calls("authorize") != 0 {
		t.Fatalf("%d orders, %d cart lines, %d calls; want 0, a full cart, 0", n, f.cartLines(t, me), f.provider.Calls("authorize"))
	}
}

// The full card number is used for the one call and kept nowhere: not in the
// payment, its ledger, or the stored idempotent answer.
func TestCardNumberIsNeverStored(t *testing.T) {
	f := newOrderFixture(t)
	me, home := f.customer(t)
	f.fill(t, me)
	o := decode[orderDetail](t, f.payWith(t, me, home, paymentstest.CardApprove), http.StatusCreated)

	ctx := context.Background()
	var found int
	err := testutil.DB(t).QueryRow(ctx, `
		SELECT (SELECT count(*) FROM payments p WHERE p.order_id = $1 AND row_to_json(p)::text LIKE '%' || $2 || '%')
		     + (SELECT count(*) FROM payment_events e JOIN payments p ON p.id = e.payment_id
		        WHERE p.order_id = $1 AND row_to_json(e)::text LIKE '%' || $2 || '%')
		     + (SELECT count(*) FROM idempotency_keys WHERE position(convert_to($2, 'UTF8') IN response_body) > 0)`,
		o.ID, paymentstest.CardApprove).Scan(&found)
	if err != nil {
		t.Fatal(err)
	}
	if found != 0 {
		t.Fatalf("the card number is stored in %d places", found)
	}
	raw, _ := json.Marshal(o)
	if strings.Contains(string(raw), paymentstest.CardApprove) {
		t.Fatal("the answer contains the card number")
	}
}

// The restaurant sees the order but not the customer's payment.
func TestRestaurantDoesntSeeThePayment(t *testing.T) {
	f := newOrderFixture(t)
	me, home := f.customer(t)
	f.fill(t, me)
	o := decode[orderDetail](t, f.payWith(t, me, home, paymentstest.CardApprove), http.StatusCreated)

	rec := send(t, f.h, http.MethodPost, "/orders/"+o.ID.String()+"/accept", f.owner, "")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"payment"`) {
		t.Fatalf("accept: %d %s; want 200 without a payment", rec.Code, rec.Body)
	}
}

// The money follows every button: accept captures, reject or cancel of a
// held payment voids it, reject after accepting refunds. The customer sees
// it in the order's payment and ledger.
func TestMoneyFollowsTheOrder(t *testing.T) {
	f := newOrderFixture(t)
	cases := []struct {
		name    string
		buttons [][2]string // who presses what, in order: "owner"/"customer", action
		ledger  []payments.Status
	}{
		{"accept", [][2]string{{"owner", "accept"}},
			[]payments.Status{payments.Pending, payments.Authorized, payments.Captured}},
		{"reject a paid order", [][2]string{{"owner", "reject"}},
			[]payments.Status{payments.Pending, payments.Authorized, payments.Voided}},
		{"reject after accepting", [][2]string{{"owner", "accept"}, {"owner", "reject"}},
			[]payments.Status{payments.Pending, payments.Authorized, payments.Captured, payments.Refunded}},
		{"customer cancels", [][2]string{{"customer", "cancel"}},
			[]payments.Status{payments.Pending, payments.Authorized, payments.Voided}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			me, home := f.customer(t)
			o := f.placePaid(t, me, home)
			path := "/orders/" + o.ID.String()
			for _, b := range tc.buttons {
				token := me
				if b[0] == "owner" {
					token = f.owner
				}
				if rec := send(t, f.h, http.MethodPost, path+"/"+b[1], token, ""); rec.Code != http.StatusOK {
					t.Fatalf("%s %s: %d %s", b[0], b[1], rec.Code, rec.Body)
				}
			}
			got := decode[orderDetail](t, send(t, f.h, http.MethodGet, path, me, ""), http.StatusOK)
			if l := ledgerOf(got.Payment); !slices.Equal(l, tc.ledger) {
				t.Fatalf("ledger %v, want %v", l, tc.ledger)
			}
			if held := f.provider.Payments(o.ID.String()); len(held) != 1 || held[0].Status != tc.ledger[len(tc.ledger)-1] {
				t.Fatalf("the provider has %+v, want one payment, %s", held, tc.ledger[len(tc.ledger)-1])
			}
		})
	}
}

// The capture times out. The restaurant's accept still stands (they did
// accept), the payment stays authorized, and it's left due for the
// reconcile loop, which then captures it once.
func TestAcceptStandsWhenTheCaptureTimesOut(t *testing.T) {
	f := newOrderFixture(t)
	me, home := f.customer(t)
	o := f.placePaid(t, me, home)
	f.provider.BeforeCall = func(call string) error {
		if call == "capture" {
			return payments.ErrNoAnswer
		}
		return nil
	}
	rec := send(t, f.h, http.MethodPost, "/orders/"+o.ID.String()+"/accept", f.owner, "")
	if a := decode[orderDetail](t, rec, http.StatusOK); a.Status != orders.Accepted {
		t.Fatalf("accept: %s, want accepted", a.Status)
	}
	ctx := context.Background()
	p, err := f.srv.payments.ForOrder(ctx, o.ID)
	if err != nil || p.Status != payments.Authorized {
		t.Fatalf("payment %+v, %v; want still authorized", p, err)
	}
	var due bool
	if err := testutil.DB(t).QueryRow(ctx,
		`SELECT next_check_at < now() + interval '1 hour' FROM payments WHERE order_id = $1`, o.ID).Scan(&due); err != nil || !due {
		t.Fatalf("payment due soon: %v, %v; want true", due, err)
	}

	f.provider.BeforeCall = nil
	if err := f.srv.payments.Sync(ctx, o.ID); err != nil { // what the loop will do
		t.Fatal(err)
	}
	if held := f.provider.Payments(o.ID.String()); len(held) != 1 || held[0].Status != payments.Captured {
		t.Fatalf("the provider has %+v, want one captured payment", held)
	}
}

// A move notes, in its own commit, that the payment needs work, so a crash
// right after the move (before the money step runs) loses nothing: the
// payment is due for the reconcile loop.
func TestMoveMarksThePaymentDue(t *testing.T) {
	f := newOrderFixture(t)
	me, home := f.customer(t)
	o := f.placePaid(t, me, home)
	ctx := context.Background()
	db := testutil.DB(t)
	if err := f.srv.payments.Sync(ctx, o.ID); err != nil { // paid: nothing to do, off the list
		t.Fatal(err)
	}

	var customer uuid.UUID
	if err := db.QueryRow(ctx, `SELECT customer_profile_id FROM orders WHERE id = $1`, o.ID).Scan(&customer); err != nil {
		t.Fatal(err)
	}
	// The cancel, and then "the crash": no Sync.
	if _, err := f.srv.orders.Transition(ctx, o.ID, orders.Cancel, orders.Actor{Role: "customer", ProfileID: customer}); err != nil {
		t.Fatal(err)
	}
	var due bool
	if err := db.QueryRow(ctx, `SELECT next_check_at <= now() FROM payments WHERE order_id = $1`, o.ID).Scan(&due); err != nil || !due {
		t.Fatalf("payment due: %v, %v; want due now", due, err)
	}
}

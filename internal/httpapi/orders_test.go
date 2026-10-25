package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/atalaymudanyali/quickbytes/internal/carts"
	"github.com/atalaymudanyali/quickbytes/internal/orders"
)

// orderFixture is the cart fixture plus the server itself, for moves no endpoint
// offers yet (the payment arriving).
type orderFixture struct {
	cartFixture
	srv *Server
}

func newOrderFixture(t *testing.T) orderFixture {
	t.Helper()
	srv, h := newAuthTestServer(t)
	return orderFixture{cartFixture: newCartFixtureWith(t, h), srv: srv}
}

// customer signs up a customer with one saved address.
func (f orderFixture) customer(t *testing.T) (token, addressID string) {
	t.Helper()
	token = signUp(t, f.h).Tokens.AccessToken
	return token, createAddress(t, f.h, token, homeBody).ID.String()
}

// fill puts 2 × Adana Büyük and 3 × Ayran in the cart: 1180 TL + 29,90 TL.
func (f orderFixture) fill(t *testing.T, token string) {
	t.Helper()
	decode[carts.Cart](t, f.add(t, token, f.adana.ID.String(), 2, f.buyuk), http.StatusOK)
	decode[carts.Cart](t, f.add(t, token, f.ayran.ID.String(), 3), http.StatusOK)
}

func (f orderFixture) checkout(t *testing.T, token, addressID string) *httptest.ResponseRecorder {
	t.Helper()
	return send(t, f.h, http.MethodPost, "/orders", token, `{"address_id": "`+addressID+`"}`)
}

func TestCheckoutAndCancel(t *testing.T) {
	f := newOrderFixture(t)
	me, home := f.customer(t)
	f.fill(t, me)

	o := decode[orderDetail](t, f.checkout(t, me, home), http.StatusCreated)
	if o.Status != orders.Placed || o.TotalKurus != 120990 || len(o.Items) != 2 || o.Address.Label != "Ev" {
		t.Fatalf("order = %+v; want placed, 1209,90 TL, 2 lines, to Ev", o.Order)
	}
	if len(o.History) != 1 || o.History[0].From != nil || o.History[0].To != orders.Placed || o.History[0].By != "customer" {
		t.Fatalf("history = %+v; want one entry: placed by the customer", o.History)
	}
	if c := decode[carts.Cart](t, send(t, f.h, http.MethodGet, "/cart", me, ""), http.StatusOK); len(c.Items) != 0 {
		t.Fatalf("cart after checkout has %d lines, want 0", len(c.Items))
	}

	list := decode[orderList](t, send(t, f.h, http.MethodGet, "/orders", me, ""), http.StatusOK)
	if len(list.Orders) != 1 || list.Orders[0].ID != o.ID {
		t.Fatalf("list = %+v, want the one order", list.Orders)
	}
	path := "/orders/" + o.ID.String()
	if got := decode[orderDetail](t, send(t, f.h, http.MethodGet, path, me, ""), http.StatusOK); got.ID != o.ID || len(got.History) != 1 {
		t.Fatalf("get = %+v", got)
	}

	c := decode[orderDetail](t, send(t, f.h, http.MethodPost, path+"/cancel", me, ""), http.StatusOK)
	if c.Status != orders.Cancelled || *c.CancelReason != orders.ByCustomer || len(c.History) != 2 {
		t.Fatalf("after cancel: %s (%v), %d history entries; want cancelled (by_customer), 2", c.Status, c.CancelReason, len(c.History))
	}
	rec := send(t, f.h, http.MethodPost, path+"/cancel", me, "")
	if e := decodeError(t, rec); rec.Code != http.StatusConflict || e.Code != "invalid_transition" {
		t.Fatalf("cancelling twice: %d %+v, want 409 invalid_transition", rec.Code, e)
	}
}

func TestCancelAfterPayingButNotAfterExpiry(t *testing.T) {
	f := newOrderFixture(t)
	me, home := f.customer(t)
	ctx := context.Background()

	f.fill(t, me)
	paid := decode[orderDetail](t, f.checkout(t, me, home), http.StatusCreated)
	if _, err := f.srv.orders.Transition(ctx, paid.ID, orders.Pay, orders.System); err != nil {
		t.Fatal(err)
	}
	c := decode[orderDetail](t, send(t, f.h, http.MethodPost, "/orders/"+paid.ID.String()+"/cancel", me, ""), http.StatusOK)
	if c.Status != orders.Cancelled || len(c.History) != 3 || c.History[1].By != "system" {
		t.Fatalf("cancel a paid order: %+v", c.History)
	}

	f.fill(t, me)
	expired := decode[orderDetail](t, f.checkout(t, me, home), http.StatusCreated)
	if _, err := f.srv.orders.Transition(ctx, expired.ID, orders.Expire, orders.System); err != nil {
		t.Fatal(err)
	}
	if rec := send(t, f.h, http.MethodPost, "/orders/"+expired.ID.String()+"/cancel", me, ""); rec.Code != http.StatusConflict {
		t.Fatalf("cancel an expired order: %d, want 409", rec.Code)
	}
}

func TestCheckoutRefusals(t *testing.T) {
	f := newOrderFixture(t)

	expect := func(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) errorDetail {
		t.Helper()
		e := decodeError(t, rec)
		if rec.Code != status || e.Code != code {
			t.Fatalf("got %d %s (%s), want %d %s", rec.Code, e.Code, e.Message, status, code)
		}
		return e
	}

	t.Run("an empty cart", func(t *testing.T) {
		me, home := f.customer(t)
		expect(t, f.checkout(t, me, home), http.StatusConflict, "cart_empty")
	})
	t.Run("no address", func(t *testing.T) {
		me, _ := f.customer(t)
		f.fill(t, me)
		e := expect(t, send(t, f.h, http.MethodPost, "/orders", me, `{}`), http.StatusUnprocessableEntity, "validation_failed")
		if e.Fields["address_id"] == "" {
			t.Fatalf("fields = %v, want address_id", e.Fields)
		}
	})
	t.Run("someone else's address", func(t *testing.T) {
		me, _ := f.customer(t)
		_, theirs := f.customer(t)
		f.fill(t, me)
		e := expect(t, f.checkout(t, me, theirs), http.StatusUnprocessableEntity, "validation_failed")
		if e.Fields["address_id"] != "no such address" {
			t.Fatalf("fields = %v", e.Fields)
		}
	})
	t.Run("below the minimum", func(t *testing.T) {
		me, home := f.customer(t)
		decode[carts.Cart](t, f.add(t, me, f.ayran.ID.String(), 1), http.StatusOK)
		e := expect(t, f.checkout(t, me, home), http.StatusConflict, "below_minimum")
		if !strings.Contains(e.Message, "260,00 TL") {
			t.Fatalf("message %q should say how much is missing (260,00 TL)", e.Message)
		}
	})
	t.Run("a dish switched off", func(t *testing.T) {
		g := newOrderFixture(t)
		me, home := g.customer(t)
		g.fill(t, me)
		send(t, g.h, http.MethodPatch, g.base+"/items/"+g.ayran.ID.String(), g.owner, `{"available": false}`)
		expect(t, g.checkout(t, me, home), http.StatusConflict, "cart_has_problems")
	})
	t.Run("a paused restaurant", func(t *testing.T) {
		g := newOrderFixture(t)
		me, home := g.customer(t)
		g.fill(t, me)
		send(t, g.h, http.MethodPatch, g.restaurantPath, g.owner, `{"accepting_orders": false}`)
		expect(t, g.checkout(t, me, home), http.StatusConflict, "restaurant_not_accepting_orders")
	})
}

func TestOrdersArePrivate(t *testing.T) {
	f := newOrderFixture(t)
	me, home := f.customer(t)
	f.fill(t, me)
	o := decode[orderDetail](t, f.checkout(t, me, home), http.StatusCreated)
	path := "/orders/" + o.ID.String()

	// Another customer, and the restaurant's owner using their own customer
	// profile: to both, the order doesn't exist.
	other, _ := f.customer(t)
	for name, token := range map[string]string{"another customer": other, "the owner as a customer": f.owner} {
		for _, req := range [][2]string{{http.MethodGet, path}, {http.MethodPost, path + "/cancel"}} {
			if rec := send(t, f.h, req[0], req[1], token, ""); rec.Code != http.StatusNotFound || decodeError(t, rec).Code != "order_not_found" {
				t.Errorf("%s: %s %s → %d, want 404 order_not_found", name, req[0], req[1], rec.Code)
			}
		}
	}
	if list := decode[orderList](t, send(t, f.h, http.MethodGet, "/orders", other, ""), http.StatusOK); len(list.Orders) != 0 {
		t.Errorf("another customer's list has %d orders, want 0", len(list.Orders))
	}
	for _, p := range []string{"/orders/not-a-uuid", "/orders/" + uuid.Must(uuid.NewV7()).String()} {
		if rec := send(t, f.h, http.MethodGet, p, me, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s → %d, want 404", p, rec.Code)
		}
	}
	if rec := send(t, f.h, http.MethodGet, "/orders", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no login → %d, want 401", rec.Code)
	}
	if s := decode[orderDetail](t, send(t, f.h, http.MethodGet, path, me, ""), http.StatusOK).Status; s != orders.Placed {
		t.Errorf("status = %s; strangers' attempts changed the order", s)
	}
}

// placePaid checks a full cart out and plays the payment arriving, so the order
// is where the restaurant's buttons start.
func (f orderFixture) placePaid(t *testing.T, token, addressID string) orderDetail {
	t.Helper()
	f.fill(t, token)
	o := decode[orderDetail](t, f.checkout(t, token, addressID), http.StatusCreated)
	if _, err := f.srv.orders.Transition(context.Background(), o.ID, orders.Pay, orders.System); err != nil {
		t.Fatal(err)
	}
	return o
}

func (f orderFixture) kitchen(t *testing.T, token string) []orders.Order {
	t.Helper()
	return decode[orderList](t, send(t, f.h, http.MethodGet, "/me"+f.restaurantPath+"/orders", token, ""), http.StatusOK).Orders
}

func TestRestaurantWorksThroughAnOrder(t *testing.T) {
	f := newOrderFixture(t)
	me, home := f.customer(t)

	// Unpaid orders don't reach the kitchen.
	f.fill(t, me)
	decode[orderDetail](t, f.checkout(t, me, home), http.StatusCreated)
	if got := f.kitchen(t, f.owner); len(got) != 0 {
		t.Fatalf("kitchen shows %d orders before any is paid, want 0", len(got))
	}

	o := f.placePaid(t, me, home)
	if got := f.kitchen(t, f.owner); len(got) != 1 || got[0].ID != o.ID || len(got[0].Items) != 2 {
		t.Fatalf("kitchen = %+v, want the paid order with its 2 dishes", got)
	}
	path := "/orders/" + o.ID.String()

	accepted := decode[orderDetail](t, send(t, f.h, http.MethodPost, path+"/accept", f.owner, ""), http.StatusOK)
	if accepted.Status != orders.Accepted {
		t.Fatalf("after accept: %s", accepted.Status)
	}
	// Too late for the customer to cancel now.
	if rec := send(t, f.h, http.MethodPost, path+"/cancel", me, ""); rec.Code != http.StatusConflict {
		t.Fatalf("customer cancel after accept: %d, want 409", rec.Code)
	}

	ready := decode[orderDetail](t, send(t, f.h, http.MethodPost, path+"/ready", f.owner, ""), http.StatusOK)
	var trail []string
	for _, h := range ready.History {
		trail = append(trail, string(h.To)+" by "+h.By)
	}
	if want := "placed by customer, paid by system, accepted by restaurant, ready by restaurant"; strings.Join(trail, ", ") != want {
		t.Fatalf("history: %s\nwant:    %s", strings.Join(trail, ", "), want)
	}

	for _, button := range []string{"/accept", "/reject", "/ready"} {
		rec := send(t, f.h, http.MethodPost, path+button, f.owner, "")
		if e := decodeError(t, rec); rec.Code != http.StatusConflict || e.Code != "invalid_transition" {
			t.Errorf("%s on a ready order: %d %s, want 409 invalid_transition", button, rec.Code, e.Code)
		}
	}
}

func TestRestaurantRejects(t *testing.T) {
	f := newOrderFixture(t)
	me, home := f.customer(t)

	for _, acceptFirst := range []bool{false, true} {
		o := f.placePaid(t, me, home)
		path := "/orders/" + o.ID.String()
		if acceptFirst {
			decode[orderDetail](t, send(t, f.h, http.MethodPost, path+"/accept", f.owner, ""), http.StatusOK)
		}
		r := decode[orderDetail](t, send(t, f.h, http.MethodPost, path+"/reject", f.owner, ""), http.StatusOK)
		if r.Status != orders.Cancelled || *r.CancelReason != orders.Rejected {
			t.Fatalf("reject (accepted first: %v): %s %v, want cancelled (rejected)", acceptFirst, r.Status, r.CancelReason)
		}
	}
	if got := f.kitchen(t, f.owner); len(got) != 0 {
		t.Fatalf("rejected orders still in the kitchen list: %d", len(got))
	}

	// An unpaid order can't be accepted or rejected yet.
	f.fill(t, me)
	unpaid := decode[orderDetail](t, f.checkout(t, me, home), http.StatusCreated)
	if rec := send(t, f.h, http.MethodPost, "/orders/"+unpaid.ID.String()+"/accept", f.owner, ""); rec.Code != http.StatusConflict {
		t.Fatalf("accept an unpaid order: %d, want 409", rec.Code)
	}
}

// A restaurant can't touch another restaurant's orders, and a customer can't
// press the restaurant's buttons.
func TestRestaurantButtonsAreForTheOwner(t *testing.T) {
	f := newOrderFixture(t)
	me, home := f.customer(t)
	o := f.placePaid(t, me, home)
	path := "/orders/" + o.ID.String()

	otherOwner := ownerToken(t, f.h)
	createRestaurant(t, f.h, otherOwner, ciyaBody)
	for _, button := range []string{"/accept", "/reject", "/ready"} {
		rec := send(t, f.h, http.MethodPost, path+button, otherOwner, "")
		if e := decodeError(t, rec); rec.Code != http.StatusNotFound || e.Code != "order_not_found" {
			t.Errorf("another restaurant %s: %d %s, want 404 order_not_found", button, rec.Code, e.Code)
		}
		rec = send(t, f.h, http.MethodPost, path+button, me, "")
		if e := decodeError(t, rec); rec.Code != http.StatusForbidden || e.Code != "profile_required" {
			t.Errorf("the customer %s: %d %s, want 403 profile_required", button, rec.Code, e.Code)
		}
	}
	rec := send(t, f.h, http.MethodGet, "/me"+f.restaurantPath+"/orders", otherOwner, "")
	if e := decodeError(t, rec); rec.Code != http.StatusNotFound || e.Code != "restaurant_not_found" {
		t.Errorf("another owner reading the kitchen list: %d %s, want 404 restaurant_not_found", rec.Code, e.Code)
	}
	if s := decode[orderDetail](t, send(t, f.h, http.MethodGet, path, me, ""), http.StatusOK).Status; s != orders.Paid {
		t.Errorf("status = %s; strangers' attempts changed the order", s)
	}
}

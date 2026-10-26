package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atalaymudanyali/quickbytes/internal/carts"
	"github.com/atalaymudanyali/quickbytes/internal/idempotency"
)

// cartLines says how many lines are in the customer's cart: a checkout that
// didn't happen leaves them there.
func (f orderFixture) cartLines(t *testing.T, token string) int {
	t.Helper()
	return len(decode[carts.Cart](t, send(t, f.h, http.MethodGet, "/cart", token, ""), http.StatusOK).Items)
}

func (f orderFixture) orderCount(t *testing.T, token string) int {
	t.Helper()
	return len(decode[orderList](t, send(t, f.h, http.MethodGet, "/orders", token, ""), http.StatusOK).Orders)
}

func TestCheckoutNeedsAnIdempotencyKey(t *testing.T) {
	f := newOrderFixture(t)
	me, home := f.customer(t)
	f.fill(t, me)

	for name, tc := range map[string]struct{ key, code string }{
		"no key":      {"", "idempotency_key_missing"},
		"a space":     {"has space", "idempotency_key_invalid"},
		"256 chars":   {strings.Repeat("k", 256), "idempotency_key_invalid"},
		"not ASCII":   {"sipariş-1", "idempotency_key_invalid"},
		"a line feed": {"k\n1", "idempotency_key_invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := f.checkoutWithKey(t, me, tc.key, home)
			if e := decodeError(t, rec); rec.Code != http.StatusBadRequest || e.Code != tc.code {
				t.Fatalf("got %d %s, want 400 %s", rec.Code, e.Code, tc.code)
			}
		})
	}
	if n := f.cartLines(t, me); n != 2 {
		t.Fatalf("cart has %d lines, want the 2 still there", n)
	}
	// The longest allowed key works.
	decode[orderDetail](t, f.checkoutWithKey(t, me, strings.Repeat("k", 255), home), http.StatusCreated)
}

func TestRetriedCheckoutReplaysTheFirstAnswer(t *testing.T) {
	f := newOrderFixture(t)
	me, home := f.customer(t)
	f.fill(t, me)
	key := uuid.NewString()

	first := f.checkoutWithKey(t, me, key, home)
	if first.Code != http.StatusCreated {
		t.Fatalf("first attempt: %d %s", first.Code, first.Body)
	}
	if first.Header().Get(replayedHeader) != "" {
		t.Error("the first attempt is marked as a replay")
	}

	for i := range 3 {
		again := f.checkoutWithKey(t, me, key, home)
		if again.Code != http.StatusCreated || !bytes.Equal(again.Body.Bytes(), first.Body.Bytes()) {
			t.Fatalf("attempt %d: %d %s\nwant the first answer: %s", i+2, again.Code, again.Body, first.Body)
		}
		if again.Header().Get(replayedHeader) != "true" {
			t.Errorf("attempt %d isn't marked %s: true", i+2, replayedHeader)
		}
	}
	if n := f.orderCount(t, me); n != 1 {
		t.Fatalf("%d orders, want 1", n)
	}
}

func TestKeyReusedForAnotherCheckout(t *testing.T) {
	f := newOrderFixture(t)
	me, home := f.customer(t)
	work := createAddress(t, f.h, me, homeBody).ID.String()
	f.fill(t, me)
	key := uuid.NewString()

	decode[orderDetail](t, f.checkoutWithKey(t, me, key, home), http.StatusCreated)
	rec := f.checkoutWithKey(t, me, key, work)
	if e := decodeError(t, rec); rec.Code != http.StatusUnprocessableEntity || e.Code != "idempotency_key_reused" {
		t.Fatalf("got %d %s, want 422 idempotency_key_reused", rec.Code, e.Code)
	}
}

func TestCheckoutWhileTheFirstAttemptRuns(t *testing.T) {
	f := newOrderFixture(t)
	me, home := f.customer(t)
	f.fill(t, me)
	key := uuid.NewString()

	// Play the first attempt: hold the key, as a request still running would.
	claims, err := f.srv.tokens.Parse(me)
	if err != nil {
		t.Fatal(err)
	}
	userID, err := claims.UserID()
	if err != nil {
		t.Fatal(err)
	}
	fp := idempotency.FingerprintOf(http.MethodPost, "/orders", []byte(`{"address_id": "`+home+`"}`))
	if _, _, err := f.srv.idempotency.Begin(context.Background(), userID, key, fp); err != nil {
		t.Fatal(err)
	}

	rec := f.checkoutWithKey(t, me, key, home)
	if e := decodeError(t, rec); rec.Code != http.StatusConflict || e.Code != "idempotency_in_progress" {
		t.Fatalf("got %d %s, want 409 idempotency_in_progress", rec.Code, e.Code)
	}
	if rec.Header().Get("Retry-After") != "1" {
		t.Errorf("Retry-After = %q, want 1", rec.Header().Get("Retry-After"))
	}
	if n := f.cartLines(t, me); n != 2 {
		t.Fatalf("cart has %d lines, want the 2 still there", n)
	}
}

func TestRefusalsAreReplayedToo(t *testing.T) {
	f := newOrderFixture(t)
	me, home := f.customer(t)
	key := uuid.NewString()

	// The answer to this attempt was "your cart is empty", and it stays that,
	// even after the cart is filled: a retry repeats the attempt, it doesn't
	// start a new one.
	rec := f.checkoutWithKey(t, me, key, home)
	if e := decodeError(t, rec); rec.Code != http.StatusConflict || e.Code != "cart_empty" {
		t.Fatalf("got %d %s, want 409 cart_empty", rec.Code, e.Code)
	}
	f.fill(t, me)
	again := f.checkoutWithKey(t, me, key, home)
	if again.Code != http.StatusConflict || again.Header().Get(replayedHeader) != "true" {
		t.Fatalf("retry: %d (replayed %q), want the stored 409", again.Code, again.Header().Get(replayedHeader))
	}
	// A new tap is a new key.
	decode[orderDetail](t, f.checkout(t, me, home), http.StatusCreated)
}

func TestIdempotencyKeysArePerUser(t *testing.T) {
	f := newOrderFixture(t)
	alice, aliceHome := f.customer(t)
	bob, bobHome := f.customer(t)
	f.fill(t, alice)
	f.fill(t, bob)

	a := decode[orderDetail](t, f.checkoutWithKey(t, alice, "same-key", aliceHome), http.StatusCreated)
	rec := f.checkoutWithKey(t, bob, "same-key", bobHome)
	b := decode[orderDetail](t, rec, http.StatusCreated)
	if a.ID == b.ID || rec.Header().Get(replayedHeader) != "" {
		t.Fatal("Bob got Alice's answer")
	}
}

// TestServerErrorsReleaseTheKey uses stand-in handlers: a real 500 or panic
// can't be caused from outside.
func TestServerErrorsReleaseTheKey(t *testing.T) {
	srv, h := newAuthTestServer(t)
	token := signUp(t, h).Tokens.AccessToken

	calls := 0
	var fail func(w http.ResponseWriter)
	stub := srv.requireAuth(recoverPanics(discardLogger(), srv.idempotent(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if deadline, ok := r.Context().Deadline(); !ok || time.Until(deadline) > requestDeadline {
			t.Errorf("the handler runs without the %s deadline", requestDeadline)
		}
		if fail != nil {
			fail(w)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]int{"call": calls})
	})))

	for name, f := range map[string]func(w http.ResponseWriter){
		"a 500":   func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) },
		"a panic": func(http.ResponseWriter) { panic("boom") },
	} {
		t.Run(name, func(t *testing.T) {
			calls, fail = 0, f
			key := uuid.NewString()
			if rec := sendWithKey(t, stub, "/stub", token, key, `{}`); rec.Code != http.StatusInternalServerError {
				t.Fatalf("first attempt: %d, want 500", rec.Code)
			}
			fail = nil
			// The key was given back, so the retry does the work...
			if rec := sendWithKey(t, stub, "/stub", token, key, `{}`); rec.Code != http.StatusCreated {
				t.Fatalf("retry: %d, want 201", rec.Code)
			}
			// ...and the one after that replays it.
			rec := sendWithKey(t, stub, "/stub", token, key, `{}`)
			if rec.Code != http.StatusCreated || calls != 2 || strings.TrimSpace(rec.Body.String()) != `{"call":2}` {
				t.Fatalf("third attempt: %d %s after %d calls; want the retry's answer, 2 calls", rec.Code, rec.Body, calls)
			}
		})
	}
}

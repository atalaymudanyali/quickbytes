package payprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atalaymudanyali/quickbytes/internal/idempotency"
	"github.com/atalaymudanyali/quickbytes/internal/testutil"
)

// client calls the provider as one merchant, the way the API will.
type client struct {
	t        *testing.T
	h        http.Handler
	pool     *pgxpool.Pool
	merchant Merchant
	key      string
}

func newClient(t *testing.T) *client {
	t.Helper()
	pool := testutil.PayProviderDB(t)
	m, key := newMerchant(t, pool)
	return &client{t: t, h: newHandler(t, pool), pool: pool, merchant: m, key: key}
}

// post sends a POST with an Idempotency-Key (none when idemKey is "").
func (c *client) post(path, idemKey, body string) *httptest.ResponseRecorder {
	c.t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+c.key)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if idemKey != "" {
		req.Header.Set(idempotencyKeyHeader, idemKey)
	}
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, req)
	return rec
}

func (c *client) get(path string) *httptest.ResponseRecorder {
	c.t.Helper()
	return call(c.t, c.h, "GET", path, c.key)
}

// authorize starts a payment with a new key and returns the answer.
func (c *client) authorize(card string, amount int) *httptest.ResponseRecorder {
	c.t.Helper()
	return c.post("/v1/payments", newKey(), authorizeBody(card, amount))
}

func authorizeBody(card string, amount int) string {
	b, _ := json.Marshal(map[string]any{"amount_kurus": amount, "card_number": card})
	return string(b)
}

func newKey() string { return uuid.NewString() }

func decodePayment(t *testing.T, rec *httptest.ResponseRecorder) Payment {
	t.Helper()
	var p Payment
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode payment %q: %v", rec.Body.String(), err)
	}
	return p
}

func decodeRefusal(t *testing.T, rec *httptest.ResponseRecorder) refusalBody {
	t.Helper()
	var r refusalBody
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode refusal %q: %v", rec.Body.String(), err)
	}
	return r
}

// countPayments counts the merchant's payments, to prove retries add none.
func (c *client) countPayments() int {
	c.t.Helper()
	var n int
	if err := c.pool.QueryRow(context.Background(),
		"SELECT count(*) FROM payments WHERE merchant_id = $1", c.merchant.ID).Scan(&n); err != nil {
		c.t.Fatal(err)
	}
	return n
}

func TestAuthorizeOverHTTP(t *testing.T) {
	c := newClient(t)

	t.Run("approved", func(t *testing.T) {
		rec := c.authorize(cardApprove, 32000)
		if rec.Code != http.StatusCreated {
			t.Fatalf("got %d %s", rec.Code, rec.Body)
		}
		if p := decodePayment(t, rec); p.Status != StatusAuthorized || p.CardLast4 != "4242" {
			t.Errorf("payment = %+v", p)
		}
	})
	t.Run("slow", func(t *testing.T) {
		rec := c.authorize(cardSlow, 32000)
		if rec.Code != http.StatusCreated || decodePayment(t, rec).Status != StatusProcessing {
			t.Errorf("got %d %s", rec.Code, rec.Body)
		}
	})
	for card, code := range map[string]string{
		cardDecline:      DeclineCardDeclined,
		cardInsufficient: DeclineInsufficientFunds,
	} {
		t.Run(code, func(t *testing.T) {
			rec := c.authorize(card, 32000)
			if rec.Code != http.StatusPaymentRequired {
				t.Fatalf("got %d %s", rec.Code, rec.Body)
			}
			r := decodeRefusal(t, rec)
			if r.Error.Code != code || r.Payment.Status != StatusFailed || r.Payment.ID == uuid.Nil {
				t.Errorf("refusal = %+v", r)
			}
			// The failed payment was stored and can be looked up.
			if got := c.get("/v1/payments/" + r.Payment.ID.String()); got.Code != http.StatusOK {
				t.Errorf("get failed payment: %d", got.Code)
			}
		})
	}
}

func TestAuthorizeRefusals(t *testing.T) {
	c := newClient(t)
	tests := []struct {
		name    string
		idemKey string
		body    string
		status  int
		code    string
	}{
		{"no idempotency key", "", authorizeBody(cardApprove, 100), 400, "idempotency_key_missing"},
		{"key with a space", "a b", authorizeBody(cardApprove, 100), 400, "idempotency_key_invalid"},
		{"not a test card", newKey(), authorizeBody("5555555555554444", 100), 400, "unknown_test_card"},
		{"zero amount", newKey(), authorizeBody(cardApprove, 0), 422, "amount_invalid"},
		{"unknown field", newKey(), `{"amount": 100, "card_number": "4242424242424242"}`, 400, "invalid_json"},
		{"empty reference", newKey(), `{"amount_kurus": 100, "card_number": "4242424242424242", "reference": ""}`, 422, "reference_invalid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := c.post("/v1/payments", tt.idemKey, tt.body)
			if rec.Code != tt.status || errorCode(t, rec) != tt.code {
				t.Errorf("got %d %s, want %d %s", rec.Code, rec.Body, tt.status, tt.code)
			}
		})
	}
	t.Run("not JSON", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/v1/payments", strings.NewReader("amount=1"))
		req.Header.Set("Authorization", "Bearer "+c.key)
		req.Header.Set(idempotencyKeyHeader, newKey())
		rec := httptest.NewRecorder()
		c.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Errorf("got %d %s", rec.Code, rec.Body)
		}
	})
	if n := c.countPayments(); n != 0 {
		t.Errorf("%d payments stored by refused requests, want 0", n)
	}
}

// The acceptance criterion: retrying with the same key never authorizes,
// captures or refunds twice.
func TestRetriesNeverMoveMoneyTwice(t *testing.T) {
	c := newClient(t)
	key := newKey()
	body := authorizeBody(cardApprove, 32000)

	first := c.post("/v1/payments", key, body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first authorize: %d %s", first.Code, first.Body)
	}
	retry := c.post("/v1/payments", key, body)
	if retry.Code != http.StatusCreated || retry.Body.String() != first.Body.String() {
		t.Errorf("retry: %d %s, want the first answer %s", retry.Code, retry.Body, first.Body)
	}
	if retry.Header().Get(replayedHeader) != "true" {
		t.Error("retry not marked as replayed")
	}
	if n := c.countPayments(); n != 1 {
		t.Errorf("%d payments after a retried authorize, want 1", n)
	}
	id := decodePayment(t, first).ID.String()

	// The same for capture and refund: the retry gets the first 200 again,
	// not a 409 for "already captured".
	for _, action := range []string{"capture", "refund"} {
		key := newKey()
		first := c.post("/v1/payments/"+id+"/"+action, key, "")
		retry := c.post("/v1/payments/"+id+"/"+action, key, "")
		if first.Code != http.StatusOK || retry.Code != http.StatusOK || retry.Body.String() != first.Body.String() {
			t.Errorf("%s: first %d, retry %d %s", action, first.Code, retry.Code, retry.Body)
		}
	}
	// A new key is a new action: refunding again is refused.
	if again := c.post("/v1/payments/"+id+"/refund", newKey(), ""); again.Code != http.StatusConflict {
		t.Errorf("second refund with a new key: %d %s", again.Code, again.Body)
	}

	var events int
	if err := c.pool.QueryRow(context.Background(),
		"SELECT count(*) FROM payment_events WHERE payment_id = $1", id).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 3 {
		t.Errorf("%d events, want 3 (authorized, captured, refunded)", events)
	}
}

func TestDeclineIsReplayedToo(t *testing.T) {
	c := newClient(t)
	key, body := newKey(), authorizeBody(cardDecline, 32000)
	first := c.post("/v1/payments", key, body)
	retry := c.post("/v1/payments", key, body)
	if first.Code != http.StatusPaymentRequired || retry.Code != http.StatusPaymentRequired ||
		decodeRefusal(t, retry).Payment.ID != decodeRefusal(t, first).Payment.ID {
		t.Errorf("first %d, retry %d %s", first.Code, retry.Code, retry.Body)
	}
	if n := c.countPayments(); n != 1 {
		t.Errorf("%d payments, want 1", n)
	}
}

func TestKeyReusedForAnotherRequest(t *testing.T) {
	c := newClient(t)
	key := newKey()
	c.post("/v1/payments", key, authorizeBody(cardApprove, 100))
	rec := c.post("/v1/payments", key, authorizeBody(cardApprove, 200))
	if rec.Code != http.StatusUnprocessableEntity || errorCode(t, rec) != "idempotency_key_reused" {
		t.Errorf("got %d %s", rec.Code, rec.Body)
	}
}

// While the first attempt with a key still runs, a retry is told to wait.
func TestRetryWhileInProgress(t *testing.T) {
	c := newClient(t)
	key, body := newKey(), authorizeBody(cardApprove, 100)
	// Claim the key the way a running first attempt would.
	fp := idempotency.FingerprintOf("POST", "/v1/payments", []byte(body))
	if _, _, err := idempotency.NewStore(c.pool).Begin(context.Background(), c.merchant.ID, key, fp); err != nil {
		t.Fatal(err)
	}
	rec := c.post("/v1/payments", key, body)
	if rec.Code != http.StatusConflict || errorCode(t, rec) != "idempotency_in_progress" || rec.Header().Get("Retry-After") == "" {
		t.Errorf("got %d %s", rec.Code, rec.Body)
	}
	if n := c.countPayments(); n != 0 {
		t.Errorf("%d payments, want 0", n)
	}
}

// The answer is stored in the payment's own transaction: once the payment
// exists, its key is done with exactly the answer that was sent.
func TestAnswerIsStoredWithThePayment(t *testing.T) {
	c := newClient(t)
	key := newKey()
	rec := c.post("/v1/payments", key, authorizeBody(cardApprove, 100))
	var (
		status int
		body   []byte
	)
	err := c.pool.QueryRow(context.Background(), `
		SELECT response_status, response_body FROM idempotency_keys
		WHERE user_id = $1 AND key = $2 AND status = 'done'`, c.merchant.ID, key).Scan(&status, &body)
	if err != nil {
		t.Fatalf("stored answer: %v", err)
	}
	if status != rec.Code || string(body) != rec.Body.String() {
		t.Errorf("stored %d %s, sent %d %s", status, body, rec.Code, rec.Body)
	}
}

func TestMovesOverHTTP(t *testing.T) {
	c := newClient(t)
	newPayment := func() string {
		return decodePayment(t, c.authorize(cardApprove, 5000)).ID.String()
	}

	t.Run("capture", func(t *testing.T) {
		rec := c.post("/v1/payments/"+newPayment()+"/capture", newKey(), "")
		if rec.Code != http.StatusOK || decodePayment(t, rec).Status != StatusCaptured {
			t.Errorf("got %d %s", rec.Code, rec.Body)
		}
	})
	t.Run("void, then capture is refused", func(t *testing.T) {
		id := newPayment()
		if rec := c.post("/v1/payments/"+id+"/void", newKey(), ""); rec.Code != http.StatusOK {
			t.Fatalf("void: %d %s", rec.Code, rec.Body)
		}
		rec := c.post("/v1/payments/"+id+"/capture", newKey(), "")
		if rec.Code != http.StatusConflict {
			t.Fatalf("capture after void: %d %s", rec.Code, rec.Body)
		}
		r := decodeRefusal(t, rec)
		if r.Error.Code != "invalid_state" || r.Payment.Status != StatusVoided {
			t.Errorf("refusal = %+v", r)
		}
	})
	t.Run("unknown payment", func(t *testing.T) {
		rec := c.post("/v1/payments/"+uuid.NewString()+"/capture", newKey(), "")
		if rec.Code != http.StatusNotFound || errorCode(t, rec) != "payment_not_found" {
			t.Errorf("got %d %s", rec.Code, rec.Body)
		}
	})
	t.Run("no idempotency key", func(t *testing.T) {
		rec := c.post("/v1/payments/"+newPayment()+"/refund", "", "")
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "idempotency_key_missing" {
			t.Errorf("got %d %s", rec.Code, rec.Body)
		}
	})
}

func TestGetPaymentOverHTTP(t *testing.T) {
	c := newClient(t)
	id := decodePayment(t, c.authorize(cardApprove, 5000)).ID.String()

	if rec := c.get("/v1/payments/" + id); rec.Code != http.StatusOK || decodePayment(t, rec).ID.String() != id {
		t.Errorf("own payment: %d %s", rec.Code, rec.Body)
	}
	for name, path := range map[string]string{
		"unknown ID":  "/v1/payments/" + uuid.NewString(),
		"not an ID":   "/v1/payments/nope",
		"other's one": "",
	} {
		t.Run(name, func(t *testing.T) {
			var rec *httptest.ResponseRecorder
			if path == "" {
				// Another merchant asks for this merchant's payment.
				other := newClient(t)
				rec = other.get("/v1/payments/" + id)
			} else {
				rec = c.get(path)
			}
			if rec.Code != http.StatusNotFound || errorCode(t, rec) != "payment_not_found" {
				t.Errorf("got %d %s", rec.Code, rec.Body)
			}
		})
	}
}

// A merchant finds its payments by its own reference, newest first, and only
// its own: another merchant using the same reference sees nothing of them.
func TestFindPaymentsByReference(t *testing.T) {
	c := newClient(t)
	ref := uuid.NewString()
	body := func(card string) string {
		b, _ := json.Marshal(map[string]any{"amount_kurus": 5000, "card_number": card, "reference": ref})
		return string(b)
	}
	first := decodePayment(t, c.post("/v1/payments", newKey(), body(cardApprove)))
	second := decodePayment(t, c.post("/v1/payments", newKey(), body(cardApprove)))

	find := func(c *client, ref string) (int, []Payment) {
		t.Helper()
		rec := c.get("/v1/payments?reference=" + url.QueryEscape(ref))
		var list paymentList
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
				t.Fatal(err)
			}
		}
		return rec.Code, list.Payments
	}

	code, list := find(c, ref)
	if code != http.StatusOK || len(list) != 2 || list[0].ID != second.ID || list[1].ID != first.ID {
		t.Fatalf("got %d %+v, want both, newest first", code, list)
	}
	if code, list := find(c, uuid.NewString()); code != http.StatusOK || len(list) != 0 {
		t.Errorf("unused reference: %d %+v, want an empty list", code, list)
	}
	if code, list := find(newClient(t), ref); code != http.StatusOK || len(list) != 0 {
		t.Errorf("other merchant: %d %+v, want an empty list", code, list)
	}
	if rec := c.get("/v1/payments"); rec.Code != http.StatusBadRequest || errorCode(t, rec) != "reference_required" {
		t.Errorf("no reference: %d %s", rec.Code, rec.Body)
	}
}

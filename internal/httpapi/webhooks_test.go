package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atalaymudanyali/quickbytes/internal/orders"
	"github.com/atalaymudanyali/quickbytes/internal/payments"
	"github.com/atalaymudanyali/quickbytes/internal/payments/paymentstest"
	"github.com/atalaymudanyali/quickbytes/internal/payprovider"
	"github.com/atalaymudanyali/quickbytes/internal/testutil"
	"github.com/atalaymudanyali/quickbytes/internal/webhook"
)

const webhookSecret = "whsec_test_0123456789abcdef0123456789"

// webhookBody is a webhook as the provider writes it.
func webhookBody(eventID, status, paymentID string, amount int, reference uuid.UUID) []byte {
	b, _ := json.Marshal(map[string]any{
		"id": eventID, "type": "payment." + status, "created_at": time.Now(),
		"data": map[string]any{"payment": map[string]any{
			"id": paymentID, "amount_kurus": amount, "currency": "TRY", "status": status,
			"card_last4": "7777", "reference": reference.String(),
			"created_at": time.Now(), "updated_at": time.Now(),
		}},
	})
	return b
}

func postWebhook(t *testing.T, h http.Handler, signature string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/payprovider", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	if signature != "" {
		req.Header.Set(webhook.Header, signature)
	}
	return do(t, h, req)
}

// slowOrder checks out with the slow card: the order waits, placed, with its
// payment processing at the (fake) provider.
func (f orderFixture) slowOrder(t *testing.T) (token string, o orderDetail, providerID string) {
	t.Helper()
	me, home := f.customer(t)
	f.fill(t, me)
	o = decode[orderDetail](t, f.payWith(t, me, home, paymentstest.CardSlow), http.StatusCreated)
	p, err := f.srv.payments.ForOrder(context.Background(), o.ID)
	if err != nil || p.ProviderPaymentID == nil {
		t.Fatalf("payment: %+v, %v", p, err)
	}
	return me, o, *p.ProviderPaymentID
}

func TestWebhookEndpoint(t *testing.T) {
	f := newOrderFixture(t)
	f.srv.webhookSecret = webhookSecret
	me, o, providerID := f.slowOrder(t)
	f.provider.Settle(providerID, true)
	body := webhookBody(uuid.NewString(), "authorized", providerID, o.TotalKurus, o.ID)
	now := time.Now()

	orderStatus := func() orders.Status {
		t.Helper()
		return decode[orderDetail](t, send(t, f.h, http.MethodGet, "/orders/"+o.ID.String(), me, ""), http.StatusOK).Status
	}

	// Refused, and nothing changes: no signature, the wrong secret, a
	// signature 6 minutes old (a replay), a body changed after signing.
	refused := map[string]struct {
		sig  string
		body []byte
	}{
		"no signature":   {"", body},
		"wrong secret":   {webhook.Sign("whsec_someone_else_0123456789abcdef", now, body), body},
		"6 minutes old":  {webhook.Sign(webhookSecret, now.Add(-6*time.Minute), body), body},
		"body changed":   {webhook.Sign(webhookSecret, now, body), []byte(strings.Replace(string(body), `"authorized"`, `"captured"`, 1))},
		"signed garbage": {webhook.Sign(webhookSecret, now, []byte("{}")), []byte("{}")},
	}
	for name, tc := range refused {
		t.Run(name, func(t *testing.T) {
			rec := postWebhook(t, f.h, tc.sig, tc.body)
			if rec.Code != http.StatusBadRequest || decodeError(t, rec).Code != "invalid_webhook" {
				t.Fatalf("%d %s, want 400 invalid_webhook", rec.Code, rec.Body)
			}
			if s := orderStatus(); s != orders.Placed {
				t.Fatalf("order %s after a refused webhook, want still placed", s)
			}
		})
	}

	// The genuine one pays the order; sending it again is a repeat.
	sig := webhook.Sign(webhookSecret, now, body)
	if got := decode[webhookAnswer](t, postWebhook(t, f.h, sig, body), http.StatusOK); got.Outcome != payments.Applied {
		t.Fatalf("outcome %q, want applied", got.Outcome)
	}
	if s := orderStatus(); s != orders.Paid {
		t.Fatalf("order %s, want paid", s)
	}
	if got := decode[webhookAnswer](t, postWebhook(t, f.h, sig, body), http.StatusOK); got.Outcome != payments.Repeat {
		t.Fatalf("second time: outcome %q, want repeat", got.Outcome)
	}
}

func TestWebhookEndpointOddCases(t *testing.T) {
	f := newOrderFixture(t)
	f.srv.webhookSecret = webhookSecret
	signed := func(b []byte) string { return webhook.Sign(webhookSecret, time.Now(), b) }

	// A genuine event type we don't handle: 200, so it isn't sent for hours.
	disputed := webhookBody(uuid.NewString(), "disputed", "pay_x", 100, uuid.New())
	if got := decode[webhookAnswer](t, postWebhook(t, f.h, signed(disputed), disputed), http.StatusOK); got.Outcome != "ignored" {
		t.Errorf("unknown type: outcome %q", got.Outcome)
	}
	// About no payment of ours.
	stranger := webhookBody(uuid.NewString(), "authorized", "pay_"+uuid.NewString(), 100, uuid.New())
	if got := decode[webhookAnswer](t, postWebhook(t, f.h, signed(stranger), stranger), http.StatusOK); got.Outcome != payments.Unmatched {
		t.Errorf("stranger: outcome %q", got.Outcome)
	}
	// Too big to even look at.
	huge := []byte(`{"id": "` + strings.Repeat("x", maxWebhookBytes) + `"}`)
	if rec := postWebhook(t, f.h, signed(huge), huge); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("huge body: %d, want 413", rec.Code)
	}
}

// live is the real thing, end to end: the real provider (its HTTP server, its
// database, its webhook sender) and the API over real HTTP, each calling the
// other.
type live struct {
	orderFixture
	providerDB *pgxpool.Pool
	payments   *payprovider.Payments
	sender     *payprovider.Sender
	endpoint   payprovider.Endpoint
}

// newLive starts both. wrap, if not nil, sits in front of the API, to play
// it failing.
func newLive(t *testing.T, cfg payprovider.SenderConfig, wrap func(http.Handler) http.Handler) live {
	t.Helper()
	ctx := context.Background()
	providerDB := testutil.PayProviderDB(t)
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	key := "sk_test_" + hex.EncodeToString(b)
	merchant, err := payprovider.NewMerchants(providerDB).Ensure(ctx, "test-"+key[8:20], key)
	if err != nil {
		t.Fatal(err)
	}
	// A long slow-card delay: the test settles its payment itself.
	pp := payprovider.NewPayments(providerDB, time.Hour)
	providerSrv := httptest.NewServer(payprovider.NewServer(providerDB, pp, discardLogger()).Routes())
	t.Cleanup(providerSrv.Close)

	srv, h := newTestServerWith(t, testutil.RedisIfConfigured(t),
		payments.NewPayProvider(providerSrv.URL, key, 2*time.Second))
	srv.webhookSecret = webhookSecret
	var api http.Handler = h
	if wrap != nil {
		api = wrap(h)
	}
	apiSrv := httptest.NewServer(api)
	t.Cleanup(apiSrv.Close)

	ep, err := payprovider.NewEndpoints(providerDB).Ensure(ctx, merchant.ID, apiSrv.URL+"/webhooks/payprovider", webhookSecret)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 2 * time.Second
	}
	return live{
		orderFixture: orderFixture{cartFixture: newCartFixtureWith(t, h), srv: srv},
		providerDB:   providerDB, payments: pp, endpoint: ep,
		sender: payprovider.NewSender(providerDB, cfg, discardLogger()),
	}
}

// settle plays the slow card's bank answering: the provider's settler
// finishes the payment and queues its webhook.
func (l live) settle(t *testing.T, providerID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := l.providerDB.Exec(ctx,
		`UPDATE payments SET settle_at = now() - interval '1 second' WHERE id = $1`, providerID); err != nil {
		t.Fatal(err)
	}
	if _, err := l.payments.SettleDue(ctx); err != nil {
		t.Fatal(err)
	}
}

// sendWebhooks runs the provider's sender once over this test's endpoint,
// first making waiting retries due (instead of waiting out the backoff).
func (l live) sendWebhooks(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := l.providerDB.Exec(ctx,
		`UPDATE webhook_deliveries SET next_attempt_at = now() WHERE endpoint_id = $1 AND status = 'pending'`, l.endpoint.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := l.sender.SendDueTo(ctx, l.endpoint.ID); err != nil {
		t.Fatal(err)
	}
}

func (l live) order(t *testing.T, token string, id uuid.UUID) orderDetail {
	t.Helper()
	return decode[orderDetail](t, send(t, l.h, http.MethodGet, "/orders/"+id.String(), token, ""), http.StatusOK)
}

// paidMoves counts how many times the order was moved to paid.
func (l live) paidMoves(t *testing.T, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := testutil.DB(t).QueryRow(context.Background(),
		`SELECT count(*) FROM order_status_history WHERE order_id = $1 AND to_status = 'paid'`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The slow card, end to end: checkout leaves the order placed; when the bank
// answers, the provider's webhook pays it. Nobody polled.
func TestLiveSlowCardIsPaidByWebhook(t *testing.T) {
	l := newLive(t, payprovider.SenderConfig{}, nil)
	me, o, providerID := l.slowOrder(t)
	if o.Status != orders.Placed {
		t.Fatalf("after checkout: %s, want placed", o.Status)
	}

	l.settle(t, providerID)
	l.sendWebhooks(t)

	got := l.order(t, me, o.ID)
	if got.Status != orders.Paid || got.Payment.Status != payments.Authorized {
		t.Fatalf("order %s, payment %s; want paid, authorized", got.Status, got.Payment.Status)
	}
	if last := got.Payment.Events[len(got.Payment.Events)-1]; last.Via != payments.ViaWebhook {
		t.Errorf("learned %s via %s, want via webhook", last.Status, last.Via)
	}
}

// The provider's test switch sends every webhook twice: one status change.
func TestLiveDuplicateWebhookChangesOnce(t *testing.T) {
	l := newLive(t, payprovider.SenderConfig{SendTwice: true}, nil)
	me, o, providerID := l.slowOrder(t)
	l.settle(t, providerID)
	l.sendWebhooks(t)

	got := l.order(t, me, o.ID)
	if got.Status != orders.Paid {
		t.Fatalf("order %s, want paid", got.Status)
	}
	if n := l.paidMoves(t, o.ID); n != 1 {
		t.Errorf("moved to paid %d times, want once", n)
	}
	if n := len(got.Payment.Events); n != 3 { // pending, processing, authorized
		t.Errorf("%d ledger lines, want 3: %+v", n, got.Payment.Events)
	}
}

// The API fails the first two tries (a 500, as when its database is down):
// the provider keeps trying, and the third try gets through.
func TestLiveProviderRetriesThroughAPIFailures(t *testing.T) {
	var webhooks atomic.Int32
	failFirst := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/webhooks/payprovider" && webhooks.Add(1) <= 2 {
				http.Error(w, "database is down", http.StatusInternalServerError)
				return
			}
			h.ServeHTTP(w, r)
		})
	}
	l := newLive(t, payprovider.SenderConfig{}, failFirst)
	me, o, providerID := l.slowOrder(t)
	l.settle(t, providerID)

	l.sendWebhooks(t) // 500
	l.sendWebhooks(t) // 500
	if got := l.order(t, me, o.ID); got.Status != orders.Placed {
		t.Fatalf("order %s while the API was failing, want placed", got.Status)
	}
	l.sendWebhooks(t) // gets through

	if got := l.order(t, me, o.ID); got.Status != orders.Paid {
		t.Fatalf("order %s, want paid", got.Status)
	}
	var status string
	var attempts int
	if err := l.providerDB.QueryRow(context.Background(),
		`SELECT status::text, attempts FROM webhook_deliveries WHERE endpoint_id = $1`, l.endpoint.ID).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "delivered" || attempts != 3 {
		t.Errorf("delivery %s after %d tries, want delivered after 3", status, attempts)
	}
}

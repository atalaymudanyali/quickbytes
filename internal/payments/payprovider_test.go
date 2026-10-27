package payments

// Tests of the plug against the real provider: its own code and database
// (payprovider_test), served over real HTTP by httptest. Only the client's
// timeout is short (300 ms), so the hang card really times out in a test.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atalaymudanyali/quickbytes/internal/payprovider"
	"github.com/atalaymudanyali/quickbytes/internal/testutil"
	"github.com/atalaymudanyali/quickbytes/internal/webhook"
)

// The provider's test cards.
const (
	cardApprove      = "4242424242424242"
	cardDecline      = "4000000000000002"
	cardInsufficient = "4000000000009995"
	cardSlow         = "4000000000007777"
	cardHang         = "4000000000003333"
)

const testTimeout = 300 * time.Millisecond

// newProvider starts the real provider for a merchant of its own and returns a
// plug connected to it.
func newProvider(t *testing.T) *PayProvider {
	t.Helper()
	p, _ := newProviderWith(t, 10*time.Second)
	return p
}

// newProviderWith is newProvider with the slow card's delay chosen, and the
// provider's own payments, so a test can settle slow payments (as its
// settler loop would).
func newProviderWith(t *testing.T, slowDelay time.Duration) (*PayProvider, *payprovider.Payments) {
	t.Helper()
	pool := testutil.PayProviderDB(t)
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	key := "sk_test_" + hex.EncodeToString(b)
	if _, err := payprovider.NewMerchants(pool).Ensure(context.Background(), "test-"+key[8:20], key); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pp := payprovider.NewPayments(pool, slowDelay)
	srv := httptest.NewServer(payprovider.NewServer(pool, pp, logger).Routes())
	t.Cleanup(srv.Close)
	return NewPayProvider(srv.URL, key, testTimeout), pp
}

func authorizeReq(card string) AuthorizeRequest {
	return AuthorizeRequest{Key: uuid.NewString() + ":authorize", AmountKurus: 47990, CardNumber: card, Reference: uuid.NewString()}
}

// An approved card, then every move a payment can make, each answered with
// the payment in our words.
func TestPlugMovesMoney(t *testing.T) {
	p := newProvider(t)
	ctx := context.Background()

	got, err := p.Authorize(ctx, authorizeReq(cardApprove))
	if err != nil || got.Status != Authorized || got.ID == "" || got.AmountKurus != 47990 {
		t.Fatalf("authorize: %+v, %v", got, err)
	}
	if got, err = p.Capture(ctx, uuid.NewString(), got.ID); err != nil || got.Status != Captured {
		t.Fatalf("capture: %+v, %v", got, err)
	}
	if got, err = p.Refund(ctx, uuid.NewString(), got.ID); err != nil || got.Status != Refunded {
		t.Fatalf("refund: %+v, %v", got, err)
	}
	if got, err = p.Get(ctx, got.ID); err != nil || got.Status != Refunded {
		t.Fatalf("get: %+v, %v", got, err)
	}

	held, _ := p.Authorize(ctx, authorizeReq(cardApprove))
	if got, err = p.Void(ctx, uuid.NewString(), held.ID); err != nil || got.Status != Voided {
		t.Fatalf("void: %+v, %v", got, err)
	}
}

// A declined card is an answer, not an error: the payment, failed, with the
// reason in our words. The slow card is an answer too: processing.
func TestPlugDeclinesAndSlowCard(t *testing.T) {
	p := newProvider(t)
	ctx := context.Background()
	for card, code := range map[string]string{cardDecline: CardDeclined, cardInsufficient: InsufficientFunds} {
		got, err := p.Authorize(ctx, authorizeReq(card))
		if err != nil || got.Status != Failed || got.ID == "" || got.DeclineCode == nil || *got.DeclineCode != code {
			t.Errorf("%s: %+v, %v; want failed with %s", card, got, err, code)
		}
	}
	if got, err := p.Authorize(ctx, authorizeReq(cardSlow)); err != nil || got.Status != Processing {
		t.Errorf("slow card: %+v, %v", got, err)
	}
}

// A move the payment's status doesn't allow (voiding a captured payment)
// comes back as the payment as it is, so the caller sees where it stands.
func TestPlugRefusedMoveShowsThePayment(t *testing.T) {
	p := newProvider(t)
	ctx := context.Background()
	pay, _ := p.Authorize(ctx, authorizeReq(cardApprove))
	if _, err := p.Capture(ctx, uuid.NewString(), pay.ID); err != nil {
		t.Fatal(err)
	}
	got, err := p.Void(ctx, uuid.NewString(), pay.ID)
	if err != nil || got.Status != Captured {
		t.Fatalf("got %+v, %v; want the payment, still captured", got, err)
	}
}

// Requests the provider judges and refuses are a final no.
func TestPlugFinalNo(t *testing.T) {
	p := newProvider(t)
	ctx := context.Background()

	_, err := p.Authorize(ctx, authorizeReq("1234123412341234"))
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Code != "unknown_test_card" || errors.Is(err, ErrNoAnswer) {
		t.Errorf("unknown card: %v", err)
	}
	if _, err := p.Get(ctx, uuid.NewString()); !IsRefused(err) {
		t.Errorf("unknown payment: %v", err)
	}
}

// The hang card: the money is held at once, but the answer comes too late.
// The call gives up after the client's timeout with "no answer", not a
// failure. Asking afterwards finds the payment, and a retry with the same key
// gets the same payment back: one charge, however often we ask.
func TestPlugTimeoutIsNoAnswer(t *testing.T) {
	p := newProvider(t)
	ctx := context.Background()
	req := authorizeReq(cardHang)

	start := time.Now()
	_, err := p.Authorize(ctx, req)
	if !errors.Is(err, ErrNoAnswer) || IsRefused(err) {
		t.Fatalf("got %v, want ErrNoAnswer", err)
	}
	if took := time.Since(start); took > testTimeout+time.Second {
		t.Fatalf("gave up after %v, want about %v", took, testTimeout)
	}

	found, err := p.FindByReference(ctx, req.Reference)
	if err != nil || len(found) != 1 || found[0].Status != Authorized {
		t.Fatalf("find by reference: %+v, %v; want one authorized payment", found, err)
	}

	// The provider stored its answer before hanging, so a retry with the same
	// key is answered with it (it hangs again, so give this one more time).
	slow := NewPayProvider(p.baseURL, p.apiKey, 0)
	slowCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	again, err := slow.Authorize(slowCtx, req)
	if err != nil || again.ID != found[0].ID {
		t.Fatalf("retry with the same key: %+v, %v; want payment %s", again, err, found[0].ID)
	}
	if found, _ := p.FindByReference(ctx, req.Reference); len(found) != 1 {
		t.Fatalf("%d payments after the retry, want 1", len(found))
	}
}

// Nothing listening, a provider that breaks, a provider still busy with the
// same key, a wrong API key: none of them decides anything about the
// payment, so all are "no answer".
func TestPlugNoAnswers(t *testing.T) {
	ctx := context.Background()

	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	cases := map[string]*PayProvider{"nothing listening": NewPayProvider(closed.URL, "k", testTimeout)}

	for name, answer := range map[string]struct {
		status int
		body   string
	}{
		"server error": {500, `{"error":{"code":"internal_error","message":"boom"}}`},
		"in progress":  {409, `{"error":{"code":"idempotency_in_progress","message":"wait"}}`},
		"wrong key":    {401, `{"error":{"code":"unauthenticated","message":"no"}}`},
		"not JSON":     {200, `<html>`},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(answer.status)
			_, _ = io.WriteString(w, answer.body)
		}))
		t.Cleanup(srv.Close)
		cases[name] = NewPayProvider(srv.URL, "k", testTimeout)
	}
	cases["wrong key at the real provider"] = NewPayProvider(newProvider(t).baseURL, "sk_test_wrong", testTimeout)

	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := p.Capture(ctx, "key", "pay"); !errors.Is(err, ErrNoAnswer) || IsRefused(err) {
				t.Errorf("got %v, want ErrNoAnswer", err)
			}
		})
	}
}

// The plug reads the webhooks the real provider writes: its own Event type,
// encoded and signed as its sender does.
func TestPlugReadsProviderWebhooks(t *testing.T) {
	const secret = "whsec_test_0123456789abcdef0123456789"
	now := time.Now()
	ref := uuid.NewString()
	declined := payprovider.DeclineCardDeclined
	event := func(typ, status string, decline *string) []byte {
		b, err := json.Marshal(payprovider.Event{
			ID: uuid.New(), Type: typ, CreatedAt: now,
			Data: payprovider.EventData{Payment: payprovider.Payment{
				ID: uuid.New(), AmountKurus: 47990, Currency: "TRY", Status: status, DeclineCode: decline,
				CardLast4: "4242", Reference: &ref, CreatedAt: now, UpdatedAt: now,
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	body := event("payment.failed", payprovider.StatusFailed, &declined)
	ev, err := ParsePayProviderWebhook(secret, webhook.Sign(secret, now, body), body, now)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Type != "payment.failed" || ev.Payment.Status != Failed || ev.Payment.DeclineCode == nil ||
		*ev.Payment.DeclineCode != CardDeclined || ev.Payment.AmountKurus != 47990 || ev.Reference != ref || ev.ID == "" {
		t.Errorf("parsed %+v", ev)
	}

	good := event("payment.authorized", payprovider.StatusAuthorized, nil)
	mixed := event("payment.captured", payprovider.StatusAuthorized, nil)
	unknown := event("payment.disputed", "disputed", nil)
	for name, tc := range map[string]struct {
		sig  string
		body []byte
		want error
	}{
		"wrong secret":           {webhook.Sign("whsec_someone_else_0123456789abcdef", now, good), good, ErrBadWebhook},
		"no signature":           {"", good, ErrBadWebhook},
		"10 minutes old":         {webhook.Sign(secret, now.Add(-10*time.Minute), good), good, ErrBadWebhook},
		"not JSON":               {webhook.Sign(secret, now, []byte("hello")), []byte("hello"), ErrBadWebhook},
		"type and status differ": {webhook.Sign(secret, now, mixed), mixed, ErrBadWebhook},
		"a type we don't know":   {webhook.Sign(secret, now, unknown), unknown, ErrUnsupportedWebhook},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParsePayProviderWebhook(secret, tc.sig, tc.body, now); !errors.Is(err, tc.want) {
				t.Errorf("got %v, want %v", err, tc.want)
			}
		})
	}
}

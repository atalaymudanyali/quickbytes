package payprovider

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const testSecret = "whsec_test_0123456789abcdef0123456789"

// withEndpoint gives the merchant a webhook endpoint.
func withEndpoint(t *testing.T, pool *pgxpool.Pool, merchant uuid.UUID) Endpoint {
	t.Helper()
	ep, err := NewEndpoints(pool).Ensure(context.Background(), merchant, "http://merchant.test/webhooks", testSecret)
	if err != nil {
		t.Fatal(err)
	}
	return ep
}

// queued returns, oldest first, the event status of each webhook queued for a
// payment.
func queued(t *testing.T, pool *pgxpool.Pool, paymentID uuid.UUID) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT ev.status::text FROM webhook_deliveries d
		JOIN payment_events ev ON ev.id = d.payment_event_id
		WHERE ev.payment_id = $1 ORDER BY d.id`, paymentID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// Every event after the payment is decided queues one webhook, in order.
func TestEventsQueueWebhooks(t *testing.T) {
	ctx := context.Background()
	p, pool, merchant := newPayments(t)
	withEndpoint(t, pool, merchant)

	refunded := authorize(t, p, merchant, cardApprove)
	if _, err := p.Capture(ctx, merchant, refunded.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Refund(ctx, merchant, refunded.ID); err != nil {
		t.Fatal(err)
	}
	if got := queued(t, pool, refunded.ID); !slices.Equal(got, []string{StatusAuthorized, StatusCaptured, StatusRefunded}) {
		t.Errorf("approve, capture, refund queued %v", got)
	}

	voided := authorize(t, p, merchant, cardApprove)
	if _, err := p.Void(ctx, merchant, voided.ID); err != nil {
		t.Fatal(err)
	}
	if got := queued(t, pool, voided.ID); !slices.Equal(got, []string{StatusAuthorized, StatusVoided}) {
		t.Errorf("approve, void queued %v", got)
	}

	declined := authorize(t, p, merchant, cardDecline)
	if got := queued(t, pool, declined.ID); !slices.Equal(got, []string{StatusFailed}) {
		t.Errorf("decline queued %v", got)
	}

	// A refused move changes nothing, so it queues nothing.
	if _, err := p.Capture(ctx, merchant, voided.ID); err == nil {
		t.Fatal("capturing a voided payment worked")
	}
	if got := queued(t, pool, voided.ID); len(got) != 2 {
		t.Errorf("after a refused capture: %v", got)
	}
}

// The slow card queues nothing while processing ("not decided yet" isn't
// news), and its webhook when the settler decides it.
func TestSettlerQueuesTheLateWebhook(t *testing.T) {
	ctx := context.Background()
	p, pool, merchant := newPayments(t)
	withEndpoint(t, pool, merchant)

	slow := authorize(t, p, merchant, cardSlow)
	if got := queued(t, pool, slow.ID); len(got) != 0 {
		t.Fatalf("processing queued %v", got)
	}
	makeDue(t, p, slow.ID)
	if _, err := p.SettleDue(ctx); err != nil {
		t.Fatal(err)
	}
	if got := queued(t, pool, slow.ID); !slices.Equal(got, []string{StatusAuthorized}) {
		t.Errorf("settling queued %v", got)
	}
}

// No endpoint, or a disabled one: events are still recorded, but no webhook
// is queued.
func TestNoEndpointNoWebhooks(t *testing.T) {
	ctx := context.Background()
	p, pool, merchant := newPayments(t)

	before := authorize(t, p, merchant, cardApprove)
	if got := queued(t, pool, before.ID); len(got) != 0 {
		t.Errorf("without an endpoint: queued %v", got)
	}

	withEndpoint(t, pool, merchant)
	if err := NewEndpoints(pool).Disable(ctx, merchant); err != nil {
		t.Fatal(err)
	}
	disabled := authorize(t, p, merchant, cardApprove)
	if got := queued(t, pool, disabled.ID); len(got) != 0 {
		t.Errorf("with a disabled endpoint: queued %v", got)
	}
	if h := history(t, pool, disabled.ID); !slices.Equal(h, []string{StatusAuthorized}) {
		t.Errorf("history = %v, want the event recorded anyway", h)
	}

	// Ensure turns it back on.
	withEndpoint(t, pool, merchant)
	after := authorize(t, p, merchant, cardApprove)
	if got := queued(t, pool, after.ID); len(got) != 1 {
		t.Errorf("re-enabled: queued %v", got)
	}
}

// Other merchants' endpoints get nothing about this merchant's payments.
func TestWebhooksGoOnlyToTheirMerchant(t *testing.T) {
	p, pool, merchant := newPayments(t)
	other, _ := newMerchant(t, pool)
	withEndpoint(t, pool, other.ID)

	pay := authorize(t, p, merchant, cardApprove)
	if got := queued(t, pool, pay.ID); len(got) != 0 {
		t.Errorf("queued %v for another merchant's endpoint", got)
	}
}

// Break it: when the change is undone, so is its webhook. No webhook is ever
// sent about something that didn't happen.
func TestUndoneChangeQueuesNothing(t *testing.T) {
	ctx := context.Background()
	p, pool, merchant := newPayments(t)
	withEndpoint(t, pool, merchant)
	boom := errors.New("could not store the answer")
	fail := func(context.Context, pgx.Tx, Payment) error { return boom }

	pay := authorize(t, p, merchant, cardApprove)
	if _, err := p.MoveAnd(ctx, merchant, pay.ID, ActionCapture, fail); !errors.Is(err, boom) {
		t.Fatalf("capture: got %v, want the finish error", err)
	}
	if got := queued(t, pool, pay.ID); !slices.Equal(got, []string{StatusAuthorized}) {
		t.Errorf("queued %v, want only the authorize's webhook", got)
	}
}

// Ensure replaces the URL and secret: one endpoint per merchant.
func TestEnsureEndpointReplaces(t *testing.T) {
	ctx := context.Background()
	pool, merchant := newPaymentsPool(t)
	endpoints := NewEndpoints(pool)

	first, err := endpoints.Ensure(ctx, merchant, "http://old.test/hook", testSecret)
	if err != nil {
		t.Fatal(err)
	}
	second, err := endpoints.Ensure(ctx, merchant, "https://new.test/hook", testSecret+"-new")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.URL != "https://new.test/hook" || second.Secret != testSecret+"-new" || !second.Enabled {
		t.Errorf("after replacing: %+v", second)
	}

	// The database refuses what the config check would also refuse.
	if _, err := endpoints.Ensure(ctx, merchant, "ftp://x.test", testSecret); err == nil {
		t.Error("a non-HTTP URL was accepted")
	}
	if _, err := endpoints.Ensure(ctx, merchant, "http://x.test", "short"); err == nil {
		t.Error("a short secret was accepted")
	}
}

func newPaymentsPool(t *testing.T) (*pgxpool.Pool, uuid.UUID) {
	t.Helper()
	_, pool, merchant := newPayments(t)
	return pool, merchant
}

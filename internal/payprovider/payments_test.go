package payprovider

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atalaymudanyali/quickbytes/internal/testutil"
)

const (
	cardApprove      = "4242424242424242"
	cardDecline      = "4000000000000002"
	cardInsufficient = "4000000000009995"
	cardSlow         = "4000000000007777"
	cardHang         = "4000000000003333"
)

// newPayments returns a store and a fresh merchant to own the payments.
func newPayments(t *testing.T) (*Payments, *pgxpool.Pool, uuid.UUID) {
	t.Helper()
	pool := testutil.PayProviderDB(t)
	m, _ := newMerchant(t, pool)
	return NewPayments(pool, 10*time.Second), pool, m.ID
}

func authorize(t *testing.T, p *Payments, merchant uuid.UUID, card string) Payment {
	t.Helper()
	pay, err := p.Authorize(context.Background(), merchant, AuthorizeInput{AmountKurus: 32000, CardNumber: card})
	if err != nil {
		t.Fatalf("authorize with %s: %v", card, err)
	}
	return pay
}

// history returns the statuses in a payment's event history, oldest first.
func history(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		"SELECT status::text FROM payment_events WHERE payment_id = $1 ORDER BY id", id)
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan history: %v", err)
		}
		got = append(got, s)
	}
	return got
}

func TestAuthorizeTestCards(t *testing.T) {
	p, pool, merchant := newPayments(t)
	tests := []struct {
		card        string
		status      string
		declineCode string
	}{
		{cardApprove, StatusAuthorized, ""},
		{cardDecline, StatusFailed, DeclineCardDeclined},
		{cardInsufficient, StatusFailed, DeclineInsufficientFunds},
		{cardSlow, StatusProcessing, ""},
		// The hang card is held at once; only its answer is late (HTTP layer).
		{cardHang, StatusAuthorized, ""},
	}
	for _, tt := range tests {
		t.Run(tt.card, func(t *testing.T) {
			pay := authorize(t, p, merchant, tt.card)
			if pay.Status != tt.status {
				t.Errorf("status = %s, want %s", pay.Status, tt.status)
			}
			gotCode := ""
			if pay.DeclineCode != nil {
				gotCode = *pay.DeclineCode
			}
			if gotCode != tt.declineCode {
				t.Errorf("decline code = %q, want %q", gotCode, tt.declineCode)
			}
			if pay.CardLast4 != tt.card[12:] || pay.AmountKurus != 32000 || pay.Currency != "TRY" {
				t.Errorf("payment = %+v", pay)
			}
			if h := history(t, pool, pay.ID); !slices.Equal(h, []string{tt.status}) {
				t.Errorf("history = %v, want [%s]", h, tt.status)
			}
		})
	}

	t.Run("slow card settles later", func(t *testing.T) {
		pay := authorize(t, p, merchant, cardSlow)
		var in time.Duration
		err := pool.QueryRow(context.Background(),
			"SELECT settle_at - created_at FROM payments WHERE id = $1", pay.ID).Scan(&in)
		if err != nil {
			t.Fatal(err)
		}
		if in < 9*time.Second || in > 11*time.Second {
			t.Errorf("settles in %v, want about 10s", in)
		}
	})
}

func TestAuthorizeRefusesBadRequests(t *testing.T) {
	p, _, merchant := newPayments(t)
	tests := []struct {
		name string
		in   AuthorizeInput
		want error
	}{
		{"zero amount", AuthorizeInput{AmountKurus: 0, CardNumber: cardApprove}, ErrInvalidAmount},
		{"negative amount", AuthorizeInput{AmountKurus: -100, CardNumber: cardApprove}, ErrInvalidAmount},
		{"too much", AuthorizeInput{AmountKurus: MaxAmountKurus + 1, CardNumber: cardApprove}, ErrInvalidAmount},
		{"not a test card", AuthorizeInput{AmountKurus: 100, CardNumber: "5555555555554444"}, ErrUnknownCard},
		{"empty card", AuthorizeInput{AmountKurus: 100}, ErrUnknownCard},
		{"empty reference", AuthorizeInput{AmountKurus: 100, CardNumber: cardApprove, Reference: ptr("")}, ErrInvalidReference},
		{"long reference", AuthorizeInput{AmountKurus: 100, CardNumber: cardApprove, Reference: ptr(strings.Repeat("x", 256))}, ErrInvalidReference},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := p.Authorize(context.Background(), merchant, tt.in); !errors.Is(err, tt.want) {
				t.Errorf("got %v, want %v", err, tt.want)
			}
		})
	}

	// The biggest amount is fine, and the reference is kept.
	pay, err := p.Authorize(context.Background(), merchant,
		AuthorizeInput{AmountKurus: MaxAmountKurus, CardNumber: cardApprove, Reference: ptr("order-42")})
	if err != nil || pay.Reference == nil || *pay.Reference != "order-42" {
		t.Errorf("max amount with reference: %+v, %v", pay, err)
	}
}

func TestPaymentLifecycles(t *testing.T) {
	ctx := context.Background()
	p, pool, merchant := newPayments(t)

	t.Run("authorize, capture, refund", func(t *testing.T) {
		pay := authorize(t, p, merchant, cardApprove)
		if got, err := p.Capture(ctx, merchant, pay.ID); err != nil || got.Status != StatusCaptured {
			t.Fatalf("capture: %v, %v", got.Status, err)
		}
		if got, err := p.Refund(ctx, merchant, pay.ID); err != nil || got.Status != StatusRefunded {
			t.Fatalf("refund: %v, %v", got.Status, err)
		}
		want := []string{StatusAuthorized, StatusCaptured, StatusRefunded}
		if h := history(t, pool, pay.ID); !slices.Equal(h, want) {
			t.Errorf("history = %v, want %v", h, want)
		}
		if got, err := p.Get(ctx, merchant, pay.ID); err != nil || got.Status != StatusRefunded {
			t.Errorf("get: %v, %v", got.Status, err)
		}
	})

	t.Run("authorize, void", func(t *testing.T) {
		pay := authorize(t, p, merchant, cardApprove)
		if got, err := p.Void(ctx, merchant, pay.ID); err != nil || got.Status != StatusVoided {
			t.Fatalf("void: %v, %v", got.Status, err)
		}
		want := []string{StatusAuthorized, StatusVoided}
		if h := history(t, pool, pay.ID); !slices.Equal(h, want) {
			t.Errorf("history = %v, want %v", h, want)
		}
	})
}

func TestIllegalMovesAreRefused(t *testing.T) {
	ctx := context.Background()
	p, pool, merchant := newPayments(t)

	type step func(context.Context, uuid.UUID, uuid.UUID) (Payment, error)
	tests := []struct {
		name   string
		card   string
		before []step // moves that succeed first
		action string
		move   step
		status string // the status the refusal reports
	}{
		{"capture a voided payment", cardApprove, []step{p.Void}, "capture", p.Capture, StatusVoided},
		{"capture twice", cardApprove, []step{p.Capture}, "capture", p.Capture, StatusCaptured},
		{"refund twice", cardApprove, []step{p.Capture, p.Refund}, "refund", p.Refund, StatusRefunded},
		{"refund before capture", cardApprove, nil, "refund", p.Refund, StatusAuthorized},
		{"void a captured payment", cardApprove, []step{p.Capture}, "void", p.Void, StatusCaptured},
		{"void twice", cardApprove, []step{p.Void}, "void", p.Void, StatusVoided},
		{"capture a declined payment", cardDecline, nil, "capture", p.Capture, StatusFailed},
		{"capture while processing", cardSlow, nil, "capture", p.Capture, StatusProcessing},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pay := authorize(t, p, merchant, tt.card)
			for i, s := range tt.before {
				if _, err := s(ctx, merchant, pay.ID); err != nil {
					t.Fatalf("step %d: %v", i+1, err)
				}
			}
			before := history(t, pool, pay.ID)

			_, err := tt.move(ctx, merchant, pay.ID)
			var se *StateError
			if !errors.As(err, &se) || se.Action != tt.action || se.Status != tt.status {
				t.Fatalf("got %v, want StateError{%s, %s}", err, tt.action, tt.status)
			}
			// A refused move changes nothing.
			if h := history(t, pool, pay.ID); !slices.Equal(h, before) {
				t.Errorf("history changed: %v, was %v", h, before)
			}
		})
	}
}

func TestOtherMerchantsPaymentsAreNotFound(t *testing.T) {
	ctx := context.Background()
	p, pool, merchant := newPayments(t)
	other, _ := newMerchant(t, pool)
	pay := authorize(t, p, merchant, cardApprove)

	if _, err := p.Get(ctx, other.ID, pay.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("get: got %v, want ErrNotFound", err)
	}
	if _, err := p.Capture(ctx, other.ID, pay.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("capture: got %v, want ErrNotFound", err)
	}
	if _, err := p.Get(ctx, merchant, uuid.Must(uuid.NewV7())); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown ID: got %v, want ErrNotFound", err)
	}
	// Still authorized: the other merchant's capture did nothing.
	if got, _ := p.Get(ctx, merchant, pay.ID); got.Status != StatusAuthorized {
		t.Errorf("status = %s, want authorized", got.Status)
	}
}

// Two captures at the same moment, made deterministic: the test changes the
// payment in its own transaction and holds it open, then a real Capture starts.
// The Capture must wait for the test's transaction, then see the payment is
// already captured and refuse, instead of capturing it a second time.
//
// The test uses its own SQL (not Capture) for the first change, so it doesn't
// rely on the code it checks.
func TestCaptureWaitsForAConcurrentChange(t *testing.T) {
	ctx := context.Background()
	p, pool, merchant := newPayments(t)
	pay := authorize(t, p, merchant, cardApprove)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		"UPDATE payments SET status = 'captured' WHERE id = $1", pay.ID); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := p.Capture(ctx, merchant, pay.ID)
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("capture didn't wait for the other change: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	err = <-done
	var se *StateError
	if !errors.As(err, &se) || se.Status != StatusCaptured {
		t.Fatalf("capture after the other one: got %v, want StateError (captured)", err)
	}
}

// The same race with real goroutines: ten captures at once, one wins.
func TestParallelCapturesCaptureOnce(t *testing.T) {
	ctx := context.Background()
	p, pool, merchant := newPayments(t)
	pay := authorize(t, p, merchant, cardApprove)

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		won     int
		refused int
	)
	for range 10 {
		wg.Go(func() {
			_, err := p.Capture(ctx, merchant, pay.ID)
			mu.Lock()
			defer mu.Unlock()
			var se *StateError
			switch {
			case err == nil:
				won++
			case errors.As(err, &se):
				refused++
			default:
				t.Errorf("capture: %v", err)
			}
		})
	}
	wg.Wait()

	if won != 1 || refused != 9 {
		t.Errorf("won %d, refused %d; want 1 and 9", won, refused)
	}
	want := []string{StatusAuthorized, StatusCaptured}
	if h := history(t, pool, pay.ID); !slices.Equal(h, want) {
		t.Errorf("history = %v, want %v", h, want)
	}
}

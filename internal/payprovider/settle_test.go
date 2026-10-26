package payprovider

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
)

// makeDue moves a processing payment's settle time into the past, with the
// test's own SQL, instead of waiting for it.
func makeDue(t *testing.T, p *Payments, id uuid.UUID) {
	t.Helper()
	if _, err := p.pool.Exec(context.Background(),
		"UPDATE payments SET settle_at = now() - interval '1 second' WHERE id = $1", id); err != nil {
		t.Fatal(err)
	}
}

func TestSettleDueFinishesOnlyDuePayments(t *testing.T) {
	ctx := context.Background()
	p, pool, merchant := newPayments(t)
	due := authorize(t, p, merchant, cardSlow)
	notYet := authorize(t, p, merchant, cardSlow)
	makeDue(t, p, due.ID)

	if _, err := p.SettleDue(ctx); err != nil {
		t.Fatal(err)
	}

	got, _ := p.Get(ctx, merchant, due.ID)
	if got.Status != StatusAuthorized {
		t.Errorf("due payment: %s, want authorized", got.Status)
	}
	if h := history(t, pool, due.ID); !slices.Equal(h, []string{StatusProcessing, StatusAuthorized}) {
		t.Errorf("history = %v", h)
	}
	// Settled once: settling again adds nothing.
	if _, err := p.SettleDue(ctx); err != nil {
		t.Fatal(err)
	}
	if h := history(t, pool, due.ID); len(h) != 2 {
		t.Errorf("history after a second run = %v", h)
	}
	if got, _ := p.Get(ctx, merchant, notYet.ID); got.Status != StatusProcessing {
		t.Errorf("payment not due yet: %s, want processing", got.Status)
	}

	// Once authorized, the slow payment captures like any other.
	if _, err := p.Capture(ctx, merchant, due.ID); err != nil {
		t.Errorf("capture after settling: %v", err)
	}
}

// A payment another transaction holds is skipped, not waited for: the
// settler moves on, and settles it on a later run.
func TestSettleSkipsLockedPayments(t *testing.T) {
	ctx := context.Background()
	p, _, merchant := newPayments(t)
	pay := authorize(t, p, merchant, cardSlow)
	makeDue(t, p, pay.ID)

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT 1 FROM payments WHERE id = $1 FOR UPDATE", pay.ID); err != nil {
		t.Fatal(err)
	}

	// Without SKIP LOCKED this would wait for tx and hit the deadline.
	short, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if _, err := p.SettleDue(short); err != nil {
		t.Fatalf("settle while a payment is locked: %v", err)
	}
	if got, _ := p.Get(ctx, merchant, pay.ID); got.Status != StatusProcessing {
		t.Errorf("locked payment: %s, want processing (skipped)", got.Status)
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := p.SettleDue(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.Get(ctx, merchant, pay.ID); got.Status != StatusAuthorized {
		t.Errorf("after the lock is gone: %s, want authorized", got.Status)
	}
}

// The whole slow-card story over HTTP, with the real loop: processing at
// first, authorized when GET asks a little later.
func TestSlowCardSettlesInTheBackground(t *testing.T) {
	c := newClient(t)
	p := NewPayments(c.pool, 300*time.Millisecond)
	c.h = validatedBySpec(t, NewServer(c.pool, p, discardLogger()).Routes())

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go p.RunSettler(ctx, 50*time.Millisecond, discardLogger())

	rec := c.authorize(cardSlow, 5000)
	pay := decodePayment(t, rec)
	if rec.Code != http.StatusCreated || pay.Status != StatusProcessing {
		t.Fatalf("authorize: %d %s", rec.Code, rec.Body)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		got := decodePayment(t, c.get("/v1/payments/"+pay.ID.String()))
		if got.Status == StatusAuthorized {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("still %s after 3 s", got.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The hang card: the money is held at once, but the answer comes only after
// hangFor. Meanwhile, a caller that gave up can already find out what
// happened, with GET or with a retry of the same key, which is how the API
// will recover from a timeout without authorizing twice.
func TestHangCard(t *testing.T) {
	c := newClient(t)
	srv := NewServer(c.pool, NewPayments(c.pool, time.Second), discardLogger())
	srv.hangFor = time.Second
	c.h = validatedBySpec(t, srv.Routes())

	key := newKey()
	ref := "hang-" + uuid.NewString()
	body := `{"amount_kurus": 5000, "card_number": "` + cardHang + `", "reference": "` + ref + `"}`

	start := time.Now()
	type answer struct {
		rec  *httptest.ResponseRecorder
		took time.Duration
	}
	first := make(chan answer, 1)
	go func() {
		rec := c.post("/v1/payments", key, body)
		first <- answer{rec, time.Since(start)}
	}()

	// Wait for the payment to exist (it's stored before the wait begins).
	var id uuid.UUID
	for id == uuid.Nil {
		if time.Since(start) > 900*time.Millisecond {
			t.Fatal("no payment stored before the hang")
		}
		_ = c.pool.QueryRow(context.Background(),
			"SELECT id FROM payments WHERE reference = $1", ref).Scan(&id)
		time.Sleep(20 * time.Millisecond)
	}

	// The caller gave up; GET says the money is held.
	if got := decodePayment(t, c.get("/v1/payments/"+id.String())); got.Status != StatusAuthorized {
		t.Errorf("GET during the hang: %s, want authorized", got.Status)
	}
	// A retry with the same key gets the stored answer at once.
	retry := c.post("/v1/payments", key, body)
	if retry.Code != http.StatusCreated || retry.Header().Get(replayedHeader) != "true" {
		t.Errorf("retry during the hang: %d %s", retry.Code, retry.Body)
	}
	if time.Since(start) >= time.Second {
		t.Errorf("the retry waited for the hang (%v)", time.Since(start))
	}

	a := <-first
	if a.took < time.Second {
		t.Errorf("first answer after %v, want at least 1 s", a.took)
	}
	if a.rec.Code != http.StatusCreated || strings.TrimSpace(a.rec.Body.String()) != strings.TrimSpace(retry.Body.String()) {
		t.Errorf("first answer %d %s, retry %s", a.rec.Code, a.rec.Body, retry.Body)
	}
	if n := c.countPayments(); n != 1 {
		t.Errorf("%d payments, want 1", n)
	}
	var p Payment
	_ = json.Unmarshal(a.rec.Body.Bytes(), &p)
	if p.ID != id {
		t.Errorf("answer is payment %v, want %v", p.ID, id)
	}
}

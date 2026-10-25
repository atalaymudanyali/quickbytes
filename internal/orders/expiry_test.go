package orders

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The tests use the real 15-minute window and make their own orders look old,
// instead of shrinking the window: other packages' tests place orders in the
// same database at the same time, and those must not expire under them.
const testPayWindow = 15 * time.Minute

// age moves an order's created_at back, as if it was placed that long ago.
func (f fixture) age(t *testing.T, id uuid.UUID, by time.Duration) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE orders SET created_at = created_at - make_interval(secs => $2) WHERE id = $1`,
		id, by.Seconds()); err != nil {
		t.Fatal(err)
	}
}

// expireAll runs ExpireUnpaid until a run comes back short of a full batch.
// Earlier test runs may have left old placed orders behind, more than one batch.
func (f fixture) expireAll(t *testing.T) {
	t.Helper()
	for {
		n, err := f.orders.ExpireUnpaid(context.Background(), testPayWindow)
		if err != nil {
			t.Fatal(err)
		}
		if n < expiryBatch {
			return
		}
	}
}

func TestUnpaidOrdersExpire(t *testing.T) {
	f := setup(t)
	p := f.cast(t)

	old := f.newOrder(t, p, Placed)
	f.age(t, old, 16*time.Minute)
	fresh := f.newOrder(t, p, Placed)
	f.age(t, fresh, 14*time.Minute)
	oldButPaid := f.newOrder(t, p, Paid)
	f.age(t, oldButPaid, time.Hour)

	f.expireAll(t)

	if s, r := f.status(t, old); s != Cancelled || r == nil || *r != Expired {
		t.Fatalf("old unpaid order: %s (%v), want cancelled (expired)", s, r)
	}
	if h := f.history(t, old); len(h) != 1 || h[0].From != Placed || h[0].ByRole != "system" {
		t.Fatalf("history %+v; want one row placed → cancelled by the system", h)
	}
	if s, _ := f.status(t, fresh); s != Placed {
		t.Fatalf("14-minute-old order: %s, want still placed", s)
	}
	if s, _ := f.status(t, oldButPaid); s != Paid {
		t.Fatalf("paid order: %s, want still paid", s)
	}

	// Running again changes nothing: an expired order isn't placed any more.
	f.expireAll(t)
	if h := f.history(t, old); len(h) != 1 {
		t.Fatalf("a second run added history: %d rows", len(h))
	}
}

// An order someone else has locked right now (the payment arriving, or another
// API copy expiring it) is skipped, not waited for, and picked up next time.
func TestExpirySkipsLockedOrders(t *testing.T) {
	f := setup(t)
	p := f.cast(t)
	ctx := context.Background()
	id := f.newOrder(t, p, Placed)
	f.age(t, id, 16*time.Minute)

	hold, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback(ctx)
	if _, err := hold.Exec(ctx, `SELECT 1 FROM orders WHERE id = $1 FOR UPDATE`, id); err != nil {
		t.Fatal(err)
	}

	// t.Fatal may only be called from the test's own goroutine, so this one
	// hands its error back.
	done := make(chan error, 1)
	go func() {
		_, err := f.orders.ExpireUnpaid(ctx, testPayWindow)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("expiry waited for the locked order instead of skipping it")
	}
	if s, _ := f.status(t, id); s != Placed {
		t.Fatalf("locked order: %s, want skipped (still placed)", s)
	}

	if err := hold.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	f.expireAll(t)
	if s, _ := f.status(t, id); s != Cancelled {
		t.Fatalf("after the lock is gone: %s, want cancelled", s)
	}
}

func TestRunExpiryStopsWithTheContext(t *testing.T) {
	f := setup(t)
	p := f.cast(t)
	id := f.newOrder(t, p, Placed)
	f.age(t, id, 16*time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		f.orders.RunExpiry(ctx, 10*time.Millisecond, testPayWindow, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(stopped)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if s, _ := f.status(t, id); s == Cancelled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the loop didn't expire the order")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("the loop kept running after its context was cancelled")
	}
}

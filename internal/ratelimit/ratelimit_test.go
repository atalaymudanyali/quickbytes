package ratelimit_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/atalaymudanyali/quickbytes/internal/cache"
	"github.com/atalaymudanyali/quickbytes/internal/ratelimit"
	"github.com/atalaymudanyali/quickbytes/internal/testutil"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// Tests share one Redis, so each uses its own subject (a unique email works well).

func TestHitBlocksOverTheLimit(t *testing.T) {
	ctx := context.Background()
	l := ratelimit.New(testutil.Redis(t), quiet)
	rule := ratelimit.Rule{Name: "test:hit", Limit: 3, Window: time.Minute}
	subject := testutil.UniqueEmail()

	for i := 1; i <= 3; i++ {
		if r := l.Hit(ctx, rule, subject); !r.Allowed {
			t.Fatalf("attempt %d refused; the limit is 3", i)
		}
	}
	r := l.Hit(ctx, rule, subject)
	if r.Allowed {
		t.Fatal("attempt 4 allowed; the limit is 3")
	}
	if r.RetryAfter <= 0 || r.RetryAfter > time.Minute {
		t.Errorf("RetryAfter = %v, want between 0 and the 1 minute window", r.RetryAfter)
	}

	// Another subject has its own counter.
	if r := l.Hit(ctx, rule, testutil.UniqueEmail()); !r.Allowed {
		t.Error("a different subject was refused")
	}
}

func TestCheckDoesNotCount(t *testing.T) {
	ctx := context.Background()
	l := ratelimit.New(testutil.Redis(t), quiet)
	rule := ratelimit.Rule{Name: "test:check", Limit: 2, Window: time.Minute}
	subject := testutil.UniqueEmail()

	for i := 0; i < 5; i++ {
		if r := l.Check(ctx, rule, subject); !r.Allowed {
			t.Fatal("Check refused before any Hit; it must not count")
		}
	}
	l.Hit(ctx, rule, subject)
	if r := l.Check(ctx, rule, subject); !r.Allowed {
		t.Fatal("refused after 1 failure; the limit is 2")
	}
	l.Hit(ctx, rule, subject)
	r := l.Check(ctx, rule, subject)
	if r.Allowed {
		t.Fatal("allowed after 2 failures; the limit is 2")
	}
	if r.RetryAfter <= 0 {
		t.Errorf("RetryAfter = %v, want > 0", r.RetryAfter)
	}

	l.Reset(ctx, rule, subject)
	if r := l.Check(ctx, rule, subject); !r.Allowed {
		t.Error("still refused after Reset")
	}
}

func TestCounterExpiresAfterTheWindow(t *testing.T) {
	ctx := context.Background()
	l := ratelimit.New(testutil.Redis(t), quiet)
	rule := ratelimit.Rule{Name: "test:expiry", Limit: 1, Window: 100 * time.Millisecond}
	subject := testutil.UniqueEmail()

	l.Hit(ctx, rule, subject)
	if r := l.Hit(ctx, rule, subject); r.Allowed {
		t.Fatal("attempt 2 allowed; the limit is 1")
	}
	time.Sleep(150 * time.Millisecond)
	if r := l.Hit(ctx, rule, subject); !r.Allowed {
		t.Error("still refused after the window ended")
	}
}

func TestFailsOpen(t *testing.T) {
	ctx := context.Background()
	rule := ratelimit.Rule{Name: "test:open", Limit: 0, Window: time.Minute} // would refuse everything

	t.Run("no Redis", func(t *testing.T) {
		l := ratelimit.New(nil, quiet)
		if !l.Hit(ctx, rule, "x").Allowed || !l.Check(ctx, rule, "x").Allowed {
			t.Error("refused without Redis; the limiter must fail open")
		}
		l.Reset(ctx, rule, "x") // must not panic
	})

	t.Run("Redis down", func(t *testing.T) {
		down, err := cache.Open("redis://127.0.0.1:1/0") // nothing listens on port 1
		if err != nil {
			t.Fatal(err)
		}
		defer down.Close()
		l := ratelimit.New(down, quiet)

		start := time.Now()
		if !l.Hit(ctx, rule, "x").Allowed || !l.Check(ctx, rule, "x").Allowed {
			t.Error("refused with Redis down; the limiter must fail open")
		}
		if took := time.Since(start); took > 3*time.Second {
			t.Errorf("took %v; a broken Redis must not slow logins down for long", took)
		}
	})
}

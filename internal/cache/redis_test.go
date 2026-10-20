package cache_test

import (
	"context"
	"testing"
	"time"

	"github.com/atalaymudanyali/quickbytes/internal/cache"
	"github.com/atalaymudanyali/quickbytes/internal/testutil"
)

func TestPing(t *testing.T) {
	c := testutil.Redis(t) // skips without TEST_REDIS_URL, and fails if the ping fails
	if err := c.Set(context.Background(), "test:ping", "pong", time.Minute).Err(); err != nil {
		t.Fatalf("write to redis: %v", err)
	}
}

func TestBadURL(t *testing.T) {
	if _, err := cache.Open("not a url"); err == nil {
		t.Fatal("an invalid URL was accepted")
	}
}

// If Redis is unreachable, the client must give up quickly, so that callers can
// fall back to the database instead of making every request wait.
func TestUnreachableRedisFailsFast(t *testing.T) {
	c, err := cache.Open("redis://127.0.0.1:1/0") // port 1: nothing listens there
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	start := time.Now()
	err = c.Get(context.Background(), "anything").Err()
	if err == nil {
		t.Fatal("expected an error from an unreachable Redis")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("gave up after %v; a cache that's down must not slow requests down", took)
	}
}

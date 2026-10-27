package outbox

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atalaymudanyali/quickbytes/internal/testutil"
)

// stamp sets an event's created_at and published_at to that long ago; a
// negative publishedAgo leaves it waiting.
func stamp(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, createdAgo, publishedAgo time.Duration) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		UPDATE outbox SET created_at = now() - make_interval(secs => $2),
			published_at = CASE WHEN $3 < 0 THEN NULL ELSE now() - make_interval(secs => $3) END
		WHERE id = $1`, id, createdAgo.Seconds(), publishedAgo.Seconds())
	if err != nil {
		t.Fatal(err)
	}
}

func exists(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM outbox WHERE id = $1)`, id).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestCleanupDeletesOnlyOldPublishedEvents(t *testing.T) {
	pool := testutil.DB(t)
	ids := add(t, pool, uuid.New(), uuid.New(), uuid.New(), uuid.New())
	day := 24 * time.Hour
	oldPublished, recentPublished, oldWaiting, fresh := ids[0], ids[1], ids[2], ids[3]
	stamp(t, pool, oldPublished, 9*day, 8*day)
	stamp(t, pool, recentPublished, 9*day, 6*day) // created long ago, but published recently
	stamp(t, pool, oldWaiting, 30*day, -1)        // never sent: must survive any age

	for {
		n, err := DeletePublished(context.Background(), pool)
		if err != nil {
			t.Fatal(err)
		}
		if n < cleanupBatch {
			break
		}
	}
	if exists(t, pool, oldPublished) {
		t.Fatal("event published 8 days ago is still there")
	}
	for name, id := range map[string]uuid.UUID{"published 6 days ago": recentPublished, "waiting": oldWaiting, "fresh": fresh} {
		if !exists(t, pool, id) {
			t.Fatalf("event %s was deleted", name)
		}
	}
}

// More than one batch: RunCleanup keeps going until a short batch comes back.
func TestRunCleanupDeletesInBatches(t *testing.T) {
	pool := testutil.DB(t)
	ctx := context.Background()
	agg := uuid.New()
	n := cleanupBatch + 200
	_, err := pool.Exec(ctx, `
		INSERT INTO outbox (id, topic, aggregate_id, type, payload, created_at, published_at)
		SELECT gen_random_uuid(), 'orders', $1, 'order.test', '{}', now() - interval '9 days', now() - interval '8 days'
		FROM generate_series(1, $2)`, agg, n)
	if err != nil {
		t.Fatal(err)
	}
	count := func() int {
		var c int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE aggregate_id = $1`, agg).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}

	runCtx, cancel := context.WithCancel(ctx)
	stopped := make(chan struct{})
	go func() {
		RunCleanup(runCtx, pool, 10*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(stopped)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for count() > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d old events left after 5 s", count(), n)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("RunCleanup didn't stop with its context")
	}
}

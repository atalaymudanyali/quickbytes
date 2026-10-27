package outbox

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// Keep is how long published events stay in the outbox: long enough to
	// look into what was sent, or publish something again by hand.
	Keep = 7 * 24 * time.Hour
	// cleanupBatch caps how many rows one delete takes, so a big backlog is
	// deleted in short statements instead of one long one.
	cleanupBatch = 1000
)

// DeletePublished deletes up to cleanupBatch events published more than Keep
// ago, and returns how many it deleted. Events still waiting are never
// deleted, however old: they haven't been sent yet.
func DeletePublished(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	tag, err := pool.Exec(ctx, `
		DELETE FROM outbox
		WHERE id IN (
			SELECT id FROM outbox
			WHERE published_at < now() - make_interval(secs => $1)
			LIMIT $2)`,
		Keep.Seconds(), cleanupBatch)
	if err != nil {
		return 0, fmt.Errorf("delete published outbox events: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// RunCleanup deletes old published events every interval until ctx is
// cancelled (the API shutting down). Like the idempotency cleanup, it keeps
// going while full batches come back, and a failed run is logged and tried
// again next time.
func RunCleanup(ctx context.Context, pool *pgxpool.Pool, every time.Duration, logger *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for {
			n, err := DeletePublished(ctx, pool)
			if err != nil {
				if ctx.Err() == nil {
					logger.Error("delete published outbox events", "err", err)
				}
				break
			}
			if n > 0 {
				logger.Info("deleted published outbox events", "count", n)
			}
			if n < cleanupBatch {
				break
			}
		}
	}
}

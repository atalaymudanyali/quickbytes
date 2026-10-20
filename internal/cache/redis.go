// Package cache connects to Redis, the in-memory store used to keep ready-made
// copies of often-read data (like menus) so they don't have to be rebuilt from
// PostgreSQL on every request.
package cache

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// Open creates a Redis client from a URL like redis://localhost:6379/0. It doesn't
// contact Redis yet: the client connects on first use, and reconnects by itself
// if Redis restarts.
//
// The timeouts are short on purpose. A cache is only useful if it's fast; if
// Redis is slow or unreachable, it's better to give up within a fraction of a
// second and read from the database than to make every request wait.
func Open(url string) (*redis.Client, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	opts.DialTimeout = time.Second
	opts.ReadTimeout = 200 * time.Millisecond
	opts.WriteTimeout = 200 * time.Millisecond
	// Don't retry: one failed attempt is enough to fall back to the database.
	// (The library's defaults retry a command 3 times and a connection 5 times.)
	opts.MaxRetries = -1
	opts.DialerRetries = 1
	return redis.NewClient(opts), nil
}

// UseLogger sends the Redis library's own messages (like "failed to dial") to our
// structured logger, so they appear as JSON log lines like everything else
// instead of plain text in the middle of the log.
func UseLogger(l *slog.Logger) {
	redis.SetLogger(slogPrinter{l})
}

type slogPrinter struct{ l *slog.Logger }

func (p slogPrinter) Printf(ctx context.Context, format string, v ...any) {
	p.l.WarnContext(ctx, "redis client", "detail", fmt.Sprintf(format, v...))
}

// Ping checks Redis answers, giving up after a second.
func Ping(ctx context.Context, c *redis.Client) error {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return c.Ping(ctx).Err()
}

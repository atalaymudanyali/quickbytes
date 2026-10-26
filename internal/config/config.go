// Package config reads service settings from environment variables, so the same
// binary runs unchanged on a laptop, in Docker and in CI.
package config

import (
	"fmt"
	"os"
	"time"
)

type Config struct {
	HTTPAddr    string
	DatabaseURL string
	// RedisURL is optional: without it the API runs with no cache and reads
	// everything from the database.
	RedisURL        string
	JWTSecret       []byte
	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration
	ShutdownTimeout time.Duration
	// OrderPayWindow is how long a placed order may wait for its payment before
	// it's cancelled as expired; OrderExpiryEvery is how often that's checked.
	OrderPayWindow   time.Duration
	OrderExpiryEvery time.Duration
	// IdempotencyCleanupEvery is how often idempotency keys older than 24
	// hours are deleted.
	IdempotencyCleanupEvery time.Duration
}

func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:        getenv("HTTP_ADDR", ":8080"),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		RedisURL:        os.Getenv("REDIS_URL"),
		JWTSecret:       []byte(os.Getenv("JWT_SECRET")),
		AccessTokenTTL:  15 * time.Minute,
		RefreshTokenTTL: 30 * 24 * time.Hour,
		ShutdownTimeout: 10 * time.Second,
	}
	var err error
	// Both can be shortened to watch an order expire in a demo, e.g.
	// ORDER_PAY_WINDOW=1m ORDER_EXPIRY_EVERY=5s.
	if cfg.OrderPayWindow, err = getDuration("ORDER_PAY_WINDOW", 15*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.OrderExpiryEvery, err = getDuration("ORDER_EXPIRY_EVERY", 30*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.IdempotencyCleanupEvery, err = getDuration("IDEMPOTENCY_CLEANUP_EVERY", 10*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	if len(cfg.JWTSecret) < 32 {
		return Config{}, fmt.Errorf("JWT_SECRET is required and must be at least 32 characters (generate one with: openssl rand -base64 48)")
	}
	return cfg, nil
}

// getDuration reads a duration like "15m" or "30s".
func getDuration(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration like 15m or 30s, got %q", key, v)
	}
	return d, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

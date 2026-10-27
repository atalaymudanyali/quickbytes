// Package config reads service settings from environment variables, so the same
// binary runs unchanged on a laptop, in Docker and in CI.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
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
	// PayProviderURL and PayProviderKey are where the payment provider is and
	// the API's merchant key there. PayProviderTimeout is how long one call
	// may take before it counts as unanswered.
	PayProviderURL     string
	PayProviderKey     string
	PayProviderTimeout time.Duration
	// PayProviderWebhookSecret checks the signatures on the provider's
	// webhooks (the provider signs with the same one).
	PayProviderWebhookSecret string
	// PaymentReconcileEvery is how often the reconcile loop looks for
	// payments that need work (an unknown result, a slow card, a capture
	// that didn't go through).
	PaymentReconcileEvery time.Duration
	// OutboxRelay turns on the relay that publishes order events from the
	// outbox to the event stream at KafkaBrokers ("host:port", comma
	// separated). With it off, events wait in the outbox. OutboxRelayEvery
	// is how often the relay looks for waiting events.
	OutboxRelay      bool
	KafkaBrokers     []string
	OutboxRelayEvery time.Duration
	// OutboxCleanupEvery is how often published events older than 7 days
	// are deleted from the outbox.
	OutboxCleanupEvery time.Duration
}

func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:                 getenv("HTTP_ADDR", ":8080"),
		DatabaseURL:              os.Getenv("DATABASE_URL"),
		RedisURL:                 os.Getenv("REDIS_URL"),
		JWTSecret:                []byte(os.Getenv("JWT_SECRET")),
		AccessTokenTTL:           15 * time.Minute,
		RefreshTokenTTL:          30 * 24 * time.Hour,
		ShutdownTimeout:          10 * time.Second,
		PayProviderURL:           getenv("PAYPROVIDER_URL", "http://localhost:8090"),
		PayProviderKey:           os.Getenv("PAYPROVIDER_KEY"),
		PayProviderWebhookSecret: os.Getenv("PAYPROVIDER_WEBHOOK_SECRET"),
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
	if cfg.PayProviderTimeout, err = getDuration("PAYPROVIDER_TIMEOUT", 5*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.PaymentReconcileEvery, err = getDuration("PAYMENT_RECONCILE_EVERY", 5*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.OutboxRelayEvery, err = getDuration("OUTBOX_RELAY_EVERY", 500*time.Millisecond); err != nil {
		return Config{}, err
	}
	if cfg.OutboxCleanupEvery, err = getDuration("OUTBOX_CLEANUP_EVERY", time.Hour); err != nil {
		return Config{}, err
	}
	if cfg.OutboxRelay, err = getBool("OUTBOX_RELAY"); err != nil {
		return Config{}, err
	}
	for b := range strings.SplitSeq(os.Getenv("KAFKA_BROKERS"), ",") {
		if b = strings.TrimSpace(b); b != "" {
			cfg.KafkaBrokers = append(cfg.KafkaBrokers, b)
		}
	}
	if cfg.OutboxRelay && len(cfg.KafkaBrokers) == 0 {
		return Config{}, fmt.Errorf("OUTBOX_RELAY is on, so KAFKA_BROKERS is required: where Redpanda is (localhost:19092 from this machine, redpanda:9092 in Docker)")
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	if len(cfg.JWTSecret) < 32 {
		return Config{}, fmt.Errorf("JWT_SECRET is required and must be at least 32 characters (generate one with: openssl rand -base64 48)")
	}
	if cfg.PayProviderKey == "" {
		return Config{}, fmt.Errorf("PAYPROVIDER_KEY is required: the API's merchant key at the payment provider (in development, the PAYPROVIDER_MERCHANT_KEY in docker-compose.yml)")
	}
	if len(cfg.PayProviderWebhookSecret) < 32 {
		return Config{}, fmt.Errorf("PAYPROVIDER_WEBHOOK_SECRET is required and must be at least 32 characters: it checks the provider's webhooks (in development, the WEBHOOK_SECRET in docker-compose.yml)")
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

// getBool reads true/false (also 1/0); unset is false.
func getBool(key string) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false, got %q", key, v)
	}
	return b, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

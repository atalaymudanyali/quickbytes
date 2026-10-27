package payprovider

import (
	"fmt"
	"os"
	"time"
)

// Config holds the provider's settings, read from environment variables like
// the API's.
type Config struct {
	HTTPAddr    string
	DatabaseURL string
	// MerchantName and MerchantKey describe the one merchant (QuickBytes) that
	// may call the provider. It's created at startup if missing; a changed key
	// replaces the old one (a key rotation).
	MerchantName string
	MerchantKey  string
	// WebhookURL is where the merchant's webhooks go; empty turns them off.
	// WebhookSecret signs them (the merchant checks with the same one).
	WebhookURL    string
	WebhookSecret string
	// SlowCardDelay is how long a payment with the slow test card stays
	// processing before it's authorized.
	SlowCardDelay time.Duration
	// SettleEvery is how often processing payments are checked for being due.
	SettleEvery time.Duration
	// IdempotencyCleanupEvery is how often keys older than 24 hours are
	// deleted.
	IdempotencyCleanupEvery time.Duration
	ShutdownTimeout         time.Duration
}

// minKeyLength is the shortest merchant key accepted: long enough that it
// can't be guessed.
const minKeyLength = 32

func LoadConfig() (Config, error) {
	cfg := Config{
		HTTPAddr:        getenv("HTTP_ADDR", ":8090"),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		MerchantName:    getenv("PAYPROVIDER_MERCHANT_NAME", "quickbytes"),
		MerchantKey:     os.Getenv("PAYPROVIDER_MERCHANT_KEY"),
		WebhookURL:      os.Getenv("WEBHOOK_URL"),
		WebhookSecret:   os.Getenv("WEBHOOK_SECRET"),
		ShutdownTimeout: 10 * time.Second,
	}
	var err error
	if cfg.SlowCardDelay, err = getDuration("SLOW_CARD_DELAY", 10*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.SettleEvery, err = getDuration("SETTLE_EVERY", time.Second); err != nil {
		return Config{}, err
	}
	if cfg.IdempotencyCleanupEvery, err = getDuration("IDEMPOTENCY_CLEANUP_EVERY", 10*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	if len(cfg.MerchantKey) < minKeyLength {
		return Config{}, fmt.Errorf("PAYPROVIDER_MERCHANT_KEY is required and must be at least %d characters (generate one with: echo sk_test_$(openssl rand -hex 24))", minKeyLength)
	}
	if cfg.WebhookURL != "" && len(cfg.WebhookSecret) < minKeyLength {
		return Config{}, fmt.Errorf("WEBHOOK_SECRET is required with WEBHOOK_URL and must be at least %d characters (generate one with: echo whsec_$(openssl rand -hex 24))", minKeyLength)
	}
	return cfg, nil
}

func getDuration(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration like 10s or 1m, got %q", key, v)
	}
	return d, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

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
	MerchantName    string
	MerchantKey     string
	ShutdownTimeout time.Duration
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
		ShutdownTimeout: 10 * time.Second,
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	if len(cfg.MerchantKey) < minKeyLength {
		return Config{}, fmt.Errorf("PAYPROVIDER_MERCHANT_KEY is required and must be at least %d characters (generate one with: echo sk_test_$(openssl rand -hex 24))", minKeyLength)
	}
	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

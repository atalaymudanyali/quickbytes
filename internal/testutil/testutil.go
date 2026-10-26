// Package testutil has helpers shared by tests that need a real database.
package testutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/atalaymudanyali/quickbytes/internal/cache"
	"github.com/atalaymudanyali/quickbytes/internal/database"
	"github.com/atalaymudanyali/quickbytes/migrations"
	providermigrations "github.com/atalaymudanyali/quickbytes/migrations/payprovider"
)

// DB connects to the test database and applies migrations. The test is skipped
// when TEST_DATABASE_URL isn't set, so `go test ./...` still works without Docker.
func DB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := database.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)
	// Migrate once per test binary (one per package), not once per test.
	migrateOnce.Do(func() { migrateErr = database.Migrate(ctx, pool, migrations.FS) })
	if migrateErr != nil {
		t.Fatalf("migrate test database: %v", migrateErr)
	}
	return pool
}

var (
	migrateOnce sync.Once
	migrateErr  error
)

// PayProviderDB is DB for the mock payment provider: it connects to the
// provider's own test database (TEST_PAYPROVIDER_DATABASE_URL) and applies the
// provider's migrations. Skipped when the variable isn't set.
func PayProviderDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_PAYPROVIDER_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_PAYPROVIDER_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := database.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect to payment provider test database: %v", err)
	}
	t.Cleanup(pool.Close)
	providerMigrateOnce.Do(func() {
		providerMigrateErr = database.Migrate(ctx, pool, providermigrations.FS)
	})
	if providerMigrateErr != nil {
		t.Fatalf("migrate payment provider test database: %v", providerMigrateErr)
	}
	return pool
}

var (
	providerMigrateOnce sync.Once
	providerMigrateErr  error
)

// UniqueEmail returns an email no other test uses. Test packages run in parallel
// against the same database, so fixed emails would collide.
func UniqueEmail() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "test-" + hex.EncodeToString(b) + "@example.com"
}

// RedisIfConfigured is like Redis, but returns nil instead of skipping when
// TEST_REDIS_URL isn't set, for code that works with or without a cache.
func RedisIfConfigured(t *testing.T) *redis.Client {
	t.Helper()
	if os.Getenv("TEST_REDIS_URL") == "" {
		return nil
	}
	return Redis(t)
}

// Redis connects to the test Redis. The test is skipped when TEST_REDIS_URL isn't
// set. Tests share one Redis, so they should use keys no other test uses.
func Redis(t *testing.T) *redis.Client {
	t.Helper()
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL not set")
	}
	c, err := cache.Open(url)
	if err != nil {
		t.Fatalf("open test redis: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := cache.Ping(context.Background(), c); err != nil {
		t.Fatalf("test redis not reachable: %v", err)
	}
	return c
}

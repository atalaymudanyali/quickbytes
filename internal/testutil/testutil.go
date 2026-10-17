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

	"github.com/atalaymudanyali/quickbytes/internal/database"
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
	migrateOnce.Do(func() { migrateErr = database.Migrate(ctx, pool) })
	if migrateErr != nil {
		t.Fatalf("migrate test database: %v", migrateErr)
	}
	return pool
}

var (
	migrateOnce sync.Once
	migrateErr  error
)

// UniqueEmail returns an email no other test uses. Test packages run in parallel
// against the same database, so fixed emails would collide.
func UniqueEmail() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "test-" + hex.EncodeToString(b) + "@example.com"
}

package database

import (
	"context"
	"os"
	"testing"

	"github.com/atalaymudanyali/quickbytes/migrations"
)

// These tests need a real PostgreSQL with PostGIS. Locally, `make test` starts one
// with Docker Compose; in CI it runs as a service container.
func testDatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	return url
}

func TestMigrateIsRepeatable(t *testing.T) {
	ctx := context.Background()
	pool, err := Connect(ctx, testDatabaseURL(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	// Running twice must be safe: the second run finds nothing new to apply.
	for i := range 2 {
		if err := Migrate(ctx, pool, migrations.FS); err != nil {
			t.Fatalf("migrate run %d: %v", i+1, err)
		}
	}

	var version string
	if err := pool.QueryRow(ctx, "SELECT postgis_version()").Scan(&version); err != nil {
		t.Fatalf("PostGIS not available after migrating: %v", err)
	}
}

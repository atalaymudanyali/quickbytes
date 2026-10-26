// Package database opens the PostgreSQL connection pool and applies migrations.
package database

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

// Connect opens a pool of reusable connections and checks the database answers.
func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.MaxConns = 10
	cfg.MaxConnIdleTime = 5 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

// Migrate brings the schema up to the newest migration in set: the API's
// (migrations.FS) or another service's, each applied to its own database.
// Goose records applied migrations in a table in that database, so running it
// again only applies new ones.
func Migrate(ctx context.Context, pool *pgxpool.Pool, set fs.FS) error {
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	return migrate(ctx, db, set)
}

func migrate(ctx context.Context, db *sql.DB, set fs.FS) error {
	// Several copies of a service may start at once. The session locker takes a
	// Postgres advisory lock, so only one copy applies migrations and the
	// others wait. Advisory locks belong to one database, so the API and the
	// payment provider migrating at the same time don't wait for each other.
	// Retry every second, for up to 5 minutes (the default retries every 5s).
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockTimeout(1, 300))
	if err != nil {
		return fmt.Errorf("create migration lock: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, set,
		goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

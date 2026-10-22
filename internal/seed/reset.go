package seed

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atalaymudanyali/quickbytes/internal/menus"
)

// Reset deletes every demo account, and with it (through ON DELETE CASCADE)
// their profiles, refresh tokens, restaurants and menus. Users on any other
// email domain are never touched. It returns how many accounts were deleted.
//
// The cached menus of the deleted restaurants are dropped after the commit, not
// before: dropping them first would let a reader cache the old menu again while
// the restaurant still existed.
func Reset(ctx context.Context, pool *pgxpool.Pool, cache *menus.Cache) (int, error) {
	// citext makes LIKE case-insensitive too, so DEMO@Demo.QuickBytes.test matches.
	const demoUsers = `SELECT id FROM users WHERE email LIKE '%@' || $1`

	var restaurantIDs []uuid.UUID
	var deleted int
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// Collected before deleting, while the rows still exist, so their cache
		// keys can be dropped afterwards.
		rows, err := tx.Query(ctx, `
			SELECT r.id FROM restaurants r
			JOIN profiles p ON p.id = r.owner_profile_id
			WHERE p.user_id IN (`+demoUsers+`)`, EmailDomain)
		if err != nil {
			return fmt.Errorf("find demo restaurants: %w", err)
		}
		restaurantIDs, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return fmt.Errorf("find demo restaurants: %w", err)
		}

		tag, err := tx.Exec(ctx, `DELETE FROM users WHERE id IN (`+demoUsers+`)`, EmailDomain)
		if err != nil {
			return fmt.Errorf("delete demo users: %w", err)
		}
		deleted = int(tag.RowsAffected())
		return nil
	})
	if err != nil {
		return 0, err
	}

	cache.Forget(ctx, restaurantIDs...)
	return deleted, nil
}

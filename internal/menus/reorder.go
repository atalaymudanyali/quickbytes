package menus

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrOrderMismatch means a new order didn't list exactly the things being ordered:
// something was missing, listed twice, or not part of this menu.
var ErrOrderMismatch = errors.New("the order must list every active entry exactly once")

// ReorderCategories sets the display order of the restaurant's active categories.
// ids must contain each active category exactly once, in the new order. Archived
// categories keep their old positions; if restored, they reappear about where they
// were.
func (s *Store) ReorderCategories(ctx context.Context, restaurantID, ownerProfileID uuid.UUID, ids []uuid.UUID) error {
	return s.change(ctx, restaurantID, ownerProfileID, func(tx pgx.Tx) error {
		return reorder(ctx, tx, ids,
			`SELECT id FROM menu_categories WHERE restaurant_id = $1 AND archived_at IS NULL`, restaurantID,
			`menu_categories`)
	})
}

// ReorderItems sets the display order of one active category's active items.
func (s *Store) ReorderItems(ctx context.Context, restaurantID, ownerProfileID, categoryID uuid.UUID, ids []uuid.UUID) error {
	return s.change(ctx, restaurantID, ownerProfileID, func(tx pgx.Tx) error {
		if err := requireActiveCategory(ctx, tx, restaurantID, categoryID); err != nil {
			return err
		}
		return reorder(ctx, tx, ids,
			`SELECT id FROM menu_items WHERE category_id = $1 AND archived_at IS NULL`, categoryID,
			`menu_items`)
	})
}

// reorder checks that ids is exactly the set returned by currentSQL, then gives
// each one its place in the list as its position (1, 2, 3, ...).
//
// It runs inside change(), which holds the restaurant's lock, so nothing can be
// added or archived between reading the current set and writing the new order.
func reorder(ctx context.Context, tx pgx.Tx, ids []uuid.UUID, currentSQL string, arg any, table string) error {
	rows, err := tx.Query(ctx, currentSQL, arg)
	if err != nil {
		return fmt.Errorf("read current order: %w", err)
	}
	current, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return fmt.Errorf("read current order: %w", err)
	}
	if !sameSet(ids, current) {
		return ErrOrderMismatch
	}

	// unnest turns the list into rows, and WITH ORDINALITY numbers them:
	//   [kebabs, soups, drinks]  →  (kebabs, 1), (soups, 2), (drinks, 3)
	// Each row then gets its number as its new position, all in one statement.
	_, err = tx.Exec(ctx, `
		UPDATE `+table+` AS t SET position = o.pos, updated_at = now()
		FROM unnest($1::uuid[]) WITH ORDINALITY AS o(id, pos)
		WHERE t.id = o.id`, ids)
	if err != nil {
		return fmt.Errorf("write new order: %w", err)
	}
	return nil
}

// sameSet reports whether got lists exactly the IDs in want, each once.
func sameSet(got, want []uuid.UUID) bool {
	if len(got) != len(want) {
		return false
	}
	remaining := make(map[uuid.UUID]bool, len(want))
	for _, id := range want {
		remaining[id] = true
	}
	for _, id := range got {
		if !remaining[id] { // not in this menu, or listed a second time
			return false
		}
		delete(remaining, id)
	}
	return true
}

package menus

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Menu is a whole menu: categories in display order, each with its items.
type Menu struct {
	RestaurantID uuid.UUID `json:"restaurant_id"`
	// Version goes up on every change. Clients and caches can compare it to know
	// whether a menu they stored earlier is still current.
	Version    int64          `json:"version"`
	Categories []MenuCategory `json:"categories"`
}

type MenuCategory struct {
	Category
	Items []Item `json:"items"`
}

// View chooses who is reading.
type View struct {
	// OwnerProfileID set: the owner's view. The restaurant must be theirs, and
	// archived categories and items are included (marked), so they can be
	// restored. uuid.Nil: the public view, without anything archived.
	OwnerProfileID uuid.UUID
}

func (v View) owner() bool { return v.OwnerProfileID != uuid.Nil }

// Menu reads a restaurant's menu.
//
// It takes three queries (restaurant, categories, items). They run in one
// read-only REPEATABLE READ transaction, which makes all three see the database
// as it was at the same moment, like three photos taken with one click. Without
// that, an item added between the second and third query could appear in a
// category the second query didn't see.
func (s *Store) Menu(ctx context.Context, restaurantID uuid.UUID, v View) (Menu, error) {
	m := Menu{RestaurantID: restaurantID, Categories: []MenuCategory{}}
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var owner *uuid.UUID
		if v.owner() {
			owner = &v.OwnerProfileID
		}
		err := tx.QueryRow(ctx, `
			SELECT menu_version FROM restaurants
			WHERE id = $1 AND ($2::uuid IS NULL OR owner_profile_id = $2)`,
			restaurantID, owner).Scan(&m.Version)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRestaurantNotFound
		}
		if err != nil {
			return fmt.Errorf("read restaurant: %w", err)
		}

		// $2 = true means "include archived".
		rows, err := tx.Query(ctx, `
			SELECT `+categoryColumns+` FROM menu_categories
			WHERE restaurant_id = $1 AND ($2 OR archived_at IS NULL)
			ORDER BY position, id`, restaurantID, v.owner())
		if err != nil {
			return fmt.Errorf("read categories: %w", err)
		}
		cats, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Category, error) { return scanCategory(row) })
		if err != nil {
			return fmt.Errorf("read categories: %w", err)
		}

		rows, err = tx.Query(ctx, `
			SELECT `+itemColumns+` FROM menu_items
			WHERE restaurant_id = $1 AND ($2 OR archived_at IS NULL)
			ORDER BY position, id`, restaurantID, v.owner())
		if err != nil {
			return fmt.Errorf("read items: %w", err)
		}
		items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Item, error) { return scanItem(row) })
		if err != nil {
			return fmt.Errorf("read items: %w", err)
		}

		// Put each item under its category. Items whose category was left out
		// (archived, in the public view) are simply dropped.
		index := make(map[uuid.UUID]int, len(cats))
		for i, c := range cats {
			index[c.ID] = i
			m.Categories = append(m.Categories, MenuCategory{Category: c, Items: []Item{}})
		}
		for _, it := range items {
			if i, ok := index[it.CategoryID]; ok {
				m.Categories[i].Items = append(m.Categories[i].Items, it)
			}
		}
		return nil
	})
	if err != nil {
		return Menu{}, err
	}
	return m, nil
}

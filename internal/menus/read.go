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
	Items []MenuItem `json:"items"`
}

// MenuItem is an item as the menu shows it: with its option groups, each with
// its choices, in display order. Unavailable choices are included, marked, like
// sold-out items.
type MenuItem struct {
	Item
	OptionGroups []MenuOptionGroup `json:"option_groups"`
}

type MenuOptionGroup struct {
	OptionGroup
	Choices []OptionChoice `json:"choices"`
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
// It takes five queries (restaurant, categories, items, option groups, option
// choices). They run in one read-only REPEATABLE READ transaction, which makes
// all five see the database as it was at the same moment, like five photos taken
// with one click. Without that, an item added between the second and third query
// could appear in a category the second query didn't see.
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

		// Groups and choices are read for the whole restaurant at once (two
		// queries), not item by item (which would be one query per item: the
		// "N+1 queries" problem). Archived items' options are read too; they're
		// dropped below along with their item when the view leaves it out.
		rows, err = tx.Query(ctx, `
			SELECT `+groupColumns+` FROM option_groups
			WHERE restaurant_id = $1
			ORDER BY position, id`, restaurantID)
		if err != nil {
			return fmt.Errorf("read option groups: %w", err)
		}
		groups, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (MenuOptionGroup, error) {
			g, err := scanGroup(row)
			return MenuOptionGroup{OptionGroup: g, Choices: []OptionChoice{}}, err
		})
		if err != nil {
			return fmt.Errorf("read option groups: %w", err)
		}

		rows, err = tx.Query(ctx, `
			SELECT `+choiceColumns+` FROM option_choices
			WHERE restaurant_id = $1
			ORDER BY position, id`, restaurantID)
		if err != nil {
			return fmt.Errorf("read option choices: %w", err)
		}
		choices, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (OptionChoice, error) { return scanChoice(row) })
		if err != nil {
			return fmt.Errorf("read option choices: %w", err)
		}

		// Put each choice under its group, then each group under its item. The
		// lists are already sorted, so appending keeps display order.
		groupIndex := make(map[uuid.UUID]int, len(groups))
		for i, g := range groups {
			groupIndex[g.ID] = i
		}
		for _, c := range choices {
			i := groupIndex[c.GroupID]
			groups[i].Choices = append(groups[i].Choices, c)
		}
		groupsByItem := make(map[uuid.UUID][]MenuOptionGroup)
		for _, g := range groups {
			groupsByItem[g.ItemID] = append(groupsByItem[g.ItemID], g)
		}

		// Put each item under its category. Items whose category was left out
		// (archived, in the public view) are simply dropped.
		index := make(map[uuid.UUID]int, len(cats))
		for i, c := range cats {
			index[c.ID] = i
			m.Categories = append(m.Categories, MenuCategory{Category: c, Items: []MenuItem{}})
		}
		for _, it := range items {
			if i, ok := index[it.CategoryID]; ok {
				mi := MenuItem{Item: it, OptionGroups: groupsByItem[it.ID]}
				if mi.OptionGroups == nil {
					mi.OptionGroups = []MenuOptionGroup{} // [] in JSON, not null
				}
				m.Categories[i].Items = append(m.Categories[i].Items, mi)
			}
		}
		return nil
	})
	if err != nil {
		return Menu{}, err
	}
	return m, nil
}

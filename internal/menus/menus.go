// Package menus stores restaurant menus: categories ("Kebaplar") and the items in
// them ("Adana Kebap, 320 TL").
package menus

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Category struct {
	ID         uuid.UUID  `json:"id"`
	Name       string     `json:"name"`
	Position   int        `json:"position"`
	ArchivedAt *time.Time `json:"archived_at,omitempty"`
}

type Item struct {
	ID          uuid.UUID  `json:"id"`
	CategoryID  uuid.UUID  `json:"category_id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	PriceKurus  int        `json:"price_kurus"`
	Available   bool       `json:"available"`
	Position    int        `json:"position"`
	ArchivedAt  *time.Time `json:"archived_at,omitempty"`
}

type NewItem struct {
	CategoryID  uuid.UUID
	Name        string
	Description string
	PriceKurus  int
}

// ItemPatch holds the fields an owner can change. nil means "leave as it is".
type ItemPatch struct {
	CategoryID  *uuid.UUID
	Name        *string
	Description *string
	PriceKurus  *int
	Available   *bool
}

const MaxPriceKurus = 10_000_000 // 100,000 TL

var (
	// ErrRestaurantNotFound means the restaurant doesn't exist or isn't the
	// caller's. The two look the same on purpose.
	ErrRestaurantNotFound = errors.New("restaurant not found")
	ErrCategoryNotFound   = errors.New("category not found")
	ErrItemNotFound       = errors.New("item not found")
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// change runs one menu change in a transaction. Its first statement does three
// jobs at once:
//
//  1. Ownership: it only matches the restaurant if ownerProfileID owns it.
//  2. Version: it bumps menu_version, so caches know the menu changed.
//  3. Lock: updating the row locks it until the transaction ends, so two changes
//     to the same menu run one after the other, never interleaved. That makes
//     "check, then act" steps inside fn safe (e.g. "is this category still
//     active? then add the item to it").
func (s *Store) change(ctx context.Context, restaurantID, ownerProfileID uuid.UUID, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE restaurants SET menu_version = menu_version + 1
			WHERE id = $1 AND owner_profile_id = $2`,
			restaurantID, ownerProfileID)
		if err != nil {
			return fmt.Errorf("bump menu version: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrRestaurantNotFound
		}
		return fn(tx)
	})
}

// --- Categories ---------------------------------------------------------------

const categoryColumns = `id, name, position, archived_at`

func scanCategory(row pgx.Row) (Category, error) {
	var c Category
	err := row.Scan(&c.ID, &c.Name, &c.Position, &c.ArchivedAt)
	return c, err
}

// CreateCategory adds a category at the end of the menu.
func (s *Store) CreateCategory(ctx context.Context, restaurantID, ownerProfileID uuid.UUID, name string) (Category, error) {
	var c Category
	err := s.change(ctx, restaurantID, ownerProfileID, func(tx pgx.Tx) error {
		var err error
		c, err = scanCategory(tx.QueryRow(ctx, `
			INSERT INTO menu_categories (id, restaurant_id, name, position)
			VALUES ($1, $2, $3,
				(SELECT COALESCE(MAX(position), 0) + 1 FROM menu_categories WHERE restaurant_id = $2))
			RETURNING `+categoryColumns,
			newID(), restaurantID, name))
		if err != nil {
			return fmt.Errorf("insert category: %w", err)
		}
		return nil
	})
	return c, err
}

func (s *Store) RenameCategory(ctx context.Context, restaurantID, ownerProfileID, categoryID uuid.UUID, name string) (Category, error) {
	return s.updateCategory(ctx, restaurantID, ownerProfileID, categoryID,
		`name = $3, updated_at = now()`, name)
}

// SetCategoryArchived hides (true) or restores (false) a category. Its items keep
// their own archived state: restoring the category brings back exactly the items
// that were visible before.
func (s *Store) SetCategoryArchived(ctx context.Context, restaurantID, ownerProfileID, categoryID uuid.UUID, archived bool) (Category, error) {
	return s.updateCategory(ctx, restaurantID, ownerProfileID, categoryID,
		`archived_at = CASE WHEN $3 THEN COALESCE(archived_at, now()) ELSE NULL END, updated_at = now()`, archived)
}

func (s *Store) updateCategory(ctx context.Context, restaurantID, ownerProfileID, categoryID uuid.UUID, set string, arg any) (Category, error) {
	var c Category
	err := s.change(ctx, restaurantID, ownerProfileID, func(tx pgx.Tx) error {
		var err error
		c, err = scanCategory(tx.QueryRow(ctx, `
			UPDATE menu_categories SET `+set+`
			WHERE id = $1 AND restaurant_id = $2
			RETURNING `+categoryColumns,
			categoryID, restaurantID, arg))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCategoryNotFound
		}
		if err != nil {
			return fmt.Errorf("update category: %w", err)
		}
		return nil
	})
	return c, err
}

// --- Items --------------------------------------------------------------------

const itemColumns = `id, category_id, name, description, price_kurus, available, position, archived_at`

func scanItem(row pgx.Row) (Item, error) {
	var it Item
	err := row.Scan(&it.ID, &it.CategoryID, &it.Name, &it.Description, &it.PriceKurus,
		&it.Available, &it.Position, &it.ArchivedAt)
	return it, err
}

// CreateItem adds an item at the end of an active category of this restaurant.
func (s *Store) CreateItem(ctx context.Context, restaurantID, ownerProfileID uuid.UUID, n NewItem) (Item, error) {
	var it Item
	err := s.change(ctx, restaurantID, ownerProfileID, func(tx pgx.Tx) error {
		if err := requireActiveCategory(ctx, tx, restaurantID, n.CategoryID); err != nil {
			return err
		}
		var err error
		it, err = scanItem(tx.QueryRow(ctx, `
			INSERT INTO menu_items (id, restaurant_id, category_id, name, description, price_kurus, position)
			VALUES ($1, $2, $3, $4, $5, $6, `+nextItemPosition("$3")+`)
			RETURNING `+itemColumns,
			newID(), restaurantID, n.CategoryID, n.Name, n.Description, n.PriceKurus))
		if err != nil {
			return fmt.Errorf("insert item: %w", err)
		}
		return nil
	})
	return it, err
}

// UpdateItem applies a patch. Moving an item to another category puts it at the
// end of that category.
func (s *Store) UpdateItem(ctx context.Context, restaurantID, ownerProfileID, itemID uuid.UUID, p ItemPatch) (Item, error) {
	var it Item
	err := s.change(ctx, restaurantID, ownerProfileID, func(tx pgx.Tx) error {
		if p.CategoryID != nil {
			if err := requireActiveCategory(ctx, tx, restaurantID, *p.CategoryID); err != nil {
				return err
			}
		}
		var err error
		it, err = scanItem(tx.QueryRow(ctx, `
			UPDATE menu_items SET
				position    = CASE WHEN $3::uuid IS NULL OR $3 = category_id THEN position
				                   ELSE `+nextItemPosition("$3")+` END,
				category_id = COALESCE($3, category_id),
				name        = COALESCE($4, name),
				description = COALESCE($5, description),
				price_kurus = COALESCE($6, price_kurus),
				available   = COALESCE($7, available),
				updated_at  = now()
			WHERE id = $1 AND restaurant_id = $2
			RETURNING `+itemColumns,
			itemID, restaurantID, p.CategoryID, p.Name, p.Description, p.PriceKurus, p.Available))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrItemNotFound
		}
		if err != nil {
			return fmt.Errorf("update item: %w", err)
		}
		return nil
	})
	return it, err
}

// SetItemArchived hides (true) or restores (false) an item.
func (s *Store) SetItemArchived(ctx context.Context, restaurantID, ownerProfileID, itemID uuid.UUID, archived bool) (Item, error) {
	var it Item
	err := s.change(ctx, restaurantID, ownerProfileID, func(tx pgx.Tx) error {
		var err error
		it, err = scanItem(tx.QueryRow(ctx, `
			UPDATE menu_items SET
				archived_at = CASE WHEN $3 THEN COALESCE(archived_at, now()) ELSE NULL END,
				updated_at  = now()
			WHERE id = $1 AND restaurant_id = $2
			RETURNING `+itemColumns,
			itemID, restaurantID, archived))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrItemNotFound
		}
		if err != nil {
			return fmt.Errorf("archive item: %w", err)
		}
		return nil
	})
	return it, err
}

// requireActiveCategory checks the category belongs to this restaurant and isn't
// archived. It runs inside change(), which holds the restaurant's lock, so the
// category can't be archived between this check and the write that follows.
func requireActiveCategory(ctx context.Context, tx pgx.Tx, restaurantID, categoryID uuid.UUID) error {
	var ok bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM menu_categories
		               WHERE id = $1 AND restaurant_id = $2 AND archived_at IS NULL)`,
		categoryID, restaurantID).Scan(&ok)
	if err != nil {
		return fmt.Errorf("check category: %w", err)
	}
	if !ok {
		return ErrCategoryNotFound
	}
	return nil
}

// nextItemPosition is the SQL for "one after the last item in this category".
func nextItemPosition(categoryParam string) string {
	return `(SELECT COALESCE(MAX(position), 0) + 1 FROM menu_items WHERE category_id = ` + categoryParam + `::uuid)`
}

func newID() uuid.UUID {
	return uuid.Must(uuid.NewV7())
}

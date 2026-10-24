package menus

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// OptionGroup is one question a dish asks: "Size: pick exactly 1", "Extras: up
// to 3". MinPicks 0 makes it optional; MinPicks = MaxPicks = 1 is "pick one".
type OptionGroup struct {
	ID       uuid.UUID      `json:"id"`
	ItemID   uuid.UUID      `json:"item_id"`
	Name     string         `json:"name"`
	MinPicks int            `json:"min_picks"`
	MaxPicks int            `json:"max_picks"`
	Position int            `json:"position"`
	Choices  []OptionChoice `json:"choices"`
}

// OptionChoice is one answer in a group: "Large, +20 TL".
type OptionChoice struct {
	ID              uuid.UUID `json:"id"`
	GroupID         uuid.UUID `json:"group_id"`
	Name            string    `json:"name"`
	ExtraPriceKurus int       `json:"extra_price_kurus"`
	Available       bool      `json:"available"`
	Position        int       `json:"position"`
}

type NewOptionGroup struct {
	Name     string
	MinPicks int
	MaxPicks int
}

// OptionGroupPatch holds the fields an owner can change. nil means "leave as it is".
type OptionGroupPatch struct {
	Name     *string
	MinPicks *int
	MaxPicks *int
}

type NewOptionChoice struct {
	Name            string
	ExtraPriceKurus int
}

type OptionChoicePatch struct {
	Name            *string
	ExtraPriceKurus *int
	Available       *bool
}

// MaxPicks is the most choices one group may allow: a sanity cap, far above any
// real menu.
const MaxPicks = 20

var (
	ErrOptionGroupNotFound  = errors.New("option group not found")
	ErrOptionChoiceNotFound = errors.New("option choice not found")
	// ErrInvalidPicks means min_picks and max_picks don't fit together (after a
	// patch, the stored value of the one not sent counts too).
	ErrInvalidPicks = errors.New("min_picks and max_picks don't fit together")
)

// --- Groups -------------------------------------------------------------------

const groupColumns = `id, item_id, name, min_picks, max_picks, position`

func scanGroup(row pgx.Row) (OptionGroup, error) {
	g := OptionGroup{Choices: []OptionChoice{}}
	err := row.Scan(&g.ID, &g.ItemID, &g.Name, &g.MinPicks, &g.MaxPicks, &g.Position)
	return g, err
}

// CreateOptionGroup adds a group at the end of an item's groups. The item may be
// archived: like other edits, options can be prepared before restoring it.
func (s *Store) CreateOptionGroup(ctx context.Context, restaurantID, ownerProfileID, itemID uuid.UUID, n NewOptionGroup) (OptionGroup, error) {
	var g OptionGroup
	err := s.change(ctx, restaurantID, ownerProfileID, func(tx pgx.Tx) error {
		// INSERT ... SELECT: the row is only inserted if the SELECT finds the item
		// in this restaurant. No row back means no such item here.
		var err error
		g, err = scanGroup(tx.QueryRow(ctx, `
			INSERT INTO option_groups (id, restaurant_id, item_id, name, min_picks, max_picks, position)
			SELECT $1, $2, $3, $4, $5, $6,
				(SELECT COALESCE(MAX(position), 0) + 1 FROM option_groups WHERE item_id = $3)
			FROM menu_items WHERE id = $3 AND restaurant_id = $2
			RETURNING `+groupColumns,
			newID(), restaurantID, itemID, n.Name, n.MinPicks, n.MaxPicks))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrItemNotFound
		}
		if isPicksViolation(err) {
			return ErrInvalidPicks
		}
		if err != nil {
			return fmt.Errorf("insert option group: %w", err)
		}
		return nil
	})
	return g, err
}

// UpdateOptionGroup applies a patch. The returned group has no choices filled
// in; the menu is where groups are read with their choices.
func (s *Store) UpdateOptionGroup(ctx context.Context, restaurantID, ownerProfileID, groupID uuid.UUID, p OptionGroupPatch) (OptionGroup, error) {
	var g OptionGroup
	err := s.change(ctx, restaurantID, ownerProfileID, func(tx pgx.Tx) error {
		var err error
		g, err = scanGroup(tx.QueryRow(ctx, `
			UPDATE option_groups SET
				name       = COALESCE($3, name),
				min_picks  = COALESCE($4, min_picks),
				max_picks  = COALESCE($5, max_picks),
				updated_at = now()
			WHERE id = $1 AND restaurant_id = $2
			RETURNING `+groupColumns,
			groupID, restaurantID, p.Name, p.MinPicks, p.MaxPicks))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrOptionGroupNotFound
		}
		if isPicksViolation(err) {
			return ErrInvalidPicks
		}
		if err != nil {
			return fmt.Errorf("update option group: %w", err)
		}
		return nil
	})
	return g, err
}

// DeleteOptionGroup removes a group and, through ON DELETE CASCADE, its choices.
// A real delete, not an archive: "temporarily off" is what a choice's available
// flag is for, and orders keep their own copy of names and prices.
func (s *Store) DeleteOptionGroup(ctx context.Context, restaurantID, ownerProfileID, groupID uuid.UUID) error {
	return s.change(ctx, restaurantID, ownerProfileID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM option_groups WHERE id = $1 AND restaurant_id = $2`, groupID, restaurantID)
		if err != nil {
			return fmt.Errorf("delete option group: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrOptionGroupNotFound
		}
		return nil
	})
}

// isPicksViolation reports whether err is the database refusing min/max picks
// that don't fit together. The HTTP layer checks each value on its own, but only
// the database sees the stored value of the one a patch didn't send.
func isPicksViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.ConstraintName == "option_groups_picks_check"
}

// --- Choices ------------------------------------------------------------------

const choiceColumns = `id, group_id, name, extra_price_kurus, available, position`

func scanChoice(row pgx.Row) (OptionChoice, error) {
	var c OptionChoice
	err := row.Scan(&c.ID, &c.GroupID, &c.Name, &c.ExtraPriceKurus, &c.Available, &c.Position)
	return c, err
}

// CreateOptionChoice adds a choice at the end of a group.
func (s *Store) CreateOptionChoice(ctx context.Context, restaurantID, ownerProfileID, groupID uuid.UUID, n NewOptionChoice) (OptionChoice, error) {
	var c OptionChoice
	err := s.change(ctx, restaurantID, ownerProfileID, func(tx pgx.Tx) error {
		var err error
		c, err = scanChoice(tx.QueryRow(ctx, `
			INSERT INTO option_choices (id, restaurant_id, group_id, name, extra_price_kurus, position)
			SELECT $1, $2, $3, $4, $5,
				(SELECT COALESCE(MAX(position), 0) + 1 FROM option_choices WHERE group_id = $3)
			FROM option_groups WHERE id = $3 AND restaurant_id = $2
			RETURNING `+choiceColumns,
			newID(), restaurantID, groupID, n.Name, n.ExtraPriceKurus))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrOptionGroupNotFound
		}
		if err != nil {
			return fmt.Errorf("insert option choice: %w", err)
		}
		return nil
	})
	return c, err
}

func (s *Store) UpdateOptionChoice(ctx context.Context, restaurantID, ownerProfileID, choiceID uuid.UUID, p OptionChoicePatch) (OptionChoice, error) {
	var c OptionChoice
	err := s.change(ctx, restaurantID, ownerProfileID, func(tx pgx.Tx) error {
		var err error
		c, err = scanChoice(tx.QueryRow(ctx, `
			UPDATE option_choices SET
				name              = COALESCE($3, name),
				extra_price_kurus = COALESCE($4, extra_price_kurus),
				available         = COALESCE($5, available),
				updated_at        = now()
			WHERE id = $1 AND restaurant_id = $2
			RETURNING `+choiceColumns,
			choiceID, restaurantID, p.Name, p.ExtraPriceKurus, p.Available))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrOptionChoiceNotFound
		}
		if err != nil {
			return fmt.Errorf("update option choice: %w", err)
		}
		return nil
	})
	return c, err
}

func (s *Store) DeleteOptionChoice(ctx context.Context, restaurantID, ownerProfileID, choiceID uuid.UUID) error {
	return s.change(ctx, restaurantID, ownerProfileID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM option_choices WHERE id = $1 AND restaurant_id = $2`, choiceID, restaurantID)
		if err != nil {
			return fmt.Errorf("delete option choice: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrOptionChoiceNotFound
		}
		return nil
	})
}

// Package carts keeps each customer's shopping basket: one restaurant per cart,
// dishes with their picked options, and no prices. Prices come from the live menu
// every time the cart is read (see price).
package carts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atalaymudanyali/quickbytes/internal/menus"
	"github.com/atalaymudanyali/quickbytes/internal/restaurants"
)

const (
	// MaxQuantity is the most of one line (one dish with one set of options).
	MaxQuantity = 20
	// MaxLines caps how many different lines one cart holds.
	MaxLines = 30
)

var (
	ErrItemNotFound     = errors.New("menu item not found")
	ErrItemUnavailable  = errors.New("menu item not available")
	ErrLineNotFound     = errors.New("cart item not found")
	ErrQuantityTooHigh  = errors.New("quantity too high")
	ErrCartFull         = errors.New("cart full")
	ErrRestaurantClosed = errors.New("restaurant not accepting orders")
)

// MismatchError says the cart already belongs to another restaurant. It carries
// that restaurant, so the app can say "your basket has dishes from X".
type MismatchError struct {
	CartRestaurantID uuid.UUID
}

func (e *MismatchError) Error() string { return "cart is for another restaurant" }

// MenuReader is where the live menu comes from: the menu cache in the API.
type MenuReader interface {
	PublicMenu(ctx context.Context, restaurantID uuid.UUID) (menus.Menu, menus.Result, error)
}

type Store struct {
	pool        *pgxpool.Pool
	menus       MenuReader
	restaurants *restaurants.Store
}

func NewStore(pool *pgxpool.Pool, m MenuReader, r *restaurants.Store) *Store {
	return &Store{pool: pool, menus: m, restaurants: r}
}

// line is a cart_items row as stored: no names, no prices.
type line struct {
	ID         uuid.UUID
	MenuItemID uuid.UUID
	Name       string // the dish's name, for lines whose dish left the menu
	Quantity   int
	ChoiceIDs  []uuid.UUID
}

// sortedIDs returns the IDs in byte order, the one order the database sees, so
// the same picks always make the same array whatever order they were sent in.
func sortedIDs(ids []uuid.UUID) []uuid.UUID {
	out := slices.Clone(ids)
	if out == nil {
		out = []uuid.UUID{}
	}
	slices.SortFunc(out, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	return out
}

// findItem looks a dish up on the live menu. Archived dishes aren't on the public
// menu, so they count as not found; a dish the restaurant switched off is found
// but unavailable.
func (s *Store) findItem(ctx context.Context, restaurantID, itemID uuid.UUID) (menus.MenuItem, error) {
	m, _, err := s.menus.PublicMenu(ctx, restaurantID)
	if err != nil {
		return menus.MenuItem{}, err
	}
	for _, c := range m.Categories {
		for _, it := range c.Items {
			if it.ID == itemID {
				return it, nil
			}
		}
	}
	return menus.MenuItem{}, ErrItemNotFound
}

// checkPicks finds the dish on the live menu and checks the picks against it. It
// runs before the transaction: the menu read may go to Redis, and nothing should
// wait on that while holding a lock.
func (s *Store) checkPicks(ctx context.Context, restaurantID, itemID uuid.UUID, choiceIDs []uuid.UUID) error {
	item, err := s.findItem(ctx, restaurantID, itemID)
	if err != nil {
		return err
	}
	if !item.Available {
		return ErrItemUnavailable
	}
	_, err = menus.ValidateChoices(item, choiceIDs) // a *menus.ChoiceError on bad picks
	return err
}

// Add puts a dish in the customer's cart, creating the cart on the first dish.
// The same dish with the same picks adds to the quantity of the line already
// there instead of making a second line. It returns the line's ID.
func (s *Store) Add(ctx context.Context, customerProfileID, itemID uuid.UUID, quantity int, choiceIDs []uuid.UUID) (uuid.UUID, error) {
	var restaurantID uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT restaurant_id FROM menu_items WHERE id = $1`, itemID).Scan(&restaurantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrItemNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("find menu item: %w", err)
	}
	r, err := s.restaurants.Get(ctx, restaurantID)
	if err != nil {
		return uuid.Nil, err
	}
	if !r.AcceptingOrders {
		return uuid.Nil, ErrRestaurantClosed
	}
	if err := s.checkPicks(ctx, restaurantID, itemID, choiceIDs); err != nil {
		return uuid.Nil, err
	}
	choices := sortedIDs(choiceIDs)

	var lineID uuid.UUID
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Get the customer's cart, or make it, in one statement. On a conflict
		// (the cart already exists) DO UPDATE returns the existing row and locks
		// it until commit, so two adds at once take turns instead of both making
		// a cart or both counting the same lines.
		var cartID, cartRestaurant uuid.UUID
		err := tx.QueryRow(ctx, `
			INSERT INTO carts (id, customer_profile_id, restaurant_id) VALUES ($1, $2, $3)
			ON CONFLICT (customer_profile_id) DO UPDATE SET updated_at = now()
			RETURNING id, restaurant_id`,
			uuid.Must(uuid.NewV7()), customerProfileID, restaurantID).Scan(&cartID, &cartRestaurant)
		if err != nil {
			return fmt.Errorf("get cart: %w", err)
		}
		if cartRestaurant != restaurantID {
			return &MismatchError{CartRestaurantID: cartRestaurant}
		}

		// Same dish, same picks: add to that line.
		var have int
		err = tx.QueryRow(ctx, `
			SELECT id, quantity FROM cart_items
			WHERE cart_id = $1 AND menu_item_id = $2 AND choice_ids = $3`,
			cartID, itemID, choices).Scan(&lineID, &have)
		switch {
		case err == nil:
			if have+quantity > MaxQuantity {
				return ErrQuantityTooHigh
			}
			_, err = tx.Exec(ctx, `UPDATE cart_items SET quantity = $2, updated_at = now() WHERE id = $1`,
				lineID, have+quantity)
			return err
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("find line: %w", err)
		}

		var lines int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM cart_items WHERE cart_id = $1`, cartID).Scan(&lines); err != nil {
			return fmt.Errorf("count lines: %w", err)
		}
		if lines >= MaxLines {
			return ErrCartFull
		}
		lineID = uuid.Must(uuid.NewV7())
		_, err = tx.Exec(ctx, `
			INSERT INTO cart_items (id, cart_id, restaurant_id, menu_item_id, quantity, choice_ids)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			lineID, cartID, restaurantID, itemID, quantity, choices)
		if err != nil {
			return fmt.Errorf("insert line: %w", err)
		}
		return nil
	})
	return lineID, err
}

// LinePatch holds what can change on a line. A nil field is left as it is.
type LinePatch struct {
	Quantity  *int
	ChoiceIDs *[]uuid.UUID
}

// Change applies a patch to one of the customer's lines. New picks are checked
// against the live menu like on Add. If the new picks match another line of the
// same dish, the two lines become one, and that line's ID is returned.
func (s *Store) Change(ctx context.Context, customerProfileID, lineID uuid.UUID, p LinePatch) (uuid.UUID, error) {
	var cartID, restaurantID, itemID uuid.UUID
	err := s.pool.QueryRow(ctx, `
		SELECT ci.cart_id, ci.restaurant_id, ci.menu_item_id FROM cart_items ci
		JOIN carts c ON c.id = ci.cart_id
		WHERE ci.id = $1 AND c.customer_profile_id = $2`,
		lineID, customerProfileID).Scan(&cartID, &restaurantID, &itemID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrLineNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("find line: %w", err)
	}
	if p.ChoiceIDs != nil {
		if err := s.checkPicks(ctx, restaurantID, itemID, *p.ChoiceIDs); err != nil {
			return uuid.Nil, err
		}
	}

	resultID := lineID
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Lock the cart, as Add does, so a change and an add never interleave.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM carts WHERE id = $1 FOR UPDATE`, cartID); err != nil {
			return fmt.Errorf("lock cart: %w", err)
		}
		var have int
		var choices []uuid.UUID
		err := tx.QueryRow(ctx, `SELECT quantity, choice_ids FROM cart_items WHERE id = $1`, lineID).Scan(&have, &choices)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLineNotFound // removed while the picks were being checked
		}
		if err != nil {
			return fmt.Errorf("read line: %w", err)
		}
		if p.Quantity != nil {
			have = *p.Quantity
		}
		if p.ChoiceIDs != nil {
			choices = sortedIDs(*p.ChoiceIDs)
		}

		// Would this become a copy of another line? Then merge into that one.
		var otherID uuid.UUID
		var otherQty int
		err = tx.QueryRow(ctx, `
			SELECT id, quantity FROM cart_items
			WHERE cart_id = $1 AND menu_item_id = $2 AND choice_ids = $3 AND id <> $4`,
			cartID, itemID, choices, lineID).Scan(&otherID, &otherQty)
		switch {
		case err == nil:
			if otherQty+have > MaxQuantity {
				return ErrQuantityTooHigh
			}
			if _, err := tx.Exec(ctx, `DELETE FROM cart_items WHERE id = $1`, lineID); err != nil {
				return fmt.Errorf("merge lines: %w", err)
			}
			resultID = otherID
			_, err = tx.Exec(ctx, `UPDATE cart_items SET quantity = $2, updated_at = now() WHERE id = $1`,
				otherID, otherQty+have)
			return err
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("find matching line: %w", err)
		}

		_, err = tx.Exec(ctx, `
			UPDATE cart_items SET quantity = $2, choice_ids = $3, updated_at = now() WHERE id = $1`,
			lineID, have, choices)
		return err
	})
	return resultID, err
}

// Remove takes a line out. Taking out the last line deletes the cart too, so the
// customer is free to fill a basket at another restaurant.
func (s *Store) Remove(ctx context.Context, customerProfileID, lineID uuid.UUID) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var cartID uuid.UUID
		err := tx.QueryRow(ctx, `
			SELECT c.id FROM carts c JOIN cart_items ci ON ci.cart_id = c.id
			WHERE ci.id = $1 AND c.customer_profile_id = $2
			FOR UPDATE OF c`, lineID, customerProfileID).Scan(&cartID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLineNotFound
		}
		if err != nil {
			return fmt.Errorf("find line: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM cart_items WHERE id = $1`, lineID); err != nil {
			return fmt.Errorf("delete line: %w", err)
		}
		_, err = tx.Exec(ctx, `
			DELETE FROM carts WHERE id = $1
			AND NOT EXISTS (SELECT 1 FROM cart_items WHERE cart_id = $1)`, cartID)
		return err
	})
}

// Clear empties the customer's cart (its lines go with it, by ON DELETE CASCADE).
// An empty cart is already clear, so that's not an error.
func (s *Store) Clear(ctx context.Context, customerProfileID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM carts WHERE customer_profile_id = $1`, customerProfileID)
	if err != nil {
		return fmt.Errorf("clear cart: %w", err)
	}
	return nil
}

// Get reads the customer's cart and prices it from the live menu.
func (s *Store) Get(ctx context.Context, customerProfileID uuid.UUID) (Cart, error) {
	var restaurantID uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT restaurant_id FROM carts WHERE customer_profile_id = $1`,
		customerProfileID).Scan(&restaurantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return price(nil, nil, menus.Menu{}), nil
	}
	if err != nil {
		return Cart{}, fmt.Errorf("read cart: %w", err)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT ci.id, ci.menu_item_id, mi.name, ci.quantity, ci.choice_ids
		FROM cart_items ci
		JOIN carts c ON c.id = ci.cart_id
		JOIN menu_items mi ON mi.id = ci.menu_item_id
		WHERE c.customer_profile_id = $1
		ORDER BY ci.created_at, ci.id`, customerProfileID)
	if err != nil {
		return Cart{}, fmt.Errorf("read lines: %w", err)
	}
	lines, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (line, error) {
		var l line
		err := row.Scan(&l.ID, &l.MenuItemID, &l.Name, &l.Quantity, &l.ChoiceIDs)
		return l, err
	})
	if err != nil {
		return Cart{}, fmt.Errorf("read lines: %w", err)
	}

	r, err := s.restaurants.Get(ctx, restaurantID)
	if err != nil {
		return Cart{}, err
	}
	m, _, err := s.menus.PublicMenu(ctx, restaurantID)
	if err != nil {
		return Cart{}, err
	}
	return price(&r, lines, m), nil
}

package orders

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atalaymudanyali/quickbytes/internal/carts"
	"github.com/atalaymudanyali/quickbytes/internal/users"
)

// Order is an order as it was placed: every name and price in it was copied at
// checkout and never changes after.
type Order struct {
	ID                uuid.UUID       `json:"id"`
	CustomerProfileID uuid.UUID       `json:"-"`
	RestaurantID      uuid.UUID       `json:"restaurant_id"`
	RestaurantName    string          `json:"restaurant_name"`
	Status            Status          `json:"status"`
	CancelReason      *CancelReason   `json:"cancel_reason"`
	Address           DeliveryAddress `json:"address"`
	Items             []Item          `json:"items"`
	SubtotalKurus     int             `json:"subtotal_kurus"`
	DeliveryFeeKurus  int             `json:"delivery_fee_kurus"`
	TotalKurus        int             `json:"total_kurus"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
}

// DeliveryAddress is the copy of the address the order goes to.
type DeliveryAddress struct {
	Label       string  `json:"label"`
	AddressLine string  `json:"address_line"`
	Details     string  `json:"details"`
	Lat         float64 `json:"lat"`
	Lng         float64 `json:"lng"`
}

// Item is one dish on the order. UnitPriceKurus includes the options' extras.
type Item struct {
	ID             uuid.UUID    `json:"id"`
	MenuItemID     uuid.UUID    `json:"menu_item_id"`
	Name           string       `json:"name"`
	Quantity       int          `json:"quantity"`
	UnitPriceKurus int          `json:"unit_price_kurus"`
	LineTotalKurus int          `json:"line_total_kurus"`
	Options        []ItemOption `json:"options"`
}

// ItemOption is one picked choice on a dish.
type ItemOption struct {
	ChoiceID        uuid.UUID `json:"choice_id"`
	GroupName       string    `json:"group_name"`
	ChoiceName      string    `json:"choice_name"`
	ExtraPriceKurus int       `json:"extra_price_kurus"`
}

var (
	ErrCartEmpty        = errors.New("cart is empty")
	ErrCartHasProblems  = errors.New("some items in the cart can't be ordered as they are")
	ErrCartChanged      = errors.New("cart changed during checkout")
	ErrRestaurantClosed = errors.New("restaurant not accepting orders")
)

// BelowMinimumError says the basket is smaller than the restaurant delivers.
type BelowMinimumError struct {
	MinBasketKurus, KurusToMinimum int
}

func (e *BelowMinimumError) Error() string { return "basket below the restaurant's minimum" }

// Place turns the customer's cart into an order sent to one of their addresses.
//
// It runs in two halves:
//
//  1. Outside any transaction: price the cart (the menu may come from Redis,
//     and nothing should wait on that while holding a lock), read the address,
//     and refuse anything that can't be ordered.
//  2. In one transaction: lock the cart, check it still holds exactly what was
//     priced, copy everything into the order, write the first history row and
//     empty the cart. Either all of that happens or none of it does.
//
// Two checkouts at once queue at the cart lock. The second one then finds no
// cart (the first emptied it) and gets ErrCartEmpty, so one cart can never
// become two orders.
func (s *Store) Place(ctx context.Context, customerProfileID, addressID uuid.UUID) (Order, error) {
	cart, err := s.carts.Get(ctx, customerProfileID)
	if err != nil {
		return Order{}, err
	}
	if err := checkCart(cart); err != nil {
		return Order{}, err
	}
	addr, err := s.addresses.Get(ctx, addressID, customerProfileID)
	if err != nil {
		return Order{}, err // addresses.ErrNotFound for someone else's
	}

	o := Order{
		ID:                uuid.Must(uuid.NewV7()),
		CustomerProfileID: customerProfileID,
		RestaurantID:      cart.Restaurant.ID,
		RestaurantName:    cart.Restaurant.Name,
		Status:            Placed,
		Address: DeliveryAddress{
			Label: addr.Label, AddressLine: addr.AddressLine, Details: addr.Details, Lat: addr.Lat, Lng: addr.Lng,
		},
		Items:            make([]Item, 0, len(cart.Items)),
		SubtotalKurus:    cart.SubtotalKurus,
		DeliveryFeeKurus: cart.DeliveryFeeKurus,
		TotalKurus:       cart.TotalKurus,
	}
	for _, l := range cart.Items {
		it := Item{
			ID: uuid.Must(uuid.NewV7()), MenuItemID: l.MenuItemID, Name: l.Name, Quantity: l.Quantity,
			UnitPriceKurus: *l.UnitPriceKurus, LineTotalKurus: *l.LineTotalKurus,
			Options: make([]ItemOption, 0, len(l.Options)),
		}
		for _, p := range l.Options {
			it.Options = append(it.Options, ItemOption{
				ChoiceID: p.ChoiceID, GroupName: p.GroupName, ChoiceName: p.ChoiceName, ExtraPriceKurus: p.ExtraPriceKurus,
			})
		}
		o.Items = append(o.Items, it)
	}

	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		cartID, err := lockCart(ctx, tx, customerProfileID)
		if err != nil {
			return err
		}
		if err := sameAsPriced(ctx, tx, cartID, cart); err != nil {
			return err
		}
		if err := insertOrder(ctx, tx, &o); err != nil {
			return err
		}
		placed := Change{OrderID: o.ID, To: Placed, At: o.CreatedAt}
		if err := writeHistory(ctx, tx, placed, Actor{Role: users.RoleCustomer, ProfileID: customerProfileID}); err != nil {
			return err
		}
		// Its lines go with it (ON DELETE CASCADE).
		if _, err := tx.Exec(ctx, `DELETE FROM carts WHERE id = $1`, cartID); err != nil {
			return fmt.Errorf("empty cart: %w", err)
		}
		return nil
	})
	if err != nil {
		return Order{}, err
	}
	return o, nil
}

// checkCart refuses a cart that can't become an order. The cart only warns
// about these while the customer shops; checkout is where they're enforced.
func checkCart(c carts.Cart) error {
	if c.Restaurant == nil || len(c.Items) == 0 {
		return ErrCartEmpty
	}
	for _, l := range c.Items {
		if l.Problem != nil {
			return ErrCartHasProblems
		}
	}
	if !c.Restaurant.AcceptingOrders {
		return ErrRestaurantClosed
	}
	if c.BelowMinimum {
		return &BelowMinimumError{MinBasketKurus: c.MinBasketKurus, KurusToMinimum: c.KurusToMinimum}
	}
	return nil
}

// lockCart locks the customer's cart row until the transaction ends. Every cart
// change (add, change, remove, clear) needs the same lock, so while checkout
// holds it the cart can't change. No row means another checkout got there
// first.
func lockCart(ctx context.Context, tx pgx.Tx, customerProfileID uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM carts WHERE customer_profile_id = $1 FOR UPDATE`,
		customerProfileID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrCartEmpty
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("lock cart: %w", err)
	}
	return id, nil
}

// sameAsPriced checks the locked cart still has exactly the lines that were
// priced: the same lines, quantities and picks. Something may have changed it
// between pricing and locking (another tab, another phone); then the customer
// should look at the cart again rather than be charged for what they didn't see.
func sameAsPriced(ctx context.Context, tx pgx.Tx, cartID uuid.UUID, priced carts.Cart) error {
	type stored struct {
		quantity int
		choices  []uuid.UUID
	}
	rows, err := tx.Query(ctx, `SELECT id, quantity, choice_ids FROM cart_items WHERE cart_id = $1`, cartID)
	if err != nil {
		return fmt.Errorf("read cart lines: %w", err)
	}
	now := map[uuid.UUID]stored{}
	for rows.Next() {
		var id uuid.UUID
		var st stored
		if err := rows.Scan(&id, &st.quantity, &st.choices); err != nil {
			rows.Close()
			return fmt.Errorf("read cart lines: %w", err)
		}
		now[id] = st
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read cart lines: %w", err)
	}

	if len(now) != len(priced.Items) {
		return ErrCartChanged
	}
	for _, l := range priced.Items {
		st, ok := now[l.ID]
		if !ok || st.quantity != l.Quantity {
			return ErrCartChanged
		}
		picked := make([]uuid.UUID, 0, len(l.Options))
		for _, p := range l.Options {
			picked = append(picked, p.ChoiceID)
		}
		if !sameIDs(picked, st.choices) {
			return ErrCartChanged
		}
	}
	return nil
}

// sameIDs says whether a and b hold the same IDs, in any order.
func sameIDs(a, b []uuid.UUID) bool {
	byBytes := func(x, y uuid.UUID) int { return bytes.Compare(x[:], y[:]) }
	a, b = slices.Clone(a), slices.Clone(b)
	slices.SortFunc(a, byBytes)
	slices.SortFunc(b, byBytes)
	return slices.Equal(a, b)
}

// insertOrder writes the order, its items and their options, and fills in the
// times the database set.
func insertOrder(ctx context.Context, tx pgx.Tx, o *Order) error {
	err := tx.QueryRow(ctx, `
		INSERT INTO orders (id, customer_profile_id, restaurant_id, restaurant_name, status,
			address_label, address_line, address_details, address_location,
			subtotal_kurus, delivery_fee_kurus, total_kurus)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8,
			ST_SetSRID(ST_MakePoint($9::float8, $10::float8), 4326)::geography,
			$11, $12, $13)
		RETURNING created_at, updated_at`,
		o.ID, o.CustomerProfileID, o.RestaurantID, o.RestaurantName, o.Status,
		o.Address.Label, o.Address.AddressLine, o.Address.Details, o.Address.Lng, o.Address.Lat,
		o.SubtotalKurus, o.DeliveryFeeKurus, o.TotalKurus,
	).Scan(&o.CreatedAt, &o.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert order: %w", err)
	}
	for i, it := range o.Items {
		_, err := tx.Exec(ctx, `
			INSERT INTO order_items (id, order_id, menu_item_id, name, quantity, unit_price_kurus, line_total_kurus, position)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			it.ID, o.ID, it.MenuItemID, it.Name, it.Quantity, it.UnitPriceKurus, it.LineTotalKurus, i)
		if err != nil {
			return fmt.Errorf("insert order item: %w", err)
		}
		for j, op := range it.Options {
			_, err := tx.Exec(ctx, `
				INSERT INTO order_item_options (id, order_item_id, choice_id, group_name, choice_name, extra_price_kurus, position)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				uuid.Must(uuid.NewV7()), it.ID, op.ChoiceID, op.GroupName, op.ChoiceName, op.ExtraPriceKurus, j)
			if err != nil {
				return fmt.Errorf("insert order item option: %w", err)
			}
		}
	}
	return nil
}

package orders

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ListLimit is how many orders a list returns: the newest ones.
const ListLimit = 50

// orderColumns is the SELECT list matching scanOrder. It ends with the
// restaurant's owner, which the reader needs for the "who can see it" check.
const orderColumns = `o.id, o.customer_profile_id, o.restaurant_id, o.restaurant_name, o.status, o.cancel_reason,
	o.address_label, o.address_line, o.address_details,
	ST_Y(o.address_location::geometry), ST_X(o.address_location::geometry),
	o.subtotal_kurus, o.delivery_fee_kurus, o.total_kurus, o.created_at, o.updated_at,
	r.owner_profile_id`

func scanOrder(row pgx.Row) (Order, uuid.UUID, error) {
	var o Order
	var owner uuid.UUID
	err := row.Scan(
		&o.ID, &o.CustomerProfileID, &o.RestaurantID, &o.RestaurantName, &o.Status, &o.CancelReason,
		&o.Address.Label, &o.Address.AddressLine, &o.Address.Details, &o.Address.Lat, &o.Address.Lng,
		&o.SubtotalKurus, &o.DeliveryFeeKurus, &o.TotalKurus, &o.CreatedAt, &o.UpdatedAt,
		&owner)
	return o, owner, err
}

// Get reads one order with its items and options. Like Transition, an order the
// actor can't see is ErrNotFound.
func (s *Store) Get(ctx context.Context, id uuid.UUID, by Actor) (Order, error) {
	return get(ctx, s.pool, id, by)
}

// GetIn is Get inside tx, as the system: it sees moves made in tx that aren't
// committed yet (checkout's answer is built this way).
func GetIn(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Order, error) {
	return get(ctx, tx, id, System)
}

func get(ctx context.Context, q querier, id uuid.UUID, by Actor) (Order, error) {
	o, owner, err := scanOrder(q.QueryRow(ctx, `
		SELECT `+orderColumns+`
		FROM orders o JOIN restaurants r ON r.id = o.restaurant_id
		WHERE o.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ErrNotFound
	}
	if err != nil {
		return Order{}, fmt.Errorf("read order: %w", err)
	}
	if !canSee(by, o.CustomerProfileID, owner) {
		return Order{}, ErrNotFound
	}
	byOrder, err := itemsOf(ctx, q, []uuid.UUID{o.ID})
	if err != nil {
		return Order{}, err
	}
	o.Items = byOrder[o.ID]
	return o, nil
}

// ListForCustomer returns the customer's newest orders, newest first, with their
// items.
func (s *Store) ListForCustomer(ctx context.Context, customerProfileID uuid.UUID) ([]Order, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+orderColumns+`
		FROM orders o JOIN restaurants r ON r.id = o.restaurant_id
		WHERE o.customer_profile_id = $1
		ORDER BY o.created_at DESC, o.id DESC
		LIMIT $2`, customerProfileID, ListLimit)
	if err != nil {
		return nil, fmt.Errorf("list orders: %w", err)
	}
	return s.collect(ctx, rows)
}

// KitchenStatuses are the orders a restaurant works on: paid (waiting for a yes
// or no), accepted (cooking) and ready (waiting for pickup). Unpaid orders
// aren't shown: the restaurant shouldn't start on food nobody has paid for.
var KitchenStatuses = []Status{Paid, Accepted, Ready}

// ErrRestaurantNotFound: no such restaurant, or not the asker's.
var ErrRestaurantNotFound = errors.New("restaurant not found")

// ListForKitchen returns a restaurant's orders in KitchenStatuses, oldest first:
// the order they came in is the order to cook them. Only the restaurant's owner
// may ask.
func (s *Store) ListForKitchen(ctx context.Context, restaurantID, ownerProfileID uuid.UUID) ([]Order, error) {
	var mine bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM restaurants WHERE id = $1 AND owner_profile_id = $2)`,
		restaurantID, ownerProfileID).Scan(&mine)
	if err != nil {
		return nil, fmt.Errorf("check restaurant: %w", err)
	}
	if !mine {
		return nil, ErrRestaurantNotFound
	}
	// Sent as plain text: the driver doesn't know the order_status type, so the
	// database turns the text into it (::order_status[]).
	kitchen := make([]string, len(KitchenStatuses))
	for i, st := range KitchenStatuses {
		kitchen[i] = string(st)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+orderColumns+`
		FROM orders o JOIN restaurants r ON r.id = o.restaurant_id
		WHERE o.restaurant_id = $1 AND o.status = ANY($2::text[]::order_status[])
		ORDER BY o.created_at, o.id
		LIMIT $3`, restaurantID, kitchen, ListLimit)
	if err != nil {
		return nil, fmt.Errorf("list kitchen orders: %w", err)
	}
	return s.collect(ctx, rows)
}

// collect reads a page of orders, then all their items in one more query (not
// one query per order).
func (s *Store) collect(ctx context.Context, rows pgx.Rows) ([]Order, error) {
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Order, error) {
		o, _, err := scanOrder(row)
		return o, err
	})
	if err != nil {
		return nil, fmt.Errorf("list orders: %w", err)
	}
	ids := make([]uuid.UUID, len(list))
	for i, o := range list {
		ids[i] = o.ID
	}
	byOrder, err := itemsOf(ctx, s.pool, ids)
	if err != nil {
		return nil, err
	}
	for i := range list {
		list[i].Items = byOrder[list[i].ID]
	}
	return list, nil
}

// itemsOf reads the dishes and their options of several orders in one query.
// Each row is a dish joined with one of its options (or with none, for a dish
// without options); rows of the same dish come one after another.
func itemsOf(ctx context.Context, q querier, orderIDs []uuid.UUID) (map[uuid.UUID][]Item, error) {
	out := make(map[uuid.UUID][]Item, len(orderIDs))
	for _, id := range orderIDs {
		out[id] = []Item{}
	}
	if len(orderIDs) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `
		SELECT i.order_id, i.id, i.menu_item_id, i.name, i.quantity, i.unit_price_kurus, i.line_total_kurus,
			op.choice_id, op.group_name, op.choice_name, op.extra_price_kurus
		FROM order_items i
		LEFT JOIN order_item_options op ON op.order_item_id = i.id
		WHERE i.order_id = ANY($1)
		ORDER BY i.order_id, i.position, op.position`, orderIDs)
	if err != nil {
		return nil, fmt.Errorf("read order items: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var orderID uuid.UUID
		var it Item
		var choiceID *uuid.UUID
		var group, choice *string
		var extra *int
		if err := rows.Scan(&orderID, &it.ID, &it.MenuItemID, &it.Name, &it.Quantity, &it.UnitPriceKurus,
			&it.LineTotalKurus, &choiceID, &group, &choice, &extra); err != nil {
			return nil, fmt.Errorf("read order items: %w", err)
		}
		items := out[orderID]
		if len(items) == 0 || items[len(items)-1].ID != it.ID {
			it.Options = []ItemOption{}
			items = append(items, it)
		}
		if choiceID != nil {
			last := &items[len(items)-1]
			last.Options = append(last.Options, ItemOption{
				ChoiceID: *choiceID, GroupName: *group, ChoiceName: *choice, ExtraPriceKurus: *extra,
			})
		}
		out[orderID] = items
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read order items: %w", err)
	}
	return out, nil
}

// HistoryEntry is one status change, as the order's people see it: which role
// made it, not which profile.
type HistoryEntry struct {
	From         *Status       `json:"from"`
	To           Status        `json:"to"`
	CancelReason *CancelReason `json:"cancel_reason"`
	By           string        `json:"by"` // customer, restaurant, driver or system
	At           time.Time     `json:"at"`
}

// History returns an order's status changes, oldest first. It doesn't check who
// is asking: call it after Get, which does.
func (s *Store) History(ctx context.Context, orderID uuid.UUID) ([]HistoryEntry, error) {
	return history(ctx, s.pool, orderID)
}

// HistoryIn is History read inside tx, where an order placed in tx (and not yet
// committed) can already be seen.
func HistoryIn(ctx context.Context, tx pgx.Tx, orderID uuid.UUID) ([]HistoryEntry, error) {
	return history(ctx, tx, orderID)
}

// querier is what the pool and a transaction have in common, for reads that
// run in either.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func history(ctx context.Context, q querier, orderID uuid.UUID) ([]HistoryEntry, error) {
	rows, err := q.Query(ctx, `
		SELECT from_status, to_status, cancel_reason, by_role, created_at
		FROM order_status_history WHERE order_id = $1
		ORDER BY created_at, id`, orderID)
	if err != nil {
		return nil, fmt.Errorf("read history: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (HistoryEntry, error) {
		var h HistoryEntry
		err := row.Scan(&h.From, &h.To, &h.CancelReason, &h.By, &h.At)
		return h, err
	})
}

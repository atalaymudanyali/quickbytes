package orders

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Get reads one order with its items and options. Like Transition, an order the
// actor can't see is ErrNotFound.
func (s *Store) Get(ctx context.Context, id uuid.UUID, by Actor) (Order, error) {
	var o Order
	var owner uuid.UUID
	err := s.pool.QueryRow(ctx, `
		SELECT o.id, o.customer_profile_id, o.restaurant_id, o.restaurant_name, o.status, o.cancel_reason,
			o.address_label, o.address_line, o.address_details,
			ST_Y(o.address_location::geometry), ST_X(o.address_location::geometry),
			o.subtotal_kurus, o.delivery_fee_kurus, o.total_kurus, o.created_at, o.updated_at,
			r.owner_profile_id
		FROM orders o JOIN restaurants r ON r.id = o.restaurant_id
		WHERE o.id = $1`, id).Scan(
		&o.ID, &o.CustomerProfileID, &o.RestaurantID, &o.RestaurantName, &o.Status, &o.CancelReason,
		&o.Address.Label, &o.Address.AddressLine, &o.Address.Details, &o.Address.Lat, &o.Address.Lng,
		&o.SubtotalKurus, &o.DeliveryFeeKurus, &o.TotalKurus, &o.CreatedAt, &o.UpdatedAt,
		&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ErrNotFound
	}
	if err != nil {
		return Order{}, fmt.Errorf("read order: %w", err)
	}
	if !canSee(by, o.CustomerProfileID, owner) {
		return Order{}, ErrNotFound
	}
	if o.Items, err = s.items(ctx, o.ID); err != nil {
		return Order{}, err
	}
	return o, nil
}

// items reads an order's dishes and their options, in the order they were in
// the cart. One query: each row is a dish joined with one of its options (or
// with none, for a dish without options).
func (s *Store) items(ctx context.Context, orderID uuid.UUID) ([]Item, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT i.id, i.menu_item_id, i.name, i.quantity, i.unit_price_kurus, i.line_total_kurus,
			op.choice_id, op.group_name, op.choice_name, op.extra_price_kurus
		FROM order_items i
		LEFT JOIN order_item_options op ON op.order_item_id = i.id
		WHERE i.order_id = $1
		ORDER BY i.position, op.position`, orderID)
	if err != nil {
		return nil, fmt.Errorf("read order items: %w", err)
	}
	defer rows.Close()

	items := []Item{}
	for rows.Next() {
		var it Item
		var choiceID *uuid.UUID
		var group, choice *string
		var extra *int
		if err := rows.Scan(&it.ID, &it.MenuItemID, &it.Name, &it.Quantity, &it.UnitPriceKurus, &it.LineTotalKurus,
			&choiceID, &group, &choice, &extra); err != nil {
			return nil, fmt.Errorf("read order items: %w", err)
		}
		// Rows of the same dish come one after another (ORDER BY position).
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
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read order items: %w", err)
	}
	return items, nil
}

package orders

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atalaymudanyali/quickbytes/internal/outbox"
	"github.com/atalaymudanyali/quickbytes/internal/users"
)

// EventsTopic is the Redpanda topic order events go to. Every event of one
// order has the order ID as its key, so they stay in order.
const EventsTopic = "orders"

// EventVersion is the version of Event's shape. A change that would break a
// consumer (a field renamed or removed) gets a new version; adding a field
// doesn't.
const EventVersion = 1

// Event is what's published for every move: "order.placed", "order.paid",
// "order.accepted", "order.ready", "order.cancelled" (phase 3 adds
// "order.picked_up" and "order.delivered"). It carries the IDs and the change;
// a consumer that needs more (the items, the address) reads the order.
//
// Delivery is at least once: the same event can arrive twice, so consumers
// drop repeats by ID.
type Event struct {
	ID         uuid.UUID `json:"id"`
	Type       string    `json:"type"`
	Version    int       `json:"version"`
	OccurredAt time.Time `json:"occurred_at"`
	OrderID    uuid.UUID `json:"order_id"`
	Data       EventData `json:"data"`
}

// EventData is the move itself, plus the order's facts that never change.
type EventData struct {
	From              *Status       `json:"from"` // null when the order was just placed
	To                Status        `json:"to"`
	CancelReason      *CancelReason `json:"cancel_reason"` // only for cancelled
	ByRole            users.Role    `json:"by_role"`
	RestaurantID      uuid.UUID     `json:"restaurant_id"`
	CustomerProfileID uuid.UUID     `json:"customer_profile_id"`
	TotalKurus        int           `json:"total_kurus"`
}

// EventType is the event type of a move to status: "order.<status>".
func EventType(to Status) string {
	return "order." + string(to)
}

// facts are what an event says about the order besides the move.
type facts struct {
	restaurantID      uuid.UUID
	customerProfileID uuid.UUID
	totalKurus        int
}

// record writes everything a move leaves behind, inside the move's
// transaction: the history row and the event. Every move (checkout included)
// goes through here, so none can happen without its event, and a move that's
// rolled back takes its event with it.
func record(ctx context.Context, tx pgx.Tx, c Change, by Actor, f facts) error {
	if err := writeHistory(ctx, tx, c, by); err != nil {
		return err
	}
	return writeEvent(ctx, tx, c, by, f)
}

func writeEvent(ctx context.Context, tx pgx.Tx, c Change, by Actor, f facts) error {
	e := Event{
		ID:         uuid.Must(uuid.NewV7()),
		Type:       EventType(c.To),
		Version:    EventVersion,
		OccurredAt: c.At,
		OrderID:    c.OrderID,
		Data: EventData{
			From:              nullIfEmpty(c.From),
			To:                c.To,
			CancelReason:      nullIfEmpty(c.Reason),
			ByRole:            by.Role,
			RestaurantID:      f.restaurantID,
			CustomerProfileID: f.customerProfileID,
			TotalKurus:        f.totalKurus,
		},
	}
	payload, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encode %s event: %w", e.Type, err)
	}
	return outbox.AddIn(ctx, tx, outbox.Event{
		ID: e.ID, Topic: EventsTopic, AggregateID: c.OrderID, Type: e.Type, Payload: payload,
	})
}

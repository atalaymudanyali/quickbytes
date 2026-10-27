// Package outbox keeps events that must be published to the event stream
// (Redpanda), using the transactional outbox pattern.
//
// Publishing straight to Redpanda after a database change can't be made safe:
// a crash between the two loses the event (or, done the other way round,
// announces a change that never committed). So the event is written into the
// outbox table in the same transaction as the change instead. Either both are
// saved or neither is. A relay publishes the waiting rows afterwards.
//
// The package knows nothing about orders: the caller builds the event's JSON.
package outbox

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Event is one event to publish.
type Event struct {
	// ID is the event's own ID. Consumers drop repeats by it, so it should
	// also be inside Payload.
	ID uuid.UUID
	// Topic is the Redpanda topic it goes to, like "orders".
	Topic string
	// AggregateID is the thing the event is about (an order). It's the
	// message key, which keeps one thing's events in order.
	AggregateID uuid.UUID
	// Type is what happened, like "order.paid".
	Type string
	// Payload is the message body, as JSON.
	Payload []byte
}

// AddIn saves an event inside tx, the transaction making the change the event
// describes. If tx is rolled back, the event is gone too.
func AddIn(ctx context.Context, tx pgx.Tx, e Event) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO outbox (id, topic, aggregate_id, type, payload)
		VALUES ($1, $2, $3, $4, $5)`,
		e.ID, e.Topic, e.AggregateID, e.Type, e.Payload)
	if err != nil {
		return fmt.Errorf("add %s event to outbox: %w", e.Type, err)
	}
	return nil
}

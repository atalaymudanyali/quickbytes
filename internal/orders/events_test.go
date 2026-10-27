package orders

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atalaymudanyali/quickbytes/internal/users"
)

// outboxRow is one outbox row with its payload decoded.
type outboxRow struct {
	id, aggregate uuid.UUID
	topic, typ    string
	published     bool
	event         Event
}

// events reads an order's outbox rows, oldest first.
func (f fixture) events(t *testing.T, orderID uuid.UUID) []outboxRow {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `
		SELECT id, aggregate_id, topic, type, published_at IS NOT NULL, payload
		FROM outbox WHERE aggregate_id = $1 ORDER BY seq`, orderID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		var payload []byte
		if err := rows.Scan(&r.id, &r.aggregate, &r.topic, &r.typ, &r.published, &payload); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(payload, &r.event); err != nil {
			t.Fatalf("payload %s: %v", payload, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// checkRow checks what every event row must hold: the right topic, key and
// type, the row's ID inside the payload, not published yet.
func checkRow(t *testing.T, r outboxRow, orderID uuid.UUID, to Status) {
	t.Helper()
	e := r.event
	if r.topic != EventsTopic || r.aggregate != orderID || r.typ != EventType(to) || r.published {
		t.Fatalf("row topic %q, key %s, type %q, published %v; want %q, %s, %q, false",
			r.topic, r.aggregate, r.typ, r.published, EventsTopic, orderID, EventType(to))
	}
	if e.ID != r.id || e.Type != r.typ || e.Version != EventVersion || e.OrderID != orderID || e.Data.To != to {
		t.Fatalf("payload %+v doesn't match its row (id %s, type %s)", e, r.id, r.typ)
	}
	if e.OccurredAt.IsZero() {
		t.Fatal("occurred_at missing")
	}
}

func TestCheckoutWritesOrderPlaced(t *testing.T) {
	f := setup(t)
	s := f.newShop(t)
	me := f.newShopper(t)
	f.fullCart(t, s, me)

	o, err := f.orders.Place(context.Background(), me.ProfileID, me.home)
	if err != nil {
		t.Fatal(err)
	}
	ev := f.events(t, o.ID)
	if len(ev) != 1 {
		t.Fatalf("%d events, want 1", len(ev))
	}
	checkRow(t, ev[0], o.ID, Placed)
	d := ev[0].event.Data
	if d.From != nil || d.CancelReason != nil || d.ByRole != users.RoleCustomer ||
		d.RestaurantID != s.id || d.CustomerProfileID != me.ProfileID || d.TotalKurus != o.TotalKurus {
		t.Fatalf("data %+v; want from null, by customer, restaurant %s, customer %s, total %d",
			d, s.id, me.ProfileID, o.TotalKurus)
	}
	if !ev[0].event.OccurredAt.Equal(o.CreatedAt) {
		t.Fatalf("occurred_at %v, want the order's created_at %v", ev[0].event.OccurredAt, o.CreatedAt)
	}
}

// The same grid as TestEveryMoveThroughTheDatabase: every allowed move writes
// exactly one event describing it, and a refused move writes none.
func TestEveryMoveWritesItsEvent(t *testing.T) {
	f := setup(t)
	p := f.cast(t)
	ctx := context.Background()
	for _, from := range Statuses {
		for _, a := range Actions {
			if pressedBy[a] == users.RoleDriver {
				continue
			}
			t.Run(string(from)+"/"+string(a), func(t *testing.T) {
				id := f.newOrder(t, p, from)
				by := p.actorFor(a)
				c, err := f.orders.Transition(ctx, id, a, by)
				want, ok := allowed[from][a]
				if !ok {
					if n := len(f.events(t, id)); n != 0 {
						t.Fatalf("a refused move (%v) wrote %d events", err, n)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				ev := f.events(t, id)
				if len(ev) != 1 {
					t.Fatalf("%d events, want 1", len(ev))
				}
				checkRow(t, ev[0], id, want)
				e := ev[0].event
				if e.Data.From == nil || *e.Data.From != from || e.Data.ByRole != by.Role || !e.OccurredAt.Equal(c.At) {
					t.Fatalf("event %+v; want from %s, by %s, at %v", e, from, by.Role, c.At)
				}
				if (e.Data.CancelReason == nil) != (wantReason[a] == "") ||
					(e.Data.CancelReason != nil && *e.Data.CancelReason != wantReason[a]) {
					t.Fatalf("cancel reason %v; want %q", e.Data.CancelReason, wantReason[a])
				}
				// newOrder's restaurant and total.
				if e.Data.RestaurantID != p.restaurant || e.Data.CustomerProfileID != p.customer.ProfileID ||
					e.Data.TotalKurus != 47990 {
					t.Fatalf("data %+v; want restaurant %s, customer %s, total 47990",
						e.Data, p.restaurant, p.customer.ProfileID)
				}
			})
		}
	}
}

// The payment flow moves orders with TransitionIn and expiry with its own
// loop: both go through the same transition, so both write events.
func TestPaymentAndExpiryMovesWriteEvents(t *testing.T) {
	f := setup(t)
	p := f.cast(t)
	ctx := context.Background()

	paid := f.newOrder(t, p, Placed)
	err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		_, err := TransitionIn(ctx, tx, paid, Pay, System)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	ev := f.events(t, paid)
	if len(ev) != 1 {
		t.Fatalf("%d events after paying, want 1", len(ev))
	}
	checkRow(t, ev[0], paid, Paid)
	if ev[0].event.Data.ByRole != RoleSystem {
		t.Fatalf("by %s, want system", ev[0].event.Data.ByRole)
	}

	old := f.newOrder(t, p, Placed)
	f.age(t, old, 2*testPayWindow)
	f.expireAll(t)
	ev = f.events(t, old)
	if len(ev) != 1 {
		t.Fatalf("%d events after expiry, want 1", len(ev))
	}
	checkRow(t, ev[0], old, Cancelled)
	if r := ev[0].event.Data.CancelReason; r == nil || *r != Expired {
		t.Fatalf("cancel reason %v, want expired", r)
	}
}

// One order's events come out of the outbox in the order of its moves.
func TestEventsFollowTheMoves(t *testing.T) {
	f := setup(t)
	p := f.cast(t)
	ctx := context.Background()

	id := f.newOrder(t, p, Placed)
	for _, a := range []Action{Pay, Accept, MarkReady} {
		if _, err := f.orders.Transition(ctx, id, a, p.actorFor(a)); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, r := range f.events(t, id) {
		got = append(got, r.typ)
	}
	want := []string{"order.paid", "order.accepted", "order.ready"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("events %v, want %v", got, want)
	}
}

// The heart of the pattern: an event exists if and only if its move
// committed. A move undone by a failing hook, or a checkout undone by a
// failing finish, leaves no event behind.
func TestNoEventWithoutItsMove(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	t.Run("a failing move hook", func(t *testing.T) {
		p := f.cast(t)
		id := f.newOrder(t, p, Paid)
		f.orders.OnMove(func(_ context.Context, _ pgx.Tx, c Change) error {
			if c.OrderID == id { // the shared test database has other tests' orders
				return errors.New("hook failed")
			}
			return nil
		})
		if _, err := f.orders.Transition(ctx, id, Accept, p.owner); err == nil {
			t.Fatal("want the hook's error")
		}
		if n := len(f.events(t, id)); n != 0 {
			t.Fatalf("%d events for an undone move, want 0", n)
		}
	})

	t.Run("a failing checkout", func(t *testing.T) {
		s := f.newShop(t)
		me := f.newShopper(t)
		f.fullCart(t, s, me)
		boom := errors.New("could not store the answer")
		_, err := f.orders.PlaceAnd(ctx, me.ProfileID, me.home, func(context.Context, pgx.Tx, Order) error {
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("got %v, want finish's own error", err)
		}
		// The order's ID never got out, so look for any event about this customer.
		var n int
		if err := f.pool.QueryRow(ctx, `
			SELECT count(*) FROM outbox WHERE payload->'data'->>'customer_profile_id' = $1`,
			me.ProfileID.String()).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%d events for an undone checkout, want 0", n)
		}
	})
}

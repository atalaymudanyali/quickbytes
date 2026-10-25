package orders

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atalaymudanyali/quickbytes/internal/restaurants"
	"github.com/atalaymudanyali/quickbytes/internal/testutil"
	"github.com/atalaymudanyali/quickbytes/internal/users"
)

type fixture struct {
	pool        *pgxpool.Pool
	orders      *Store
	users       *users.Store
	restaurants *restaurants.Store
}

func setup(t *testing.T) fixture {
	t.Helper()
	pool := testutil.DB(t)
	return fixture{pool: pool, orders: NewStore(pool), users: users.NewStore(pool), restaurants: restaurants.NewStore(pool)}
}

// people is one order's cast: its customer, the owner of its restaurant, and a
// stranger in each role.
type people struct {
	customer, owner                   Actor
	otherCustomer, otherOwner, driver Actor
	restaurant                        uuid.UUID
}

func (f fixture) profile(t *testing.T, role users.Role) Actor {
	t.Helper()
	ctx := context.Background()
	u, p, err := f.users.Create(ctx, testutil.UniqueEmail(), "hash", "Test")
	if err != nil {
		t.Fatal(err)
	}
	if role != users.RoleCustomer {
		if p, err = f.users.AddProfile(ctx, u.ID, role); err != nil {
			t.Fatal(err)
		}
	}
	return Actor{Role: role, ProfileID: p.ID}
}

func (f fixture) restaurant(t *testing.T, owner Actor) uuid.UUID {
	t.Helper()
	r, err := f.restaurants.Create(context.Background(), owner.ProfileID, restaurants.New{
		Name: "Hasan Usta", Address: "Moda Cd. 12", Lat: 40.9875, Lng: 29.0262, DeliveryRadiusM: 3000,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r.ID
}

func (f fixture) cast(t *testing.T) people {
	t.Helper()
	p := people{
		customer:      f.profile(t, users.RoleCustomer),
		owner:         f.profile(t, users.RoleRestaurant),
		otherCustomer: f.profile(t, users.RoleCustomer),
		otherOwner:    f.profile(t, users.RoleRestaurant),
		driver:        f.profile(t, users.RoleDriver),
	}
	p.restaurant = f.restaurant(t, p.owner)
	f.restaurant(t, p.otherOwner) // so otherOwner really owns a restaurant
	return p
}

// newOrder writes an order straight into the database in any status, so tests
// can start from anywhere in the state machine. (Checkout comes in its own
// commit.)
func (f fixture) newOrder(t *testing.T, p people, status Status) uuid.UUID {
	t.Helper()
	var reason *CancelReason
	if status == Cancelled {
		r := ByCustomer
		reason = &r
	}
	id := uuid.Must(uuid.NewV7())
	_, err := f.pool.Exec(context.Background(), `
		INSERT INTO orders (id, customer_profile_id, restaurant_id, restaurant_name, status, cancel_reason,
			address_label, address_line, address_details, address_location,
			subtotal_kurus, delivery_fee_kurus, total_kurus)
		VALUES ($1, $2, $3, 'Hasan Usta', $4, $5, 'Ev', 'Moda Cd. 1', '',
			ST_SetSRID(ST_MakePoint(29.03, 40.98), 4326)::geography, 45000, 2990, 47990)`,
		id, p.customer.ProfileID, p.restaurant, status, reason)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (f fixture) status(t *testing.T, id uuid.UUID) (Status, *CancelReason) {
	t.Helper()
	var s Status
	var r *CancelReason
	if err := f.pool.QueryRow(context.Background(),
		`SELECT status, cancel_reason FROM orders WHERE id = $1`, id).Scan(&s, &r); err != nil {
		t.Fatal(err)
	}
	return s, r
}

type historyRow struct {
	From, To Status
	Reason   *CancelReason
	ByRole   string
	By       *uuid.UUID
}

func (f fixture) history(t *testing.T, id uuid.UUID) []historyRow {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `
		SELECT coalesce(from_status::text, ''), to_status, cancel_reason, by_role, by_profile_id
		FROM order_status_history WHERE order_id = $1 ORDER BY created_at, id`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []historyRow
	for rows.Next() {
		var h historyRow
		if err := rows.Scan(&h.From, &h.To, &h.Reason, &h.ByRole, &h.By); err != nil {
			t.Fatal(err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// actorFor is who presses each action in the database tests.
func (p people) actorFor(a Action) Actor {
	switch pressedBy[a] {
	case users.RoleCustomer:
		return p.customer
	case users.RoleRestaurant:
		return p.owner
	case users.RoleDriver:
		return p.driver
	default:
		return System
	}
}

// The same grid as TestEveryStatusAndAction, through the database: allowed moves
// change the row and leave one history row; refused moves change nothing.
// Driver actions are left out: drivers can't see orders until phase 3.
func TestEveryMoveThroughTheDatabase(t *testing.T) {
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
				c, err := f.orders.Transition(ctx, id, a, p.actorFor(a))
				want, ok := allowed[from][a]
				if !ok {
					if !errors.Is(err, ErrInvalidTransition) {
						t.Fatalf("got %v; want ErrInvalidTransition", err)
					}
					if s, _ := f.status(t, id); s != from || len(f.history(t, id)) != 0 {
						t.Fatalf("a refused move changed the order: %s, %d history rows", s, len(f.history(t, id)))
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				s, reason := f.status(t, id)
				if s != want || c.From != from || c.To != want {
					t.Fatalf("status %s (change %s → %s); want %s → %s", s, c.From, c.To, from, want)
				}
				if (reason == nil) != (wantReason[a] == "") || (reason != nil && *reason != wantReason[a]) {
					t.Fatalf("cancel reason %v; want %q", reason, wantReason[a])
				}
				h := f.history(t, id)
				if len(h) != 1 || h[0].From != from || h[0].To != want {
					t.Fatalf("history %+v; want one row %s → %s", h, from, want)
				}
			})
		}
	}
}

func TestHistoryRecordsWhoMovedTheOrder(t *testing.T) {
	f := setup(t)
	p := f.cast(t)
	ctx := context.Background()
	id := f.newOrder(t, p, Placed)

	for _, a := range []Action{Pay, Accept, MarkReady} {
		if _, err := f.orders.Transition(ctx, id, a, p.actorFor(a)); err != nil {
			t.Fatalf("%s: %v", a, err)
		}
	}
	h := f.history(t, id)
	want := []historyRow{
		{From: Placed, To: Paid, ByRole: "system"},
		{From: Paid, To: Accepted, ByRole: "restaurant", By: &p.owner.ProfileID},
		{From: Accepted, To: Ready, ByRole: "restaurant", By: &p.owner.ProfileID},
	}
	if len(h) != len(want) {
		t.Fatalf("got %d history rows, want %d", len(h), len(want))
	}
	for i := range want {
		g, w := h[i], want[i]
		if g.From != w.From || g.To != w.To || g.ByRole != w.ByRole ||
			(g.By == nil) != (w.By == nil) || (g.By != nil && *g.By != *w.By) {
			t.Errorf("row %d: got %+v, want %+v", i, g, w)
		}
	}
}

// A customer can't touch another customer's order, a restaurant can't touch
// another restaurant's, and drivers have none yet. To them the order doesn't
// exist (ErrNotFound), and it stays as it was.
func TestStrangersCantTouchAnOrder(t *testing.T) {
	f := setup(t)
	p := f.cast(t)
	ctx := context.Background()

	cases := []struct {
		name   string
		status Status
		action Action
		by     Actor
	}{
		{"other customer cancels", Placed, Cancel, p.otherCustomer},
		{"other restaurant accepts", Paid, Accept, p.otherOwner},
		{"other restaurant rejects", Paid, Reject, p.otherOwner},
		{"driver picks up", Ready, PickUp, p.driver},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := f.newOrder(t, p, tc.status)
			if _, err := f.orders.Transition(ctx, id, tc.action, tc.by); !errors.Is(err, ErrNotFound) {
				t.Fatalf("got %v, want ErrNotFound", err)
			}
			if s, _ := f.status(t, id); s != tc.status {
				t.Fatalf("status changed to %s", s)
			}
		})
	}

	t.Run("no such order", func(t *testing.T) {
		if _, err := f.orders.Transition(ctx, uuid.Must(uuid.NewV7()), Expire, System); !errors.Is(err, ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})
	t.Run("the right person in the wrong role", func(t *testing.T) {
		// The order's own customer pressing the restaurant's button.
		id := f.newOrder(t, p, Paid)
		if _, err := f.orders.Transition(ctx, id, Accept, Actor{Role: users.RoleRestaurant, ProfileID: p.customer.ProfileID}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
		if _, err := f.orders.Transition(ctx, id, Accept, p.customer); !errors.Is(err, ErrNotAllowed) {
			t.Fatalf("got %v, want ErrNotAllowed", err)
		}
	})
}

// Two moves on one placed order at the same moment: the payment arriving just as
// the expiry loop gives up on it. Both only start from placed, so only one may
// happen.
//
// Racing goroutines rarely overlap for real (each move takes a millisecond), so
// the test sets the overlap up on purpose: move A runs inside a transaction that
// stays open, move B starts while A is still open, then A commits. With the row
// lock, B waits for A and then sees "expired". Without it, B would also read
// "placed" and both moves would win.
func TestSimultaneousMovesOnlyOneWins(t *testing.T) {
	f := setup(t)
	p := f.cast(t)
	ctx := context.Background()
	id := f.newOrder(t, p, Placed)

	txA, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer txA.Rollback(ctx)
	if _, err := transition(ctx, txA, id, Expire, System); err != nil {
		t.Fatal(err)
	}

	resultB := make(chan error, 1)
	go func() {
		_, err := f.orders.Transition(ctx, id, Pay, System)
		resultB <- err
	}()
	select {
	case err := <-resultB:
		t.Fatalf("move B finished while move A was still open (%v); it should have waited", err)
	case <-time.After(200 * time.Millisecond):
	}

	if err := txA.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-resultB; !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("move B: got %v, want ErrInvalidTransition", err)
	}
	s, reason := f.status(t, id)
	if h := f.history(t, id); s != Cancelled || reason == nil || *reason != Expired || len(h) != 1 {
		t.Fatalf("status %s (%v), %d history rows; want cancelled (expired), 1 row", s, reason, len(h))
	}
}

package payments

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atalaymudanyali/quickbytes/internal/restaurants"
	"github.com/atalaymudanyali/quickbytes/internal/testutil"
	"github.com/atalaymudanyali/quickbytes/internal/users"
)

type fixture struct {
	pool     *pgxpool.Pool
	payments *Store
}

func setup(t *testing.T) fixture {
	t.Helper()
	pool := testutil.DB(t)
	return fixture{pool: pool, payments: NewStore(pool)}
}

// newOrder writes a placed order straight into the database, with a customer
// and a restaurant of its own. Checkout itself is tested in internal/orders.
func (f fixture) newOrder(t *testing.T) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	us := users.NewStore(f.pool)
	_, customer, err := us.Create(ctx, testutil.UniqueEmail(), "hash", "Customer")
	if err != nil {
		t.Fatal(err)
	}
	u, _, err := us.Create(ctx, testutil.UniqueEmail(), "hash", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := us.AddProfile(ctx, u.ID, users.RoleRestaurant)
	if err != nil {
		t.Fatal(err)
	}
	r, err := restaurants.NewStore(f.pool).Create(ctx, owner.ID, restaurants.New{
		Name: "Hasan Usta", Address: "Moda Cd. 12", Lat: 40.9875, Lng: 29.0262, DeliveryRadiusM: 3000,
	})
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.Must(uuid.NewV7())
	_, err = f.pool.Exec(ctx, `
		INSERT INTO orders (id, customer_profile_id, restaurant_id, restaurant_name,
			address_label, address_line, address_details, address_location,
			subtotal_kurus, delivery_fee_kurus, total_kurus)
		VALUES ($1, $2, $3, 'Hasan Usta', 'Ev', 'Moda Cd. 1', '',
			ST_SetSRID(ST_MakePoint(29.03, 40.98), 4326)::geography, 45000, 2990, 47990)`,
		id, customer.ID, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// inTx runs fn in a transaction that commits.
func (f fixture) inTx(t *testing.T, fn func(tx pgx.Tx) error) {
	t.Helper()
	if err := pgx.BeginFunc(context.Background(), f.pool, fn); err != nil {
		t.Fatal(err)
	}
}

func (f fixture) create(t *testing.T, orderID uuid.UUID) Payment {
	t.Helper()
	var p Payment
	f.inTx(t, func(tx pgx.Tx) error {
		var err error
		p, err = CreateIn(context.Background(), tx, orderID, 47990, "4242", time.Minute)
		return err
	})
	return p
}

func (f fixture) update(t *testing.T, id uuid.UUID, c Change, via Via) Payment {
	t.Helper()
	var p Payment
	f.inTx(t, func(tx pgx.Tx) error {
		var err error
		p, err = updateIn(context.Background(), tx, id, c, via)
		return err
	})
	return p
}

func statuses(events []Event) []Status {
	out := make([]Status, len(events))
	for i, e := range events {
		out[i] = e.Status
	}
	return out
}

func ptr[T any](v T) *T { return &v }

// A new payment starts pending, with one ledger line, and isn't due for the
// reconcile loop until checkout's own call has had its chance.
func TestCreateStartsPending(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	order := f.newOrder(t)
	p := f.create(t, order)

	if p.Status != Pending || p.AmountKurus != 47990 || p.CardLast4 != "4242" || p.ProviderPaymentID != nil {
		t.Fatalf("got %+v", p)
	}
	got, err := f.payments.ForOrder(ctx, order)
	if err != nil || got.ID != p.ID {
		t.Fatalf("ForOrder: %+v, %v", got, err)
	}
	var wait time.Duration
	if err := f.pool.QueryRow(ctx,
		`SELECT next_check_at - now() FROM payments WHERE id = $1`, p.ID).Scan(&wait); err != nil {
		t.Fatal(err)
	}
	if wait < 50*time.Second {
		t.Fatalf("next check in %v, want about a minute", wait)
	}
	events, err := f.payments.Events(ctx, p.ID)
	if err != nil || len(events) != 1 || events[0].Status != Pending || events[0].Via != ViaRequest {
		t.Fatalf("events: %+v, %v", events, err)
	}
}

// Each status change adds one ledger line; hearing the same status again adds
// none, and the provider ID, once known, stays known.
func TestUpdateRecordsEachChangeOnce(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p := f.create(t, f.newOrder(t))
	providerID := "pay_" + uuid.NewString() // unique: the test database outlives each run

	f.update(t, p.ID, Change{Status: Unknown}, ViaRequest)
	f.update(t, p.ID, Change{Status: Authorized, ProviderPaymentID: ptr(providerID)}, ViaReconcile)
	again := f.update(t, p.ID, Change{Status: Authorized}, ViaReconcile)
	if again.ProviderPaymentID == nil || *again.ProviderPaymentID != providerID {
		t.Fatalf("provider ID lost: %+v", again)
	}

	events, err := f.payments.Events(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := statuses(events), []Status{Pending, Unknown, Authorized}; !equal(got, want) {
		t.Fatalf("ledger %v, want %v", got, want)
	}
	if events[2].Via != ViaReconcile {
		t.Fatalf("authorized learned via %q, want reconcile", events[2].Via)
	}
}

// A failed payment carries its reason in the payment and in its ledger line.
func TestFailedKeepsItsReason(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p := f.create(t, f.newOrder(t))

	got := f.update(t, p.ID, Change{Status: Failed, ProviderPaymentID: ptr("pay_" + uuid.NewString()), DeclineCode: ptr(CardDeclined)}, ViaRequest)
	if got.DeclineCode == nil || *got.DeclineCode != CardDeclined {
		t.Fatalf("got %+v", got)
	}
	events, err := f.payments.Events(ctx, p.ID)
	if err != nil || len(events) != 2 || events[1].DeclineCode == nil || *events[1].DeclineCode != CardDeclined {
		t.Fatalf("events: %+v, %v", events, err)
	}
}

// The database refuses what the code must never do: a second payment for one
// order, a failed payment without a reason, an authorized one without the
// provider's ID.
func TestDatabaseRefusesBadPayments(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	order := f.newOrder(t)
	p := f.create(t, order)

	cases := map[string]func(tx pgx.Tx) error{
		"payments_order_id_key": func(tx pgx.Tx) error {
			_, err := CreateIn(ctx, tx, order, 100, "4242", 0)
			return err
		},
		"payments_failed_has_reason": func(tx pgx.Tx) error {
			_, err := updateIn(ctx, tx, p.ID, Change{Status: Failed}, ViaRequest)
			return err
		},
		"payments_known_has_provider_id": func(tx pgx.Tx) error {
			_, err := updateIn(ctx, tx, p.ID, Change{Status: Authorized}, ViaRequest)
			return err
		},
	}
	for constraint, fn := range cases {
		t.Run(constraint, func(t *testing.T) {
			err := pgx.BeginFunc(ctx, f.pool, fn)
			if err == nil || !strings.Contains(err.Error(), constraint) {
				t.Fatalf("got %v, want a %s violation", err, constraint)
			}
		})
	}
}

// The ledger is append-only: lines can't be changed or deleted, only added.
// Deleting the whole order (the demo reset) still takes them with it.
func TestLedgerIsAppendOnly(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	order := f.newOrder(t)
	p := f.create(t, order)

	for _, sql := range []string{
		`UPDATE payment_events SET status = 'captured' WHERE payment_id = $1`,
		`DELETE FROM payment_events WHERE payment_id = $1`,
	} {
		_, err := f.pool.Exec(ctx, sql, p.ID)
		if err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("%s: got %v, want refused", sql, err)
		}
	}

	if _, err := f.pool.Exec(ctx, `DELETE FROM orders WHERE id = $1`, order); err != nil {
		t.Fatalf("delete order: %v", err)
	}
	var left int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM payment_events WHERE payment_id = $1`, p.ID).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("%d events left after deleting the order", left)
	}
}

func equal(a, b []Status) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

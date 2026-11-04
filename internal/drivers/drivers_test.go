package drivers_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atalaymudanyali/quickbytes/internal/drivers"
	"github.com/atalaymudanyali/quickbytes/internal/testutil"
	"github.com/atalaymudanyali/quickbytes/internal/users"
)

// newProfile signs up a user and returns their new driver profile's ID.
func newProfile(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	u, ctx := users.NewStore(pool), context.Background()
	user, _, err := u.Create(ctx, testutil.UniqueEmail(), "hash", "Driver")
	if err != nil {
		t.Fatal(err)
	}
	p, err := u.AddProfile(ctx, user.ID, users.RoleDriver)
	if err != nil {
		t.Fatal(err)
	}
	return p.ID
}

// newDriver also saves the profile's driver details.
func newDriver(t *testing.T, pool *pgxpool.Pool, store *drivers.Store) uuid.UUID {
	t.Helper()
	id := newProfile(t, pool)
	if _, err := store.Save(context.Background(), id, drivers.VehicleMotorcycle); err != nil {
		t.Fatal(err)
	}
	return id
}

// setStatus stands in for dispatch, which sets on_delivery from #38 on.
func setStatus(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, status drivers.Status) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE drivers SET status = $2 WHERE profile_id = $1`, id, status); err != nil {
		t.Fatal(err)
	}
}

func TestSave(t *testing.T) {
	pool := testutil.DB(t)
	store, ctx := drivers.NewStore(pool), context.Background()
	id := newProfile(t, pool)

	if _, err := store.Get(ctx, id); !errors.Is(err, drivers.ErrNotFound) {
		t.Fatalf("get before saving: err = %v, want ErrNotFound", err)
	}
	d, err := store.Save(ctx, id, drivers.VehicleBicycle)
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != id || d.Vehicle != drivers.VehicleBicycle || d.Status != drivers.StatusOffline {
		t.Fatalf("got %+v, want a new offline driver on a bicycle", d)
	}
	if _, err := store.StartShift(ctx, id); err != nil {
		t.Fatal(err)
	}
	// Saving again edits the row. It must not end the shift.
	d, err = store.Save(ctx, id, drivers.VehicleCar)
	if err != nil {
		t.Fatal(err)
	}
	if d.Vehicle != drivers.VehicleCar || d.Status != drivers.StatusAvailable {
		t.Fatalf("got %+v, want a car and the shift still on", d)
	}
	if got, err := store.Get(ctx, id); err != nil || got.Vehicle != drivers.VehicleCar {
		t.Fatalf("get = %+v, %v; want the car", got, err)
	}
}

func TestDatabaseRefusesUnknownValues(t *testing.T) {
	pool := testutil.DB(t)
	store, ctx := drivers.NewStore(pool), context.Background()
	if _, err := store.Save(ctx, newProfile(t, pool), "skateboard"); err == nil {
		t.Error("saved an unknown vehicle")
	}
	id := newDriver(t, pool, store)
	if _, err := pool.Exec(ctx, `UPDATE drivers SET status = 'napping' WHERE profile_id = $1`, id); err == nil {
		t.Error("saved an unknown status")
	}
}

func TestShiftStatusChanges(t *testing.T) {
	pool := testutil.DB(t)
	store := drivers.NewStore(pool)
	start, end := store.StartShift, store.EndShift

	tests := []struct {
		name    string
		from    drivers.Status
		action  func(context.Context, uuid.UUID) (drivers.Driver, error)
		want    drivers.Status
		wantErr error
	}{
		{"start turns the light on", drivers.StatusOffline, start, drivers.StatusAvailable, nil},
		{"starting twice changes nothing", drivers.StatusAvailable, start, drivers.StatusAvailable, nil},
		{"starting on a delivery doesn't look free", drivers.StatusOnDelivery, start, drivers.StatusOnDelivery, nil},
		{"end turns the light off", drivers.StatusAvailable, end, drivers.StatusOffline, nil},
		{"ending twice changes nothing", drivers.StatusOffline, end, drivers.StatusOffline, nil},
		{"can't end on a delivery", drivers.StatusOnDelivery, end, drivers.StatusOnDelivery, drivers.ErrOnDelivery},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			id := newDriver(t, pool, store)
			setStatus(t, pool, id, tt.from)

			if _, err := tt.action(ctx, id); !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			// Read it back: what counts is what's saved, not what was answered.
			got, err := store.Get(ctx, id)
			if err != nil || got.Status != tt.want {
				t.Fatalf("status = %q, %v; want %q", got.Status, err, tt.want)
			}
		})
	}
}

func TestShiftsNeedDriverDetails(t *testing.T) {
	pool := testutil.DB(t)
	store, ctx := drivers.NewStore(pool), context.Background()
	id := newProfile(t, pool)

	if _, err := store.StartShift(ctx, id); !errors.Is(err, drivers.ErrNotFound) {
		t.Errorf("start: err = %v, want ErrNotFound", err)
	}
	if _, err := store.EndShift(ctx, id); !errors.Is(err, drivers.ErrNotFound) {
		t.Errorf("end: err = %v, want ErrNotFound", err)
	}
}

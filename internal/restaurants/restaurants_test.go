package restaurants_test

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/google/uuid"

	"github.com/atalaymudanyali/quickbytes/internal/restaurants"
	"github.com/atalaymudanyali/quickbytes/internal/testutil"
	"github.com/atalaymudanyali/quickbytes/internal/users"
)

// Real places in Istanbul, used across the tests.
var (
	kadikoyIskele = [2]float64{40.9917, 29.0233} // Kadıköy ferry pier
	modaParki     = [2]float64{40.9810, 29.0270} // Moda park, ~1.2 km south
	taksim        = [2]float64{41.0370, 28.9850} // Taksim square, across the Bosphorus
)

type fixture struct {
	store *restaurants.Store
	users *users.Store
}

func setup(t *testing.T) fixture {
	t.Helper()
	pool := testutil.DB(t)
	return fixture{store: restaurants.NewStore(pool), users: users.NewStore(pool)}
}

// newOwner creates a user with a restaurant profile and returns that profile's ID.
func (f fixture) newOwner(t *testing.T) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	u, _, err := f.users.Create(ctx, testutil.UniqueEmail(), "hash", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.users.AddProfile(ctx, u.ID, users.RoleRestaurant)
	if err != nil {
		t.Fatal(err)
	}
	return p.ID
}

func (f fixture) newRestaurant(t *testing.T, owner uuid.UUID, at [2]float64, radius int) restaurants.Restaurant {
	t.Helper()
	r, err := f.store.Create(context.Background(), owner, restaurants.New{
		Name: "Çiya Sofrası", Address: "Caferağa, Güneşli Bahçe Sk. No:43, Kadıköy",
		Lat: at[0], Lng: at[1], DeliveryRadiusM: radius,
	})
	if err != nil {
		t.Fatalf("create restaurant: %v", err)
	}
	return r
}

func TestCreateAndGetKeepsCoordinates(t *testing.T) {
	f := setup(t)
	created := f.newRestaurant(t, f.newOwner(t), kadikoyIskele, 3000)

	got, err := f.store.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// Latitude and longitude must come back exactly where they went in, and not
	// swapped: PostGIS points are (longitude, latitude), the opposite of how
	// people usually write them.
	if math.Abs(got.Lat-kadikoyIskele[0]) > 1e-9 || math.Abs(got.Lng-kadikoyIskele[1]) > 1e-9 {
		t.Fatalf("location = (%v, %v), want %v", got.Lat, got.Lng, kadikoyIskele)
	}
	if !got.AcceptingOrders {
		t.Fatal("new restaurants should accept orders by default")
	}
}

func TestUpdateOnlyByOwner(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	owner, stranger := f.newOwner(t), f.newOwner(t)
	r := f.newRestaurant(t, owner, kadikoyIskele, 3000)

	closed := false
	if _, err := f.store.Update(ctx, r.ID, stranger, restaurants.Patch{AcceptingOrders: &closed}); !errors.Is(err, restaurants.ErrNotFound) {
		t.Fatalf("stranger update err = %v, want ErrNotFound", err)
	}

	name := "Çiya Kebap"
	got, err := f.store.Update(ctx, r.ID, owner, restaurants.Patch{
		Name: &name, Lat: &taksim[0], Lng: &taksim[1], AcceptingOrders: &closed,
	})
	if err != nil {
		t.Fatalf("owner update: %v", err)
	}
	if got.Name != name || got.AcceptingOrders || got.Lat != taksim[0] || got.Lng != taksim[1] {
		t.Fatalf("after update = %+v", got)
	}
	if got.Address != r.Address || got.DeliveryRadiusM != r.DeliveryRadiusM {
		t.Fatal("fields left out of the patch were changed")
	}
}

func TestListByOwner(t *testing.T) {
	f := setup(t)
	owner := f.newOwner(t)
	f.newRestaurant(t, owner, kadikoyIskele, 3000)
	f.newRestaurant(t, owner, modaParki, 2000)
	f.newRestaurant(t, f.newOwner(t), taksim, 3000) // someone else's

	list, err := f.store.ListByOwner(context.Background(), owner)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %d restaurants, err = %v; want the owner's 2", len(list), err)
	}
}

func TestInIstanbul(t *testing.T) {
	cases := map[string]struct {
		lat, lng float64
		want     bool
	}{
		"Kadıköy":             {kadikoyIskele[0], kadikoyIskele[1], true},
		"lat/lng swapped":     {kadikoyIskele[1], kadikoyIskele[0], false},
		"0,0 (missing value)": {0, 0, false},
		"Ankara":              {39.93, 32.85, false},
	}
	for name, c := range cases {
		if got := restaurants.InIstanbul(c.lat, c.lng); got != c.want {
			t.Errorf("%s: InIstanbul = %v, want %v", name, got, c.want)
		}
	}
}

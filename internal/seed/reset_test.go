package seed_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/atalaymudanyali/quickbytes/internal/auth"
	"github.com/atalaymudanyali/quickbytes/internal/menus"
	"github.com/atalaymudanyali/quickbytes/internal/restaurants"
	"github.com/atalaymudanyali/quickbytes/internal/seed"
	"github.com/atalaymudanyali/quickbytes/internal/testutil"
	"github.com/atalaymudanyali/quickbytes/internal/users"
)

func TestReset(t *testing.T) {
	pool := testutil.DB(t)
	rdb := testutil.RedisIfConfigured(t) // nil without Redis: the cache part is then skipped
	ctx := context.Background()
	data, err := seed.Istanbul()
	if err != nil {
		t.Fatal(err)
	}

	userStore, restaurantStore, menuStore := users.NewStore(pool), restaurants.NewStore(pool), menus.NewStore(pool)
	cache := menus.NewCache(menuStore, rdb, time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s := &seed.Seeder{Users: userStore, Restaurants: restaurantStore, Menus: menuStore,
		Hasher: auth.NewHasher(auth.FastParams), Password: password}

	before, err := s.Run(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	demoRestaurant := before.Restaurants[0].ID

	// Someone who isn't a demo account, with a restaurant of their own.
	outsider, _, err := userStore.Create(ctx, testutil.UniqueEmail(), "not-a-real-hash", "Not Demo")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := userStore.AddProfile(ctx, outsider.ID, users.RoleRestaurant)
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := restaurantStore.Create(ctx, owner.ID, restaurants.New{
		Name: "Not Demo Kebap", Address: "Moda Cd. 1", Lat: 40.98, Lng: 29.03, DeliveryRadiusM: 3000,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Put a demo menu in the cache, to check that reset drops it.
	if _, _, err := cache.PublicMenu(ctx, demoRestaurant); err != nil {
		t.Fatal(err)
	}

	deleted, err := seed.Reset(ctx, pool, cache)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != len(data.Accounts) {
		t.Errorf("deleted %d accounts, want the %d demo ones", deleted, len(data.Accounts))
	}

	t.Run("demo data is gone", func(t *testing.T) {
		for _, a := range data.Accounts {
			if _, err := userStore.GetByEmail(ctx, a.Email); !errors.Is(err, users.ErrNotFound) {
				t.Errorf("%s: err = %v, want ErrNotFound", a.Email, err)
			}
		}
		if _, err := restaurantStore.Get(ctx, demoRestaurant); !errors.Is(err, restaurants.ErrNotFound) {
			t.Errorf("demo restaurant: err = %v, want ErrNotFound", err)
		}
		// With a leftover cached copy this would be a HIT for a deleted restaurant.
		if _, res, err := cache.PublicMenu(ctx, demoRestaurant); !errors.Is(err, menus.ErrRestaurantNotFound) {
			t.Errorf("cached demo menu: result %s, err = %v, want ErrRestaurantNotFound", res, err)
		}
	})

	t.Run("everyone else is untouched", func(t *testing.T) {
		if _, err := userStore.GetByEmail(ctx, outsider.Email); err != nil {
			t.Errorf("outsider: %v", err)
		}
		if _, err := restaurantStore.Get(ctx, theirs.ID); err != nil {
			t.Errorf("outsider's restaurant: %v", err)
		}
	})

	t.Run("seeding again recreates everything", func(t *testing.T) {
		after, err := s.Run(ctx, data)
		if err != nil {
			t.Fatal(err)
		}
		if after.Existing != (seed.Counts{}) {
			t.Errorf("found %+v already there, want nothing", after.Existing)
		}
		if after.Created.Accounts != len(data.Accounts) || after.Created.Restaurants != len(data.Restaurants) {
			t.Errorf("created %+v, want every account and restaurant", after.Created)
		}
	})
}

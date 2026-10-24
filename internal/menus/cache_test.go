package menus

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/atalaymudanyali/quickbytes/internal/cache"
	"github.com/atalaymudanyali/quickbytes/internal/restaurants"
	"github.com/atalaymudanyali/quickbytes/internal/testutil"
	"github.com/atalaymudanyali/quickbytes/internal/users"
)

// These tests are inside the package (not menus_test), so the race test can call
// storeIfCurrent directly and replay the race step by step.

type cacheFixture struct {
	store        *Store
	cache        *Cache
	redis        *redis.Client
	restaurantID uuid.UUID
	owner        uuid.UUID
	category     uuid.UUID
}

func newCacheFixture(t *testing.T) cacheFixture {
	t.Helper()
	ctx := context.Background()
	pool := testutil.DB(t)
	rdb := testutil.Redis(t)
	restaurantID, owner := newTestRestaurant(t, pool)

	store := NewStore(pool)
	c := NewCache(store, rdb, time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)))
	cat, err := store.CreateCategory(ctx, restaurantID, owner, "Çorbalar")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateItem(ctx, restaurantID, owner, NewItem{CategoryID: cat.ID, Name: "Mercimek", PriceKurus: 9000}); err != nil {
		t.Fatal(err)
	}
	return cacheFixture{store: store, cache: c, redis: rdb, restaurantID: restaurantID, owner: owner, category: cat.ID}
}

func newTestRestaurant(t *testing.T, pool *pgxpool.Pool) (restaurantID, owner uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	u, _, err := users.NewStore(pool).Create(ctx, testutil.UniqueEmail(), "hash", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	p, err := users.NewStore(pool).AddProfile(ctx, u.ID, users.RoleRestaurant)
	if err != nil {
		t.Fatal(err)
	}
	r, err := restaurants.NewStore(pool).Create(ctx, p.ID, restaurants.New{
		Name: "Hamdi", Address: "Eminönü", Lat: 41.0170, Lng: 28.9720, DeliveryRadiusM: 3000,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r.ID, p.ID
}

func (f cacheFixture) read(t *testing.T) (Menu, Result) {
	t.Helper()
	m, res, err := f.cache.PublicMenu(context.Background(), f.restaurantID)
	if err != nil {
		t.Fatalf("read menu: %v", err)
	}
	return m, res
}

func TestMissThenHit(t *testing.T) {
	f := newCacheFixture(t)

	first, res := f.read(t)
	if res != Miss {
		t.Fatalf("first read = %s, want MISS", res)
	}
	second, res := f.read(t)
	if res != Hit {
		t.Fatalf("second read = %s, want HIT", res)
	}
	if second.Version != first.Version || second.Categories[0].Items[0].Name != "Mercimek" {
		t.Fatalf("cached copy differs: %+v vs %+v", second, first)
	}

	ttl := f.redis.TTL(context.Background(), menuKey(f.restaurantID)).Val()
	if ttl <= 0 || ttl > time.Minute {
		t.Errorf("cached copy expires in %v, want within the 1 minute TTL", ttl)
	}
}

func TestChangeDropsTheCopy(t *testing.T) {
	ctx := context.Background()
	f := newCacheFixture(t)
	f.read(t) // MISS: stores a copy
	f.read(t) // HIT

	if _, err := f.store.CreateItem(ctx, f.restaurantID, f.owner, NewItem{CategoryID: f.category, Name: "Ezogelin", PriceKurus: 9000}); err != nil {
		t.Fatal(err)
	}

	m, res := f.read(t)
	if res != Miss {
		t.Fatalf("read after a change = %s, want MISS (the old copy should be gone)", res)
	}
	if len(m.Categories[0].Items) != 2 {
		t.Fatalf("read after a change shows %d items, want the new one too", len(m.Categories[0].Items))
	}
}

// Options are part of the cached menu, so changing them must drop the copy too.
func TestOptionChangeDropsTheCopy(t *testing.T) {
	ctx := context.Background()
	f := newCacheFixture(t)
	m, _ := f.read(t) // MISS: stores a copy
	f.read(t)         // HIT

	itemID := m.Categories[0].Items[0].ID
	if _, err := f.store.CreateOptionGroup(ctx, f.restaurantID, f.owner, itemID, NewOptionGroup{Name: "Boy", MinPicks: 1, MaxPicks: 1}); err != nil {
		t.Fatal(err)
	}

	m, res := f.read(t)
	if res != Miss {
		t.Fatalf("read after an option change = %s, want MISS", res)
	}
	if got := m.Categories[0].Items[0].OptionGroups; len(got) != 1 || got[0].Name != "Boy" {
		t.Fatalf("read after an option change shows groups %+v, want the new one", got)
	}
}

// Replays the race step by step: a reader reads the old menu, a change lands and
// drops the cached copy, and only then does the reader try to save its old copy.
func TestSlowReaderCannotCacheAnOldMenu(t *testing.T) {
	ctx := context.Background()
	f := newCacheFixture(t)

	// 1. A reader gets the menu from the database (version N)...
	old, err := f.store.Menu(ctx, f.restaurantID, View{})
	if err != nil {
		t.Fatal(err)
	}
	// 2. ...meanwhile the owner changes it (version N+1), which drops the copy...
	if _, err := f.store.CreateItem(ctx, f.restaurantID, f.owner, NewItem{CategoryID: f.category, Name: "Ezogelin", PriceKurus: 9000}); err != nil {
		t.Fatal(err)
	}
	// 3. ...and only now does the slow reader try to save its old copy.
	f.cache.storeIfCurrent(ctx, old)

	m, res := f.read(t)
	if res != Miss || len(m.Categories[0].Items) != 2 {
		t.Fatalf("got %s with %d items; the old copy must have been refused", res, len(m.Categories[0].Items))
	}
}

func TestWorksWithoutRedis(t *testing.T) {
	ctx := context.Background()
	pool := testutil.DB(t)
	restaurantID, _ := newTestRestaurant(t, pool)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("no Redis configured", func(t *testing.T) {
		c := NewCache(NewStore(pool), nil, time.Minute, quiet)
		if _, res, err := c.PublicMenu(ctx, restaurantID); err != nil || res != Bypass {
			t.Fatalf("got %s, %v; want BYPASS and no error", res, err)
		}
	})

	t.Run("Redis down", func(t *testing.T) {
		down, err := cache.Open("redis://127.0.0.1:1/0") // nothing listens on port 1
		if err != nil {
			t.Fatal(err)
		}
		defer down.Close()
		c := NewCache(NewStore(pool), down, time.Minute, quiet)

		start := time.Now()
		_, res, err := c.PublicMenu(ctx, restaurantID)
		if err != nil || res != Bypass {
			t.Fatalf("got %s, %v; want BYPASS and no error", res, err)
		}
		if took := time.Since(start); took > time.Second {
			t.Errorf("took %v; a broken cache must not slow the read down", took)
		}
	})
}

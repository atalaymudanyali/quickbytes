package carts_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atalaymudanyali/quickbytes/internal/carts"
	"github.com/atalaymudanyali/quickbytes/internal/menus"
	"github.com/atalaymudanyali/quickbytes/internal/restaurants"
	"github.com/atalaymudanyali/quickbytes/internal/testutil"
	"github.com/atalaymudanyali/quickbytes/internal/users"
)

type fixture struct {
	carts       *carts.Store
	menus       *menus.Store
	users       *users.Store
	restaurants *restaurants.Store
}

func setup(t *testing.T) fixture {
	t.Helper()
	pool := testutil.DB(t)
	m := menus.NewStore(pool)
	r := restaurants.NewStore(pool)
	// No Redis: the cache then reads every menu straight from the database.
	cache := menus.NewCache(m, nil, time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return fixture{carts: carts.NewStore(pool, cache, r), menus: m, users: users.NewStore(pool), restaurants: r}
}

// kitchen is a restaurant with a two-dish menu: Adana (Boy: Normal or Büyük,
// pick exactly 1) and Ayran (no options).
type kitchen struct {
	id, owner            uuid.UUID
	adana, ayran         uuid.UUID
	normal, buyuk, group uuid.UUID
}

func (f fixture) newKitchen(t *testing.T) kitchen {
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
	r, err := f.restaurants.Create(ctx, p.ID, restaurants.New{
		Name: "Hasan Usta", Address: "Moda Cd. 12", Lat: 40.9875, Lng: 29.0262, DeliveryRadiusM: 3000,
		DeliveryFeeKurus: 2990, MinBasketKurus: 30000,
	})
	if err != nil {
		t.Fatal(err)
	}
	k := kitchen{id: r.ID, owner: p.ID}
	cat, err := f.menus.CreateCategory(ctx, k.id, k.owner, "Kebaplar")
	if err != nil {
		t.Fatal(err)
	}
	adana, err := f.menus.CreateItem(ctx, k.id, k.owner, menus.NewItem{CategoryID: cat.ID, Name: "Adana", PriceKurus: 45000})
	if err != nil {
		t.Fatal(err)
	}
	ayran, err := f.menus.CreateItem(ctx, k.id, k.owner, menus.NewItem{CategoryID: cat.ID, Name: "Ayran", PriceKurus: 4000})
	if err != nil {
		t.Fatal(err)
	}
	g, err := f.menus.CreateOptionGroup(ctx, k.id, k.owner, adana.ID, menus.NewOptionGroup{Name: "Boy", MinPicks: 1, MaxPicks: 1})
	if err != nil {
		t.Fatal(err)
	}
	normal, err := f.menus.CreateOptionChoice(ctx, k.id, k.owner, g.ID, menus.NewOptionChoice{Name: "Normal"})
	if err != nil {
		t.Fatal(err)
	}
	buyuk, err := f.menus.CreateOptionChoice(ctx, k.id, k.owner, g.ID, menus.NewOptionChoice{Name: "Büyük", ExtraPriceKurus: 8000})
	if err != nil {
		t.Fatal(err)
	}
	k.adana, k.ayran, k.group, k.normal, k.buyuk = adana.ID, ayran.ID, g.ID, normal.ID, buyuk.ID
	return k
}

func (f fixture) newCustomer(t *testing.T) uuid.UUID {
	t.Helper()
	_, p, err := f.users.Create(context.Background(), testutil.UniqueEmail(), "hash", "Customer")
	if err != nil {
		t.Fatal(err)
	}
	return p.ID
}

func (f fixture) add(t *testing.T, customer, item uuid.UUID, qty int, choices ...uuid.UUID) uuid.UUID {
	t.Helper()
	id, err := f.carts.Add(context.Background(), customer, item, qty, choices)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	return id
}

func (f fixture) get(t *testing.T, customer uuid.UUID) carts.Cart {
	t.Helper()
	c, err := f.carts.Get(context.Background(), customer)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAddMergesTheSameDishAndOptions(t *testing.T) {
	f := setup(t)
	k, me := f.newKitchen(t), f.newCustomer(t)

	first := f.add(t, me, k.adana, 1, k.buyuk)
	again := f.add(t, me, k.adana, 2, k.buyuk)
	other := f.add(t, me, k.adana, 1, k.normal)
	if again != first || other == first {
		t.Fatal("same picks should share a line, different picks should not")
	}

	c := f.get(t, me)
	if len(c.Items) != 2 || c.Items[0].Quantity != 3 {
		t.Fatalf("lines = %+v; want Adana Büyük ×3 and Adana Normal ×1", c.Items)
	}
	// 3 × 530 TL + 450 TL = 2040 TL, plus 29,90 TL.
	if c.SubtotalKurus != 204000 || c.TotalKurus != 206990 {
		t.Fatalf("subtotal %d, total %d", c.SubtotalKurus, c.TotalKurus)
	}
}

func TestAddChecks(t *testing.T) {
	f := setup(t)
	k, me := f.newKitchen(t), f.newCustomer(t)
	ctx := context.Background()

	var ce *menus.ChoiceError
	if _, err := f.carts.Add(ctx, me, k.adana, 1, nil); !errors.As(err, &ce) || ce.Code != menus.TooFewPicks {
		t.Errorf("no size: err = %v, want too_few_picks", err)
	}
	if _, err := f.carts.Add(ctx, me, uuid.New(), 1, nil); !errors.Is(err, carts.ErrItemNotFound) {
		t.Errorf("unknown dish: err = %v, want ErrItemNotFound", err)
	}
	f.add(t, me, k.ayran, 15)
	if _, err := f.carts.Add(ctx, me, k.ayran, 6, nil); !errors.Is(err, carts.ErrQuantityTooHigh) {
		t.Errorf("15 + 6 ayran: err = %v, want ErrQuantityTooHigh", err)
	}

	off := false
	if _, err := f.menus.UpdateItem(ctx, k.id, k.owner, k.ayran, menus.ItemPatch{Available: &off}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.carts.Add(ctx, me, k.ayran, 1, nil); !errors.Is(err, carts.ErrItemUnavailable) {
		t.Errorf("switched-off dish: err = %v, want ErrItemUnavailable", err)
	}

	if _, err := f.restaurants.Update(ctx, k.id, k.owner, restaurants.Patch{AcceptingOrders: &off}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.carts.Add(ctx, me, k.adana, 1, []uuid.UUID{k.normal}); !errors.Is(err, carts.ErrRestaurantClosed) {
		t.Errorf("paused restaurant: err = %v, want ErrRestaurantClosed", err)
	}
}

func TestOneRestaurantPerCart(t *testing.T) {
	f := setup(t)
	a, b, me := f.newKitchen(t), f.newKitchen(t), f.newCustomer(t)
	f.add(t, me, a.ayran, 1)

	_, err := f.carts.Add(context.Background(), me, b.ayran, 1, nil)
	var mismatch *carts.MismatchError
	if !errors.As(err, &mismatch) || mismatch.CartRestaurantID != a.id {
		t.Fatalf("err = %v, want a MismatchError naming the first restaurant", err)
	}

	// After clearing, the other restaurant is fine.
	if err := f.carts.Clear(context.Background(), me); err != nil {
		t.Fatal(err)
	}
	f.add(t, me, b.ayran, 1)
	if c := f.get(t, me); c.Restaurant.ID != b.id {
		t.Fatalf("cart is for %s, want the second restaurant", c.Restaurant.ID)
	}
}

func TestChange(t *testing.T) {
	f := setup(t)
	k, me := f.newKitchen(t), f.newCustomer(t)
	ctx := context.Background()
	big := f.add(t, me, k.adana, 2, k.buyuk)
	small := f.add(t, me, k.adana, 1, k.normal)

	five := 5
	if _, err := f.carts.Change(ctx, me, big, carts.LinePatch{Quantity: &five}); err != nil {
		t.Fatal(err)
	}
	// Switching the small one to Büyük makes it a copy of the big line: they merge.
	picks := []uuid.UUID{k.buyuk}
	merged, err := f.carts.Change(ctx, me, small, carts.LinePatch{ChoiceIDs: &picks})
	if err != nil || merged != big {
		t.Fatalf("merge: id %v, err %v; want the big line's id", merged, err)
	}
	if c := f.get(t, me); len(c.Items) != 1 || c.Items[0].Quantity != 6 {
		t.Fatalf("lines = %+v, want one Adana Büyük ×6", c.Items)
	}

	none := []uuid.UUID{}
	var ce *menus.ChoiceError
	if _, err := f.carts.Change(ctx, me, big, carts.LinePatch{ChoiceIDs: &none}); !errors.As(err, &ce) {
		t.Errorf("picks removed: err = %v, want a ChoiceError", err)
	}
	if _, err := f.carts.Change(ctx, f.newCustomer(t), big, carts.LinePatch{Quantity: &five}); !errors.Is(err, carts.ErrLineNotFound) {
		t.Errorf("someone else's line: err = %v, want ErrLineNotFound", err)
	}
}

func TestRemovingTheLastLineDeletesTheCart(t *testing.T) {
	f := setup(t)
	a, b, me := f.newKitchen(t), f.newKitchen(t), f.newCustomer(t)
	line := f.add(t, me, a.ayran, 1)

	if err := f.carts.Remove(context.Background(), f.newCustomer(t), line); !errors.Is(err, carts.ErrLineNotFound) {
		t.Fatalf("someone else's remove: err = %v, want ErrLineNotFound", err)
	}
	if err := f.carts.Remove(context.Background(), me, line); err != nil {
		t.Fatal(err)
	}
	if c := f.get(t, me); c.Restaurant != nil {
		t.Fatalf("cart still there: %+v", c)
	}
	f.add(t, me, b.ayran, 1) // free to shop elsewhere now
}

func TestMenuChangesShowUpAtOnce(t *testing.T) {
	f := setup(t)
	k, me := f.newKitchen(t), f.newCustomer(t)
	ctx := context.Background()
	f.add(t, me, k.ayran, 2)
	f.add(t, me, k.adana, 1, k.buyuk)

	newPrice := 5000
	if _, err := f.menus.UpdateItem(ctx, k.id, k.owner, k.ayran, menus.ItemPatch{PriceKurus: &newPrice}); err != nil {
		t.Fatal(err)
	}
	if err := f.menus.DeleteOptionChoice(ctx, k.id, k.owner, k.buyuk); err != nil {
		t.Fatal(err)
	}

	c := f.get(t, me)
	if *c.Items[0].UnitPriceKurus != 5000 {
		t.Errorf("ayran = %d, want the new 5000", *c.Items[0].UnitPriceKurus)
	}
	if p := c.Items[1].Problem; p == nil || p.Code != carts.OptionsChanged {
		t.Errorf("adana with a deleted choice: problem = %+v, want options_changed", p)
	}
	if c.SubtotalKurus != 10000 {
		t.Errorf("subtotal = %d, want only the ayran (2 × 50 TL)", c.SubtotalKurus)
	}
}

func TestConcurrentAddsRespectTheLineLimit(t *testing.T) {
	f := setup(t)
	k, me := f.newKitchen(t), f.newCustomer(t)
	ctx := context.Background()

	// Fill the cart to one below the limit with distinct dishes, then race two
	// new dishes for the last slot. Without the cart lock both could fit.
	cat, err := f.menus.CreateCategory(ctx, k.id, k.owner, "Tatlılar")
	if err != nil {
		t.Fatal(err)
	}
	var items []uuid.UUID
	for i := range carts.MaxLines + 1 {
		it, err := f.menus.CreateItem(ctx, k.id, k.owner, menus.NewItem{CategoryID: cat.ID, Name: "Tatlı " + string(rune('A'+i)), PriceKurus: 1000})
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, it.ID)
	}
	for _, id := range items[:carts.MaxLines-1] {
		f.add(t, me, id, 1)
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, id := range []uuid.UUID{items[carts.MaxLines-1], items[carts.MaxLines]} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = f.carts.Add(ctx, me, id, 1, nil)
		}()
	}
	wg.Wait()
	full := 0
	for _, err := range errs {
		if errors.Is(err, carts.ErrCartFull) {
			full++
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if full != 1 || len(f.get(t, me).Items) != carts.MaxLines {
		t.Fatalf("%d refused, %d lines; want 1 refused and exactly %d lines", full, len(f.get(t, me).Items), carts.MaxLines)
	}
}

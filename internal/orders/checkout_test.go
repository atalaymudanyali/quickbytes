package orders

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atalaymudanyali/quickbytes/internal/addresses"
	"github.com/atalaymudanyali/quickbytes/internal/menus"
	"github.com/atalaymudanyali/quickbytes/internal/restaurants"
	"github.com/atalaymudanyali/quickbytes/internal/users"
)

// shop is a restaurant with a two-dish menu: Adana, 450 TL, with Boy (Normal, or
// Büyük for +80 TL; pick exactly 1), and Ayran, 40 TL. Delivery 29.90 TL, minimum
// basket 300 TL.
type shop struct {
	id, owner     uuid.UUID
	adana, ayran  uuid.UUID
	normal, buyuk uuid.UUID
}

func (f fixture) newShop(t *testing.T) shop {
	t.Helper()
	ctx := context.Background()
	owner := f.profile(t, users.RoleRestaurant)
	r, err := f.restaurants.Create(ctx, owner.ProfileID, restaurants.New{
		Name: "Hasan Usta", Address: "Moda Cd. 12", Lat: 40.9875, Lng: 29.0262, DeliveryRadiusM: 3000,
		DeliveryFeeKurus: 2990, MinBasketKurus: 30000,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := shop{id: r.ID, owner: owner.ProfileID}
	cat, err := f.menus.CreateCategory(ctx, s.id, s.owner, "Kebaplar")
	if err != nil {
		t.Fatal(err)
	}
	adana, err := f.menus.CreateItem(ctx, s.id, s.owner, menus.NewItem{CategoryID: cat.ID, Name: "Adana", PriceKurus: 45000})
	if err != nil {
		t.Fatal(err)
	}
	ayran, err := f.menus.CreateItem(ctx, s.id, s.owner, menus.NewItem{CategoryID: cat.ID, Name: "Ayran", PriceKurus: 4000})
	if err != nil {
		t.Fatal(err)
	}
	g, err := f.menus.CreateOptionGroup(ctx, s.id, s.owner, adana.ID, menus.NewOptionGroup{Name: "Boy", MinPicks: 1, MaxPicks: 1})
	if err != nil {
		t.Fatal(err)
	}
	normal, err := f.menus.CreateOptionChoice(ctx, s.id, s.owner, g.ID, menus.NewOptionChoice{Name: "Normal"})
	if err != nil {
		t.Fatal(err)
	}
	buyuk, err := f.menus.CreateOptionChoice(ctx, s.id, s.owner, g.ID, menus.NewOptionChoice{Name: "Büyük", ExtraPriceKurus: 8000})
	if err != nil {
		t.Fatal(err)
	}
	s.adana, s.ayran, s.normal, s.buyuk = adana.ID, ayran.ID, normal.ID, buyuk.ID
	return s
}

// shopper is a customer with one saved address.
type shopper struct {
	Actor
	home uuid.UUID
}

func (f fixture) newShopper(t *testing.T) shopper {
	t.Helper()
	c := f.profile(t, users.RoleCustomer)
	a, err := f.addresses.Create(context.Background(), c.ProfileID, addresses.New{
		Label: "Ev", AddressLine: "Caferağa Mh., Moda Cd. No:40, Kadıköy", Details: "Kat 3, daire 7",
		Lat: 40.9840, Lng: 29.0270,
	})
	if err != nil {
		t.Fatal(err)
	}
	return shopper{Actor: c, home: a.ID}
}

func (f fixture) add(t *testing.T, c shopper, item uuid.UUID, qty int, choices ...uuid.UUID) {
	t.Helper()
	if _, err := f.carts.Add(context.Background(), c.ProfileID, item, qty, choices); err != nil {
		t.Fatalf("add to cart: %v", err)
	}
}

// fullCart fills a cart worth checking out: 2 × Adana Büyük (530 TL each) and
// 3 × Ayran (40 TL each): 1,180 TL of food plus 29.90 TL delivery.
func (f fixture) fullCart(t *testing.T, s shop, c shopper) {
	t.Helper()
	f.add(t, c, s.adana, 2, s.buyuk)
	f.add(t, c, s.ayran, 3)
}

func (f fixture) orderCount(t *testing.T, c shopper) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM orders WHERE customer_profile_id = $1`, c.ProfileID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPlaceCopiesTheCartIntoAnOrder(t *testing.T) {
	f := setup(t)
	s, me := f.newShop(t), f.newShopper(t)
	ctx := context.Background()
	f.fullCart(t, s, me)

	o, err := f.orders.Place(ctx, me.ProfileID, me.home)
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != Placed || o.SubtotalKurus != 118000 || o.DeliveryFeeKurus != 2990 || o.TotalKurus != 120990 {
		t.Fatalf("order %s: %d + %d = %d; want placed, 118000 + 2990 = 120990",
			o.Status, o.SubtotalKurus, o.DeliveryFeeKurus, o.TotalKurus)
	}
	if len(o.Items) != 2 || o.Items[0].Name != "Adana" || o.Items[0].UnitPriceKurus != 53000 ||
		o.Items[0].LineTotalKurus != 106000 || len(o.Items[0].Options) != 1 ||
		o.Items[0].Options[0].ChoiceName != "Büyük" || o.Items[0].Options[0].ExtraPriceKurus != 8000 {
		t.Fatalf("items: %+v", o.Items)
	}
	if o.Address.Label != "Ev" || o.Address.Details != "Kat 3, daire 7" || o.RestaurantName != "Hasan Usta" {
		t.Fatalf("address or restaurant not copied: %+v, %q", o.Address, o.RestaurantName)
	}

	// The cart is gone, and the order's life starts with one history row.
	if c, err := f.carts.Get(ctx, me.ProfileID); err != nil || len(c.Items) != 0 {
		t.Fatalf("cart after checkout: %+v, %v; want empty", c, err)
	}
	h := f.history(t, o.ID)
	if len(h) != 1 || h[0].From != "" || h[0].To != Placed || h[0].ByRole != "customer" || *h[0].By != me.ProfileID {
		t.Fatalf("history %+v; want one row: placed by the customer", h)
	}

	// Reading it back gives the same order.
	got, err := f.orders.Get(ctx, o.ID, me.Actor)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, o) {
		t.Fatalf("read back:\n%+v\nwant\n%+v", got, o)
	}
}

// The order is a receipt: changing the menu, the restaurant or the address book
// afterwards doesn't touch it.
func TestOrdersDontChangeWhenTheMenuDoes(t *testing.T) {
	f := setup(t)
	s, me := f.newShop(t), f.newShopper(t)
	ctx := context.Background()
	f.fullCart(t, s, me)
	o, err := f.orders.Place(ctx, me.ProfileID, me.home)
	if err != nil {
		t.Fatal(err)
	}

	price, name := 99900, "Adana Dürüm"
	if _, err := f.menus.UpdateItem(ctx, s.id, s.owner, s.adana, menus.ItemPatch{PriceKurus: &price, Name: &name}); err != nil {
		t.Fatal(err)
	}
	if err := f.menus.DeleteOptionChoice(ctx, s.id, s.owner, s.buyuk); err != nil {
		t.Fatal(err)
	}
	newName, fee := "Hasan Usta Kadıköy", 5000
	if _, err := f.restaurants.Update(ctx, s.id, s.owner, restaurants.Patch{Name: &newName, DeliveryFeeKurus: &fee}); err != nil {
		t.Fatal(err)
	}
	if err := f.addresses.Delete(ctx, me.home, me.ProfileID); err != nil {
		t.Fatal(err)
	}

	got, err := f.orders.Get(ctx, o.ID, me.Actor)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, o) {
		t.Fatalf("the order changed:\n%+v\nwant\n%+v", got, o)
	}
}

func TestPlaceRefuses(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	unavailable, paused := false, false

	cases := []struct {
		name    string
		prepare func(t *testing.T, s shop, me shopper) uuid.UUID // returns the address to send to
		want    error
	}{
		{"an empty cart", func(t *testing.T, s shop, me shopper) uuid.UUID {
			return me.home
		}, ErrCartEmpty},
		{"a dish that can't be ordered", func(t *testing.T, s shop, me shopper) uuid.UUID {
			f.fullCart(t, s, me)
			if _, err := f.menus.UpdateItem(ctx, s.id, s.owner, s.ayran, menus.ItemPatch{Available: &unavailable}); err != nil {
				t.Fatal(err)
			}
			return me.home
		}, ErrCartHasProblems},
		{"a restaurant that paused orders", func(t *testing.T, s shop, me shopper) uuid.UUID {
			f.fullCart(t, s, me)
			if _, err := f.restaurants.Update(ctx, s.id, s.owner, restaurants.Patch{AcceptingOrders: &paused}); err != nil {
				t.Fatal(err)
			}
			return me.home
		}, ErrRestaurantClosed},
		{"someone else's address", func(t *testing.T, s shop, me shopper) uuid.UUID {
			f.fullCart(t, s, me)
			return f.newShopper(t).home
		}, addresses.ErrNotFound},
		{"an address that doesn't exist", func(t *testing.T, s shop, me shopper) uuid.UUID {
			f.fullCart(t, s, me)
			return uuid.Must(uuid.NewV7())
		}, addresses.ErrNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, me := f.newShop(t), f.newShopper(t)
			addr := tc.prepare(t, s, me)
			before, err := f.carts.Get(ctx, me.ProfileID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.orders.Place(ctx, me.ProfileID, addr); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			after, err := f.carts.Get(ctx, me.ProfileID)
			if err != nil {
				t.Fatal(err)
			}
			if f.orderCount(t, me) != 0 || len(after.Items) != len(before.Items) {
				t.Fatal("a refused checkout made an order or changed the cart")
			}
		})
	}

	t.Run("a basket below the minimum", func(t *testing.T) {
		s, me := f.newShop(t), f.newShopper(t)
		f.add(t, me, s.ayran, 1) // 40 TL, minimum 300 TL
		_, err := f.orders.Place(ctx, me.ProfileID, me.home)
		var below *BelowMinimumError
		if !errors.As(err, &below) || below.MinBasketKurus != 30000 || below.KurusToMinimum != 26000 {
			t.Fatalf("got %v (%+v), want BelowMinimumError: 260 TL to go", err, below)
		}
	})
}

// holdCartLock locks the customer's cart inside tx, the way checkout does. It's
// written out here rather than calling lockCart, so breaking lockCart can't
// also break the test's own grip on the cart.
func holdCartLock(t *testing.T, tx pgx.Tx, c shopper) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := tx.QueryRow(context.Background(),
		`SELECT id FROM carts WHERE customer_profile_id = $1 FOR UPDATE`, c.ProfileID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// waitsAtTheLock checks that nothing arrives on done for a moment: the work
// behind it is queued at a lock the test holds.
func waitsAtTheLock[T any](t *testing.T, done <-chan T) {
	t.Helper()
	select {
	case v := <-done:
		t.Fatalf("finished while the cart was locked (%v); it should have waited", v)
	case <-time.After(200 * time.Millisecond):
	}
}

// Double-tapping "Place order": two checkouts of one cart at the same moment.
//
// The test holds the cart lock itself, starts both checkouts (both price the
// cart and then queue at the lock), and lets go. Exactly one order must come
// out; the other checkout finds the cart already gone. Without the lock in
// Place, both would read the same lines and both would make an order.
func TestTwoCheckoutsOfOneCart(t *testing.T) {
	f := setup(t)
	s, me := f.newShop(t), f.newShopper(t)
	ctx := context.Background()
	f.fullCart(t, s, me)

	hold, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback(ctx)
	holdCartLock(t, hold, me)

	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := f.orders.Place(ctx, me.ProfileID, me.home)
			results <- err
		}()
	}
	waitsAtTheLock(t, results)
	if err := hold.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	errs := []error{<-results, <-results}
	placed, empty := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			placed++
		case errors.Is(err, ErrCartEmpty):
			empty++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if placed != 1 || empty != 1 || f.orderCount(t, me) != 1 {
		t.Fatalf("%d placed, %d found the cart empty, %d orders; want 1, 1, 1", placed, empty, f.orderCount(t, me))
	}
}

// The cart changes after it was priced but before checkout locks it (the
// customer adds an Ayran from another phone). Checkout must notice instead of
// charging the old total for the new cart.
func TestCheckoutNoticesACartThatChanged(t *testing.T) {
	f := setup(t)
	s, me := f.newShop(t), f.newShopper(t)
	ctx := context.Background()
	f.fullCart(t, s, me)

	hold, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback(ctx)
	cartID := holdCartLock(t, hold, me)

	result := make(chan error, 1)
	go func() {
		_, err := f.orders.Place(ctx, me.ProfileID, me.home)
		result <- err
	}()
	waitsAtTheLock(t, result)
	// While checkout waits, one more Ayran lands in the cart.
	if _, err := hold.Exec(ctx, `UPDATE cart_items SET quantity = quantity + 1
		WHERE cart_id = $1 AND menu_item_id = $2`, cartID, s.ayran); err != nil {
		t.Fatal(err)
	}
	if err := hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	if err := <-result; !errors.Is(err, ErrCartChanged) {
		t.Fatalf("got %v, want ErrCartChanged", err)
	}
	if f.orderCount(t, me) != 0 {
		t.Fatal("an order was made from a cart the customer hadn't seen")
	}
}

func TestOnlyTheOrdersPeopleCanReadIt(t *testing.T) {
	f := setup(t)
	s, me := f.newShop(t), f.newShopper(t)
	ctx := context.Background()
	f.fullCart(t, s, me)
	o, err := f.orders.Place(ctx, me.ProfileID, me.home)
	if err != nil {
		t.Fatal(err)
	}

	owner := Actor{Role: users.RoleRestaurant, ProfileID: s.owner}
	for _, by := range []Actor{me.Actor, owner, System} {
		if _, err := f.orders.Get(ctx, o.ID, by); err != nil {
			t.Errorf("%s can't read the order: %v", by.Role, err)
		}
	}
	for _, by := range []Actor{f.newShopper(t).Actor, f.profile(t, users.RoleRestaurant)} {
		if _, err := f.orders.Get(ctx, o.ID, by); !errors.Is(err, ErrNotFound) {
			t.Errorf("a stranger (%s) got %v, want ErrNotFound", by.Role, err)
		}
	}
}

func TestListAndHistory(t *testing.T) {
	f := setup(t)
	s, me := f.newShop(t), f.newShopper(t)
	ctx := context.Background()

	var placed []Order
	for range 2 {
		f.fullCart(t, s, me)
		o, err := f.orders.Place(ctx, me.ProfileID, me.home)
		if err != nil {
			t.Fatal(err)
		}
		placed = append(placed, o)
	}
	f.newShopper(t) // someone else, with no orders

	list, err := f.orders.ListForCustomer(ctx, me.ProfileID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != placed[1].ID || list[1].ID != placed[0].ID {
		t.Fatalf("got %d orders; want both, newest first", len(list))
	}
	if !reflect.DeepEqual(list[1], placed[0]) {
		t.Fatalf("listed order differs from the placed one:\n%+v\nwant\n%+v", list[1], placed[0])
	}

	if _, err := f.orders.Transition(ctx, placed[0].ID, Cancel, me.Actor); err != nil {
		t.Fatal(err)
	}
	h, err := f.orders.History(ctx, placed[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 2 || h[0].From != nil || h[0].To != Placed || h[0].By != "customer" ||
		*h[1].From != Placed || h[1].To != Cancelled || *h[1].CancelReason != ByCustomer {
		t.Fatalf("history %+v; want placed, then placed → cancelled (by_customer)", h)
	}
}

func TestKitchenList(t *testing.T) {
	f := setup(t)
	s, me := f.newShop(t), f.newShopper(t)
	ctx := context.Background()
	owner := Actor{Role: users.RoleRestaurant, ProfileID: s.owner}

	place := func() Order {
		t.Helper()
		f.fullCart(t, s, me)
		o, err := f.orders.Place(ctx, me.ProfileID, me.home)
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	move := func(o Order, a Action, by Actor) {
		t.Helper()
		if _, err := f.orders.Transition(ctx, o.ID, a, by); err != nil {
			t.Fatalf("%s: %v", a, err)
		}
	}
	unpaid := place()
	first, second := place(), place()
	move(first, Pay, System)
	move(second, Pay, System)
	move(second, Accept, owner)
	cancelled := place()
	move(cancelled, Pay, System)
	move(cancelled, Reject, owner)

	list, err := f.orders.ListForKitchen(ctx, s.id, s.owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != first.ID || list[1].ID != second.ID || list[1].Status != Accepted {
		t.Fatalf("got %d orders; want the paid one, then the accepted one (not %s unpaid or the rejected one)", len(list), unpaid.ID)
	}
	if len(list[0].Items) != 2 {
		t.Fatalf("kitchen orders need their dishes; got %d", len(list[0].Items))
	}

	stranger := f.profile(t, users.RoleRestaurant)
	if _, err := f.orders.ListForKitchen(ctx, s.id, stranger.ProfileID); !errors.Is(err, ErrRestaurantNotFound) {
		t.Fatalf("another owner: got %v, want ErrRestaurantNotFound", err)
	}
}

// The API stores its idempotent answer in finish. If that fails, there must be
// no order either: an order whose answer was never stored leaves its key in
// progress, and the retry that takes the key over would then hear "your cart is
// empty" about an order that exists.
func TestFinishRunsInTheOrdersTransaction(t *testing.T) {
	f := setup(t)
	s := f.newShop(t)
	ctx := context.Background()

	t.Run("it sees the new order", func(t *testing.T) {
		me := f.newShopper(t)
		f.fullCart(t, s, me)
		var seen []HistoryEntry
		o, err := f.orders.PlaceAnd(ctx, me.ProfileID, me.home, func(ctx context.Context, tx pgx.Tx, o Order) error {
			var err error
			seen, err = HistoryIn(ctx, tx, o.ID)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(seen) != 1 || seen[0].To != Placed || !seen[0].At.Equal(o.CreatedAt) {
			t.Fatalf("finish saw history %+v; want the placed entry", seen)
		}
	})

	t.Run("its failure undoes the checkout", func(t *testing.T) {
		me := f.newShopper(t)
		f.fullCart(t, s, me)
		boom := errors.New("could not store the answer")
		_, err := f.orders.PlaceAnd(ctx, me.ProfileID, me.home, func(context.Context, pgx.Tx, Order) error {
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("got %v, want finish's own error", err)
		}
		if n := f.orderCount(t, me); n != 0 {
			t.Fatalf("%d orders, want 0", n)
		}
		c, err := f.carts.Get(ctx, me.ProfileID)
		if err != nil {
			t.Fatal(err)
		}
		if len(c.Items) != 2 {
			t.Fatalf("cart has %d lines, want the 2 still there", len(c.Items))
		}
	})
}

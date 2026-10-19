package menus_test

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atalaymudanyali/quickbytes/internal/menus"
	"github.com/atalaymudanyali/quickbytes/internal/restaurants"
	"github.com/atalaymudanyali/quickbytes/internal/testutil"
	"github.com/atalaymudanyali/quickbytes/internal/users"
)

type fixture struct {
	pool  *pgxpool.Pool
	menus *menus.Store
}

// restaurant is one test restaurant and the profile that owns it.
type restaurant struct {
	id, owner uuid.UUID
}

func setup(t *testing.T) fixture {
	t.Helper()
	pool := testutil.DB(t)
	return fixture{pool: pool, menus: menus.NewStore(pool)}
}

func (f fixture) newRestaurant(t *testing.T) restaurant {
	t.Helper()
	ctx := context.Background()
	u, _, err := users.NewStore(f.pool).Create(ctx, testutil.UniqueEmail(), "hash", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	p, err := users.NewStore(f.pool).AddProfile(ctx, u.ID, users.RoleRestaurant)
	if err != nil {
		t.Fatal(err)
	}
	r, err := restaurants.NewStore(f.pool).Create(ctx, p.ID, restaurants.New{
		Name: "Hamdi", Address: "Eminönü", Lat: 41.0170, Lng: 28.9720, DeliveryRadiusM: 3000,
	})
	if err != nil {
		t.Fatal(err)
	}
	return restaurant{id: r.ID, owner: p.ID}
}

func (f fixture) category(t *testing.T, r restaurant, name string) menus.Category {
	t.Helper()
	c, err := f.menus.CreateCategory(context.Background(), r.id, r.owner, name)
	if err != nil {
		t.Fatalf("create category %q: %v", name, err)
	}
	return c
}

func (f fixture) item(t *testing.T, r restaurant, categoryID uuid.UUID, name string, price int) menus.Item {
	t.Helper()
	it, err := f.menus.CreateItem(context.Background(), r.id, r.owner, menus.NewItem{
		CategoryID: categoryID, Name: name, PriceKurus: price,
	})
	if err != nil {
		t.Fatalf("create item %q: %v", name, err)
	}
	return it
}

func (f fixture) menuVersion(t *testing.T, r restaurant) int64 {
	t.Helper()
	var v int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT menu_version FROM restaurants WHERE id = $1`, r.id).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestNewThingsGoToTheEndAndBumpTheVersion(t *testing.T) {
	f := setup(t)
	r := f.newRestaurant(t)
	before := f.menuVersion(t, r)

	soups := f.category(t, r, "Çorbalar")
	kebabs := f.category(t, r, "Kebaplar")
	adana := f.item(t, r, kebabs.ID, "Adana Kebap", 32000)
	urfa := f.item(t, r, kebabs.ID, "Urfa Kebap", 31000)

	if soups.Position != 1 || kebabs.Position != 2 {
		t.Errorf("category positions = %d, %d; want 1, 2", soups.Position, kebabs.Position)
	}
	if adana.Position != 1 || urfa.Position != 2 {
		t.Errorf("item positions = %d, %d; want 1, 2", adana.Position, urfa.Position)
	}
	if !adana.Available || adana.ArchivedAt != nil {
		t.Error("new items should be available and not archived")
	}
	if got := f.menuVersion(t, r); got != before+4 {
		t.Errorf("menu_version = %d after 4 changes, want %d", got, before+4)
	}
}

func TestOnlyTheOwnerCanChangeTheMenu(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	r, stranger := f.newRestaurant(t), f.newRestaurant(t)
	kebabs := f.category(t, r, "Kebaplar")
	before := f.menuVersion(t, r)

	if _, err := f.menus.CreateCategory(ctx, r.id, stranger.owner, "Tatlılar"); !errors.Is(err, menus.ErrRestaurantNotFound) {
		t.Errorf("stranger adding a category: err = %v, want ErrRestaurantNotFound", err)
	}
	if _, err := f.menus.CreateItem(ctx, r.id, stranger.owner, menus.NewItem{CategoryID: kebabs.ID, Name: "x", PriceKurus: 100}); !errors.Is(err, menus.ErrRestaurantNotFound) {
		t.Errorf("stranger adding an item: err = %v, want ErrRestaurantNotFound", err)
	}
	if _, err := f.menus.CreateCategory(ctx, uuid.Must(uuid.NewV7()), r.owner, "x"); !errors.Is(err, menus.ErrRestaurantNotFound) {
		t.Errorf("unknown restaurant: err = %v, want ErrRestaurantNotFound", err)
	}
	if got := f.menuVersion(t, r); got != before {
		t.Errorf("refused changes still bumped menu_version (%d → %d)", before, got)
	}
}

func TestItemsNeedAnActiveCategoryOfTheSameRestaurant(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	r, other := f.newRestaurant(t), f.newRestaurant(t)
	kebabs := f.category(t, r, "Kebaplar")
	theirs := f.category(t, other, "Pideler")

	add := func(categoryID uuid.UUID) error {
		_, err := f.menus.CreateItem(ctx, r.id, r.owner, menus.NewItem{CategoryID: categoryID, Name: "Lahmacun", PriceKurus: 9000})
		return err
	}

	if err := add(theirs.ID); !errors.Is(err, menus.ErrCategoryNotFound) {
		t.Errorf("another restaurant's category: err = %v, want ErrCategoryNotFound", err)
	}

	if _, err := f.menus.SetCategoryArchived(ctx, r.id, r.owner, kebabs.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := add(kebabs.ID); !errors.Is(err, menus.ErrCategoryNotFound) {
		t.Errorf("archived category: err = %v, want ErrCategoryNotFound", err)
	}

	restored, err := f.menus.SetCategoryArchived(ctx, r.id, r.owner, kebabs.ID, false)
	if err != nil || restored.ArchivedAt != nil {
		t.Fatalf("restore: %+v, %v", restored, err)
	}
	if err := add(kebabs.ID); err != nil {
		t.Errorf("restored category: %v", err)
	}
}

func TestUpdateItemChangesOnlyWhatIsSent(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	r := f.newRestaurant(t)
	kebabs, grills := f.category(t, r, "Kebaplar"), f.category(t, r, "Izgaralar")
	adana := f.item(t, r, kebabs.ID, "Adana Kebap", 32000)
	f.item(t, r, grills.ID, "Köfte", 25000)

	newPrice := 34500
	got, err := f.menus.UpdateItem(ctx, r.id, r.owner, adana.ID, menus.ItemPatch{PriceKurus: &newPrice})
	if err != nil {
		t.Fatal(err)
	}
	if got.PriceKurus != 34500 || got.Name != "Adana Kebap" || !got.Available || got.CategoryID != kebabs.ID {
		t.Fatalf("after price change: %+v", got)
	}

	soldOut := false
	got, _ = f.menus.UpdateItem(ctx, r.id, r.owner, adana.ID, menus.ItemPatch{Available: &soldOut})
	if got.Available || got.PriceKurus != 34500 {
		t.Fatalf("after sold out: %+v", got)
	}

	got, err = f.menus.UpdateItem(ctx, r.id, r.owner, adana.ID, menus.ItemPatch{CategoryID: &grills.ID})
	if err != nil || got.CategoryID != grills.ID || got.Position != 2 {
		t.Fatalf("after moving to Izgaralar: %+v, %v; want position 2 (after Köfte)", got, err)
	}

	if _, err := f.menus.UpdateItem(ctx, r.id, r.owner, uuid.Must(uuid.NewV7()), menus.ItemPatch{Available: &soldOut}); !errors.Is(err, menus.ErrItemNotFound) {
		t.Errorf("unknown item: err = %v, want ErrItemNotFound", err)
	}
}

func TestArchiveAndRestoreItem(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	r := f.newRestaurant(t)
	soups := f.category(t, r, "Çorbalar")
	lentil := f.item(t, r, soups.ID, "Mercimek Çorbası", 9000)

	archived, err := f.menus.SetItemArchived(ctx, r.id, r.owner, lentil.ID, true)
	if err != nil || archived.ArchivedAt == nil {
		t.Fatalf("archive: %+v, %v", archived, err)
	}
	restored, err := f.menus.SetItemArchived(ctx, r.id, r.owner, lentil.ID, false)
	if err != nil || restored.ArchivedAt != nil || restored.Name != "Mercimek Çorbası" {
		t.Fatalf("restore: %+v, %v", restored, err)
	}
}

func TestDatabaseRejectsBadPrices(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	r := f.newRestaurant(t)
	soups := f.category(t, r, "Çorbalar")

	// The API validates prices before they get here. These check the database's
	// own rule, the last line of defence.
	for _, price := range []int{0, -100, menus.MaxPriceKurus + 1} {
		if _, err := f.menus.CreateItem(ctx, r.id, r.owner, menus.NewItem{CategoryID: soups.ID, Name: "x", PriceKurus: price}); err == nil {
			t.Errorf("price %d kuruş was accepted", price)
		}
	}
}

func TestSimultaneousAddsGetDifferentPositions(t *testing.T) {
	f := setup(t)
	r := f.newRestaurant(t)

	const n = 10
	positions := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := f.menus.CreateCategory(context.Background(), r.id, r.owner, "Kategori")
			if err != nil {
				t.Errorf("create: %v", err)
				return
			}
			positions[i] = c.Position
		}()
	}
	wg.Wait()

	// Without the restaurant lock, two of these could both read "the last
	// position is 3" and both become 4.
	sort.Ints(positions)
	for i, p := range positions {
		if p != i+1 {
			t.Fatalf("positions = %v, want 1 to %d with no repeats", positions, n)
		}
	}
}

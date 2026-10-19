package menus_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/atalaymudanyali/quickbytes/internal/menus"
)

func TestMenuViews(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	r := f.newRestaurant(t)

	soups := f.category(t, r, "Çorbalar")
	kebabs := f.category(t, r, "Kebaplar")
	winter := f.category(t, r, "Kış Menüsü")
	lentil := f.item(t, r, soups.ID, "Mercimek", 9000)
	adana := f.item(t, r, kebabs.ID, "Adana Kebap", 32000)
	beyti := f.item(t, r, kebabs.ID, "Beyti", 36000)
	f.item(t, r, winter.ID, "Kelle Paça", 15000)

	mustUpdateItem(t, f, r, adana.ID, menus.ItemPatch{Available: ptr(false)}) // sold out today
	mustUpdateItem(t, f, r, beyti.ID, menus.ItemPatch{Archived: ptr(true)})   // no longer served
	if _, err := f.menus.UpdateCategory(ctx, r.id, r.owner, winter.ID, menus.CategoryPatch{Archived: ptr(true)}); err != nil {
		t.Fatal(err)
	}

	t.Run("public", func(t *testing.T) {
		m, err := f.menus.Menu(ctx, r.id, menus.View{})
		if err != nil {
			t.Fatal(err)
		}
		if got := shape(m); got != "Çorbalar[Mercimek] Kebaplar[Adana Kebap]" {
			t.Fatalf("public menu = %s; want archived category and item hidden", got)
		}
		if m.Categories[1].Items[0].Available {
			t.Error("sold-out Adana should be shown as not available")
		}
		if m.Version != f.menuVersion(t, r) {
			t.Errorf("version = %d, want %d", m.Version, f.menuVersion(t, r))
		}
	})

	t.Run("owner", func(t *testing.T) {
		m, err := f.menus.Menu(ctx, r.id, menus.View{OwnerProfileID: r.owner})
		if err != nil {
			t.Fatal(err)
		}
		if got := shape(m); got != "Çorbalar[Mercimek] Kebaplar[Adana Kebap Beyti] Kış Menüsü[Kelle Paça]" {
			t.Fatalf("owner menu = %s; want everything", got)
		}
		if m.Categories[1].Items[1].ArchivedAt == nil || m.Categories[2].ArchivedAt == nil {
			t.Error("archived things should be marked with archived_at in the owner's view")
		}
		if m.Categories[0].Items[0].ID != lentil.ID {
			t.Error("unexpected first item")
		}
	})

	t.Run("someone else's owner view", func(t *testing.T) {
		stranger := f.newRestaurant(t)
		if _, err := f.menus.Menu(ctx, r.id, menus.View{OwnerProfileID: stranger.owner}); !errors.Is(err, menus.ErrRestaurantNotFound) {
			t.Fatalf("err = %v, want ErrRestaurantNotFound", err)
		}
	})

	t.Run("unknown restaurant", func(t *testing.T) {
		if _, err := f.menus.Menu(ctx, uuid.Must(uuid.NewV7()), menus.View{}); !errors.Is(err, menus.ErrRestaurantNotFound) {
			t.Fatalf("err = %v, want ErrRestaurantNotFound", err)
		}
	})
}

func TestEmptyMenuIsEmptyNotNull(t *testing.T) {
	f := setup(t)
	r := f.newRestaurant(t)
	m, err := f.menus.Menu(context.Background(), r.id, menus.View{})
	if err != nil || m.Categories == nil || len(m.Categories) != 0 {
		t.Fatalf("menu = %+v, err = %v; want an empty list", m, err)
	}
}

func mustUpdateItem(t *testing.T, f fixture, r restaurant, id uuid.UUID, p menus.ItemPatch) {
	t.Helper()
	if _, err := f.menus.UpdateItem(context.Background(), r.id, r.owner, id, p); err != nil {
		t.Fatal(err)
	}
}

// shape writes a menu as "Category[Item Item] Category[Item]" for easy comparison.
func shape(m menus.Menu) string {
	out := ""
	for i, c := range m.Categories {
		if i > 0 {
			out += " "
		}
		out += c.Name + "["
		for j, it := range c.Items {
			if j > 0 {
				out += " "
			}
			out += it.Name
		}
		out += "]"
	}
	return out
}

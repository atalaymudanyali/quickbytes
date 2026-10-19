package menus_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/atalaymudanyali/quickbytes/internal/menus"
)

func TestReorderCategories(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	r := f.newRestaurant(t)
	soups, kebabs, drinks := f.category(t, r, "Çorbalar"), f.category(t, r, "Kebaplar"), f.category(t, r, "İçecekler")
	winter := f.category(t, r, "Kış Menüsü")
	if _, err := f.menus.UpdateCategory(ctx, r.id, r.owner, winter.ID, menus.CategoryPatch{Archived: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	before := f.menuVersion(t, r)

	// Archived "Kış Menüsü" isn't part of the visible order, so it's not listed.
	if err := f.menus.ReorderCategories(ctx, r.id, r.owner, []uuid.UUID{kebabs.ID, soups.ID, drinks.ID}); err != nil {
		t.Fatalf("reorder: %v", err)
	}
	m, _ := f.menus.Menu(ctx, r.id, menus.View{})
	if got := shape(m); got != "Kebaplar[] Çorbalar[] İçecekler[]" {
		t.Fatalf("after reorder: %s", got)
	}
	if got := f.menuVersion(t, r); got != before+1 {
		t.Errorf("menu_version went from %d to %d; one reorder is one change", before, got)
	}
}

func TestReorderMustListExactlyTheActiveEntries(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	r, other := f.newRestaurant(t), f.newRestaurant(t)
	soups, kebabs := f.category(t, r, "Çorbalar"), f.category(t, r, "Kebaplar")
	theirs := f.category(t, other, "Pideler")

	for name, ids := range map[string][]uuid.UUID{
		"one missing":          {kebabs.ID},
		"listed twice":         {kebabs.ID, kebabs.ID},
		"another restaurant's": {kebabs.ID, theirs.ID},
		"extra on top":         {kebabs.ID, soups.ID, theirs.ID},
		"made-up ID":           {kebabs.ID, uuid.Must(uuid.NewV7())},
	} {
		t.Run(name, func(t *testing.T) {
			if err := f.menus.ReorderCategories(ctx, r.id, r.owner, ids); !errors.Is(err, menus.ErrOrderMismatch) {
				t.Fatalf("err = %v, want ErrOrderMismatch", err)
			}
		})
	}

	// Nothing moved after all those refusals.
	m, _ := f.menus.Menu(ctx, r.id, menus.View{})
	if got := shape(m); got != "Çorbalar[] Kebaplar[]" {
		t.Fatalf("order changed by a refused request: %s", got)
	}
}

func TestReorderItemsWithinACategory(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	r := f.newRestaurant(t)
	kebabs := f.category(t, r, "Kebaplar")
	adana := f.item(t, r, kebabs.ID, "Adana", 32000)
	urfa := f.item(t, r, kebabs.ID, "Urfa", 31000)
	beyti := f.item(t, r, kebabs.ID, "Beyti", 36000)

	if err := f.menus.ReorderItems(ctx, r.id, r.owner, kebabs.ID, []uuid.UUID{beyti.ID, adana.ID, urfa.ID}); err != nil {
		t.Fatalf("reorder: %v", err)
	}
	m, _ := f.menus.Menu(ctx, r.id, menus.View{})
	if got := shape(m); got != "Kebaplar[Beyti Adana Urfa]" {
		t.Fatalf("after reorder: %s", got)
	}

	t.Run("archived category", func(t *testing.T) {
		if _, err := f.menus.UpdateCategory(ctx, r.id, r.owner, kebabs.ID, menus.CategoryPatch{Archived: ptr(true)}); err != nil {
			t.Fatal(err)
		}
		err := f.menus.ReorderItems(ctx, r.id, r.owner, kebabs.ID, []uuid.UUID{adana.ID, urfa.ID, beyti.ID})
		if !errors.Is(err, menus.ErrCategoryNotFound) {
			t.Fatalf("err = %v, want ErrCategoryNotFound", err)
		}
	})
}

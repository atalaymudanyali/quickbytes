package menus_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/atalaymudanyali/quickbytes/internal/menus"
)

func (f fixture) group(t *testing.T, r restaurant, itemID uuid.UUID, name string, min, max int) menus.OptionGroup {
	t.Helper()
	g, err := f.menus.CreateOptionGroup(context.Background(), r.id, r.owner, itemID,
		menus.NewOptionGroup{Name: name, MinPicks: min, MaxPicks: max})
	if err != nil {
		t.Fatalf("create option group %q: %v", name, err)
	}
	return g
}

func (f fixture) choice(t *testing.T, r restaurant, groupID uuid.UUID, name string, extra int) menus.OptionChoice {
	t.Helper()
	c, err := f.menus.CreateOptionChoice(context.Background(), r.id, r.owner, groupID,
		menus.NewOptionChoice{Name: name, ExtraPriceKurus: extra})
	if err != nil {
		t.Fatalf("create option choice %q: %v", name, err)
	}
	return c
}

func (f fixture) choiceCount(t *testing.T, groupID uuid.UUID) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM option_choices WHERE group_id = $1`, groupID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestOptionsGoToTheEndAndBumpTheVersion(t *testing.T) {
	f := setup(t)
	r := f.newRestaurant(t)
	pide := f.item(t, r, f.category(t, r, "Pideler").ID, "Kıymalı Pide", 22000)
	before := f.menuVersion(t, r)

	size := f.group(t, r, pide.ID, "Boy", 1, 1)
	extras := f.group(t, r, pide.ID, "Ekstralar", 0, 3)
	small := f.choice(t, r, size.ID, "Küçük", 0)
	large := f.choice(t, r, size.ID, "Büyük", 4000)

	if size.Position != 1 || extras.Position != 2 {
		t.Errorf("group positions = %d, %d; want 1, 2", size.Position, extras.Position)
	}
	if small.Position != 1 || large.Position != 2 {
		t.Errorf("choice positions = %d, %d; want 1, 2", small.Position, large.Position)
	}
	if size.ItemID != pide.ID || large.GroupID != size.ID || large.ExtraPriceKurus != 4000 || !large.Available {
		t.Errorf("unexpected fields: group %+v, choice %+v", size, large)
	}
	if got := f.menuVersion(t, r); got != before+4 {
		t.Errorf("menu_version = %d after 4 changes, want %d", got, before+4)
	}
}

func TestOptionsBelongToTheirOwnRestaurant(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	r, other := f.newRestaurant(t), f.newRestaurant(t)
	pide := f.item(t, r, f.category(t, r, "Pideler").ID, "Kıymalı Pide", 22000)
	size := f.group(t, r, pide.ID, "Boy", 1, 1)
	small := f.choice(t, r, size.ID, "Küçük", 0)

	// A stranger can't touch r's options at all.
	if _, err := f.menus.CreateOptionGroup(ctx, r.id, other.owner, pide.ID, menus.NewOptionGroup{Name: "x", MaxPicks: 1}); !errors.Is(err, menus.ErrRestaurantNotFound) {
		t.Errorf("stranger adding a group: err = %v, want ErrRestaurantNotFound", err)
	}
	// Another owner using their own restaurant ID but r's item, group or choice:
	// those don't exist in their restaurant.
	if _, err := f.menus.CreateOptionGroup(ctx, other.id, other.owner, pide.ID, menus.NewOptionGroup{Name: "x", MaxPicks: 1}); !errors.Is(err, menus.ErrItemNotFound) {
		t.Errorf("group on another restaurant's item: err = %v, want ErrItemNotFound", err)
	}
	if _, err := f.menus.CreateOptionChoice(ctx, other.id, other.owner, size.ID, menus.NewOptionChoice{Name: "x"}); !errors.Is(err, menus.ErrOptionGroupNotFound) {
		t.Errorf("choice in another restaurant's group: err = %v, want ErrOptionGroupNotFound", err)
	}
	if _, err := f.menus.UpdateOptionGroup(ctx, other.id, other.owner, size.ID, menus.OptionGroupPatch{}); !errors.Is(err, menus.ErrOptionGroupNotFound) {
		t.Errorf("editing another restaurant's group: err = %v, want ErrOptionGroupNotFound", err)
	}
	if err := f.menus.DeleteOptionChoice(ctx, other.id, other.owner, small.ID); !errors.Is(err, menus.ErrOptionChoiceNotFound) {
		t.Errorf("deleting another restaurant's choice: err = %v, want ErrOptionChoiceNotFound", err)
	}
	if n := f.choiceCount(t, size.ID); n != 1 {
		t.Errorf("r's group has %d choices after the attempts, want 1", n)
	}
}

func TestUpdateOptionGroupKeepsPicksConsistent(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	r := f.newRestaurant(t)
	pide := f.item(t, r, f.category(t, r, "Pideler").ID, "Kıymalı Pide", 22000)
	extras := f.group(t, r, pide.ID, "Ekstralar", 0, 3)

	// Only min_picks is sent; together with the stored max_picks (3) it breaks
	// min <= max, which only the database can see.
	before := f.menuVersion(t, r)
	five := 5
	if _, err := f.menus.UpdateOptionGroup(ctx, r.id, r.owner, extras.ID, menus.OptionGroupPatch{MinPicks: &five}); !errors.Is(err, menus.ErrInvalidPicks) {
		t.Fatalf("min_picks above max_picks: err = %v, want ErrInvalidPicks", err)
	}
	if got := f.menuVersion(t, r); got != before {
		t.Errorf("a refused change bumped menu_version to %d, want %d", got, before)
	}

	// Sending both together is fine.
	name, two := "Soslar", 2
	g, err := f.menus.UpdateOptionGroup(ctx, r.id, r.owner, extras.ID,
		menus.OptionGroupPatch{Name: &name, MinPicks: &two, MaxPicks: &five})
	if err != nil {
		t.Fatal(err)
	}
	if g.Name != "Soslar" || g.MinPicks != 2 || g.MaxPicks != 5 || g.Position != extras.Position {
		t.Errorf("updated group = %+v", g)
	}
	if _, err := f.menus.CreateOptionGroup(ctx, r.id, r.owner, pide.ID, menus.NewOptionGroup{Name: "x", MinPicks: 0, MaxPicks: 21}); !errors.Is(err, menus.ErrInvalidPicks) {
		t.Errorf("max_picks 21: err = %v, want ErrInvalidPicks", err)
	}
}

func TestUpdateOptionChoice(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	r := f.newRestaurant(t)
	pide := f.item(t, r, f.category(t, r, "Pideler").ID, "Kıymalı Pide", 22000)
	cheese := f.choice(t, r, f.group(t, r, pide.ID, "Ekstralar", 0, 3).ID, "Kaşar", 1500)

	off, price := false, 2000
	c, err := f.menus.UpdateOptionChoice(ctx, r.id, r.owner, cheese.ID,
		menus.OptionChoicePatch{ExtraPriceKurus: &price, Available: &off})
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "Kaşar" || c.ExtraPriceKurus != 2000 || c.Available {
		t.Errorf("updated choice = %+v", c)
	}
	if _, err := f.menus.UpdateOptionChoice(ctx, r.id, r.owner, uuid.New(), menus.OptionChoicePatch{Available: &off}); !errors.Is(err, menus.ErrOptionChoiceNotFound) {
		t.Errorf("unknown choice: err = %v, want ErrOptionChoiceNotFound", err)
	}
}

func TestDeletingAGroupDeletesItsChoices(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	r := f.newRestaurant(t)
	pide := f.item(t, r, f.category(t, r, "Pideler").ID, "Kıymalı Pide", 22000)
	size := f.group(t, r, pide.ID, "Boy", 1, 1)
	small := f.choice(t, r, size.ID, "Küçük", 0)
	f.choice(t, r, size.ID, "Büyük", 4000)

	before := f.menuVersion(t, r)
	if err := f.menus.DeleteOptionChoice(ctx, r.id, r.owner, small.ID); err != nil {
		t.Fatal(err)
	}
	if n := f.choiceCount(t, size.ID); n != 1 {
		t.Errorf("%d choices left after deleting one of two, want 1", n)
	}
	if err := f.menus.DeleteOptionGroup(ctx, r.id, r.owner, size.ID); err != nil {
		t.Fatal(err)
	}
	if n := f.choiceCount(t, size.ID); n != 0 {
		t.Errorf("%d choices left after deleting their group, want 0", n)
	}
	if got := f.menuVersion(t, r); got != before+2 {
		t.Errorf("menu_version = %d after 2 deletes, want %d", got, before+2)
	}
	if err := f.menus.DeleteOptionGroup(ctx, r.id, r.owner, size.ID); !errors.Is(err, menus.ErrOptionGroupNotFound) {
		t.Errorf("deleting twice: err = %v, want ErrOptionGroupNotFound", err)
	}
}

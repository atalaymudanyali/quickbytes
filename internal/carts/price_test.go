package carts

import (
	"testing"

	"github.com/google/uuid"

	"github.com/atalaymudanyali/quickbytes/internal/menus"
	"github.com/atalaymudanyali/quickbytes/internal/restaurants"
)

// A small menu, built by hand: price is pure, so no database is needed.
var (
	adanaID, ayranID, kunefeID = uuid.New(), uuid.New(), uuid.New()
	sizeID, smallID, largeID   = uuid.New(), uuid.New(), uuid.New()

	kebapci = restaurants.Restaurant{ID: uuid.New(), Name: "Hasan Usta", AcceptingOrders: true,
		DeliveryFeeKurus: 2990, MinBasketKurus: 30000}
)

func testMenu() menus.Menu {
	adana := menus.MenuItem{
		Item: menus.Item{ID: adanaID, Name: "Adana Kebap", PriceKurus: 45000, Available: true},
		OptionGroups: []menus.MenuOptionGroup{{
			OptionGroup: menus.OptionGroup{ID: sizeID, Name: "Boy", MinPicks: 1, MaxPicks: 1},
			Choices: []menus.OptionChoice{
				{ID: smallID, GroupID: sizeID, Name: "Normal", Available: true},
				{ID: largeID, GroupID: sizeID, Name: "Büyük", ExtraPriceKurus: 8000, Available: true},
			},
		}},
	}
	ayran := menus.MenuItem{Item: menus.Item{ID: ayranID, Name: "Ayran", PriceKurus: 4000, Available: true},
		OptionGroups: []menus.MenuOptionGroup{}}
	kunefe := menus.MenuItem{Item: menus.Item{ID: kunefeID, Name: "Künefe", PriceKurus: 18000, Available: false},
		OptionGroups: []menus.MenuOptionGroup{}}
	return menus.Menu{Categories: []menus.MenuCategory{{Items: []menus.MenuItem{adana, ayran, kunefe}}}}
}

func TestPriceEmptyCart(t *testing.T) {
	c := price(nil, nil, menus.Menu{})
	if c.Restaurant != nil || len(c.Items) != 0 || c.TotalKurus != 0 || c.BelowMinimum {
		t.Fatalf("got %+v, want an empty cart with zeros", c)
	}
}

func TestPriceAddsUpFromTheMenu(t *testing.T) {
	c := price(&kebapci, []line{
		{ID: uuid.New(), MenuItemID: adanaID, Quantity: 2, ChoiceIDs: []uuid.UUID{largeID}},
		{ID: uuid.New(), MenuItemID: ayranID, Quantity: 3, ChoiceIDs: []uuid.UUID{}},
	}, testMenu())

	// 2 × (450 + 80 TL) + 3 × 40 TL = 1060 + 120 = 1180 TL, plus 29,90 TL delivery.
	if c.SubtotalKurus != 118000 || c.DeliveryFeeKurus != 2990 || c.TotalKurus != 120990 {
		t.Fatalf("subtotal %d, fee %d, total %d; want 118000, 2990, 120990",
			c.SubtotalKurus, c.DeliveryFeeKurus, c.TotalKurus)
	}
	adana := c.Items[0]
	if *adana.UnitPriceKurus != 53000 || *adana.LineTotalKurus != 106000 || adana.Problem != nil {
		t.Fatalf("adana line = %+v", adana)
	}
	if len(adana.Options) != 1 || adana.Options[0].ChoiceName != "Büyük" || adana.Options[0].GroupName != "Boy" {
		t.Fatalf("adana options = %+v, want Boy: Büyük", adana.Options)
	}
	if c.BelowMinimum {
		t.Fatal("1180 TL is above the 300 TL minimum")
	}
}

func TestPriceFollowsTheMenuNotTheCart(t *testing.T) {
	m := testMenu()
	m.Categories[0].Items[1].PriceKurus = 5000 // ayran went up to 50 TL
	c := price(&kebapci, []line{{ID: uuid.New(), MenuItemID: ayranID, Quantity: 1}}, m)
	if *c.Items[0].UnitPriceKurus != 5000 {
		t.Fatalf("unit price = %d, want the menu's new 5000", *c.Items[0].UnitPriceKurus)
	}
}

func TestPriceBelowMinimum(t *testing.T) {
	c := price(&kebapci, []line{{ID: uuid.New(), MenuItemID: ayranID, Quantity: 2}}, testMenu())
	if !c.BelowMinimum || c.KurusToMinimum != 22000 {
		t.Fatalf("below %v, missing %d; want true and 22000 (300 TL - 80 TL)", c.BelowMinimum, c.KurusToMinimum)
	}
	if c.TotalKurus != 8000+2990 {
		t.Fatalf("total = %d; below the minimum is a warning, the total still adds up", c.TotalKurus)
	}
}

func TestPriceFlagsProblemLinesWithoutDroppingThem(t *testing.T) {
	gone := uuid.New()
	c := price(&kebapci, []line{
		{ID: uuid.New(), MenuItemID: kunefeID, Name: "Künefe", Quantity: 1},                    // switched off
		{ID: uuid.New(), MenuItemID: gone, Name: "Eski Tatlı", Quantity: 1},                    // archived
		{ID: uuid.New(), MenuItemID: adanaID, Name: "Adana", Quantity: 1},                      // no size picked
		{ID: uuid.New(), MenuItemID: adanaID, Quantity: 1, ChoiceIDs: []uuid.UUID{uuid.New()}}, // choice deleted
	}, testMenu())

	want := []ProblemCode{ItemUnavailable, ItemRemoved, OptionsChanged, OptionsChanged}
	if len(c.Items) != len(want) {
		t.Fatalf("%d lines, want all %d kept", len(c.Items), len(want))
	}
	for i, w := range want {
		l := c.Items[i]
		if l.Problem == nil || l.Problem.Code != w || l.UnitPriceKurus != nil || l.LineTotalKurus != nil {
			t.Errorf("line %d (%s) = problem %+v, price %v; want %s and no price", i, l.Name, l.Problem, l.UnitPriceKurus, w)
		}
	}
	if c.Items[1].Name != "Eski Tatlı" {
		t.Errorf("a removed dish keeps its name, got %q", c.Items[1].Name)
	}
	if c.SubtotalKurus != 0 || c.DeliveryFeeKurus != 0 || c.TotalKurus != 0 {
		t.Fatalf("subtotal %d, fee %d, total %d; nothing orderable should cost nothing",
			c.SubtotalKurus, c.DeliveryFeeKurus, c.TotalKurus)
	}
}

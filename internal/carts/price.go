package carts

import (
	"errors"

	"github.com/google/uuid"

	"github.com/atalaymudanyali/quickbytes/internal/menus"
	"github.com/atalaymudanyali/quickbytes/internal/restaurants"
)

// Cart is a priced cart, as the customer sees it. An empty cart has no
// restaurant, no items and every amount 0.
type Cart struct {
	Restaurant *CartRestaurant `json:"restaurant"`
	Items      []Line          `json:"items"`
	// SubtotalKurus is the dishes and their extras; problem lines don't count.
	SubtotalKurus    int `json:"subtotal_kurus"`
	DeliveryFeeKurus int `json:"delivery_fee_kurus"`
	TotalKurus       int `json:"total_kurus"`
	MinBasketKurus   int `json:"min_basket_kurus"`
	// BelowMinimum is a warning, not a block: a basket may be half full while
	// the customer shops. Checkout is where the minimum is enforced.
	BelowMinimum   bool `json:"below_minimum"`
	KurusToMinimum int  `json:"kurus_to_minimum"`
}

// CartRestaurant is the part of the restaurant a basket needs to show.
type CartRestaurant struct {
	ID              uuid.UUID `json:"id"`
	Name            string    `json:"name"`
	AcceptingOrders bool      `json:"accepting_orders"`
}

// Line is one priced line. The prices are null on a line with a problem: there
// is no honest price for a dish that can't be ordered as it is.
type Line struct {
	ID             uuid.UUID      `json:"id"`
	MenuItemID     uuid.UUID      `json:"menu_item_id"`
	Name           string         `json:"name"`
	Quantity       int            `json:"quantity"`
	Options        []PickedOption `json:"options"`
	UnitPriceKurus *int           `json:"unit_price_kurus"`
	LineTotalKurus *int           `json:"line_total_kurus"`
	Problem        *Problem       `json:"problem"`
}

// PickedOption is one picked choice, named the way the menu names it now.
type PickedOption struct {
	GroupID         uuid.UUID `json:"group_id"`
	GroupName       string    `json:"group_name"`
	ChoiceID        uuid.UUID `json:"choice_id"`
	ChoiceName      string    `json:"choice_name"`
	ExtraPriceKurus int       `json:"extra_price_kurus"`
}

// Problem says why a line can't be ordered as it is. The line stays in the cart,
// so the customer sees what happened instead of a dish quietly disappearing.
type Problem struct {
	Code    ProblemCode `json:"code"`
	Message string      `json:"message"`
}

type ProblemCode string

const (
	ItemUnavailable ProblemCode = "item_unavailable" // switched off for now; may come back
	ItemRemoved     ProblemCode = "item_removed"     // no longer on the menu
	OptionsChanged  ProblemCode = "options_changed"  // the picks no longer fit the dish's options
)

// price works out a cart's lines and totals from the live menu. It's pure, like
// menus.ValidateChoices: no database, no clock, so it's tested with plain values.
// r is nil for a customer with no cart.
func price(r *restaurants.Restaurant, lines []line, m menus.Menu) Cart {
	c := Cart{Items: []Line{}}
	if r == nil {
		return c
	}
	c.Restaurant = &CartRestaurant{ID: r.ID, Name: r.Name, AcceptingOrders: r.AcceptingOrders}

	onMenu := map[uuid.UUID]menus.MenuItem{}
	for _, cat := range m.Categories {
		for _, it := range cat.Items {
			onMenu[it.ID] = it
		}
	}

	priced := 0
	for _, l := range lines {
		out := Line{ID: l.ID, MenuItemID: l.MenuItemID, Name: l.Name, Quantity: l.Quantity, Options: []PickedOption{}}
		c.Items = append(c.Items, out)
		last := &c.Items[len(c.Items)-1]

		item, ok := onMenu[l.MenuItemID]
		if !ok {
			last.Problem = &Problem{Code: ItemRemoved, Message: l.Name + " is no longer on the menu"}
			continue
		}
		last.Name = item.Name // the menu's current name
		if !item.Available {
			last.Problem = &Problem{Code: ItemUnavailable, Message: item.Name + " is not available right now"}
			continue
		}
		sel, err := menus.ValidateChoices(item, l.ChoiceIDs)
		if err != nil {
			msg := err.Error()
			var ce *menus.ChoiceError
			if errors.As(err, &ce) {
				msg = "the options for " + item.Name + " have changed: " + ce.Message
			}
			last.Problem = &Problem{Code: OptionsChanged, Message: msg}
			continue
		}

		for _, p := range sel.Picks {
			last.Options = append(last.Options, PickedOption{
				GroupID: p.Group.ID, GroupName: p.Group.Name,
				ChoiceID: p.Choice.ID, ChoiceName: p.Choice.Name, ExtraPriceKurus: p.Choice.ExtraPriceKurus,
			})
		}
		unit := item.PriceKurus + sel.ExtraKurus
		total := unit * l.Quantity
		last.UnitPriceKurus, last.LineTotalKurus = &unit, &total
		c.SubtotalKurus += total
		priced++
	}

	// The fee is charged for delivering something: a cart with nothing orderable
	// shows none.
	if priced > 0 {
		c.DeliveryFeeKurus = r.DeliveryFeeKurus
	}
	c.TotalKurus = c.SubtotalKurus + c.DeliveryFeeKurus
	c.MinBasketKurus = r.MinBasketKurus
	if c.SubtotalKurus < r.MinBasketKurus {
		c.BelowMinimum = true
		c.KurusToMinimum = r.MinBasketKurus - c.SubtotalKurus
	}
	return c
}

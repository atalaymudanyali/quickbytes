package seed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// OrderPlan says whose demo orders to place, and where. The restaurant must be
// one of Owner's.
type OrderPlan struct {
	Customer   string // email
	Owner      string // email
	Restaurant string // name
}

// DemoOrders fills Hasan's kitchen queue with orders from the demo customer.
var DemoOrders = OrderPlan{
	Customer:   "musteri@demo.quickbytes.test",
	Owner:      "hasan@demo.quickbytes.test",
	Restaurant: "Hasan Usta Kebap Kadıköy",
}

// The provider's test cards (see the README).
const (
	cardApprove = "4242424242424242"
	cardDecline = "4000000000000002"
)

// step is one button pressed on an order: by the customer or the owner.
type step struct {
	byOwner bool
	action  string // the last part of POST /orders/{id}/<action>
}

var (
	accept = step{byOwner: true, action: "accept"}
	ready  = step{byOwner: true, action: "ready"}
	reject = step{byOwner: true, action: "reject"}
	cancel = step{action: "cancel"}
)

// stories are the demo orders, one per status worth seeing. Each ends in its own
// status and leaves the money in its own state at the provider. Picked up and
// delivered need a driver, which comes in phase 3.
var stories = []struct {
	card  string
	steps []step
}{
	{cardApprove, nil},                    // paid: waiting in the kitchen queue, money held
	{cardApprove, []step{accept}},         // accepted: money taken (captured)
	{cardApprove, []step{accept, ready}},  // ready for pickup
	{cardApprove, []step{cancel}},         // cancelled by the customer: hold dropped (voided)
	{cardApprove, []step{accept, reject}}, // rejected after accepting: money given back (refunded)
	{cardDecline, nil},                    // cancelled, payment_failed
}

// PlacedOrder is one order the seeder placed, as the API last showed it.
type PlacedOrder struct {
	ID           string  `json:"id"`
	Status       string  `json:"status"`
	CancelReason *string `json:"cancel_reason"`
}

// OrderReport is what PlaceOrders did. Skipped means the customer already had
// orders, so nothing was placed.
type OrderReport struct {
	Skipped bool
	Orders  []PlacedOrder
}

// OrderSeeder places demo orders through the running API, exactly as the app
// would: log in, fill the cart, check out with a test card, then press the
// restaurant's and the customer's buttons. Going through the API (not the
// stores) means the orders, the payments at the provider, our payment ledger
// and the order events all agree, and nothing here copies checkout's logic.
type OrderSeeder struct {
	API      string // base URL, such as http://localhost:8080
	Client   *http.Client
	Password string // of both accounts
}

// PlaceOrders places one order per story for the plan's customer. Like the rest
// of the seed it only adds what's missing: a customer who already has orders is
// left alone (make seed-reset starts over).
func (s *OrderSeeder) PlaceOrders(ctx context.Context, plan OrderPlan) (OrderReport, error) {
	customer, err := s.login(ctx, plan.Customer)
	if err != nil {
		return OrderReport{}, err
	}
	var mine struct {
		Orders []PlacedOrder `json:"orders"`
	}
	if err := s.call(ctx, customer, "GET", "/orders", nil, http.StatusOK, &mine); err != nil {
		return OrderReport{}, err
	}
	if len(mine.Orders) > 0 {
		return OrderReport{Skipped: true}, nil
	}

	var addrs struct {
		Addresses []struct {
			ID string `json:"id"`
		} `json:"addresses"`
	}
	if err := s.call(ctx, customer, "GET", "/me/addresses", nil, http.StatusOK, &addrs); err != nil {
		return OrderReport{}, err
	}
	if len(addrs.Addresses) == 0 {
		return OrderReport{}, fmt.Errorf("%s has no delivery address", plan.Customer)
	}
	address := addrs.Addresses[0].ID

	owner, err := s.login(ctx, plan.Owner)
	if err != nil {
		return OrderReport{}, err
	}
	restaurantID, minBasket, err := s.findRestaurant(ctx, owner, plan)
	if err != nil {
		return OrderReport{}, err
	}
	dishes, err := s.simpleDishes(ctx, restaurantID)
	if err != nil {
		return OrderReport{}, err
	}

	var rep OrderReport
	for i, st := range stories {
		// A different dish for each order, enough of it to reach the minimum basket.
		d := dishes[i%len(dishes)]
		quantity := min(20, max(1, (minBasket+d.price-1)/d.price))
		if err := s.call(ctx, customer, "DELETE", "/cart", nil, http.StatusNoContent, nil); err != nil {
			return rep, err
		}
		item := map[string]any{"menu_item_id": d.id, "quantity": quantity, "choice_ids": []string{}}
		if err := s.call(ctx, customer, "POST", "/cart/items", item, http.StatusOK, nil); err != nil {
			return rep, err
		}
		var o PlacedOrder
		checkout := map[string]any{"address_id": address, "card_number": st.card}
		if err := s.call(ctx, customer, "POST", "/orders", checkout, http.StatusCreated, &o); err != nil {
			return rep, err
		}
		for _, p := range st.steps {
			token := customer
			if p.byOwner {
				token = owner
			}
			if err := s.call(ctx, token, "POST", "/orders/"+o.ID+"/"+p.action, nil, http.StatusOK, &o); err != nil {
				return rep, err
			}
		}
		rep.Orders = append(rep.Orders, o)
	}
	return rep, nil
}

type dish struct {
	id    string
	price int
}

// findRestaurant finds the plan's restaurant among the owner's.
func (s *OrderSeeder) findRestaurant(ctx context.Context, owner string, plan OrderPlan) (string, int, error) {
	var mine struct {
		Restaurants []struct {
			ID             string `json:"id"`
			Name           string `json:"name"`
			MinBasketKurus int    `json:"min_basket_kurus"`
		} `json:"restaurants"`
	}
	if err := s.call(ctx, owner, "GET", "/me/restaurants", nil, http.StatusOK, &mine); err != nil {
		return "", 0, err
	}
	for _, r := range mine.Restaurants {
		if r.Name == plan.Restaurant {
			return r.ID, r.MinBasketKurus, nil
		}
	}
	return "", 0, fmt.Errorf("%s owns no restaurant named %q", plan.Owner, plan.Restaurant)
}

// simpleDishes are the restaurant's dishes that can be ordered without picking
// any option, in menu order.
func (s *OrderSeeder) simpleDishes(ctx context.Context, restaurantID string) ([]dish, error) {
	var menu struct {
		Categories []struct {
			Items []struct {
				ID           string `json:"id"`
				PriceKurus   int    `json:"price_kurus"`
				Available    bool   `json:"available"`
				OptionGroups []struct {
					MinPicks int `json:"min_picks"`
				} `json:"option_groups"`
			} `json:"items"`
		} `json:"categories"`
	}
	if err := s.call(ctx, "", "GET", "/restaurants/"+restaurantID+"/menu", nil, http.StatusOK, &menu); err != nil {
		return nil, err
	}
	var out []dish
	for _, c := range menu.Categories {
	items:
		for _, it := range c.Items {
			if !it.Available || it.PriceKurus <= 0 {
				continue
			}
			for _, g := range it.OptionGroups {
				if g.MinPicks > 0 {
					continue items
				}
			}
			out = append(out, dish{id: it.ID, price: it.PriceKurus})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("restaurant %s has no dish that needs no picks", restaurantID)
	}
	return out, nil
}

func (s *OrderSeeder) login(ctx context.Context, email string) (string, error) {
	var res struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
	}
	body := map[string]string{"email": email, "password": s.Password}
	if err := s.call(ctx, "", "POST", "/auth/login", body, http.StatusOK, &res); err != nil {
		return "", err
	}
	return res.Tokens.AccessToken, nil
}

// call sends one request to the API and decodes the answer into out (if not
// nil). Any status but want is an error that shows the API's answer. POST
// /orders gets a fresh Idempotency-Key: each call is a new checkout.
func (s *OrderSeeder) call(ctx context.Context, token, method, path string, body any, want int, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(s.API, "/")+path, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if method == "POST" && path == "/orders" {
		req.Header.Set("Idempotency-Key", uuid.NewString())
	}
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w (is the API running? make up starts it)", method, path, err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	if res.StatusCode != want {
		return fmt.Errorf("%s %s: %d, want %d: %s", method, path, res.StatusCode, want, bytes.TrimSpace(b))
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

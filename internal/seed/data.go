// Package seed fills the dev database with Istanbul demo data: accounts for each
// profile, restaurants in real neighbourhoods, and their menus.
package seed

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/atalaymudanyali/quickbytes/internal/menus"
	"github.com/atalaymudanyali/quickbytes/internal/restaurants"
	"github.com/atalaymudanyali/quickbytes/internal/users"
)

// The data set is compiled into the program, so the seeder needs no files next
// to it at run time.
//
//go:embed istanbul.json
var istanbulJSON []byte

// EmailDomain is the domain every demo account uses. .test is reserved (RFC 2606),
// so these can never be real inboxes, and reset deletes exactly the users on it.
const EmailDomain = "demo.quickbytes.test"

type Data struct {
	Accounts    []Account    `json:"accounts"`
	Restaurants []Restaurant `json:"restaurants"`
}

type Account struct {
	Email    string `json:"email"`
	FullName string `json:"full_name"`
	// Every account gets a customer profile at sign-up. These are the others.
	ExtraRoles []users.Role `json:"extra_roles"`
}

type Restaurant struct {
	// Email of the account that owns it. That account needs a restaurant profile.
	Owner           string     `json:"owner"`
	Name            string     `json:"name"`
	Address         string     `json:"address"`
	Lat             float64    `json:"lat"`
	Lng             float64    `json:"lng"`
	DeliveryRadiusM int        `json:"delivery_radius_m"`
	AcceptingOrders bool       `json:"accepting_orders"`
	Categories      []Category `json:"categories"`
}

type Category struct {
	Name  string `json:"name"`
	Items []Item `json:"items"`
}

type Item struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	PriceKurus  int    `json:"price_kurus"`
	// Missing means available. A pointer tells "not written" apart from false.
	Available *bool `json:"available,omitempty"`
}

func (i Item) IsAvailable() bool { return i.Available == nil || *i.Available }

// Istanbul returns the built-in data set, already checked with Validate.
func Istanbul() (Data, error) {
	return Parse(istanbulJSON)
}

// Parse reads a data set and checks it. Unknown fields are an error, so a typo
// like "price_kuruş" fails loudly instead of silently becoming 0.
func Parse(b []byte) (Data, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var d Data
	if err := dec.Decode(&d); err != nil {
		return Data{}, fmt.Errorf("parse seed data: %w", err)
	}
	if err := d.Validate(); err != nil {
		return Data{}, err
	}
	return d, nil
}

// Validate checks the data against the same rules the API and the database
// enforce, and reports every problem at once instead of stopping at the first.
func (d Data) Validate() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	owners := map[string]bool{} // email → has a restaurant profile
	for _, a := range d.Accounts {
		if _, ok := owners[a.Email]; ok {
			bad("account %s: listed twice", a.Email)
		}
		if !strings.HasSuffix(a.Email, "@"+EmailDomain) {
			bad("account %s: email must end in @%s", a.Email, EmailDomain)
		}
		if !lengthBetween(a.FullName, 1, 100) {
			bad("account %s: full name must be 1 to 100 characters", a.Email)
		}
		owners[a.Email] = false
		for _, r := range a.ExtraRoles {
			if !r.Valid() || r == users.RoleCustomer {
				bad("account %s: extra role %q must be restaurant or driver", a.Email, r)
			}
			if r == users.RoleRestaurant {
				owners[a.Email] = true
			}
		}
	}

	names := map[string]bool{}
	for _, r := range d.Restaurants {
		where := "restaurant " + r.Name
		if names[r.Name] {
			bad("%s: name used twice", where)
		}
		names[r.Name] = true
		if !lengthBetween(r.Name, 1, 100) {
			bad("%s: name must be 1 to 100 characters", where)
		}
		if !lengthBetween(r.Address, 1, 300) {
			bad("%s: address must be 1 to 300 characters", where)
		}
		if !restaurants.InIstanbul(r.Lat, r.Lng) {
			bad("%s: %.4f,%.4f is outside Istanbul", where, r.Lat, r.Lng)
		}
		if r.DeliveryRadiusM < restaurants.MinDeliveryRadiusM || r.DeliveryRadiusM > restaurants.MaxDeliveryRadiusM {
			bad("%s: delivery radius must be %d to %d m", where,
				restaurants.MinDeliveryRadiusM, restaurants.MaxDeliveryRadiusM)
		}
		if isOwner, ok := owners[r.Owner]; !ok {
			bad("%s: owner %s is not in accounts", where, r.Owner)
		} else if !isOwner {
			bad("%s: owner %s has no restaurant profile", where, r.Owner)
		}
		if len(r.Categories) == 0 {
			bad("%s: has no menu", where)
		}

		categories, items := map[string]bool{}, map[string]bool{}
		for _, c := range r.Categories {
			if categories[c.Name] {
				bad("%s: category %s used twice", where, c.Name)
			}
			categories[c.Name] = true
			if !lengthBetween(c.Name, 1, 60) {
				bad("%s: category %q must be 1 to 60 characters", where, c.Name)
			}
			if len(c.Items) == 0 {
				bad("%s: category %s has no items", where, c.Name)
			}
			for _, it := range c.Items {
				// Unique within the whole restaurant, not just the category, so a
				// name alone finds an item when seeding again.
				if items[it.Name] {
					bad("%s: item %s used twice", where, it.Name)
				}
				items[it.Name] = true
				if !lengthBetween(it.Name, 1, 100) {
					bad("%s: item %q must be 1 to 100 characters", where, it.Name)
				}
				if utf8.RuneCountInString(it.Description) > 500 {
					bad("%s: item %s: description must be at most 500 characters", where, it.Name)
				}
				if it.PriceKurus < 1 || it.PriceKurus > menus.MaxPriceKurus {
					bad("%s: item %s: price must be 1 to %d kuruş", where, it.Name, menus.MaxPriceKurus)
				}
			}
		}
	}
	return errors.Join(errs...)
}

// lengthBetween counts characters, not bytes: "Çorbalar" is 8 characters but 9
// bytes, and the database's length() counts characters too.
func lengthBetween(s string, lo, hi int) bool {
	n := utf8.RuneCountInString(s)
	return n >= lo && n <= hi
}

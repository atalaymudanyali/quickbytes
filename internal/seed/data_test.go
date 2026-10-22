package seed_test

import (
	"strings"
	"testing"

	"github.com/atalaymudanyali/quickbytes/internal/seed"
	"github.com/atalaymudanyali/quickbytes/internal/users"
)

func TestIstanbulDataIsValid(t *testing.T) {
	d, err := seed.Istanbul()
	if err != nil {
		t.Fatal(err)
	}

	// The data is there to show features off, so check the showcase pieces exist.
	branches := map[string]int{}
	var paused, soldOut bool
	for _, r := range d.Restaurants {
		branches[r.Owner]++
		paused = paused || !r.AcceptingOrders
		for _, c := range r.Categories {
			for _, it := range c.Items {
				soldOut = soldOut || !it.IsAvailable()
			}
		}
	}
	var multiBranch bool
	for _, n := range branches {
		multiBranch = multiBranch || n >= 2
	}
	var driver, allProfiles bool
	for _, a := range d.Accounts {
		driver = driver || has(a.ExtraRoles, users.RoleDriver)
		allProfiles = allProfiles || (has(a.ExtraRoles, users.RoleDriver) && has(a.ExtraRoles, users.RoleRestaurant))
	}

	for what, ok := range map[string]bool{
		"a paused restaurant":            paused,
		"a sold-out item":                soldOut,
		"an owner with several branches": multiBranch,
		"a driver":                       driver,
		"an account with every profile":  allProfiles,
	} {
		if !ok {
			t.Errorf("data set has no %s", what)
		}
	}
}

func has(roles []users.Role, r users.Role) bool {
	for _, x := range roles {
		if x == r {
			return true
		}
	}
	return false
}

// A small valid data set that each case below breaks in one way.
const valid = `{
  "accounts": [
    {"email": "owner@demo.quickbytes.test", "full_name": "Owner", "extra_roles": ["restaurant"]},
    {"email": "eater@demo.quickbytes.test", "full_name": "Eater", "extra_roles": []}
  ],
  "restaurants": [{
    "owner": "owner@demo.quickbytes.test", "name": "Test Kebap", "address": "Moda Cd. 1",
    "lat": 40.98, "lng": 29.03, "delivery_radius_m": 3000, "accepting_orders": true,
    "categories": [{"name": "Kebaplar", "items": [
      {"name": "Adana", "description": "", "price_kurus": 45000}
    ]}]
  }]
}`

func TestParse(t *testing.T) {
	if _, err := seed.Parse([]byte(valid)); err != nil {
		t.Fatalf("valid data rejected: %v", err)
	}

	tests := []struct {
		name, from, to, wantErr string
	}{
		{"real email domain", "owner@demo.quickbytes.test\", \"full", "owner@gmail.com\", \"full", "must end in @demo.quickbytes.test"},
		{"unknown field", `"price_kurus"`, `"price_kuruş"`, "unknown field"},
		{"swapped lat and lng", `"lat": 40.98, "lng": 29.03`, `"lat": 29.03, "lng": 40.98`, "outside Istanbul"},
		{"zero price", `"price_kurus": 45000`, `"price_kurus": 0`, "price must be"},
		{"radius too small", `"delivery_radius_m": 3000`, `"delivery_radius_m": 100`, "delivery radius must be"},
		{"owner without restaurant profile", `"owner": "owner@`, `"owner": "eater@`, "has no restaurant profile"},
		{"unknown owner", `"owner": "owner@`, `"owner": "nobody@`, "not in accounts"},
		{"customer as extra role", `"extra_roles": []`, `"extra_roles": ["customer"]`, "must be restaurant or driver"},
		{"empty category", `"items": [
      {"name": "Adana", "description": "", "price_kurus": 45000}
    ]`, `"items": []`, "has no items"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(valid, tt.from) {
				t.Fatalf("test is broken: %q not in the valid data", tt.from)
			}
			_, err := seed.Parse([]byte(strings.Replace(valid, tt.from, tt.to, 1)))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

package seed_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/atalaymudanyali/quickbytes/internal/addresses"
	"github.com/atalaymudanyali/quickbytes/internal/auth"
	"github.com/atalaymudanyali/quickbytes/internal/menus"
	"github.com/atalaymudanyali/quickbytes/internal/restaurants"
	"github.com/atalaymudanyali/quickbytes/internal/seed"
	"github.com/atalaymudanyali/quickbytes/internal/testutil"
	"github.com/atalaymudanyali/quickbytes/internal/users"
)

const password = "test-demo-password"

// The real data set, seeded twice into the test database. The demo emails are
// fixed, so a database seeded by an earlier test run may already have them: the
// first run may then create nothing, and that's fine. What matters is that the
// second run never creates anything and finds the same restaurants.
func TestRunTwice(t *testing.T) {
	pool := testutil.DB(t)
	ctx := context.Background()
	data, err := seed.Istanbul()
	if err != nil {
		t.Fatal(err)
	}
	hasher := auth.NewHasher(auth.FastParams)
	userStore, restaurantStore, menuStore := users.NewStore(pool), restaurants.NewStore(pool), menus.NewStore(pool)
	s := &seed.Seeder{Users: userStore, Restaurants: restaurantStore, Menus: menuStore, Addresses: addresses.NewStore(pool), Hasher: hasher, Password: password}

	first, err := s.Run(ctx, data)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	second, err := s.Run(ctx, data)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}

	if second.Created != (seed.Counts{}) {
		t.Errorf("second run created %+v, want nothing", second.Created)
	}
	all := seed.Counts{Accounts: len(data.Accounts), Restaurants: len(data.Restaurants)}
	for _, a := range data.Accounts {
		all.Addresses += len(a.Addresses)
	}
	for _, r := range data.Restaurants {
		all.Categories += len(r.Categories)
		for _, c := range r.Categories {
			all.Items += len(c.Items)
			for _, it := range c.Items {
				all.OptionGroups += len(it.Options)
				for _, g := range it.Options {
					all.OptionChoices += len(g.Choices)
				}
			}
		}
	}
	if second.Existing != all {
		t.Errorf("second run found %+v, want everything: %+v", second.Existing, all)
	}
	for i := range first.Restaurants {
		if first.Restaurants[i].ID != second.Restaurants[i].ID {
			t.Errorf("%s: ID changed between runs", first.Restaurants[i].Name)
		}
	}

	t.Run("demo accounts can log in", func(t *testing.T) {
		u, err := userStore.GetByEmail(ctx, "demo@"+seed.EmailDomain)
		if err != nil {
			t.Fatal(err)
		}
		ok, err := hasher.Verify(password, u.PasswordHash)
		if err != nil || !ok {
			t.Fatalf("password doesn't verify: ok=%v err=%v", ok, err)
		}
		profiles, err := userStore.ListProfiles(ctx, u.ID)
		if err != nil || len(profiles) != 3 {
			t.Fatalf("demo@ has %d profiles (err %v), want all 3", len(profiles), err)
		}
	})

	byName := map[string]uuid.UUID{}
	for _, r := range second.Restaurants {
		byName[r.Name] = r.ID
	}

	t.Run("paused restaurant is paused", func(t *testing.T) {
		r, err := restaurantStore.Get(ctx, byName["Bahariye Lahmacun"])
		if err != nil {
			t.Fatal(err)
		}
		if r.AcceptingOrders {
			t.Error("Bahariye Lahmacun accepts orders, want paused")
		}
	})

	t.Run("sold-out item is sold out", func(t *testing.T) {
		m, err := menuStore.Menu(ctx, byName["Şişli Mantı Evi"], menus.View{})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range m.Categories {
			for _, it := range c.Items {
				if it.Name == "Hingel" && it.Available {
					t.Error("Hingel is available, want sold out")
				}
				if it.Name == "Kayseri Mantısı" && !it.Available {
					t.Error("Kayseri Mantısı is sold out, want available")
				}
			}
		}
	})
}

func TestSeededOptions(t *testing.T) {
	pool := testutil.DB(t)
	ctx := context.Background()
	data, err := seed.Istanbul()
	if err != nil {
		t.Fatal(err)
	}
	menuStore := menus.NewStore(pool)
	s := &seed.Seeder{Users: users.NewStore(pool), Restaurants: restaurants.NewStore(pool), Menus: menuStore,
		Addresses: addresses.NewStore(pool), Hasher: auth.NewHasher(auth.FastParams), Password: password}
	rep, err := s.Run(ctx, data)
	if err != nil {
		t.Fatal(err)
	}

	var pide menus.MenuItem
	for _, r := range rep.Restaurants {
		if r.Name != "Hasan Usta Kebap Kadıköy" {
			continue
		}
		m, err := menuStore.Menu(ctx, r.ID, menus.View{})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range m.Categories {
			for _, it := range c.Items {
				if it.Name == "Kıymalı Pide" {
					pide = it
				}
			}
		}
	}
	if len(pide.OptionGroups) != 1 || len(pide.OptionGroups[0].Choices) != 3 {
		t.Fatalf("Kıymalı Pide options = %+v, want Ekstralar with 3 choices", pide.OptionGroups)
	}
	extras := pide.OptionGroups[0]
	if extras.Name != "Ekstralar" || extras.MinPicks != 0 || extras.MaxPicks != 3 {
		t.Errorf("group = %+v", extras.OptionGroup)
	}
	for _, c := range extras.Choices {
		if c.Name == "Sucuk" && c.Available {
			t.Error("Sucuk is available, want switched off")
		}
		if c.Name == "Kaşar" && (!c.Available || c.ExtraPriceKurus != 4000) {
			t.Errorf("Kaşar = %+v, want available at 4000 kuruş", c)
		}
	}

	// The seeded rules are what the validator enforces.
	var kasar, sucuk uuid.UUID
	for _, c := range extras.Choices {
		switch c.Name {
		case "Kaşar":
			kasar = c.ID
		case "Sucuk":
			sucuk = c.ID
		}
	}
	if sel, err := menus.ValidateChoices(pide, []uuid.UUID{kasar}); err != nil || sel.ExtraKurus != 4000 {
		t.Errorf("Kaşar: sel %+v, err %v; want valid, +4000", sel, err)
	}
	if _, err := menus.ValidateChoices(pide, []uuid.UUID{sucuk}); err == nil {
		t.Error("Sucuk was accepted, but it's switched off")
	}
}

func TestCheckLocal(t *testing.T) {
	tests := []struct {
		url string
		ok  bool
	}{
		{"postgres://u:p@localhost:5432/quickbytes", true},
		{"postgres://u:p@127.0.0.1/quickbytes", true},
		{"postgres://u:p@postgres:5432/quickbytes", true},
		{"postgres://u:p@db.example.com:5432/quickbytes", false},
		{"postgres://u:p@10.0.0.5/quickbytes", false},
		{"host=localhost,db.example.com user=u dbname=quickbytes", false},
	}
	for _, tt := range tests {
		err := seed.CheckLocal(tt.url)
		if (err == nil) != tt.ok {
			t.Errorf("CheckLocal(%q) = %v, want ok=%v", tt.url, err, tt.ok)
		}
	}
}

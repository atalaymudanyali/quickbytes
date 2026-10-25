package seed

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/atalaymudanyali/quickbytes/internal/addresses"
	"github.com/atalaymudanyali/quickbytes/internal/auth"
	"github.com/atalaymudanyali/quickbytes/internal/menus"
	"github.com/atalaymudanyali/quickbytes/internal/restaurants"
	"github.com/atalaymudanyali/quickbytes/internal/users"
)

// Seeder writes a data set through the same stores the API uses, so the data
// goes through the same rules: hashed passwords, ownership checks, menu versions.
type Seeder struct {
	Users       *users.Store
	Restaurants *restaurants.Store
	Menus       *menus.Store
	Addresses   *addresses.Store
	Hasher      *auth.Hasher
	// Password is given to every account the seeder creates.
	Password string
}

// Counts says how many things of each kind a run created, or found already there.
type Counts struct {
	Accounts, Addresses, Restaurants, Categories, Items, OptionGroups, OptionChoices int
}

type Report struct {
	Created, Existing Counts
	Restaurants       []restaurants.Restaurant
}

// Run creates whatever in d is missing and leaves the rest alone, so running it
// again changes nothing and every ID stays the same. Things are found by their
// natural key: accounts by email, addresses by label within their account, restaurants by owner and name, categories and
// items by name within their restaurant, option groups by name within their item,
// and choices by name within their group.
func (s *Seeder) Run(ctx context.Context, d Data) (Report, error) {
	var rep Report
	if err := d.Validate(); err != nil {
		return rep, err
	}
	if n := utf8.RuneCountInString(s.Password); n < 8 || n > 128 {
		return rep, errors.New("demo password must be 8 to 128 characters, like any other password")
	}

	owners := map[string]uuid.UUID{} // email → restaurant profile ID
	for _, a := range d.Accounts {
		u, err := s.account(ctx, a, &rep)
		if err != nil {
			return rep, fmt.Errorf("account %s: %w", a.Email, err)
		}
		for _, role := range a.ExtraRoles {
			p, err := s.profile(ctx, u.ID, role)
			if err != nil {
				return rep, fmt.Errorf("account %s: %w", a.Email, err)
			}
			if role == users.RoleRestaurant {
				owners[a.Email] = p.ID
			}
		}
		if err := s.addresses(ctx, u.ID, a.Addresses, &rep); err != nil {
			return rep, fmt.Errorf("account %s: %w", a.Email, err)
		}
	}

	for _, r := range d.Restaurants {
		owner := owners[r.Owner]
		got, err := s.restaurant(ctx, owner, r, &rep)
		if err != nil {
			return rep, fmt.Errorf("restaurant %s: %w", r.Name, err)
		}
		if err := s.menu(ctx, got.ID, owner, r.Categories, &rep); err != nil {
			return rep, fmt.Errorf("restaurant %s: %w", r.Name, err)
		}
		rep.Restaurants = append(rep.Restaurants, got)
	}
	return rep, nil
}

func (s *Seeder) account(ctx context.Context, a Account, rep *Report) (users.User, error) {
	u, err := s.Users.GetByEmail(ctx, a.Email)
	if err == nil {
		rep.Existing.Accounts++
		return u, nil
	}
	if !errors.Is(err, users.ErrNotFound) {
		return users.User{}, err
	}
	hash, err := s.Hasher.Hash(s.Password)
	if err != nil {
		return users.User{}, err
	}
	u, _, err = s.Users.Create(ctx, a.Email, hash, a.FullName)
	if err != nil {
		return users.User{}, err
	}
	rep.Created.Accounts++
	return u, nil
}

func (s *Seeder) profile(ctx context.Context, userID uuid.UUID, role users.Role) (users.Profile, error) {
	p, err := s.Users.ProfileByRole(ctx, userID, role)
	if errors.Is(err, users.ErrNotFound) {
		return s.Users.AddProfile(ctx, userID, role)
	}
	return p, err
}

// addresses saves the account's missing delivery addresses on its customer
// profile, the one every account gets at sign-up.
func (s *Seeder) addresses(ctx context.Context, userID uuid.UUID, want []Address, rep *Report) error {
	if len(want) == 0 {
		return nil
	}
	customer, err := s.Users.ProfileByRole(ctx, userID, users.RoleCustomer)
	if err != nil {
		return err
	}
	saved, err := s.Addresses.List(ctx, customer.ID)
	if err != nil {
		return err
	}
	labels := map[string]bool{}
	for _, a := range saved {
		labels[a.Label] = true
	}
	for _, a := range want {
		if labels[a.Label] {
			rep.Existing.Addresses++
			continue
		}
		_, err := s.Addresses.Create(ctx, customer.ID, addresses.New{
			Label: a.Label, AddressLine: a.AddressLine, Details: a.Details, Lat: a.Lat, Lng: a.Lng,
		})
		if err != nil {
			return fmt.Errorf("address %s: %w", a.Label, err)
		}
		rep.Created.Addresses++
	}
	return nil
}

func (s *Seeder) restaurant(ctx context.Context, owner uuid.UUID, r Restaurant, rep *Report) (restaurants.Restaurant, error) {
	mine, err := s.Restaurants.ListByOwner(ctx, owner)
	if err != nil {
		return restaurants.Restaurant{}, err
	}
	for _, m := range mine {
		if m.Name == r.Name {
			rep.Existing.Restaurants++
			return m, nil
		}
	}

	got, err := s.Restaurants.Create(ctx, owner, restaurants.New{
		Name: r.Name, Address: r.Address, Lat: r.Lat, Lng: r.Lng, DeliveryRadiusM: r.DeliveryRadiusM,
		DeliveryFeeKurus: r.DeliveryFeeKurus, MinBasketKurus: r.MinBasketKurus,
	})
	if err != nil {
		return restaurants.Restaurant{}, err
	}
	// New restaurants accept orders; a paused one is switched off right after.
	if !r.AcceptingOrders {
		off := false
		got, err = s.Restaurants.Update(ctx, got.ID, owner, restaurants.Patch{AcceptingOrders: &off})
		if err != nil {
			return restaurants.Restaurant{}, err
		}
	}
	rep.Created.Restaurants++
	return got, nil
}

func (s *Seeder) menu(ctx context.Context, restaurantID, owner uuid.UUID, want []Category, rep *Report) error {
	// The owner's view includes archived categories and items, so something the
	// owner archived counts as "already there" and isn't created a second time.
	current, err := s.Menus.Menu(ctx, restaurantID, menus.View{OwnerProfileID: owner})
	if err != nil {
		return err
	}
	categories, items := map[string]uuid.UUID{}, map[string]menus.MenuItem{}
	for _, c := range current.Categories {
		categories[c.Name] = c.ID
		for _, it := range c.Items {
			items[it.Name] = it
		}
	}

	for _, c := range want {
		id, ok := categories[c.Name]
		if ok {
			rep.Existing.Categories++
		} else {
			created, err := s.Menus.CreateCategory(ctx, restaurantID, owner, c.Name)
			if err != nil {
				return fmt.Errorf("category %s: %w", c.Name, err)
			}
			id = created.ID
			rep.Created.Categories++
		}

		for _, it := range c.Items {
			existing, ok := items[it.Name]
			if ok {
				rep.Existing.Items++
			} else {
				created, err := s.Menus.CreateItem(ctx, restaurantID, owner, menus.NewItem{
					CategoryID: id, Name: it.Name, Description: it.Description, PriceKurus: it.PriceKurus,
				})
				if err != nil {
					return fmt.Errorf("item %s: %w", it.Name, err)
				}
				if !it.IsAvailable() {
					off := false
					_, err = s.Menus.UpdateItem(ctx, restaurantID, owner, created.ID, menus.ItemPatch{Available: &off})
					if err != nil {
						return fmt.Errorf("item %s: %w", it.Name, err)
					}
				}
				existing = menus.MenuItem{Item: created}
				rep.Created.Items++
			}
			// Also for items that were already there: a data set that gained
			// options gets them added on the next plain `make seed`.
			if err := s.options(ctx, restaurantID, owner, existing, it.Options, rep); err != nil {
				return fmt.Errorf("item %s: %w", it.Name, err)
			}
		}
	}
	return nil
}

// options creates the item's missing option groups and choices. Like items,
// what's already there is left as it is, even if the owner changed it since.
func (s *Seeder) options(ctx context.Context, restaurantID, owner uuid.UUID, item menus.MenuItem, want []OptionGroup, rep *Report) error {
	groups := map[string]menus.MenuOptionGroup{}
	for _, g := range item.OptionGroups {
		groups[g.Name] = g
	}

	for _, g := range want {
		group, ok := groups[g.Name]
		if ok {
			rep.Existing.OptionGroups++
		} else {
			created, err := s.Menus.CreateOptionGroup(ctx, restaurantID, owner, item.ID, menus.NewOptionGroup{
				Name: g.Name, MinPicks: g.MinPicks, MaxPicks: g.MaxPicks,
			})
			if err != nil {
				return fmt.Errorf("option group %s: %w", g.Name, err)
			}
			group = menus.MenuOptionGroup{OptionGroup: created}
			rep.Created.OptionGroups++
		}

		choices := map[string]bool{}
		for _, c := range group.Choices {
			choices[c.Name] = true
		}
		for _, c := range g.Choices {
			if choices[c.Name] {
				rep.Existing.OptionChoices++
				continue
			}
			created, err := s.Menus.CreateOptionChoice(ctx, restaurantID, owner, group.ID, menus.NewOptionChoice{
				Name: c.Name, ExtraPriceKurus: c.ExtraPriceKurus,
			})
			if err != nil {
				return fmt.Errorf("option group %s: choice %s: %w", g.Name, c.Name, err)
			}
			if !c.IsAvailable() {
				off := false
				_, err = s.Menus.UpdateOptionChoice(ctx, restaurantID, owner, created.ID, menus.OptionChoicePatch{Available: &off})
				if err != nil {
					return fmt.Errorf("option group %s: choice %s: %w", g.Name, c.Name, err)
				}
			}
			rep.Created.OptionChoices++
		}
	}
	return nil
}

// localHosts are the only database hosts the seeder writes to: this machine, or
// the postgres container from inside Docker.
var localHosts = map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true, "postgres": true}

// CheckLocal refuses any database that isn't the local dev one. Demo accounts
// with a published password must never end up on a real server, so there is no
// override.
func CheckLocal(databaseURL string) error {
	cfg, err := pgconn.ParseConfig(databaseURL)
	if err != nil {
		return fmt.Errorf("parse database url: %w", err)
	}
	// A URL can list several hosts to try in turn ("host=a,b"); all must be local.
	hosts := []string{cfg.Host}
	for _, f := range cfg.Fallbacks {
		hosts = append(hosts, f.Host)
	}
	for _, h := range hosts {
		if !localHosts[h] {
			return fmt.Errorf("refusing to seed %q: demo data only goes into a local database (localhost or the postgres container)", h)
		}
	}
	return nil
}

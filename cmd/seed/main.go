// Command seed fills the local dev database with the Istanbul demo data. Run it
// with `make seed`. Running it again only adds what's missing. With -reset
// (`make seed-reset`) it first deletes all demo data, then seeds it fresh.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/redis/go-redis/v9"

	"github.com/atalaymudanyali/quickbytes/internal/auth"
	"github.com/atalaymudanyali/quickbytes/internal/cache"
	"github.com/atalaymudanyali/quickbytes/internal/database"
	"github.com/atalaymudanyali/quickbytes/internal/menus"
	"github.com/atalaymudanyali/quickbytes/internal/restaurants"
	"github.com/atalaymudanyali/quickbytes/internal/seed"
	"github.com/atalaymudanyali/quickbytes/internal/users"
)

func main() {
	reset := flag.Bool("reset", false, "delete all demo accounts (and their restaurants and menus) before seeding")
	flag.Parse()
	if err := run(context.Background(), os.Stdout, *reset); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, out io.Writer, reset bool) error {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	// Checked before connecting, so a wrong URL never even gets a connection.
	if err := seed.CheckLocal(dbURL); err != nil {
		return err
	}
	password := os.Getenv("SEED_PASSWORD")
	if password == "" {
		return fmt.Errorf("SEED_PASSWORD is required (make seed sets the documented demo password)")
	}

	data, err := seed.Istanbul()
	if err != nil {
		return err
	}

	pool, err := database.Connect(ctx, dbURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	// The seeder may run before the API ever has, on a brand-new database.
	if err := database.Migrate(ctx, pool); err != nil {
		return err
	}

	// With Redis, the menu store tells the cache about every change, exactly as in
	// the API, so a menu cached before seeding is dropped when seeding adds to it.
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	var redisClient *redis.Client
	if url := os.Getenv("REDIS_URL"); url != "" {
		redisClient, err = cache.Open(url)
		if err != nil {
			return err
		}
		defer redisClient.Close()
	}
	menuStore := menus.NewStore(pool)
	menuCache := menus.NewCache(menuStore, redisClient, menus.DefaultTTL, logger)

	if reset {
		n, err := seed.Reset(ctx, pool, menuCache)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Deleted %d demo accounts, with their restaurants and menus.\n\n", n)
	}

	s := &seed.Seeder{
		Users:       users.NewStore(pool),
		Restaurants: restaurants.NewStore(pool),
		Menus:       menuStore,
		Hasher:      auth.NewHasher(auth.DefaultParams),
		Password:    password,
	}
	rep, err := s.Run(ctx, data)
	if err != nil {
		return err
	}
	printReport(out, data, rep)
	return nil
}

func printReport(out io.Writer, d seed.Data, rep seed.Report) {
	c, e := rep.Created, rep.Existing
	fmt.Fprintf(out, "Seeded Istanbul demo data.\n\n")
	fmt.Fprintf(out, "               created  already there\n")
	fmt.Fprintf(out, "  accounts     %7d  %13d\n", c.Accounts, e.Accounts)
	fmt.Fprintf(out, "  restaurants  %7d  %13d\n", c.Restaurants, e.Restaurants)
	fmt.Fprintf(out, "  categories   %7d  %13d\n", c.Categories, e.Categories)
	fmt.Fprintf(out, "  items        %7d  %13d\n", c.Items, e.Items)
	fmt.Fprintf(out, "  option groups %6d  %13d\n", c.OptionGroups, e.OptionGroups)
	fmt.Fprintf(out, "  choices      %7d  %13d\n\n", c.OptionChoices, e.OptionChoices)

	fmt.Fprintf(out, "Demo accounts (password: SEED_PASSWORD, see the README):\n")
	for _, a := range d.Accounts {
		roles := "customer"
		for _, r := range a.ExtraRoles {
			roles += ", " + string(r)
		}
		fmt.Fprintf(out, "  %-30s %s\n", a.Email, roles)
	}

	fmt.Fprintf(out, "\nRestaurants:\n")
	for _, r := range rep.Restaurants {
		note := ""
		if !r.AcceptingOrders {
			note = "  (paused)"
		}
		fmt.Fprintf(out, "  %s  %s%s\n", r.ID, r.Name, note)
	}

	fmt.Fprintf(out, "\nTry it (Swagger UI at http://localhost:8080/docs):\n")
	fmt.Fprintf(out, "  near Moda:        curl 'http://localhost:8080/restaurants?lat=40.9810&lng=29.0270'\n")
	if len(rep.Restaurants) > 0 {
		fmt.Fprintf(out, "  a menu:           curl -i http://localhost:8080/restaurants/%s/menu\n", rep.Restaurants[0].ID)
	}
}

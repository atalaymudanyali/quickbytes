package seed_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/atalaymudanyali/quickbytes/internal/addresses"
	"github.com/atalaymudanyali/quickbytes/internal/auth"
	"github.com/atalaymudanyali/quickbytes/internal/carts"
	"github.com/atalaymudanyali/quickbytes/internal/httpapi"
	"github.com/atalaymudanyali/quickbytes/internal/idempotency"
	"github.com/atalaymudanyali/quickbytes/internal/menus"
	"github.com/atalaymudanyali/quickbytes/internal/orders"
	"github.com/atalaymudanyali/quickbytes/internal/payments"
	"github.com/atalaymudanyali/quickbytes/internal/payments/paymentstest"
	"github.com/atalaymudanyali/quickbytes/internal/restaurants"
	"github.com/atalaymudanyali/quickbytes/internal/seed"
	"github.com/atalaymudanyali/quickbytes/internal/testutil"
	"github.com/atalaymudanyali/quickbytes/internal/users"
)

// The demo orders, placed through a real API (over HTTP, on the test database)
// with the fake provider. The customer is a fresh account each run, since the
// shared test database may already hold orders for the demo customer; the
// restaurant is the real seeded one.
func TestPlaceOrders(t *testing.T) {
	pool := testutil.DB(t)
	ctx := context.Background()
	data, err := seed.Istanbul()
	if err != nil {
		t.Fatal(err)
	}
	s := &seed.Seeder{
		Users: users.NewStore(pool), Restaurants: restaurants.NewStore(pool), Menus: menus.NewStore(pool),
		Addresses: addresses.NewStore(pool), Hasher: auth.NewHasher(auth.FastParams), Password: password,
	}
	if _, err := s.Run(ctx, data); err != nil {
		t.Fatal(err)
	}

	provider := paymentstest.NewFake()
	api := httptest.NewServer(newAPI(t, provider))
	t.Cleanup(api.Close)
	o := &seed.OrderSeeder{API: api.URL, Client: api.Client(), Password: password}

	// A fresh customer with one address.
	email := testutil.UniqueEmail()
	signup := `{"email": "` + email + `", "password": "` + password + `", "full_name": "Test Müşteri"}`
	res, err := http.Post(api.URL+"/auth/signup", "application/json", strings.NewReader(signup))
	if err != nil || res.StatusCode != http.StatusCreated {
		t.Fatalf("signup: %v %v", res.Status, err)
	}
	res.Body.Close()
	token := login(t, api.URL, email)
	req, _ := http.NewRequest("POST", api.URL+"/me/addresses", strings.NewReader(
		`{"label": "Ev", "address_line": "Moda Cd. No:1, Kadıköy", "lat": 40.987, "lng": 29.025}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	if res, err = http.DefaultClient.Do(req); err != nil || res.StatusCode != http.StatusCreated {
		t.Fatalf("add address: %v %v", res.Status, err)
	}
	res.Body.Close()

	plan := seed.DemoOrders
	plan.Customer = email
	rep, err := o.PlaceOrders(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"paid", "accepted", "ready", "cancelled (by_customer)", "cancelled (rejected)", "cancelled (payment_failed)"}
	var got []string
	for _, p := range rep.Orders {
		s := p.Status
		if p.CancelReason != nil {
			s += " (" + *p.CancelReason + ")"
		}
		got = append(got, s)
	}
	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Errorf("statuses = %v, want %v", got, want)
	}

	// The money followed each order at the provider: one hold per order, taken
	// for the two accepted ones and the rejected one (later given back), dropped
	// for the cancelled one.
	for call, n := range map[string]int{"authorize": 6, "capture": 3, "void": 1, "refund": 1} {
		if c := provider.Calls(call); c != n {
			t.Errorf("provider %s calls = %d, want %d", call, c, n)
		}
	}
	for _, p := range rep.Orders {
		if n := len(provider.Payments(p.ID)); n != 1 {
			t.Errorf("order %s has %d payments at the provider, want 1", p.ID, n)
		}
	}

	// Running it again adds nothing.
	again, err := o.PlaceOrders(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Skipped || len(again.Orders) != 0 {
		t.Errorf("second run = %+v, want skipped", again)
	}
}

func login(t *testing.T, base, email string) string {
	t.Helper()
	res, err := http.Post(base+"/auth/login", "application/json",
		strings.NewReader(`{"email": "`+email+`", "password": "`+password+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	i := strings.Index(string(b), `"access_token":"`)
	if i < 0 {
		t.Fatalf("login %s: %s", email, b)
	}
	rest := string(b)[i+len(`"access_token":"`):]
	return rest[:strings.Index(rest, `"`)]
}

// newAPI is the whole API on the test database, wired like cmd/api (no Redis,
// no rate limits).
func newAPI(t *testing.T, provider payments.Provider) http.Handler {
	t.Helper()
	pool := testutil.DB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tokens, err := auth.NewTokenIssuer([]byte(strings.Repeat("k", 32)), 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	menuStore := menus.NewStore(pool)
	menuCache := menus.NewCache(menuStore, nil, time.Minute, logger)
	restaurantStore := restaurants.NewStore(pool)
	addressStore := addresses.NewStore(pool)
	cartStore := carts.NewStore(pool, menuCache, restaurantStore)
	orderStore := orders.NewStore(pool, cartStore, addressStore)
	paymentStore := payments.NewStore(pool, provider, logger)
	orderStore.OnMove(paymentStore.MarkDueIn)
	return httpapi.NewServer(httpapi.Deps{
		DB:          pool,
		Users:       users.NewStore(pool),
		Hasher:      auth.NewHasher(auth.FastParams),
		Tokens:      tokens,
		Refresh:     auth.NewRefreshStore(pool, time.Hour),
		Restaurants: restaurantStore,
		Menus:       menuStore,
		Addresses:   addressStore,
		Carts:       cartStore,
		Orders:      orderStore,
		Payments:    paymentStore,
		Idempotency: idempotency.NewStore(pool),
		MenuCache:   menuCache,
		Logger:      logger,
	}).Routes()
}

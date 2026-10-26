package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/atalaymudanyali/quickbytes/internal/addresses"
	"github.com/atalaymudanyali/quickbytes/internal/auth"
	"github.com/atalaymudanyali/quickbytes/internal/carts"
	"github.com/atalaymudanyali/quickbytes/internal/idempotency"
	"github.com/atalaymudanyali/quickbytes/internal/menus"
	"github.com/atalaymudanyali/quickbytes/internal/orders"
	"github.com/atalaymudanyali/quickbytes/internal/restaurants"
	"github.com/atalaymudanyali/quickbytes/internal/testutil"
	"github.com/atalaymudanyali/quickbytes/internal/users"
)

func newAuthTestHandler(t *testing.T) http.Handler {
	t.Helper()
	_, h := newAuthTestServer(t)
	return h
}

// newAuthTestServer returns the server too, for tests that wrap its middleware
// around their own handlers.
func newAuthTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	return newTestServerWithRedis(t, testutil.RedisIfConfigured(t))
}

// newTestServerWithRedis is newAuthTestServer with a chosen Redis (nil: no cache),
// for tests that must cover both cases.
func newTestServerWithRedis(t *testing.T, rdb *redis.Client) (*Server, http.Handler) {
	t.Helper()
	pool := testutil.DB(t)
	tokens, err := auth.NewTokenIssuer([]byte(strings.Repeat("k", 32)), 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	menuStore := menus.NewStore(pool)
	menuCache := menus.NewCache(menuStore, rdb, time.Minute, discardLogger())
	restaurantStore := restaurants.NewStore(pool)
	addressStore := addresses.NewStore(pool)
	cartStore := carts.NewStore(pool, menuCache, restaurantStore)
	srv := NewServer(Deps{
		DB:          pool,
		Users:       users.NewStore(pool),
		Hasher:      auth.NewHasher(auth.FastParams),
		Tokens:      tokens,
		Refresh:     auth.NewRefreshStore(pool, time.Hour),
		Restaurants: restaurantStore,
		Menus:       menuStore,
		Addresses:   addressStore,
		Carts:       cartStore,
		Orders:      orders.NewStore(pool, cartStore, addressStore),
		Idempotency: idempotency.NewStore(pool),
		MenuCache:   menuCache,
		Logger:      discardLogger(),
	})
	return srv, validatedBySpec(t, srv.Routes())
}

func postJSON(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return do(t, h, req)
}

func signupBody(email, password string) string {
	b, _ := json.Marshal(signupRequest{Email: email, Password: password, FullName: "Zeynep Kaya"})
	return string(b)
}

func TestSignup(t *testing.T) {
	h := newAuthTestHandler(t)
	email := testutil.UniqueEmail()

	rec := postJSON(t, h, "/auth/signup", signupBody(email, "güvenli-şifre"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if strings.Contains(body, "password") || strings.Contains(body, "argon2") {
		t.Fatalf("response leaks the password or its hash: %s", body)
	}
	var got accountResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.User.Email != email || len(got.Profiles) != 1 || got.Profiles[0].Role != users.RoleCustomer {
		t.Fatalf("response = %+v, want the new user with a customer profile", got)
	}

	t.Run("same email again", func(t *testing.T) {
		rec := postJSON(t, h, "/auth/signup", signupBody(strings.ToUpper(email), "another-password"))
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", rec.Code)
		}
	})
}

func TestSignupValidation(t *testing.T) {
	h := newAuthTestHandler(t)

	rec := postJSON(t, h, "/auth/signup", `{"email": "Ali <ali@example.com>", "password": "short", "full_name": "  "}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	got := decodeError(t, rec)
	for _, field := range []string{"email", "password", "full_name"} {
		if got.Fields[field] == "" {
			t.Errorf("no error reported for field %q; fields = %v", field, got.Fields)
		}
	}
}

func TestSignupRejectsBadJSON(t *testing.T) {
	h := newAuthTestHandler(t)
	for name, body := range map[string]string{
		"not json":      `email=a`,
		"unknown field": `{"email": "a@b.co", "password": "12345678", "full_name": "A", "is_admin": true}`,
		"two objects":   `{"email": "a@b.co"} {"email": "c@d.co"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if rec := postJSON(t, h, "/auth/signup", body); rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
		})
	}
}

func TestLogin(t *testing.T) {
	h := newAuthTestHandler(t)
	email := testutil.UniqueEmail()
	if rec := postJSON(t, h, "/auth/signup", signupBody(email, "correct-password")); rec.Code != http.StatusCreated {
		t.Fatalf("signup failed: %d %s", rec.Code, rec.Body)
	}

	t.Run("right password", func(t *testing.T) {
		rec := postJSON(t, h, "/auth/login", `{"email": "`+strings.ToUpper(email)+`", "password": "correct-password"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
		}
	})

	// Both failures must look identical, so they can't be told apart.
	wrongPassword := postJSON(t, h, "/auth/login", `{"email": "`+email+`", "password": "wrong-password"}`)
	unknownEmail := postJSON(t, h, "/auth/login", `{"email": "`+testutil.UniqueEmail()+`", "password": "wrong-password"}`)
	for name, rec := range map[string]*httptest.ResponseRecorder{"wrong password": wrongPassword, "unknown email": unknownEmail} {
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, rec.Code)
		}
	}
	a, b := decodeError(t, wrongPassword), decodeError(t, unknownEmail)
	if a.Code != b.Code || a.Message != b.Message {
		t.Fatalf("failures differ: %+v vs %+v", a, b)
	}
}

func TestBodiesMustBeMarkedAsJSON(t *testing.T) {
	h := newAuthTestHandler(t)
	body := signupBody(testutil.UniqueEmail(), "correct-password")

	for name, contentType := range map[string]string{
		"no header":  "",
		"form":       "application/x-www-form-urlencoded",
		"plain text": "text/plain",
		"broken":     "application/",
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/auth/signup", strings.NewReader(body))
			if contentType != "" {
				req.Header.Set("Content-Type", contentType)
			}
			rec := do(t, h, req)
			if rec.Code != http.StatusUnsupportedMediaType || decodeError(t, rec).Code != "unsupported_media_type" {
				t.Fatalf("status = %d, want 415 unsupported_media_type", rec.Code)
			}
		})
	}

	t.Run("JSON with a charset is fine", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/auth/signup", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		if rec := do(t, h, req); rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201", rec.Code)
		}
	})
}

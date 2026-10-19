package httpapi

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/atalaymudanyali/quickbytes/internal/restaurants"
)

const ciyaBody = `{"name": "Çiya Sofrası", "address": "Güneşli Bahçe Sk. No:43, Kadıköy", "lat": 40.9906, "lng": 29.0262}`

// ownerToken signs up a new user, adds a restaurant profile and refreshes, so the
// returned access token lists the restaurant role.
func ownerToken(t *testing.T, h http.Handler) string {
	t.Helper()
	acc := signUp(t, h)
	if rec := addProfile(t, h, acc.Tokens.AccessToken, "restaurant"); rec.Code != http.StatusCreated {
		t.Fatalf("add restaurant profile: %d %s", rec.Code, rec.Body)
	}
	return refreshTokens(t, h, acc.Tokens.RefreshToken).AccessToken
}

func send(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return do(t, h, req)
}

func createRestaurant(t *testing.T, h http.Handler, token, body string) restaurants.Restaurant {
	t.Helper()
	rec := send(t, h, http.MethodPost, "/restaurants", token, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create restaurant: %d %s", rec.Code, rec.Body)
	}
	var r restaurants.Restaurant
	_ = json.Unmarshal(rec.Body.Bytes(), &r)
	return r
}

func TestCreateRestaurant(t *testing.T) {
	h := newAuthTestHandler(t)

	t.Run("customer can't", func(t *testing.T) {
		customer := signUp(t, h).Tokens.AccessToken
		if rec := send(t, h, http.MethodPost, "/restaurants", customer, ciyaBody); rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("owner can, with defaults", func(t *testing.T) {
		r := createRestaurant(t, h, ownerToken(t, h), ciyaBody)
		if r.DeliveryRadiusM != restaurants.DefaultDeliveryRadiusM || !r.AcceptingOrders {
			t.Fatalf("got radius %d, accepting %v; want the defaults", r.DeliveryRadiusM, r.AcceptingOrders)
		}
	})

	t.Run("invalid fields all reported", func(t *testing.T) {
		rec := send(t, h, http.MethodPost, "/restaurants", ownerToken(t, h),
			`{"name": " ", "lat": 29.0262, "lng": 40.9906, "delivery_radius_m": 50000}`)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422", rec.Code)
		}
		got := decodeError(t, rec).Fields
		for _, f := range []string{"name", "address", "location", "delivery_radius_m"} {
			if got[f] == "" {
				t.Errorf("no error for %q; fields = %v", f, got)
			}
		}
	})
}

func TestGetRestaurantIsPublic(t *testing.T) {
	h := newAuthTestHandler(t)
	r := createRestaurant(t, h, ownerToken(t, h), ciyaBody)

	if rec := send(t, h, http.MethodGet, "/restaurants/"+r.ID.String(), "", ""); rec.Code != http.StatusOK {
		t.Fatalf("without login: status = %d, want 200", rec.Code)
	}
	for _, id := range []string{"01a14bab-224e-7468-8920-42d59bbb55ee", "not-a-uuid"} {
		if rec := send(t, h, http.MethodGet, "/restaurants/"+id, "", ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want 404", id, rec.Code)
		}
	}
}

func TestUpdateRestaurant(t *testing.T) {
	h := newAuthTestHandler(t)
	owner := ownerToken(t, h)
	r := createRestaurant(t, h, owner, ciyaBody)
	path := "/restaurants/" + r.ID.String()

	t.Run("renaming leaves accepting_orders alone", func(t *testing.T) {
		rec := send(t, h, http.MethodPatch, path, owner, `{"name": "Çiya Kebap"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d; body: %s", rec.Code, rec.Body)
		}
		var got restaurants.Restaurant
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		if got.Name != "Çiya Kebap" || !got.AcceptingOrders {
			t.Fatalf("got name %q, accepting %v; want renamed and still accepting", got.Name, got.AcceptingOrders)
		}
	})

	t.Run("another owner gets the same 404 as a missing restaurant", func(t *testing.T) {
		stranger := send(t, h, http.MethodPatch, path, ownerToken(t, h), `{"accepting_orders": false}`)
		missing := send(t, h, http.MethodPatch, "/restaurants/01a14bab-224e-7468-8920-42d59bbb55ee", owner, `{"accepting_orders": false}`)
		if stranger.Code != http.StatusNotFound || missing.Code != http.StatusNotFound {
			t.Fatalf("statuses = %d and %d, want 404 for both", stranger.Code, missing.Code)
		}
		if a, b := decodeError(t, stranger), decodeError(t, missing); a.Code != b.Code || a.Message != b.Message {
			t.Fatalf("responses differ: %+v vs %+v", a, b)
		}
	})

	for name, body := range map[string]string{
		"lat without lng": `{"lat": 41.0}`,
		"empty patch":     `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			if rec := send(t, h, http.MethodPatch, path, owner, body); rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422", rec.Code)
			}
		})
	}
}

func TestMyRestaurants(t *testing.T) {
	h := newAuthTestHandler(t)
	owner := ownerToken(t, h)
	createRestaurant(t, h, owner, ciyaBody)
	createRestaurant(t, h, owner, `{"name": "Çiya Moda", "address": "Moda Cd., Kadıköy", "lat": 40.9810, "lng": 29.0270}`)
	createRestaurant(t, h, ownerToken(t, h), ciyaBody) // someone else's

	rec := send(t, h, http.MethodGet, "/me/restaurants", owner, "")
	var got struct {
		Restaurants []restaurants.Restaurant `json:"restaurants"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusOK || len(got.Restaurants) != 2 {
		t.Fatalf("status = %d, %d restaurants; want 200 and the owner's 2", rec.Code, len(got.Restaurants))
	}
}

func TestNearbyEndpoint(t *testing.T) {
	h := newAuthTestHandler(t)

	t.Run("public, with an empty list as [] and no next page", func(t *testing.T) {
		// A spot in the Black Sea, inside the bounding box but far from any test data.
		rec := send(t, h, http.MethodGet, "/restaurants?lat=41.55&lng=29.90", "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d; body: %s", rec.Code, rec.Body)
		}
		if body := rec.Body.String(); !strings.Contains(body, `"restaurants":[]`) || !strings.Contains(body, `"next_cursor":null`) {
			t.Fatalf("body = %s", body)
		}
	})

	t.Run("finds a restaurant that delivers here", func(t *testing.T) {
		// A random spot, so leftovers from earlier runs don't push ours off the page.
		lat, lng := 40.85+rand.Float64()*0.6, 28.10+rand.Float64()*1.6
		r := createRestaurant(t, h, ownerToken(t, h), fmt.Sprintf(
			`{"name": "Test Lokanta", "address": "Test Sk. 1", "lat": %f, "lng": %f}`, lat, lng))
		rec := send(t, h, http.MethodGet, fmt.Sprintf("/restaurants?lat=%f&lng=%f", lat, lng), "", "")
		if !strings.Contains(rec.Body.String(), r.ID.String()) {
			t.Fatalf("restaurant %s missing from the results near it", r.ID)
		}
	})

	for name, query := range map[string]string{
		"no location":      "",
		"outside Istanbul": "?lat=39.93&lng=32.85",
		"limit too big":    "?lat=40.99&lng=29.02&limit=500",
		"made-up cursor":   "?lat=40.99&lng=29.02&cursor=abc",
	} {
		t.Run(name, func(t *testing.T) {
			if rec := send(t, h, http.MethodGet, "/restaurants"+query, "", ""); rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422", rec.Code)
			}
		})
	}
}

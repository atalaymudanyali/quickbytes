package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/atalaymudanyali/quickbytes/internal/addresses"
)

const homeBody = `{"label": "Ev", "address_line": "Moda Cd. No:40, Kadıköy", "details": "Kat 3", "lat": 40.9840, "lng": 29.0270}`

func createAddress(t *testing.T, h http.Handler, token, body string) addresses.Address {
	t.Helper()
	rec := send(t, h, http.MethodPost, "/me/addresses", token, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create address: %d %s", rec.Code, rec.Body)
	}
	var a addresses.Address
	_ = json.Unmarshal(rec.Body.Bytes(), &a)
	return a
}

func TestAddresses(t *testing.T) {
	h := newAuthTestHandler(t)
	me := signUp(t, h).Tokens.AccessToken

	t.Run("needs a login", func(t *testing.T) {
		if rec := send(t, h, http.MethodGet, "/me/addresses", "", ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("empty list is []", func(t *testing.T) {
		rec := send(t, h, http.MethodGet, "/me/addresses", signUp(t, h).Tokens.AccessToken, "")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"addresses":[]`) {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
	})

	t.Run("create without details, then list", func(t *testing.T) {
		a := createAddress(t, h, me, `{"label": "İş", "address_line": "Büyükdere Cd. 1, Şişli", "lat": 41.0700, "lng": 29.0100}`)
		if a.Details != "" || a.Label != "İş" {
			t.Fatalf("got %+v", a)
		}
		var got struct {
			Addresses []addresses.Address `json:"addresses"`
		}
		rec := send(t, h, http.MethodGet, "/me/addresses", me, "")
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		if len(got.Addresses) != 1 || got.Addresses[0].ID != a.ID {
			t.Fatalf("list = %s, want the one address", rec.Body)
		}
	})

	t.Run("invalid fields all reported", func(t *testing.T) {
		rec := send(t, h, http.MethodPost, "/me/addresses", me,
			`{"label": " ", "details": "`+strings.Repeat("x", 201)+`", "lat": 29.0, "lng": 41.0}`)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422", rec.Code)
		}
		got := decodeError(t, rec).Fields
		for _, f := range []string{"label", "address_line", "details", "location"} {
			if got[f] == "" {
				t.Errorf("no error for %q; fields = %v", f, got)
			}
		}
	})

	t.Run("patch and delete", func(t *testing.T) {
		a := createAddress(t, h, me, homeBody)
		path := "/me/addresses/" + a.ID.String()

		rec := send(t, h, http.MethodPatch, path, me, `{"details": ""}`)
		var got addresses.Address
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		if rec.Code != http.StatusOK || got.Details != "" || got.Label != "Ev" {
			t.Fatalf("status = %d, body = %s; want details cleared, label kept", rec.Code, rec.Body)
		}
		if rec := send(t, h, http.MethodPatch, path, me, `{"lat": 41.0}`); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("lat without lng: status = %d, want 422", rec.Code)
		}
		if rec := send(t, h, http.MethodDelete, path, me, ""); rec.Code != http.StatusNoContent {
			t.Fatalf("delete: status = %d, want 204", rec.Code)
		}
		if rec := send(t, h, http.MethodDelete, path, me, ""); rec.Code != http.StatusNotFound {
			t.Fatalf("second delete: status = %d, want 404", rec.Code)
		}
	})

	t.Run("another customer's address looks missing", func(t *testing.T) {
		a := createAddress(t, h, me, homeBody)
		path := "/me/addresses/" + a.ID.String()
		stranger := signUp(t, h).Tokens.AccessToken

		patch := send(t, h, http.MethodPatch, path, stranger, `{"label": "Mine now"}`)
		del := send(t, h, http.MethodDelete, path, stranger, "")
		missing := send(t, h, http.MethodDelete, "/me/addresses/01a14bab-224e-7468-8920-42d59bbb55ee", me, "")
		for _, rec := range []*httptest.ResponseRecorder{patch, del, missing} {
			if rec.Code != http.StatusNotFound || decodeError(t, rec).Code != "address_not_found" {
				t.Fatalf("status = %d, want 404 address_not_found", rec.Code)
			}
		}
	})

	t.Run("the 11th address is refused", func(t *testing.T) {
		full := signUp(t, h).Tokens.AccessToken
		for range addresses.MaxPerCustomer {
			createAddress(t, h, full, homeBody)
		}
		rec := send(t, h, http.MethodPost, "/me/addresses", full, homeBody)
		if rec.Code != http.StatusConflict || decodeError(t, rec).Code != "address_limit_reached" {
			t.Fatalf("status = %d, body = %s; want 409 address_limit_reached", rec.Code, rec.Body)
		}
	})
}

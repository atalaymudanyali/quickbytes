package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/atalaymudanyali/quickbytes/internal/users"
)

func addProfile(t *testing.T, h http.Handler, accessToken, role string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/me/profiles", strings.NewReader(`{"role": "`+role+`"}`))
	req.Header.Set("Authorization", "Bearer "+accessToken)
	return do(t, h, req)
}

func refreshTokens(t *testing.T, h http.Handler, refreshToken string) tokenPair {
	t.Helper()
	rec := postJSON(t, h, "/auth/refresh", `{"refresh_token": "`+refreshToken+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh: %d %s", rec.Code, rec.Body)
	}
	var pair tokenPair
	_ = json.Unmarshal(rec.Body.Bytes(), &pair)
	return pair
}

func TestAddProfileThenRefreshGrantsRole(t *testing.T) {
	srv, h := newAuthTestServer(t)
	acc := signUp(t, h)

	driverOnly := withRequestID(srv.requireAuth(requireRole(users.RoleDriver, okHandler())))
	callAs := func(token string) int {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		return do(t, driverOnly, req).Code
	}

	if code := callAs(acc.Tokens.AccessToken); code != http.StatusForbidden {
		t.Fatalf("customer calling driver route: status = %d, want 403", code)
	}

	if rec := addProfile(t, h, acc.Tokens.AccessToken, "driver"); rec.Code != http.StatusCreated {
		t.Fatalf("add driver profile: %d %s", rec.Code, rec.Body)
	}
	// The old token was issued before the driver profile existed...
	if code := callAs(acc.Tokens.AccessToken); code != http.StatusForbidden {
		t.Fatalf("old token: status = %d, want 403", code)
	}
	// ...and the refreshed one includes it.
	pair := refreshTokens(t, h, acc.Tokens.RefreshToken)
	if code := callAs(pair.AccessToken); code != http.StatusOK {
		t.Fatalf("refreshed token: status = %d, want 200", code)
	}

	t.Run("same role twice", func(t *testing.T) {
		if rec := addProfile(t, h, pair.AccessToken, "driver"); rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", rec.Code)
		}
	})
	t.Run("invalid roles", func(t *testing.T) {
		for _, role := range []string{"customer", "admin", ""} {
			if rec := addProfile(t, h, pair.AccessToken, role); rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("role %q: status = %d, want 422", role, rec.Code)
			}
		}
	})
}

func TestAddProfileNeedsLogin(t *testing.T) {
	h := newAuthTestHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/me/profiles", strings.NewReader(`{"role": "driver"}`))
	if rec := do(t, h, req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
}

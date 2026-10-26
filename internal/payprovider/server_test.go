package payprovider

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atalaymudanyali/quickbytes/internal/testutil"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newHandler is the provider's HTTP side on the test database, with every
// answer checked against the spec.
func newHandler(t *testing.T, pool *pgxpool.Pool) http.Handler {
	t.Helper()
	return validatedBySpec(t, NewServer(pool, NewPayments(pool, 10*time.Second), discardLogger()).Routes())
}

// call sends one request to the provider and returns the response.
func call(t *testing.T, h http.Handler, method, path, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	return body.Error.Code
}

func TestHealth(t *testing.T) {
	h := newHandler(t, testutil.PayProviderDB(t))
	for _, path := range []string{"/healthz", "/readyz"} {
		if rec := call(t, h, "GET", path, ""); rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
	}
}

func TestAPIKeyAuth(t *testing.T) {
	pool := testutil.PayProviderDB(t)
	h := newHandler(t, pool)
	m, key := newMerchant(t, pool)

	t.Run("no key", func(t *testing.T) {
		rec := call(t, h, "GET", "/v1/account", "")
		if rec.Code != http.StatusUnauthorized || errorCode(t, rec) != "api_key_missing" {
			t.Errorf("got %d %s", rec.Code, rec.Body)
		}
	})
	t.Run("not a Bearer key", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/v1/account", nil)
		req.Header.Set("Authorization", "Basic "+key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized || errorCode(t, rec) != "api_key_missing" {
			t.Errorf("got %d %s", rec.Code, rec.Body)
		}
	})
	t.Run("wrong key", func(t *testing.T) {
		rec := call(t, h, "GET", "/v1/account", randomKey())
		if rec.Code != http.StatusUnauthorized || errorCode(t, rec) != "api_key_invalid" {
			t.Errorf("got %d %s", rec.Code, rec.Body)
		}
	})
	t.Run("right key", func(t *testing.T) {
		rec := call(t, h, "GET", "/v1/account", key)
		if rec.Code != http.StatusOK {
			t.Fatalf("got %d %s", rec.Code, rec.Body)
		}
		var got Merchant
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got != m {
			t.Errorf("account = %v (%v), want %v", got, err, m)
		}
		if rec.Header().Get(requestIDHeader) == "" {
			t.Error("no request ID header")
		}
	})
}

func TestUnknownPathIsJSON404(t *testing.T) {
	h := newHandler(t, testutil.PayProviderDB(t))
	rec := call(t, h, "GET", "/v1/nope", "")
	if rec.Code != http.StatusNotFound || errorCode(t, rec) != "not_found" {
		t.Errorf("got %d %s", rec.Code, rec.Body)
	}
}

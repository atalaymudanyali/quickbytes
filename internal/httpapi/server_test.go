package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestHandler(db Pinger) http.Handler {
	return NewServer(Deps{DB: db, Logger: discardLogger()}).Routes()
}

func do(t *testing.T, h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) errorDetail {
	t.Helper()
	var body errorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("response is not the JSON error format: %v", err)
	}
	return body.Error
}

func TestHealthEndpoints(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		dbErr      error
		wantStatus int
	}{
		{"healthz ignores the database", "/healthz", errors.New("db down"), http.StatusOK},
		{"readyz ok when database answers", "/readyz", nil, http.StatusOK},
		{"readyz unavailable when database is down", "/readyz", errors.New("db down"), http.StatusServiceUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(t, newTestHandler(fakePinger{err: tt.dbErr}), httptest.NewRequest(http.MethodGet, tt.path, nil))

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", ct)
			}
		})
	}
}

func TestRouterFallbacksUseJSONErrors(t *testing.T) {
	h := newTestHandler(fakePinger{})

	t.Run("unknown path", func(t *testing.T) {
		rec := do(t, h, httptest.NewRequest(http.MethodGet, "/nope", nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		if got := decodeError(t, rec); got.Code != "not_found" || got.RequestID == "" {
			t.Fatalf("error = %+v, want code not_found with a request ID", got)
		}
	})

	t.Run("wrong method", func(t *testing.T) {
		rec := do(t, h, httptest.NewRequest(http.MethodPost, "/healthz", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow == "" {
			t.Fatal("405 response is missing the Allow header")
		}
		if got := decodeError(t, rec); got.Code != "method_not_allowed" {
			t.Fatalf("code = %q, want method_not_allowed", got.Code)
		}
	})
}

func TestRequestID(t *testing.T) {
	h := newTestHandler(fakePinger{})

	t.Run("generated when missing", func(t *testing.T) {
		rec := do(t, h, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if id := rec.Header().Get(requestIDHeader); len(id) != 24 {
			t.Fatalf("generated request ID = %q, want 24 hex characters", id)
		}
	})

	t.Run("client ID kept", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.Header.Set(requestIDHeader, "abc-123")
		if id := do(t, h, req).Header().Get(requestIDHeader); id != "abc-123" {
			t.Fatalf("request ID = %q, want abc-123", id)
		}
	})

	t.Run("unsafe client ID replaced", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.Header.Set(requestIDHeader, "bad id with spaces")
		if id := do(t, h, req).Header().Get(requestIDHeader); id == "bad id with spaces" {
			t.Fatal("unsafe request ID was accepted")
		}
	})
}

func TestPanicBecomes500(t *testing.T) {
	panicking := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })
	h := withRequestID(recoverPanics(discardLogger(), panicking))

	rec := do(t, h, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := decodeError(t, rec); got.Code != "internal" || got.Message == "boom" {
		t.Fatalf("error = %+v, want code internal without leaking the panic value", got)
	}
}

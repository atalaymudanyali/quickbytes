package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"time"
)

const requestIDHeader = "X-Request-ID"

type ctxKey int

// Keys for values stored in a request's context. A private type means no other
// package can read or overwrite them by accident.
const (
	requestIDKey ctxKey = iota
	claimsKey
)

// validRequestID limits IDs accepted from clients, so nobody can inject huge values
// or line breaks into our logs.
var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// withRequestID gives every request an ID, echoed in the response header and in
// every log line, so one failing request can be found among thousands. An ID sent
// by the client (or by nginx) is kept, which lets one ID follow a request across
// services.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if !validRequestID.MatchString(id) {
			id = newRequestID()
		}
		w.Header().Set(requestIDHeader, id)
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func newRequestID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// logRequests writes one line per request, after it finishes.
func logRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		logger.Info("request",
			"request_id", requestIDFrom(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// recoverPanics turns a crash inside one handler into a 500 response for that
// request, instead of an empty reply. The stack trace goes to the log, never to the
// client, since it reveals how the code works inside.
func recoverPanics(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				logger.Error("panic",
					"request_id", requestIDFrom(r.Context()),
					"panic", v,
					"stack", string(debug.Stack()),
				)
				writeError(w, r, http.StatusInternalServerError, "internal", "something went wrong")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// jsonFallbacks makes the router's own 404 and 405 answers use the JSON error
// format. The standard router writes those as plain text.
func jsonFallbacks(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}
		// No route matched. Let the router decide between 404 and 405 (and set the
		// Allow header), but capture its plain-text reply instead of sending it.
		capture := &captureWriter{header: http.Header{}}
		mux.ServeHTTP(capture, r)
		if allow := capture.header.Get("Allow"); allow != "" {
			w.Header().Set("Allow", allow)
		}
		if capture.status == http.StatusMethodNotAllowed {
			writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed on this path")
			return
		}
		writeError(w, r, http.StatusNotFound, "not_found", "no such endpoint")
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

type captureWriter struct {
	header http.Header
	status int
}

func (c *captureWriter) Header() http.Header         { return c.header }
func (c *captureWriter) Write(b []byte) (int, error) { return len(b), nil }
func (c *captureWriter) WriteHeader(code int)        { c.status = code }

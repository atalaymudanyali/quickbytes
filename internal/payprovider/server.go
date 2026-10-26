package payprovider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atalaymudanyali/quickbytes/internal/idempotency"
)

// Server is the provider's HTTP side. Its middleware works like the API's
// (internal/httpapi): a request ID, one log line per request, panics turned
// into a 500, and the same error shape.
type Server struct {
	db          *pgxpool.Pool
	merchants   *Merchants
	payments    *Payments
	idempotency *idempotency.Store
	logger      *slog.Logger
	// hangFor is how long the hang test card's answer is held back (HangFor;
	// tests shorten it).
	hangFor time.Duration
}

// NewServer builds the HTTP side on the provider's database. payments comes in
// ready-made because it carries settings (the slow card's delay).
func NewServer(db *pgxpool.Pool, payments *Payments, logger *slog.Logger) *Server {
	return &Server{
		db:          db,
		merchants:   NewMerchants(db),
		payments:    payments,
		idempotency: idempotency.NewStore(db),
		logger:      logger,
		hangFor:     HangFor,
	}
}

// route is one endpoint, kept in a table so tests can compare the list with
// the provider's OpenAPI spec.
type route struct {
	pattern string
	handler http.Handler
}

func (s *Server) routes() []route {
	public := func(h http.HandlerFunc) http.Handler { return h }
	// Everything under /v1 is for merchants, who prove who they are with their
	// API key.
	merchantOnly := func(h http.HandlerFunc) http.Handler { return s.requireMerchant(h) }

	return []route{
		{"GET /healthz", public(s.handleHealthz)},
		{"GET /readyz", public(s.handleReadyz)},

		{"GET /v1/account", merchantOnly(s.handleAccount)},

		// Payments. Every write needs an Idempotency-Key, so a retried call
		// never holds, takes or gives back money twice.
		{"POST /v1/payments", merchantOnly(s.idempotent(s.handleAuthorize))},
		{"GET /v1/payments/{payment_id}", merchantOnly(s.handleGetPayment)},
		{"POST /v1/payments/{payment_id}/capture", merchantOnly(s.idempotent(s.handleMove(ActionCapture)))},
		{"POST /v1/payments/{payment_id}/void", merchantOnly(s.idempotent(s.handleMove(ActionVoid)))},
		{"POST /v1/payments/{payment_id}/refund", merchantOnly(s.idempotent(s.handleMove(ActionRefund)))},
	}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.routes() {
		mux.Handle(rt.pattern, rt.handler)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusNotFound, "not_found", "no such endpoint")
	})

	// Outermost first: every request gets an ID, then is logged, and a panic
	// anywhere inside still produces a logged 500.
	var h http.Handler = mux
	h = recoverPanics(s.logger, h)
	h = logRequests(s.logger, h)
	h = withRequestID(h)
	return h
}

// handleHealthz answers "is the process alive?", without checking the database.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz answers "can this copy serve requests right now?".
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.db.Ping(ctx); err != nil {
		s.logger.Warn("readiness check failed", "request_id", requestIDFrom(r.Context()), "err", err)
		writeError(w, r, http.StatusServiceUnavailable, "unavailable", "database unreachable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// handleAccount tells a merchant who the provider thinks it is: an easy way to
// check an API key works (Stripe has the same idea).
func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, merchantFrom(r.Context()))
}

// requireMerchant lets a request through only with a valid merchant API key,
// sent like Stripe's: "Authorization: Bearer sk_test_...". The merchant is put
// in the request's context for the handler.
func (s *Server) requireMerchant(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || key == "" {
			writeError(w, r, http.StatusUnauthorized, "api_key_missing",
				"send your API key as: Authorization: Bearer <key>")
			return
		}
		m, err := s.merchants.ByKey(r.Context(), key)
		if errors.Is(err, ErrUnknownKey) {
			writeError(w, r, http.StatusUnauthorized, "api_key_invalid", "this API key is not valid")
			return
		}
		if err != nil {
			s.writeInternalError(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), merchantKey, m)))
	})
}

type ctxKey int

const (
	requestIDKey ctxKey = iota
	merchantKey
	heldKeyKey
)

// merchantFrom returns the merchant requireMerchant found. Only called behind
// requireMerchant, so it's always there.
func merchantFrom(ctx context.Context) Merchant {
	return ctx.Value(merchantKey).(Merchant)
}

const requestIDHeader = "X-Request-ID"

// validRequestID limits IDs accepted from callers, so nobody can put huge values
// or line breaks into the logs.
var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// withRequestID gives every request an ID. An ID sent by the caller is kept, so
// once the API calls the provider, one ID can follow a checkout through both
// services' logs.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if !validRequestID.MatchString(id) {
			b := make([]byte, 12)
			_, _ = rand.Read(b)
			id = hex.EncodeToString(b)
		}
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
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

// recoverPanics turns a crash inside one handler into a 500 for that request.
// The stack trace goes to the log, never to the caller.
func recoverPanics(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				logger.Error("panic", "request_id", requestIDFrom(r.Context()),
					"panic", v, "stack", string(debug.Stack()))
				writeError(w, r, http.StatusInternalServerError, "internal", "something went wrong")
			}
		}()
		next.ServeHTTP(w, r)
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

// errorBody is the shape of every error, the same as the API's:
//
//	{"error": {"code": "api_key_invalid", "message": "...", "request_id": "..."}}
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	writeJSON(w, status, errorBody{Error: errorDetail{
		Code: code, Message: message, RequestID: requestIDFrom(r.Context()),
	}})
}

// writeInternalError logs the real cause and tells the caller only that
// something failed.
func (s *Server) writeInternalError(w http.ResponseWriter, r *http.Request, err error) {
	s.logger.Error("internal error", "request_id", requestIDFrom(r.Context()), "err", err)
	writeError(w, r, http.StatusInternalServerError, "internal", "something went wrong")
}

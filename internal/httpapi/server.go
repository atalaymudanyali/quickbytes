// Package httpapi defines the HTTP routes of the API service.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/atalaymudanyali/quickbytes/internal/auth"
	"github.com/atalaymudanyali/quickbytes/internal/restaurants"
	"github.com/atalaymudanyali/quickbytes/internal/users"
)

// Pinger is anything that can report whether it's reachable. The database pool
// satisfies it; tests pass a fake.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Deps are the things the HTTP layer needs. Tests that only touch some routes can
// leave the rest empty.
type Deps struct {
	DB          Pinger
	Users       *users.Store
	Hasher      *auth.Hasher
	Tokens      *auth.TokenIssuer
	Refresh     *auth.RefreshStore
	Restaurants *restaurants.Store
	Logger      *slog.Logger
}

type Server struct {
	db          Pinger
	users       *users.Store
	hasher      *auth.Hasher
	tokens      *auth.TokenIssuer
	refresh     *auth.RefreshStore
	restaurants *restaurants.Store
	logger      *slog.Logger

	dummyOnce sync.Once
	dummy     string // see dummyHash
}

func NewServer(d Deps) *Server {
	return &Server{
		db:          d.DB,
		users:       d.Users,
		hasher:      d.Hasher,
		tokens:      d.Tokens,
		refresh:     d.Refresh,
		restaurants: d.Restaurants,
		logger:      d.Logger,
	}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("POST /auth/signup", s.handleSignup)
	mux.HandleFunc("POST /auth/login", s.handleLogin)
	mux.HandleFunc("POST /auth/refresh", s.handleRefresh)
	mux.HandleFunc("POST /auth/logout", s.handleLogout)
	mux.Handle("GET /me", s.requireAuth(http.HandlerFunc(s.handleMe)))
	mux.Handle("POST /me/profiles", s.requireAuth(http.HandlerFunc(s.handleAddProfile)))

	// Restaurants: browsing is public, managing needs a restaurant profile.
	ownerOnly := func(h http.HandlerFunc) http.Handler {
		return s.requireAuth(requireRole(users.RoleRestaurant, h))
	}
	mux.HandleFunc("GET /restaurants", s.handleNearbyRestaurants)
	mux.HandleFunc("GET /restaurants/{id}", s.handleGetRestaurant)
	mux.Handle("POST /restaurants", ownerOnly(s.handleCreateRestaurant))
	mux.Handle("PATCH /restaurants/{id}", ownerOnly(s.handleUpdateRestaurant))
	mux.Handle("GET /me/restaurants", ownerOnly(s.handleMyRestaurants))

	// Outermost first: every request gets an ID, then is logged, and a panic
	// anywhere inside still produces a logged 500.
	var h http.Handler = jsonFallbacks(mux)
	h = recoverPanics(s.logger, h)
	h = logRequests(s.logger, h)
	h = withRequestID(h)
	return h
}

// handleHealthz answers "is the process alive?". It never checks dependencies, so a
// database outage doesn't make the orchestrator restart healthy API copies.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz answers "can this copy serve traffic right now?". A load balancer
// stops sending requests to a copy that isn't ready.
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

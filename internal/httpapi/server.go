// Package httpapi defines the HTTP routes of the API service.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/atalaymudanyali/quickbytes/internal/auth"
	"github.com/atalaymudanyali/quickbytes/internal/menus"
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
	Menus       *menus.Store
	// MenuCache serves public menus. It works without Redis too (it then reads
	// from the database), but must not be nil.
	MenuCache *menus.Cache
	Logger    *slog.Logger
}

type Server struct {
	db          Pinger
	users       *users.Store
	hasher      *auth.Hasher
	tokens      *auth.TokenIssuer
	refresh     *auth.RefreshStore
	restaurants *restaurants.Store
	menus       *menus.Store
	menuCache   *menus.Cache
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
		menus:       d.Menus,
		menuCache:   d.MenuCache,
		logger:      d.Logger,
	}
}

// route is one endpoint: its pattern ("METHOD /path") and its handler. Keeping
// them in a table, instead of registering each directly, lets tests list every
// endpoint and compare them with the OpenAPI spec.
type route struct {
	pattern string
	handler http.Handler
}

// routes is the full list of endpoints. Every one must also be in
// api/openapi.yaml; a test fails if the two lists differ.
func (s *Server) routes() []route {
	public := func(h http.HandlerFunc) http.Handler { return h }
	loggedIn := func(h http.HandlerFunc) http.Handler { return s.requireAuth(h) }
	// Managing restaurants and menus needs a restaurant profile.
	ownerOnly := func(h http.HandlerFunc) http.Handler {
		return s.requireAuth(requireRole(users.RoleRestaurant, h))
	}

	return []route{
		{"GET /healthz", public(s.handleHealthz)},
		{"GET /readyz", public(s.handleReadyz)},

		{"POST /auth/signup", public(s.handleSignup)},
		{"POST /auth/login", public(s.handleLogin)},
		{"POST /auth/refresh", public(s.handleRefresh)},
		{"POST /auth/logout", public(s.handleLogout)},
		{"GET /me", loggedIn(s.handleMe)},
		{"POST /me/profiles", loggedIn(s.handleAddProfile)},

		// Restaurants: browsing is public, managing is for owners.
		{"GET /restaurants", public(s.handleNearbyRestaurants)},
		{"GET /restaurants/{id}", public(s.handleGetRestaurant)},
		{"POST /restaurants", ownerOnly(s.handleCreateRestaurant)},
		{"PATCH /restaurants/{id}", ownerOnly(s.handleUpdateRestaurant)},
		{"GET /me/restaurants", ownerOnly(s.handleMyRestaurants)},

		// Menus: anyone can read one, only the restaurant's owner can change it.
		{"GET /restaurants/{id}/menu", public(s.handleMenu)},
		{"GET /me/restaurants/{id}/menu", ownerOnly(s.handleOwnerMenu)},
		{"POST /restaurants/{id}/menu/categories", ownerOnly(s.handleCreateCategory)},
		{"PATCH /restaurants/{id}/menu/categories/{category_id}", ownerOnly(s.handleUpdateCategory)},
		{"POST /restaurants/{id}/menu/items", ownerOnly(s.handleCreateItem)},
		{"PATCH /restaurants/{id}/menu/items/{item_id}", ownerOnly(s.handleUpdateItem)},
		{"PUT /restaurants/{id}/menu/categories/order", ownerOnly(s.handleReorderCategories)},
		{"PUT /restaurants/{id}/menu/categories/{category_id}/order", ownerOnly(s.handleReorderItems)},
	}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.routes() {
		mux.Handle(rt.pattern, rt.handler)
	}
	// Documentation, outside the API itself.
	docs := docsHandler()
	mux.Handle("GET /openapi.yaml", docs)
	mux.Handle("GET /docs", docs)
	mux.Handle("GET /docs/", docs)

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

// Package httpapi defines the HTTP routes of the API service.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// Pinger is anything that can report whether it's reachable. The database pool
// satisfies it; tests pass a fake.
type Pinger interface {
	Ping(ctx context.Context) error
}

type Server struct {
	db     Pinger
	logger *slog.Logger
}

func NewServer(db Pinger, logger *slog.Logger) *Server {
	return &Server{db: db, logger: logger}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

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

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/atalaymudanyali/quickbytes/internal/auth"
	"github.com/atalaymudanyali/quickbytes/internal/users"
)

// tokenPair is what the app stores after logging in. It sends the access token
// with every request, and the refresh token only to /auth/refresh when the access
// token has expired.
type tokenPair struct {
	TokenType       string    `json:"token_type"`
	AccessToken     string    `json:"access_token"`
	AccessExpiresAt time.Time `json:"access_expires_at"`
	RefreshToken    string    `json:"refresh_token"`
}

// startSession issues the first token pair of a new login.
func (s *Server) startSession(ctx context.Context, userID uuid.UUID, profiles []users.Profile) (*tokenPair, error) {
	refresh, err := s.refresh.Start(ctx, userID)
	if err != nil {
		return nil, err
	}
	return s.pairWith(userID, profiles, refresh)
}

func (s *Server) pairWith(userID uuid.UUID, profiles []users.Profile, refresh string) (*tokenPair, error) {
	access, expires, err := s.tokens.Issue(userID, rolesOf(profiles))
	if err != nil {
		return nil, err
	}
	return &tokenPair{TokenType: "Bearer", AccessToken: access, AccessExpiresAt: expires, RefreshToken: refresh}, nil
}

func rolesOf(profiles []users.Profile) []users.Role {
	roles := make([]users.Role, len(profiles))
	for i, p := range profiles {
		roles[i] = p.Role
	}
	return roles
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	userID, newRefresh, err := s.refresh.Rotate(r.Context(), req.RefreshToken)
	if errors.Is(err, auth.ErrRefreshReused) {
		// Worth a warning: it's either a stolen token or a buggy client. The client
		// gets the same answer as for any bad token.
		s.logger.Warn("refresh token reuse detected; family revoked",
			"request_id", requestIDFrom(r.Context()), "user_id", userID)
	}
	if errors.Is(err, auth.ErrInvalidRefresh) || errors.Is(err, auth.ErrRefreshReused) {
		writeError(w, r, http.StatusUnauthorized, "invalid_refresh_token", "refresh token is invalid or expired; log in again")
		return
	}
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}

	// Roles are read fresh from the database, so a profile added since the last
	// token shows up now.
	profiles, err := s.users.ListProfiles(r.Context(), userID)
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	pair, err := s.pairWith(userID, profiles, newRefresh)
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, pair)
}

// handleLogout revokes the refresh token's family. The current access token keeps
// working until it expires (at most 15 minutes): that's the price of access tokens
// that don't need a database check.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.refresh.Revoke(r.Context(), req.RefreshToken); err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	userID := mustUserID(r.Context())
	user, err := s.users.GetByID(r.Context(), userID)
	if errors.Is(err, users.ErrNotFound) {
		// A valid token for a user who has since been deleted.
		writeUnauthenticated(w, r)
		return
	}
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	profiles, err := s.users.ListProfiles(r.Context(), userID)
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, accountResponse{User: user, Profiles: profiles})
}

// requireAuth lets a request through only with a valid access token in the
// "Authorization: Bearer <token>" header, and puts the token's claims in the
// request context for the handler.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			writeUnauthenticated(w, r)
			return
		}
		claims, err := s.tokens.Parse(token)
		if err != nil {
			writeUnauthenticated(w, r)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), claimsKey, claims)))
	})
}

func claimsFrom(ctx context.Context) (auth.Claims, bool) {
	c, ok := ctx.Value(claimsKey).(auth.Claims)
	return c, ok
}

// mustUserID is only for handlers behind requireAuth, which guarantees valid claims.
func mustUserID(ctx context.Context) uuid.UUID {
	c, _ := claimsFrom(ctx)
	id, _ := c.UserID()
	return id
}

func writeUnauthenticated(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="quickbytes"`)
	writeError(w, r, http.StatusUnauthorized, "unauthenticated", "a valid access token is required")
}

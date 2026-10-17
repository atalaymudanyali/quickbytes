package httpapi

import (
	"errors"
	"net/http"

	"github.com/atalaymudanyali/quickbytes/internal/users"
)

type addProfileRequest struct {
	Role users.Role `json:"role"`
}

// handleAddProfile gives the signed-in user another profile, such as driver. The
// current access token doesn't list it yet: the app calls /auth/refresh next, and
// the new token includes it.
func (s *Server) handleAddProfile(w http.ResponseWriter, r *http.Request) {
	var req addProfileRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Role != users.RoleRestaurant && req.Role != users.RoleDriver {
		writeValidationError(w, r, map[string]string{"role": `must be "restaurant" or "driver"`})
		return
	}

	profile, err := s.users.AddProfile(r.Context(), mustUserID(r.Context()), req.Role)
	if errors.Is(err, users.ErrProfileExists) {
		writeError(w, r, http.StatusConflict, "profile_exists", "you already have a "+string(req.Role)+" profile")
		return
	}
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, profile)
}

// requireRole only lets through users whose access token lists the given profile.
// It must sit inside requireAuth. The check reads the token, not the database, so
// it costs nothing per request.
//
// 401 means "we don't know who you are". 403, used here, means "we know who you
// are, and you're not allowed".
func requireRole(role users.Role, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, ok := claimsFrom(r.Context())
		if !ok || !claims.HasRole(role) {
			writeError(w, r, http.StatusForbidden, "profile_required", "this needs a "+string(role)+" profile")
			return
		}
		next.ServeHTTP(w, r)
	})
}

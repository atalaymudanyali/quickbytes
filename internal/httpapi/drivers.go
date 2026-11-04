package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/atalaymudanyali/quickbytes/internal/drivers"
	"github.com/atalaymudanyali/quickbytes/internal/shiftkey"
	"github.com/atalaymudanyali/quickbytes/internal/users"
)

type driverRequest struct {
	Vehicle drivers.Vehicle `json:"vehicle"`
}

// shiftKeyResponse is the only time a driver's key leaves the server. Key is
// base64 in JSON, and the phone needs the exact expires_at to send with its
// location messages: the key was worked out from it.
type shiftKeyResponse struct {
	Key       []byte    `json:"key"`
	ExpiresAt time.Time `json:"expires_at"`
}

type startShiftResponse struct {
	Driver   drivers.Driver   `json:"driver"`
	ShiftKey shiftKeyResponse `json:"shift_key"`
}

func (s *Server) handleGetDriver(w http.ResponseWriter, r *http.Request) {
	profile, ok := s.driverProfile(w, r)
	if !ok {
		return
	}
	d, err := s.drivers.Get(r.Context(), profile)
	if errors.Is(err, drivers.ErrNotFound) {
		writeError(w, r, http.StatusNotFound, "driver_not_found", "save your driver details first")
		return
	}
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// handleSaveDriver creates the driver's details, or changes them if they exist.
func (s *Server) handleSaveDriver(w http.ResponseWriter, r *http.Request) {
	var req driverRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !req.Vehicle.Valid() {
		writeValidationError(w, r, map[string]string{"vehicle": `must be "motorcycle", "bicycle" or "car"`})
		return
	}
	profile, ok := s.driverProfile(w, r)
	if !ok {
		return
	}
	d, err := s.drivers.Save(r.Context(), profile, req.Vehicle)
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// handleStartShift turns the driver's light on and hands out a shift key. Calling
// it again is harmless and gives a fresh key, which is also how a phone renews one
// during a long shift. Keys made earlier still work until they expire.
func (s *Server) handleStartShift(w http.ResponseWriter, r *http.Request) {
	profile, ok := s.driverProfile(w, r)
	if !ok {
		return
	}
	d, err := s.drivers.StartShift(r.Context(), profile)
	if errors.Is(err, drivers.ErrNotFound) {
		writeDriverDetailsMissing(w, r)
		return
	}
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	key := shiftkey.Issue(s.shiftKeySecret, d.ID, time.Now(), s.shiftKeyTTL)
	writeJSON(w, http.StatusOK, startShiftResponse{
		Driver:   d,
		ShiftKey: shiftKeyResponse{Key: key.Secret, ExpiresAt: key.ExpiresAt},
	})
}

// handleEndShift turns the light off, unless the driver is carrying food. Calling
// it again is harmless.
func (s *Server) handleEndShift(w http.ResponseWriter, r *http.Request) {
	profile, ok := s.driverProfile(w, r)
	if !ok {
		return
	}
	d, err := s.drivers.EndShift(r.Context(), profile)
	if errors.Is(err, drivers.ErrNotFound) {
		writeDriverDetailsMissing(w, r)
		return
	}
	if errors.Is(err, drivers.ErrOnDelivery) {
		writeError(w, r, http.StatusConflict, "on_delivery", "you can't end your shift during a delivery")
		return
	}
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	s.leaveMap(r, d.ID)
	writeJSON(w, http.StatusOK, d)
}

// leaveMap takes a driver off the live map. The shift has already ended by now,
// because Postgres is what decides who is free. So if Redis is down the driver isn't
// kept working for it: they're only left on the map, where they count as silent
// after 30 seconds and Redis's next restart wipes them anyway.
func (s *Server) leaveMap(r *http.Request, driverID uuid.UUID) {
	if s.positions == nil {
		return
	}
	if err := s.positions.Remove(r.Context(), driverID); err != nil {
		s.logger.WarnContext(r.Context(), "driver left on the live map after ending their shift", "driver_id", driverID, "err", err)
	}
}

func writeDriverDetailsMissing(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusConflict, "driver_details_missing", "save your driver details first")
}

// driverProfile finds the signed-in user's driver profile, as customerProfile does
// for customers.
func (s *Server) driverProfile(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	p, err := s.users.ProfileByRole(r.Context(), mustUserID(r.Context()), users.RoleDriver)
	if errors.Is(err, users.ErrNotFound) {
		writeError(w, r, http.StatusForbidden, "profile_required", "this needs a driver profile")
		return uuid.Nil, false
	}
	if err != nil {
		s.writeInternalError(w, r, err)
		return uuid.Nil, false
	}
	return p.ID, true
}

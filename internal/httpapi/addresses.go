package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/atalaymudanyali/quickbytes/internal/addresses"
	"github.com/atalaymudanyali/quickbytes/internal/restaurants"
	"github.com/atalaymudanyali/quickbytes/internal/users"
)

// Pointer fields tell "not sent" apart from a real value, as in restaurantRequest.
type addressRequest struct {
	Label       *string  `json:"label"`
	AddressLine *string  `json:"address_line"`
	Details     *string  `json:"details"`
	Lat         *float64 `json:"lat"`
	Lng         *float64 `json:"lng"`
}

func (s *Server) handleListAddresses(w http.ResponseWriter, r *http.Request) {
	customer, ok := s.customerProfile(w, r)
	if !ok {
		return
	}
	list, err := s.addresses.List(r.Context(), customer)
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	if list == nil {
		list = []addresses.Address{} // [] rather than null in JSON
	}
	writeJSON(w, http.StatusOK, map[string]any{"addresses": list})
}

func (s *Server) handleCreateAddress(w http.ResponseWriter, r *http.Request) {
	var req addressRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	trimAddress(&req)
	fields := validateAddress(req)
	if req.Label == nil {
		fields["label"] = "is required"
	}
	if req.AddressLine == nil {
		fields["address_line"] = "is required"
	}
	if req.Lat == nil || req.Lng == nil {
		fields["location"] = "lat and lng are required"
	}
	if len(fields) > 0 {
		writeValidationError(w, r, fields)
		return
	}

	customer, ok := s.customerProfile(w, r)
	if !ok {
		return
	}
	n := addresses.New{Label: *req.Label, AddressLine: *req.AddressLine, Lat: *req.Lat, Lng: *req.Lng}
	if req.Details != nil {
		n.Details = *req.Details
	}
	created, err := s.addresses.Create(r.Context(), customer, n)
	if errors.Is(err, addresses.ErrLimitReached) {
		writeError(w, r, http.StatusConflict, "address_limit_reached",
			fmt.Sprintf("you can save up to %d addresses; delete one first", addresses.MaxPerCustomer))
		return
	}
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleUpdateAddress(w http.ResponseWriter, r *http.Request) {
	id, ok := addressID(w, r)
	if !ok {
		return
	}
	var req addressRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	trimAddress(&req)
	fields := validateAddress(req)
	if (req.Lat == nil) != (req.Lng == nil) {
		fields["location"] = "lat and lng must be changed together"
	}
	if req == (addressRequest{}) {
		fields["body"] = "nothing to update"
	}
	if len(fields) > 0 {
		writeValidationError(w, r, fields)
		return
	}

	customer, ok := s.customerProfile(w, r)
	if !ok {
		return
	}
	updated, err := s.addresses.Update(r.Context(), id, customer, addresses.Patch{
		Label: req.Label, AddressLine: req.AddressLine, Details: req.Details, Lat: req.Lat, Lng: req.Lng,
	})
	if errors.Is(err, addresses.ErrNotFound) {
		writeAddressNotFound(w, r)
		return
	}
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteAddress(w http.ResponseWriter, r *http.Request) {
	id, ok := addressID(w, r)
	if !ok {
		return
	}
	customer, ok := s.customerProfile(w, r)
	if !ok {
		return
	}
	err := s.addresses.Delete(r.Context(), id, customer)
	if errors.Is(err, addresses.ErrNotFound) {
		writeAddressNotFound(w, r)
		return
	}
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// customerProfile finds the signed-in user's customer profile, the one that owns
// addresses and carts. Like restaurantProfile, a miss means an out-of-date token.
func (s *Server) customerProfile(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	p, err := s.users.ProfileByRole(r.Context(), mustUserID(r.Context()), users.RoleCustomer)
	if errors.Is(err, users.ErrNotFound) {
		writeError(w, r, http.StatusForbidden, "profile_required", "this needs a customer profile")
		return uuid.Nil, false
	}
	if err != nil {
		s.writeInternalError(w, r, err)
		return uuid.Nil, false
	}
	return p.ID, true
}

func addressID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	return pathID(w, r, "address_id", "address_not_found", "no such address")
}

func writeAddressNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusNotFound, "address_not_found", "no such address")
}

func trimAddress(req *addressRequest) {
	for _, f := range []*string{req.Label, req.AddressLine, req.Details} {
		if f != nil {
			*f = strings.TrimSpace(*f)
		}
	}
}

// validateAddress checks the fields that were sent; the caller decides which are
// required. Details may be empty: plenty of houses need no floor or door number.
func validateAddress(req addressRequest) map[string]string {
	fields := map[string]string{}
	if req.Label != nil {
		if n := utf8.RuneCountInString(*req.Label); n < 1 || n > 30 {
			fields["label"] = "must be 1 to 30 characters"
		}
	}
	if req.AddressLine != nil {
		if n := utf8.RuneCountInString(*req.AddressLine); n < 1 || n > 300 {
			fields["address_line"] = "must be 1 to 300 characters"
		}
	}
	if req.Details != nil && utf8.RuneCountInString(*req.Details) > 200 {
		fields["details"] = "must be at most 200 characters"
	}
	if req.Lat != nil && req.Lng != nil && !restaurants.InIstanbul(*req.Lat, *req.Lng) {
		fields["location"] = "must be inside Istanbul"
	}
	return fields
}

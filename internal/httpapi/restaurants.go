package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/atalaymudanyali/quickbytes/internal/restaurants"
	"github.com/atalaymudanyali/quickbytes/internal/users"
)

// Pointer fields tell "not sent" (nil) apart from a real value. For create, the
// handler requires the ones that matter; for update, nil means "leave unchanged".
type restaurantRequest struct {
	Name            *string  `json:"name"`
	Address         *string  `json:"address"`
	Lat             *float64 `json:"lat"`
	Lng             *float64 `json:"lng"`
	DeliveryRadiusM *int     `json:"delivery_radius_m"`
	AcceptingOrders *bool    `json:"accepting_orders"`
}

func (s *Server) handleCreateRestaurant(w http.ResponseWriter, r *http.Request) {
	var req restaurantRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	trimText(&req)
	fields := validateRestaurant(req)
	if req.Name == nil {
		fields["name"] = "is required"
	}
	if req.Address == nil {
		fields["address"] = "is required"
	}
	if req.Lat == nil || req.Lng == nil {
		fields["location"] = "lat and lng are required"
	}
	if req.AcceptingOrders != nil {
		fields["accepting_orders"] = "can only be changed after creating the restaurant"
	}
	if len(fields) > 0 {
		writeValidationError(w, r, fields)
		return
	}

	owner, ok := s.restaurantProfile(w, r)
	if !ok {
		return
	}
	radius := restaurants.DefaultDeliveryRadiusM
	if req.DeliveryRadiusM != nil {
		radius = *req.DeliveryRadiusM
	}
	created, err := s.restaurants.Create(r.Context(), owner, restaurants.New{
		Name: *req.Name, Address: *req.Address, Lat: *req.Lat, Lng: *req.Lng, DeliveryRadiusM: radius,
	})
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleUpdateRestaurant(w http.ResponseWriter, r *http.Request) {
	id, ok := restaurantID(w, r)
	if !ok {
		return
	}
	var req restaurantRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	trimText(&req)
	fields := validateRestaurant(req)
	if (req.Lat == nil) != (req.Lng == nil) {
		fields["location"] = "lat and lng must be changed together"
	}
	if req == (restaurantRequest{}) {
		fields["body"] = "nothing to update"
	}
	if len(fields) > 0 {
		writeValidationError(w, r, fields)
		return
	}

	owner, ok := s.restaurantProfile(w, r)
	if !ok {
		return
	}
	updated, err := s.restaurants.Update(r.Context(), id, owner, restaurants.Patch{
		Name: req.Name, Address: req.Address, Lat: req.Lat, Lng: req.Lng,
		DeliveryRadiusM: req.DeliveryRadiusM, AcceptingOrders: req.AcceptingOrders,
	})
	if errors.Is(err, restaurants.ErrNotFound) {
		writeRestaurantNotFound(w, r)
		return
	}
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleGetRestaurant(w http.ResponseWriter, r *http.Request) {
	id, ok := restaurantID(w, r)
	if !ok {
		return
	}
	found, err := s.restaurants.Get(r.Context(), id)
	if errors.Is(err, restaurants.ErrNotFound) {
		writeRestaurantNotFound(w, r)
		return
	}
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, found)
}

func (s *Server) handleMyRestaurants(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.restaurantProfile(w, r)
	if !ok {
		return
	}
	list, err := s.restaurants.ListByOwner(r.Context(), owner)
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"restaurants": list})
}

// restaurantProfile finds the signed-in user's restaurant profile. The token says
// they have one (requireRole checked that), so a miss means the token is out of date.
func (s *Server) restaurantProfile(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	p, err := s.users.ProfileByRole(r.Context(), mustUserID(r.Context()), users.RoleRestaurant)
	if errors.Is(err, users.ErrNotFound) {
		writeError(w, r, http.StatusForbidden, "profile_required", "this needs a restaurant profile")
		return uuid.Nil, false
	}
	if err != nil {
		s.writeInternalError(w, r, err)
		return uuid.Nil, false
	}
	return p.ID, true
}

// restaurantID reads {id} from the URL. Something that isn't a UUID can't be a
// restaurant, so it gets the same 404 as an unknown ID.
func restaurantID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeRestaurantNotFound(w, r)
		return uuid.Nil, false
	}
	return id, true
}

func writeRestaurantNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusNotFound, "restaurant_not_found", "no such restaurant")
}

func trimText(req *restaurantRequest) {
	for _, f := range []*string{req.Name, req.Address} {
		if f != nil {
			*f = strings.TrimSpace(*f)
		}
	}
}

// validateRestaurant checks the fields that were sent. Whether a field is required
// is up to the caller (create needs more than update).
func validateRestaurant(req restaurantRequest) map[string]string {
	fields := map[string]string{}
	if req.Name != nil {
		if n := utf8.RuneCountInString(*req.Name); n < 1 || n > 100 {
			fields["name"] = "must be 1 to 100 characters"
		}
	}
	if req.Address != nil {
		if n := utf8.RuneCountInString(*req.Address); n < 1 || n > 300 {
			fields["address"] = "must be 1 to 300 characters"
		}
	}
	if req.Lat != nil && req.Lng != nil && !restaurants.InIstanbul(*req.Lat, *req.Lng) {
		fields["location"] = "must be inside Istanbul"
	}
	if req.DeliveryRadiusM != nil {
		if d := *req.DeliveryRadiusM; d < restaurants.MinDeliveryRadiusM || d > restaurants.MaxDeliveryRadiusM {
			fields["delivery_radius_m"] = fmt.Sprintf("must be %d to %d metres",
				restaurants.MinDeliveryRadiusM, restaurants.MaxDeliveryRadiusM)
		}
	}
	return fields
}

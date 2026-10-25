package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/atalaymudanyali/quickbytes/internal/restaurants"
	"github.com/atalaymudanyali/quickbytes/internal/users"
)

// Pointer fields tell "not sent" (nil) apart from a real value. For create, the
// handler requires the ones that matter; for update, nil means "leave unchanged".
type restaurantRequest struct {
	Name             *string  `json:"name"`
	Address          *string  `json:"address"`
	Lat              *float64 `json:"lat"`
	Lng              *float64 `json:"lng"`
	DeliveryRadiusM  *int     `json:"delivery_radius_m"`
	DeliveryFeeKurus *int     `json:"delivery_fee_kurus"`
	MinBasketKurus   *int     `json:"min_basket_kurus"`
	AcceptingOrders  *bool    `json:"accepting_orders"`
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
	// Not sent means free delivery and no minimum, the same as the database default.
	n := restaurants.New{
		Name: *req.Name, Address: *req.Address, Lat: *req.Lat, Lng: *req.Lng, DeliveryRadiusM: radius,
	}
	if req.DeliveryFeeKurus != nil {
		n.DeliveryFeeKurus = *req.DeliveryFeeKurus
	}
	if req.MinBasketKurus != nil {
		n.MinBasketKurus = *req.MinBasketKurus
	}
	created, err := s.restaurants.Create(r.Context(), owner, n)
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
		DeliveryFeeKurus: req.DeliveryFeeKurus, MinBasketKurus: req.MinBasketKurus,
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
	if req.DeliveryFeeKurus != nil {
		if f := *req.DeliveryFeeKurus; f < 0 || f > restaurants.MaxDeliveryFeeKurus {
			fields["delivery_fee_kurus"] = fmt.Sprintf("must be 0 to %d kuruş", restaurants.MaxDeliveryFeeKurus)
		}
	}
	if req.MinBasketKurus != nil {
		if m := *req.MinBasketKurus; m < 0 || m > restaurants.MaxMinBasketKurus {
			fields["min_basket_kurus"] = fmt.Sprintf("must be 0 to %d kuruş", restaurants.MaxMinBasketKurus)
		}
	}
	return fields
}

const (
	defaultPageSize = 20
	maxPageSize     = 50
)

type nearbyResponse struct {
	Restaurants []restaurants.Nearby `json:"restaurants"`
	// NextCursor is null on the last page. Otherwise send it back as ?cursor= to
	// get the next page.
	NextCursor *string `json:"next_cursor"`
}

// handleNearbyRestaurants answers GET /restaurants?lat=..&lng=..[&limit=..][&cursor=..]
// with the restaurants that deliver to that point, closest first.
func (s *Server) handleNearbyRestaurants(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	fields := map[string]string{}

	lat, latErr := strconv.ParseFloat(q.Get("lat"), 64)
	lng, lngErr := strconv.ParseFloat(q.Get("lng"), 64)
	switch {
	case latErr != nil || lngErr != nil:
		fields["location"] = "lat and lng query parameters are required numbers"
	case !restaurants.InIstanbul(lat, lng):
		fields["location"] = "must be inside Istanbul"
	}

	limit := defaultPageSize
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxPageSize {
			fields["limit"] = fmt.Sprintf("must be 1 to %d", maxPageSize)
		}
		limit = n
	}

	var after *restaurants.Cursor
	if v := q.Get("cursor"); v != "" {
		c, err := restaurants.DecodeCursor(v)
		if err != nil {
			fields["cursor"] = "is not a cursor this API returned"
		}
		after = &c
	}

	if len(fields) > 0 {
		writeValidationError(w, r, fields)
		return
	}

	page, next, err := s.restaurants.Near(r.Context(), lat, lng, limit, after)
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	resp := nearbyResponse{Restaurants: page}
	if resp.Restaurants == nil {
		resp.Restaurants = []restaurants.Nearby{} // [] rather than null in JSON
	}
	if next != nil {
		c := next.Encode()
		resp.NextCursor = &c
	}
	writeJSON(w, http.StatusOK, resp)
}

package httpapi

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/atalaymudanyali/quickbytes/internal/addresses"
	"github.com/atalaymudanyali/quickbytes/internal/orders"
	"github.com/atalaymudanyali/quickbytes/internal/users"
)

type placeOrderRequest struct {
	AddressID *uuid.UUID `json:"address_id"`
}

// orderDetail is one order with its status history, for the endpoints that show
// a single order.
type orderDetail struct {
	orders.Order
	History []orders.HistoryEntry `json:"history"`
}

type orderList struct {
	Orders []orders.Order `json:"orders"`
}

// handlePlaceOrder turns the customer's cart into an order (checkout).
func (s *Server) handlePlaceOrder(w http.ResponseWriter, r *http.Request) {
	var req placeOrderRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.AddressID == nil {
		writeValidationError(w, r, map[string]string{"address_id": "is required"})
		return
	}
	customer, ok := s.customerProfile(w, r)
	if !ok {
		return
	}

	o, err := s.orders.Place(r.Context(), customer, *req.AddressID)
	var below *orders.BelowMinimumError
	switch {
	case err == nil:
		s.writeOrder(w, r, http.StatusCreated, o)
	case errors.Is(err, addresses.ErrNotFound):
		writeValidationError(w, r, map[string]string{"address_id": "no such address"})
	case errors.Is(err, orders.ErrCartEmpty):
		writeError(w, r, http.StatusConflict, "cart_empty", "your cart is empty")
	case errors.Is(err, orders.ErrCartHasProblems):
		writeError(w, r, http.StatusConflict, "cart_has_problems",
			"some items in your cart can't be ordered as they are; check your cart")
	case errors.Is(err, orders.ErrCartChanged):
		writeError(w, r, http.StatusConflict, "cart_changed",
			"your cart changed while you were checking out; check it and try again")
	case errors.Is(err, orders.ErrRestaurantClosed):
		writeError(w, r, http.StatusConflict, "restaurant_not_accepting_orders",
			"this restaurant is not taking orders right now")
	case errors.As(err, &below):
		writeError(w, r, http.StatusConflict, "below_minimum",
			fmt.Sprintf("the minimum basket is %s; add %s more", lira(below.MinBasketKurus), lira(below.KurusToMinimum)))
	default:
		s.writeInternalError(w, r, err)
	}
}

func (s *Server) handleListMyOrders(w http.ResponseWriter, r *http.Request) {
	customer, ok := s.customerProfile(w, r)
	if !ok {
		return
	}
	list, err := s.orders.ListForCustomer(r.Context(), customer)
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, orderList{Orders: list})
}

func (s *Server) handleGetOrder(w http.ResponseWriter, r *http.Request) {
	id, ok := orderID(w, r)
	if !ok {
		return
	}
	customer, ok := s.customerProfile(w, r)
	if !ok {
		return
	}
	o, err := s.orders.Get(r.Context(), id, orders.Actor{Role: users.RoleCustomer, ProfileID: customer})
	if err != nil {
		s.writeOrderError(w, r, err, "")
		return
	}
	s.writeOrder(w, r, http.StatusOK, o)
}

func (s *Server) handleCancelOrder(w http.ResponseWriter, r *http.Request) {
	id, ok := orderID(w, r)
	if !ok {
		return
	}
	customer, ok := s.customerProfile(w, r)
	if !ok {
		return
	}
	s.moveOrder(w, r, id, orders.Cancel, orders.Actor{Role: users.RoleCustomer, ProfileID: customer},
		"an order can only be cancelled before the restaurant accepts it")
}

// moveOrder makes one move on an order and answers with the order as it is
// afterwards. refused is the message for a move the order's status doesn't
// allow.
func (s *Server) moveOrder(w http.ResponseWriter, r *http.Request, id uuid.UUID, a orders.Action, by orders.Actor, refused string) {
	if _, err := s.orders.Transition(r.Context(), id, a, by); err != nil {
		s.writeOrderError(w, r, err, refused)
		return
	}
	o, err := s.orders.Get(r.Context(), id, by)
	if err != nil {
		s.writeOrderError(w, r, err, refused)
		return
	}
	s.writeOrder(w, r, http.StatusOK, o)
}

// writeOrder answers with one order and its history.
func (s *Server) writeOrder(w http.ResponseWriter, r *http.Request, status int, o orders.Order) {
	h, err := s.orders.History(r.Context(), o.ID)
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	writeJSON(w, status, orderDetail{Order: o, History: h})
}

func (s *Server) writeOrderError(w http.ResponseWriter, r *http.Request, err error, refused string) {
	switch {
	case errors.Is(err, orders.ErrNotFound):
		writeOrderNotFound(w, r)
	case errors.Is(err, orders.ErrInvalidTransition):
		writeError(w, r, http.StatusConflict, "invalid_transition", refused)
	case errors.Is(err, orders.ErrNotAllowed):
		// The routes only offer each role its own buttons, so this means a bug.
		writeError(w, r, http.StatusForbidden, "not_allowed", "you can't do this to an order")
	default:
		s.writeInternalError(w, r, err)
	}
}

func orderID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	return pathID(w, r, "order_id", "order_not_found", "no such order")
}

func writeOrderNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusNotFound, "order_not_found", "no such order")
}

// lira formats kuruş for messages: 26000 → "260,00 TL".
func lira(kurus int) string {
	return fmt.Sprintf("%d,%02d TL", kurus/100, kurus%100)
}

// --- The restaurant's side ------------------------------------------------------

// handleKitchenOrders lists a restaurant's paid, accepted and ready orders,
// oldest first: the queue the kitchen works through.
func (s *Server) handleKitchenOrders(w http.ResponseWriter, r *http.Request) {
	restaurantID, ok := restaurantID(w, r)
	if !ok {
		return
	}
	owner, ok := s.restaurantProfile(w, r)
	if !ok {
		return
	}
	list, err := s.orders.ListForKitchen(r.Context(), restaurantID, owner)
	if errors.Is(err, orders.ErrRestaurantNotFound) {
		writeError(w, r, http.StatusNotFound, "restaurant_not_found", "no such restaurant")
		return
	}
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, orderList{Orders: list})
}

func (s *Server) handleAcceptOrder(w http.ResponseWriter, r *http.Request) {
	s.restaurantMove(w, r, orders.Accept, "only a paid order can be accepted")
}

func (s *Server) handleRejectOrder(w http.ResponseWriter, r *http.Request) {
	s.restaurantMove(w, r, orders.Reject, "only a paid or accepted order can be rejected")
}

func (s *Server) handleOrderReady(w http.ResponseWriter, r *http.Request) {
	s.restaurantMove(w, r, orders.MarkReady, "only an accepted order can be marked ready")
}

// restaurantMove is one of the restaurant's buttons on an order. The owner check
// happens inside the move, with the order locked: an order of a restaurant you
// don't own is 404, like any order that isn't yours.
func (s *Server) restaurantMove(w http.ResponseWriter, r *http.Request, a orders.Action, refused string) {
	id, ok := orderID(w, r)
	if !ok {
		return
	}
	owner, ok := s.restaurantProfile(w, r)
	if !ok {
		return
	}
	s.moveOrder(w, r, id, a, orders.Actor{Role: users.RoleRestaurant, ProfileID: owner}, refused)
}

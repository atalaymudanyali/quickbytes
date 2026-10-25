package httpapi

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/atalaymudanyali/quickbytes/internal/carts"
	"github.com/atalaymudanyali/quickbytes/internal/menus"
)

// Every cart endpoint except DELETE /cart answers with the whole priced cart, so
// the app redraws the basket from one response instead of asking again.

type addToCartRequest struct {
	MenuItemID *uuid.UUID  `json:"menu_item_id"`
	Quantity   *int        `json:"quantity"`
	ChoiceIDs  []uuid.UUID `json:"choice_ids"`
}

type changeCartItemRequest struct {
	Quantity  *int         `json:"quantity"`
	ChoiceIDs *[]uuid.UUID `json:"choice_ids"`
}

func (s *Server) handleGetCart(w http.ResponseWriter, r *http.Request) {
	customer, ok := s.customerProfile(w, r)
	if !ok {
		return
	}
	s.writeCart(w, r, customer)
}

func (s *Server) handleAddToCart(w http.ResponseWriter, r *http.Request) {
	var req addToCartRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	fields := map[string]string{}
	if req.MenuItemID == nil {
		fields["menu_item_id"] = "is required"
	}
	quantity := 1 // not sent: one
	if req.Quantity != nil {
		quantity = *req.Quantity
	}
	validateQuantity(quantity, fields)
	if len(fields) > 0 {
		writeValidationError(w, r, fields)
		return
	}

	customer, ok := s.customerProfile(w, r)
	if !ok {
		return
	}
	if _, err := s.carts.Add(r.Context(), customer, *req.MenuItemID, quantity, req.ChoiceIDs); err != nil {
		s.writeCartError(w, r, err)
		return
	}
	s.writeCart(w, r, customer)
}

func (s *Server) handleChangeCartItem(w http.ResponseWriter, r *http.Request) {
	lineID, ok := pathID(w, r, "item_id", "cart_item_not_found", "no such item in your cart")
	if !ok {
		return
	}
	var req changeCartItemRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	fields := map[string]string{}
	if req.Quantity != nil {
		validateQuantity(*req.Quantity, fields)
	}
	if req.Quantity == nil && req.ChoiceIDs == nil {
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
	_, err := s.carts.Change(r.Context(), customer, lineID, carts.LinePatch{Quantity: req.Quantity, ChoiceIDs: req.ChoiceIDs})
	if err != nil {
		s.writeCartError(w, r, err)
		return
	}
	s.writeCart(w, r, customer)
}

func (s *Server) handleRemoveCartItem(w http.ResponseWriter, r *http.Request) {
	lineID, ok := pathID(w, r, "item_id", "cart_item_not_found", "no such item in your cart")
	if !ok {
		return
	}
	customer, ok := s.customerProfile(w, r)
	if !ok {
		return
	}
	if err := s.carts.Remove(r.Context(), customer, lineID); err != nil {
		s.writeCartError(w, r, err)
		return
	}
	s.writeCart(w, r, customer)
}

func (s *Server) handleClearCart(w http.ResponseWriter, r *http.Request) {
	customer, ok := s.customerProfile(w, r)
	if !ok {
		return
	}
	if err := s.carts.Clear(r.Context(), customer); err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) writeCart(w http.ResponseWriter, r *http.Request, customer uuid.UUID) {
	c, err := s.carts.Get(r.Context(), customer)
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// writeCartError turns the cart store's errors into responses. Quantity 0 is a
// validation error rather than "remove": removing is what DELETE is for.
func (s *Server) writeCartError(w http.ResponseWriter, r *http.Request, err error) {
	var mismatch *carts.MismatchError
	var choice *menus.ChoiceError
	switch {
	case errors.As(err, &mismatch):
		msg := "your cart has dishes from another restaurant; clear it to start a new one"
		if other, getErr := s.restaurants.Get(r.Context(), mismatch.CartRestaurantID); getErr == nil {
			msg = "your cart has dishes from " + other.Name + "; clear it to start a new one"
		}
		writeError(w, r, http.StatusConflict, "cart_restaurant_mismatch", msg)
	case errors.As(err, &choice):
		// The choice error's own code (too_few_picks, ...) and which group or
		// choice it's about, so the app can scroll to it.
		writeJSON(w, http.StatusUnprocessableEntity, errorBody{Error: errorDetail{
			Code: string(choice.Code), Message: choice.Message,
			GroupID: choice.GroupID, ChoiceID: choice.ChoiceID,
			RequestID: requestIDFrom(r.Context()),
		}})
	case errors.Is(err, carts.ErrItemNotFound):
		writeError(w, r, http.StatusNotFound, "item_not_found", "no such dish on the menu")
	case errors.Is(err, carts.ErrLineNotFound):
		writeError(w, r, http.StatusNotFound, "cart_item_not_found", "no such item in your cart")
	case errors.Is(err, carts.ErrItemUnavailable):
		writeError(w, r, http.StatusConflict, "item_unavailable", "this dish is not available right now")
	case errors.Is(err, carts.ErrRestaurantClosed):
		writeError(w, r, http.StatusConflict, "restaurant_not_accepting_orders", "this restaurant is not taking orders right now")
	case errors.Is(err, carts.ErrCartFull):
		writeError(w, r, http.StatusConflict, "cart_full",
			fmt.Sprintf("a cart holds up to %d different items", carts.MaxLines))
	case errors.Is(err, carts.ErrQuantityTooHigh):
		writeValidationError(w, r, map[string]string{
			"quantity": fmt.Sprintf("would make more than %d of this item", carts.MaxQuantity),
		})
	default:
		s.writeInternalError(w, r, err)
	}
}

func validateQuantity(q int, fields map[string]string) {
	if q < 1 || q > carts.MaxQuantity {
		fields["quantity"] = fmt.Sprintf("must be 1 to %d", carts.MaxQuantity)
	}
}

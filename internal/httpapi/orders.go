package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atalaymudanyali/quickbytes/internal/addresses"
	"github.com/atalaymudanyali/quickbytes/internal/idempotency"
	"github.com/atalaymudanyali/quickbytes/internal/orders"
	"github.com/atalaymudanyali/quickbytes/internal/payments"
	"github.com/atalaymudanyali/quickbytes/internal/users"
)

type placeOrderRequest struct {
	AddressID *uuid.UUID `json:"address_id"`
	// CardNumber is one of the payment provider's test card numbers. It's
	// sent to the provider once, to authorize, and never stored or logged:
	// only its last four digits are kept.
	CardNumber *string `json:"card_number"`
}

// orderDetail is one order with its status history, for the endpoints that show
// a single order. Payment is shown to the customer only: the card is theirs.
type orderDetail struct {
	orders.Order
	History []orders.HistoryEntry `json:"history"`
	Payment *paymentDetail        `json:"payment,omitempty"`
}

// paymentDetail is the order's payment with its ledger.
type paymentDetail struct {
	payments.Payment
	Events []payments.Event `json:"events"`
}

// validCardNumber accepts 12 to 19 digits, the lengths card numbers come in.
// Which numbers work is the provider's business, not ours.
func validCardNumber(n string) bool {
	if len(n) < 12 || len(n) > 19 {
		return false
	}
	for _, c := range n {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

type orderList struct {
	Orders []orders.Order `json:"orders"`
}

// handlePlaceOrder turns the customer's cart into an order and pays for it
// (checkout). It runs in three steps, because the provider is another company
// and can't join our database transaction:
//
//	A. One transaction: place the order, write its payment as pending, and
//	   store a first answer for the Idempotency-Key (the order, placed, its
//	   payment pending). From here on the order and the payment exist, and a
//	   retry gets this answer instead of a misleading "your cart is empty".
//	B. No transaction: ask the provider to authorize the card (at most 5
//	   seconds).
//	C. One transaction: record the provider's answer, move the order to match
//	   (paid, or cancelled with payment_failed), and replace the stored answer
//	   with the final one.
//
// If C fails, the first answer stands and is sent: true when it was stored.
// The reconcile loop finishes the payment.
func (s *Server) handlePlaceOrder(w http.ResponseWriter, r *http.Request) {
	var req placeOrderRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	problems := map[string]string{}
	if req.AddressID == nil {
		problems["address_id"] = "is required"
	}
	if req.CardNumber == nil {
		problems["card_number"] = "is required"
	} else if !validCardNumber(*req.CardNumber) {
		problems["card_number"] = "must be 12 to 19 digits"
	}
	if len(problems) > 0 {
		writeValidationError(w, r, problems)
		return
	}
	customer, ok := s.customerProfile(w, r)
	if !ok {
		return
	}
	card := *req.CardNumber

	// A.
	var pay payments.Payment
	var answer []byte
	o, err := s.orders.PlaceAnd(r.Context(), customer, *req.AddressID, func(ctx context.Context, tx pgx.Tx, o orders.Order) error {
		var err error
		if pay, err = payments.CreateIn(ctx, tx, o.ID, o.TotalKurus, card[len(card)-4:], payments.AuthorizeGrace); err != nil {
			return err
		}
		if answer, err = checkoutAnswer(ctx, tx, o.ID); err != nil {
			return err
		}
		return saveAnswerIn(ctx, tx, http.StatusCreated, answer)
	})
	if err != nil {
		s.writeCheckoutError(w, r, err)
		return
	}

	// B and C.
	var final []byte
	_, err = s.payments.Authorize(r.Context(), pay, card, func(ctx context.Context, tx pgx.Tx, _ payments.Payment) error {
		var err error
		if final, err = checkoutAnswer(ctx, tx, o.ID); err != nil {
			return err
		}
		return replaceAnswerIn(ctx, tx, http.StatusCreated, final)
	})
	if err != nil {
		s.logger.Error("record checkout payment; answering with the first answer",
			"request_id", requestIDFrom(r.Context()), "order_id", o.ID, "err", err)
	} else {
		answer = final
	}
	writeJSONBody(w, http.StatusCreated, answer)
}

// checkoutAnswer is checkout's answer: the order with its history and its
// payment, read inside tx so it includes what tx did.
func checkoutAnswer(ctx context.Context, tx pgx.Tx, orderID uuid.UUID) ([]byte, error) {
	o, err := orders.GetIn(ctx, tx, orderID)
	if err != nil {
		return nil, err
	}
	h, err := orders.HistoryIn(ctx, tx, orderID)
	if err != nil {
		return nil, err
	}
	p, err := payments.ForOrderIn(ctx, tx, orderID)
	if err != nil {
		return nil, err
	}
	ev, err := payments.EventsIn(ctx, tx, p.ID)
	if err != nil {
		return nil, err
	}
	return jsonBody(orderDetail{Order: o, History: h, Payment: &paymentDetail{Payment: p, Events: ev}})
}

// writeCheckoutError answers the ways placing an order can be refused.
func (s *Server) writeCheckoutError(w http.ResponseWriter, r *http.Request, err error) {
	var below *orders.BelowMinimumError
	switch {
	case errors.Is(err, idempotency.ErrClaimLost):
		// This attempt ran so long that a retry took its key over. Nothing was
		// ordered here; the retry's answer is the one that counts.
		w.Header().Set("Retry-After", "1")
		writeError(w, r, http.StatusConflict, "idempotency_in_progress",
			"a newer attempt with this Idempotency-Key is being handled; retry shortly to get its answer")
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
	s.writeOrder(w, r, http.StatusOK, o, true)
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

// moveOrder makes one move on an order, then lets the money follow (capture
// on accept, void or refund on a cancel), and answers with the order as it is
// afterwards. refused is the message for a move the order's status doesn't
// allow.
//
// The move stands even when the money step doesn't finish (the provider
// didn't answer): the restaurant did accept. The move noted the payment as
// due in its own commit, so the reconcile loop finishes the step.
func (s *Server) moveOrder(w http.ResponseWriter, r *http.Request, id uuid.UUID, a orders.Action, by orders.Actor, refused string) {
	if _, err := s.orders.Transition(r.Context(), id, a, by); err != nil {
		s.writeOrderError(w, r, err, refused)
		return
	}
	if err := s.payments.Sync(r.Context(), id); err != nil {
		s.logger.Warn("payment step after an order move didn't finish; the reconcile loop will retry",
			"request_id", requestIDFrom(r.Context()), "order_id", id, "action", a, "err", err)
	}
	o, err := s.orders.Get(r.Context(), id, by)
	if err != nil {
		s.writeOrderError(w, r, err, refused)
		return
	}
	s.writeOrder(w, r, http.StatusOK, o, by.Role == users.RoleCustomer)
}

// writeOrder answers with one order and its history, and its payment when
// withPayment (for the customer).
func (s *Server) writeOrder(w http.ResponseWriter, r *http.Request, status int, o orders.Order, withPayment bool) {
	h, err := s.orders.History(r.Context(), o.ID)
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	d := orderDetail{Order: o, History: h}
	if withPayment {
		p, err := s.payments.ForOrder(r.Context(), o.ID)
		switch {
		case err == nil:
			ev, err := s.payments.Events(r.Context(), p.ID)
			if err != nil {
				s.writeInternalError(w, r, err)
				return
			}
			d.Payment = &paymentDetail{Payment: p, Events: ev}
		case !errors.Is(err, payments.ErrNotFound): // orders from before payments have none
			s.writeInternalError(w, r, err)
			return
		}
	}
	writeJSON(w, status, d)
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

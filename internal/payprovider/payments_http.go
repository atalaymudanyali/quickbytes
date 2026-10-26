package payprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atalaymudanyali/quickbytes/internal/idempotency"
)

// authorizeRequest is the body of POST /v1/payments.
type authorizeRequest struct {
	AmountKurus int     `json:"amount_kurus"`
	CardNumber  string  `json:"card_number"`
	Reference   *string `json:"reference"`
}

// refusalBody is a refused payment's answer: the usual error, plus the payment
// as it is now (like Stripe, which attaches the payment to a card error). The
// merchant learns why, and still has the payment's ID to look it up later.
type refusalBody struct {
	Error   errorDetail `json:"error"`
	Payment Payment     `json:"payment"`
}

// handleAuthorize starts a payment: POST /v1/payments.
//
// 201 with the payment when it's authorized (or processing, for the slow
// card). 402 Payment Required when the card is declined: the payment exists,
// as failed, and comes back inside the error.
func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	var req authorizeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	in := AuthorizeInput{AmountKurus: req.AmountKurus, CardNumber: req.CardNumber, Reference: req.Reference}

	var (
		status int
		answer []byte
	)
	_, err := s.payments.AuthorizeAnd(r.Context(), merchantFrom(r.Context()).ID, in,
		func(ctx context.Context, tx pgx.Tx, p Payment) error {
			var err error
			if status, answer, err = authorizeAnswer(r, p); err != nil {
				return err
			}
			return saveAnswerIn(ctx, tx, status, answer)
		})
	switch {
	case err == nil:
		writeJSONBody(w, status, answer)
	case errors.Is(err, ErrInvalidAmount):
		writeError(w, r, http.StatusUnprocessableEntity, "amount_invalid",
			fmt.Sprintf("amount_kurus must be a whole number of kuruş from 1 to %d", MaxAmountKurus))
	case errors.Is(err, ErrInvalidReference):
		writeError(w, r, http.StatusUnprocessableEntity, "reference_invalid",
			"reference must be 1 to 255 characters, or left out")
	case errors.Is(err, ErrUnknownCard):
		writeError(w, r, http.StatusBadRequest, "unknown_test_card",
			"this provider only accepts its test card numbers (see the README)")
	default:
		s.writeMoveError(w, r, err)
	}
}

// authorizeAnswer is what an authorize request gets back for payment p.
func authorizeAnswer(r *http.Request, p Payment) (int, []byte, error) {
	if p.Status != StatusFailed {
		body, err := jsonBody(p)
		return http.StatusCreated, body, err
	}
	code := *p.DeclineCode
	message := "the card was declined"
	if code == DeclineInsufficientFunds {
		message = "the card doesn't have enough money for this amount"
	}
	body, err := jsonBody(refusalBody{
		Error:   errorDetail{Code: code, Message: message, RequestID: requestIDFrom(r.Context())},
		Payment: p,
	})
	return http.StatusPaymentRequired, body, err
}

// handleMove is POST /v1/payments/{payment_id}/capture, /void or /refund: 200
// with the changed payment, 409 when the payment's status doesn't allow it
// (with the payment as it is), 404 for a payment this merchant doesn't have.
func (s *Server) handleMove(action Action) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := paymentID(w, r)
		if !ok {
			return
		}
		var answer []byte
		_, err := s.payments.MoveAnd(r.Context(), merchantFrom(r.Context()).ID, id, action,
			func(ctx context.Context, tx pgx.Tx, p Payment) error {
				var err error
				if answer, err = jsonBody(p); err != nil {
					return err
				}
				return saveAnswerIn(ctx, tx, http.StatusOK, answer)
			})
		if err != nil {
			s.writeMoveError(w, r, err)
			return
		}
		writeJSONBody(w, http.StatusOK, answer)
	}
}

// writeMoveError answers the errors any payment change can end in.
func (s *Server) writeMoveError(w http.ResponseWriter, r *http.Request, err error) {
	var se *StateError
	switch {
	case errors.As(err, &se):
		writeJSON(w, http.StatusConflict, refusalBody{
			Error:   errorDetail{Code: "invalid_state", Message: se.Error(), RequestID: requestIDFrom(r.Context())},
			Payment: se.Payment,
		})
	case errors.Is(err, ErrNotFound):
		writeError(w, r, http.StatusNotFound, "payment_not_found", "no such payment")
	case errors.Is(err, idempotency.ErrClaimLost):
		// This attempt ran so long that a retry took its key over. Nothing was
		// committed here; the retry's answer is the one that counts.
		w.Header().Set("Retry-After", "1")
		writeError(w, r, http.StatusConflict, "idempotency_in_progress",
			"a newer attempt with this Idempotency-Key is being handled; retry shortly to get its answer")
	default:
		s.writeInternalError(w, r, err)
	}
}

// handleGetPayment is GET /v1/payments/{payment_id}: the payment as it is now.
// A merchant that doesn't know how a call ended (it timed out) asks this
// instead of trying again.
func (s *Server) handleGetPayment(w http.ResponseWriter, r *http.Request) {
	id, ok := paymentID(w, r)
	if !ok {
		return
	}
	p, err := s.payments.Get(r.Context(), merchantFrom(r.Context()).ID, id)
	if err != nil {
		s.writeMoveError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// paymentID reads the {payment_id} in the path. Something that isn't a UUID
// can't be a payment, so it's a 404 like an unknown ID.
func paymentID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("payment_id"))
	if err != nil {
		writeError(w, r, http.StatusNotFound, "payment_not_found", "no such payment")
		return uuid.Nil, false
	}
	return id, true
}

// decodeJSON reads a JSON body into dst, refusing unknown fields (a typo like
// "amount" instead of "amount_kurus" is an error, not a silent zero). It writes
// the error answer itself and reports whether decoding worked.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
		writeError(w, r, http.StatusUnsupportedMediaType, "unsupported_media_type",
			`the body must be JSON, sent with "Content-Type: application/json"`)
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_json", "request body is not valid JSON for this endpoint: "+err.Error())
		return false
	}
	if dec.More() {
		writeError(w, r, http.StatusBadRequest, "invalid_json", "request body must contain a single JSON object")
		return false
	}
	return true
}

// jsonBody encodes v exactly as writeJSON would send it, for an answer that has
// to be stored before it's sent.
func jsonBody(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := json.NewEncoder(&b).Encode(v); err != nil {
		return nil, fmt.Errorf("encode response: %w", err)
	}
	return b.Bytes(), nil
}

// writeJSONBody sends an already encoded JSON body.
func writeJSONBody(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

package payments

import (
	"context"
	"errors"
	"fmt"
)

// Provider is everything the API needs from a payment company, in our own
// words. It works like a wall socket: the rest of the API only knows this
// shape, and each company gets a plug (an adapter) that fits it. Today the one
// plug is PayProvider, for our mock service; going live with Stripe or iyzico
// means writing one more plug, and nothing else changes.
//
// Every call that moves money takes an idempotency key. The caller makes it
// from our payment ID and the action ("<payment_id>:capture") and sends the
// same one on every retry, so the provider moves the money at most once
// however many times we ask.
//
// Every answer is one of three kinds:
//
//   - a ProviderPayment, nil error: news about the payment. Also when a card
//     is declined (status failed) or a move wasn't possible (the payment as
//     it is): either way we now know where the payment stands.
//   - a *RefusedError: a final no to the request itself (not a card the
//     provider takes, no such payment). Nothing happened, and asking again
//     won't change that.
//   - any other error (it wraps ErrNoAnswer): we don't know what happened.
//     Maybe the money moved, maybe not. Never act as if it failed: find out.
type Provider interface {
	// Authorize holds an amount on a card. Only checkout calls it, once per
	// payment: it's the only call that needs the card number.
	Authorize(ctx context.Context, req AuthorizeRequest) (ProviderPayment, error)
	// Capture takes the held amount.
	Capture(ctx context.Context, key, providerID string) (ProviderPayment, error)
	// Void drops the hold; nothing is taken.
	Void(ctx context.Context, key, providerID string) (ProviderPayment, error)
	// Refund gives the whole captured amount back.
	Refund(ctx context.Context, key, providerID string) (ProviderPayment, error)
	// Get asks how a payment stands now.
	Get(ctx context.Context, providerID string) (ProviderPayment, error)
	// FindByReference returns the payments made with our reference (the order
	// ID), newest first: how we find a payment whose authorize timed out, so
	// we never learned its ID.
	FindByReference(ctx context.Context, reference string) ([]ProviderPayment, error)
}

// AuthorizeRequest is what checkout sends to hold the order's amount.
type AuthorizeRequest struct {
	Key         string // idempotency key
	AmountKurus int
	CardNumber  string // used for this call only, never stored
	Reference   string // the order ID
}

// ProviderPayment is a payment as the provider reported it, already in our
// words: Status is one of ours (processing, authorized, captured, voided,
// refunded or failed), DeclineCode one of our decline codes.
type ProviderPayment struct {
	ID          string
	Status      Status
	DeclineCode *string
	AmountKurus int
}

// ErrNoAnswer means a call ended without telling us what happened: a timeout,
// a dropped connection, a server error, the provider still busy with the same
// key. The money may or may not have moved.
var ErrNoAnswer = errors.New("no answer from the payment provider")

// RefusedError is the provider refusing the request itself: it looked at it
// and said no, so nothing happened.
type RefusedError struct {
	HTTPStatus int
	Code       string // the provider's error code, like unknown_test_card
	Message    string
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("payment provider refused the request (%d %s): %s", e.HTTPStatus, e.Code, e.Message)
}

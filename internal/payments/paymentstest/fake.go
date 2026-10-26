// Package paymentstest has a fake payment provider for tests: it fits the
// payments.Provider socket, keeps its payments in memory, and treats the
// provider's test cards the way the real one does. Tests of the API use it
// instead of the network; internal/payments also tests against the real
// provider over HTTP.
package paymentstest

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/google/uuid"

	"github.com/atalaymudanyali/quickbytes/internal/payments"
)

// The provider's test cards (see the README).
const (
	CardApprove      = "4242424242424242"
	CardDecline      = "4000000000000002"
	CardInsufficient = "4000000000009995"
	CardSlow         = "4000000000007777" // processing
	CardHang         = "4000000000003333" // held, but the call ends with no answer
)

// Fake is an in-memory provider. Like the real one, every money move is
// idempotent: the same key gets the same answer and moves nothing again.
type Fake struct {
	// BeforeCall, if set, runs at the start of every call with its name
	// ("authorize", "capture", ...). Tests use it to hold a call open (block
	// on a channel). When it returns an error, the call ends with it and does
	// nothing: return payments.ErrNoAnswer to play a timeout.
	BeforeCall func(call string) error

	mu       sync.Mutex
	payments map[string]*payments.ProviderPayment
	refs     map[string][]string // reference → payment IDs, oldest first
	answers  map[string]answer   // idempotency key → the first answer
	calls    map[string]int
}

type answer struct {
	p   payments.ProviderPayment
	err error
}

func NewFake() *Fake {
	return &Fake{
		payments: map[string]*payments.ProviderPayment{},
		refs:     map[string][]string{},
		answers:  map[string]answer{},
		calls:    map[string]int{},
	}
}

var _ payments.Provider = (*Fake)(nil)

// Calls says how many times a call was made ("authorize", ...), replays and
// calls failed by BeforeCall included.
func (f *Fake) Calls(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[call]
}

// Payments returns every payment the fake holds for a reference: how a test
// checks that money was held once, not twice.
func (f *Fake) Payments(reference string) []payments.ProviderPayment {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []payments.ProviderPayment
	for _, id := range f.refs[reference] {
		out = append(out, *f.payments[id])
	}
	return out
}

func (f *Fake) start(call string) error {
	f.mu.Lock()
	f.calls[call]++
	f.mu.Unlock()
	if f.BeforeCall != nil {
		return f.BeforeCall(call)
	}
	return nil
}

func (f *Fake) Authorize(_ context.Context, req payments.AuthorizeRequest) (payments.ProviderPayment, error) {
	if err := f.start("authorize"); err != nil {
		return payments.ProviderPayment{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if a, ok := f.answers[req.Key]; ok {
		return a.p, a.err
	}

	p := payments.ProviderPayment{ID: "pay_" + uuid.NewString(), AmountKurus: req.AmountKurus}
	var err error
	switch req.CardNumber {
	case CardApprove:
		p.Status = payments.Authorized
	case CardDecline:
		p.Status, p.DeclineCode = payments.Failed, ptr(payments.CardDeclined)
	case CardInsufficient:
		p.Status, p.DeclineCode = payments.Failed, ptr(payments.InsufficientFunds)
	case CardSlow:
		p.Status = payments.Processing
	case CardHang:
		// The money is held, but the answer never reaches us.
		p.Status = payments.Authorized
		err = fmt.Errorf("%w: fake timeout", payments.ErrNoAnswer)
	default:
		err = &payments.RefusedError{HTTPStatus: http.StatusBadRequest, Code: "unknown_test_card", Message: "not a test card"}
		f.answers[req.Key] = answer{err: err}
		return payments.ProviderPayment{}, err
	}
	f.payments[p.ID] = &p
	f.refs[req.Reference] = append(f.refs[req.Reference], p.ID)
	// A retry of the hung call gets the real answer, like the real provider
	// (which stored it before hanging).
	f.answers[req.Key] = answer{p: p}
	if err != nil {
		return payments.ProviderPayment{}, err
	}
	return p, nil
}

// moves is the real provider's list of allowed moves.
var moves = map[string]struct{ from, to payments.Status }{
	"capture": {payments.Authorized, payments.Captured},
	"void":    {payments.Authorized, payments.Voided},
	"refund":  {payments.Captured, payments.Refunded},
}

func (f *Fake) move(call, key, id string) (payments.ProviderPayment, error) {
	if err := f.start(call); err != nil {
		return payments.ProviderPayment{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if a, ok := f.answers[key]; ok {
		return a.p, a.err
	}
	p, ok := f.payments[id]
	if !ok {
		return payments.ProviderPayment{}, &payments.RefusedError{HTTPStatus: http.StatusNotFound, Code: "payment_not_found"}
	}
	// A move from the wrong status changes nothing and answers with the
	// payment as it is, like the real provider's 409.
	if m := moves[call]; p.Status == m.from {
		p.Status = m.to
	}
	f.answers[key] = answer{p: *p}
	return *p, nil
}

func (f *Fake) Capture(_ context.Context, key, id string) (payments.ProviderPayment, error) {
	return f.move("capture", key, id)
}

func (f *Fake) Void(_ context.Context, key, id string) (payments.ProviderPayment, error) {
	return f.move("void", key, id)
}

func (f *Fake) Refund(_ context.Context, key, id string) (payments.ProviderPayment, error) {
	return f.move("refund", key, id)
}

func (f *Fake) Get(_ context.Context, id string) (payments.ProviderPayment, error) {
	if err := f.start("get"); err != nil {
		return payments.ProviderPayment{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.payments[id]
	if !ok {
		return payments.ProviderPayment{}, &payments.RefusedError{HTTPStatus: http.StatusNotFound, Code: "payment_not_found"}
	}
	return *p, nil
}

func (f *Fake) FindByReference(_ context.Context, reference string) ([]payments.ProviderPayment, error) {
	if err := f.start("find"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := f.refs[reference]
	out := make([]payments.ProviderPayment, 0, len(ids))
	for i := len(ids) - 1; i >= 0; i-- { // newest first
		out = append(out, *f.payments[ids[i]])
	}
	return out, nil
}

// Settle finishes a processing payment, as the slow card's bank finally
// answering: authorized when approve is true, failed (card_declined) when not.
func (f *Fake) Settle(id string, approve bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.payments[id]
	if approve {
		p.Status = payments.Authorized
	} else {
		p.Status, p.DeclineCode = payments.Failed, ptr(payments.CardDeclined)
	}
}

func ptr[T any](v T) *T { return &v }

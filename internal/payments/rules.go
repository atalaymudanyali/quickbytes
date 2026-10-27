package payments

import "github.com/atalaymudanyali/quickbytes/internal/orders"

// Step is the one thing to do next to bring a payment and its order in line.
type Step string

const (
	Nothing Step = "nothing"
	// Provider calls. None of them is ever an authorize: that needs the card
	// number, which only checkout has, once.
	Capture Step = "capture"
	Void    Step = "void"
	Refund  Step = "refund"
	Lookup  Step = "lookup" // find the payment by our reference: we don't know its provider ID
	Check   Step = "check"  // ask the provider for the payment's status by its ID
	// Order moves, when the payment has settled but the order doesn't show it.
	PayOrder  Step = "pay_order"
	FailOrder Step = "fail_order"
)

// nextStep works like a thermostat: it doesn't remember what happened, it
// compares where the order is (what should have happened to the money) with
// what we know about the payment, and names the step that closes the gap.
// Run it again after every step until it says Nothing.
//
// It's pure, so the whole table can be tested without a database or a
// provider.
func nextStep(order orders.Status, pay Status) Step {
	switch pay {
	case Pending, Unknown:
		return Lookup
	case Processing:
		return Check
	case Authorized:
		switch order {
		case orders.Placed:
			return PayOrder
		case orders.Accepted, orders.Ready, orders.PickedUp, orders.Delivered:
			return Capture // the restaurant committed: take the money
		case orders.Cancelled:
			return Void // drop the hold
		}
	case Captured:
		if order == orders.Cancelled {
			return Refund // taken, then cancelled (rejected after accepting)
		}
	case Failed:
		if order == orders.Placed {
			return FailOrder
		}
	}
	return Nothing
}

// rank orders our statuses from first to last. A payment only ever moves to a
// higher rank: a ratchet. Two statuses share a rank when they're two
// different endings of the same step (authorized or failed; captured or
// voided).
var rank = map[Status]int{
	Pending:    0,
	Unknown:    1,
	Processing: 2,
	Authorized: 3, Failed: 3,
	Captured: 4, Voided: 4,
	Refunded: 5,
}

// allowed says whether a payment we know as from may become to. News can
// come in any order (a webhook retried after a newer one, our own timed-out
// call after a webhook already told us), so older news must change nothing:
//
//   - the same status: allowed (nothing new, though it may bring the
//     provider's ID);
//   - out of failed, voided or refunded: never, they're final;
//   - otherwise only forward, to a higher rank. Backwards is old news;
//     sideways (authorized ↔ failed) contradicts what we know, and we keep
//     what we know.
//
// Like nextStep it's pure, so the whole grid is tested without a database.
func allowed(from, to Status) bool {
	switch {
	case from == to:
		return true
	case from == Failed || from == Voided || from == Refunded:
		return false
	default:
		return rank[to] > rank[from]
	}
}

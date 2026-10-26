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

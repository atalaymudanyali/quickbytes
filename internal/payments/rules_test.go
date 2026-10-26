package payments

import (
	"testing"

	"github.com/atalaymudanyali/quickbytes/internal/orders"
)

var allStatuses = []Status{Pending, Unknown, Processing, Authorized, Captured, Voided, Refunded, Failed}

// wantStep is the thermostat written out by hand, separately from nextStep, so
// a slip in either shows up as a failing test. Every pair not listed must be
// Nothing.
var wantStep = map[Status]map[orders.Status]Step{
	Authorized: {
		orders.Placed:   PayOrder,
		orders.Accepted: Capture, orders.Ready: Capture, orders.PickedUp: Capture, orders.Delivered: Capture,
		orders.Cancelled: Void,
	},
	Captured: {orders.Cancelled: Refund},
	Failed:   {orders.Placed: FailOrder},
}

// The full grid: every payment status × every order status.
func TestEveryPaymentAndOrderStatus(t *testing.T) {
	for _, pay := range allStatuses {
		for _, order := range orders.Statuses {
			t.Run(string(pay)+"/"+string(order), func(t *testing.T) {
				want, ok := wantStep[pay][order]
				switch {
				case pay == Pending || pay == Unknown:
					want = Lookup // whatever the order: first find out
				case pay == Processing:
					want = Check
				case !ok:
					want = Nothing
				}
				if got := nextStep(order, pay); got != want {
					t.Fatalf("got %q, want %q", got, want)
				}
			})
		}
	}
}

// Following the steps always ends: each provider call leads to a status whose
// next step is Nothing, given the order doesn't move meanwhile. (A loop that
// could keep capturing or voiding forever would be a disaster with money.)
func TestStepsSettle(t *testing.T) {
	after := map[Step]Status{Capture: Captured, Void: Voided, Refund: Refunded}
	for _, order := range orders.Statuses {
		for _, pay := range []Status{Authorized, Captured} {
			s := nextStep(order, pay)
			if to, ok := after[s]; ok {
				if again := nextStep(order, to); again != Nothing {
					t.Errorf("%s/%s: %s then %s, want nothing", pay, order, s, again)
				}
			}
		}
	}
}

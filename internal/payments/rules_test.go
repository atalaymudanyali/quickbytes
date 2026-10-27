package payments

import (
	"slices"
	"testing"
	"time"

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

// Each wait is longer than the one before, up to five minutes, with at most
// 20% jitter on top.
func TestBackoff(t *testing.T) {
	prev := time.Duration(0)
	for attempts := 1; attempts <= 30; attempts++ {
		d := backoff(attempts)
		base := min(backoffBase<<(min(attempts, 20)-1), backoffMax)
		if d < base || d > base+base/5 {
			t.Fatalf("attempt %d: %v, want %v plus at most 20%%", attempts, d, base)
		}
		if base < backoffMax && d <= prev/2 {
			t.Fatalf("attempt %d: %v after %v, want growth", attempts, d, prev)
		}
		prev = d
	}
	if backoff(0) < backoffBase {
		t.Fatal("attempt 0 must still wait")
	}
}

// forward is the ratchet written out by hand: every move a payment may make.
// Staying the same is always allowed; every pair not listed must be refused.
var forward = map[Status][]Status{
	Pending:    {Unknown, Processing, Authorized, Failed, Captured, Voided, Refunded},
	Unknown:    {Processing, Authorized, Failed, Captured, Voided, Refunded},
	Processing: {Authorized, Failed, Captured, Voided, Refunded},
	Authorized: {Captured, Voided, Refunded},
	Captured:   {Refunded},
}

func TestRatchetGrid(t *testing.T) {
	for _, from := range allStatuses {
		for _, to := range allStatuses {
			want := from == to || slices.Contains(forward[from], to)
			if got := allowed(from, to); got != want {
				t.Errorf("%s → %s: allowed = %v, want %v", from, to, got, want)
			}
		}
	}
}

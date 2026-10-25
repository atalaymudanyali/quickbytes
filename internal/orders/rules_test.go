package orders

import (
	"errors"
	"testing"

	"github.com/atalaymudanyali/quickbytes/internal/users"
)

// allowed is the state machine written out by hand, separately from rules, so a
// slip in either one shows up as a failing test. Every pair not listed here
// must be refused.
var allowed = map[Status]map[Action]Status{
	Placed:   {Pay: Paid, FailPayment: Cancelled, Expire: Cancelled, Cancel: Cancelled},
	Paid:     {Accept: Accepted, Reject: Cancelled, Cancel: Cancelled},
	Accepted: {MarkReady: Ready},
	Ready:    {PickUp: PickedUp},
	PickedUp: {Deliver: Delivered},
}

// wantReason is the cancel reason each cancelling action leaves.
var wantReason = map[Action]CancelReason{
	FailPayment: PaymentFailed, Expire: Expired, Cancel: ByCustomer, Reject: Rejected,
}

// pressedBy is who each action belongs to.
var pressedBy = map[Action]users.Role{
	Pay: RoleSystem, FailPayment: RoleSystem, Expire: RoleSystem,
	Cancel: users.RoleCustomer,
	Accept: users.RoleRestaurant, Reject: users.RoleRestaurant, MarkReady: users.RoleRestaurant,
	PickUp: users.RoleDriver, Deliver: users.RoleDriver,
}

// The full grid: every status × every action, pressed by the right role.
func TestEveryStatusAndAction(t *testing.T) {
	for _, from := range Statuses {
		for _, a := range Actions {
			t.Run(string(from)+"/"+string(a), func(t *testing.T) {
				to, reason, err := next(from, a, pressedBy[a])
				want, ok := allowed[from][a]
				if !ok {
					if !errors.Is(err, ErrInvalidTransition) {
						t.Fatalf("got %q, %v; want ErrInvalidTransition", to, err)
					}
					return
				}
				if err != nil || to != want || reason != wantReason[a] {
					t.Fatalf("got %q (%q), %v; want %q (%q)", to, reason, err, want, wantReason[a])
				}
			})
		}
	}
}

// Every action pressed by anyone other than its owner is refused, whatever the
// status: a customer can't accept their own order, a restaurant can't pay it.
func TestOnlyTheRightRoleMayAct(t *testing.T) {
	roles := []users.Role{users.RoleCustomer, users.RoleRestaurant, users.RoleDriver, RoleSystem}
	for _, a := range Actions {
		for _, by := range roles {
			if by == pressedBy[a] {
				continue
			}
			for _, from := range Statuses {
				if _, _, err := next(from, a, by); !errors.Is(err, ErrNotAllowed) {
					t.Errorf("%s by %s from %s: got %v, want ErrNotAllowed", a, by, from, err)
				}
			}
		}
	}
}

// Nothing leaves cancelled or delivered: they're the end of the line.
func TestFinalStatusesHaveNoWayOut(t *testing.T) {
	for _, from := range []Status{Cancelled, Delivered} {
		for _, a := range Actions {
			if _, _, err := next(from, a, pressedBy[a]); err == nil {
				t.Errorf("%s from %s was allowed", a, from)
			}
		}
	}
}

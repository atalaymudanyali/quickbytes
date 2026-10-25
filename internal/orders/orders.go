// Package orders keeps orders and moves them through their statuses. Every move
// goes through one function, Transition, which holds the full list of allowed
// moves (the state machine) and records each move in the status history.
package orders

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atalaymudanyali/quickbytes/internal/addresses"
	"github.com/atalaymudanyali/quickbytes/internal/carts"
	"github.com/atalaymudanyali/quickbytes/internal/users"
)

type Status string

const (
	Placed    Status = "placed"
	Paid      Status = "paid"
	Accepted  Status = "accepted"
	Ready     Status = "ready"
	PickedUp  Status = "picked_up" // phase 3 (drivers)
	Delivered Status = "delivered" // phase 3 (drivers)
	Cancelled Status = "cancelled"
)

// Statuses lists every status, in the order an order normally goes through them.
var Statuses = []Status{Placed, Paid, Accepted, Ready, PickedUp, Delivered, Cancelled}

type CancelReason string

const (
	PaymentFailed CancelReason = "payment_failed"
	Rejected      CancelReason = "rejected"
	ByCustomer    CancelReason = "by_customer"
	Expired       CancelReason = "expired"
)

// Action is a button someone can press on an order. Callers ask for an action,
// never for a status: the rules below decide where the action leads.
type Action string

const (
	Pay         Action = "pay"          // the payment flow (issue #20)
	FailPayment Action = "fail_payment" // the payment flow (issue #20)
	Accept      Action = "accept"
	Reject      Action = "reject"
	MarkReady   Action = "mark_ready"
	PickUp      Action = "pick_up" // phase 3 (drivers)
	Deliver     Action = "deliver" // phase 3 (drivers)
	Cancel      Action = "cancel"
	Expire      Action = "expire"
)

// Actions lists every action.
var Actions = []Action{Pay, FailPayment, Accept, Reject, MarkReady, PickUp, Deliver, Cancel, Expire}

// RoleSystem is the backend acting on its own: the expiry loop, the payment flow.
const RoleSystem users.Role = "system"

// Actor is who asks for a move: a profile in one of its roles, or the system,
// which has no profile.
type Actor struct {
	Role      users.Role
	ProfileID uuid.UUID
}

// System is the actor for moves nobody pressed a button for.
var System = Actor{Role: RoleSystem}

// rule is one allowed move: from which statuses, to which status, with which
// cancel reason (only for moves to cancelled), and who may make it.
type rule struct {
	from   []Status
	to     Status
	reason CancelReason
	by     users.Role
}

// rules is the state machine: the only place that says which moves exist. A move
// not listed here can't happen.
//
//	placed → paid → accepted → ready → picked_up → delivered
//	   │       │        │
//	   └───────┴────────┴──→ cancelled (with a reason)
var rules = map[Action]rule{
	Pay:         {from: []Status{Placed}, to: Paid, by: RoleSystem},
	FailPayment: {from: []Status{Placed}, to: Cancelled, reason: PaymentFailed, by: RoleSystem},
	Expire:      {from: []Status{Placed}, to: Cancelled, reason: Expired, by: RoleSystem},
	Cancel:      {from: []Status{Placed, Paid}, to: Cancelled, reason: ByCustomer, by: users.RoleCustomer},
	Accept:      {from: []Status{Paid}, to: Accepted, by: users.RoleRestaurant},
	Reject:      {from: []Status{Paid}, to: Cancelled, reason: Rejected, by: users.RoleRestaurant},
	MarkReady:   {from: []Status{Accepted}, to: Ready, by: users.RoleRestaurant},
	PickUp:      {from: []Status{Ready}, to: PickedUp, by: users.RoleDriver},
	Deliver:     {from: []Status{PickedUp}, to: Delivered, by: users.RoleDriver},
}

var (
	ErrNotFound          = errors.New("order not found")
	ErrNotAllowed        = errors.New("not allowed to do this")
	ErrInvalidTransition = errors.New("this move is not possible in the order's status")
)

// next looks a move up in the rules: where action leads from status, when made
// by role. It's pure, so the rules can be tested without a database.
func next(from Status, a Action, by users.Role) (Status, CancelReason, error) {
	r, ok := rules[a]
	if !ok || r.by != by {
		return "", "", ErrNotAllowed
	}
	if !slices.Contains(r.from, from) {
		return "", "", ErrInvalidTransition
	}
	return r.to, r.reason, nil
}

type Store struct {
	pool      *pgxpool.Pool
	carts     *carts.Store
	addresses *addresses.Store
}

func NewStore(pool *pgxpool.Pool, c *carts.Store, a *addresses.Store) *Store {
	return &Store{pool: pool, carts: c, addresses: a}
}

// Change is one move that happened.
type Change struct {
	OrderID uuid.UUID
	From    Status
	To      Status
	Reason  CancelReason // empty unless To is cancelled
	At      time.Time
}

// Transition makes one move on an order, in its own transaction.
func (s *Store) Transition(ctx context.Context, orderID uuid.UUID, a Action, by Actor) (Change, error) {
	var c Change
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		c, err = transition(ctx, tx, orderID, a, by)
		return err
	})
	return c, err
}

// transition makes one move inside a transaction it's given, so later work that
// must happen with the move (taking the payment, publishing an event) can join
// the same transaction.
//
// It locks the order row first (FOR UPDATE). A second move on the same order
// waits here until the first commits, then reads the new status, so two moves
// can never both start from the same status.
func transition(ctx context.Context, tx pgx.Tx, orderID uuid.UUID, a Action, by Actor) (Change, error) {
	var from Status
	var customer, owner uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT o.status, o.customer_profile_id, r.owner_profile_id
		FROM orders o JOIN restaurants r ON r.id = o.restaurant_id
		WHERE o.id = $1
		FOR UPDATE OF o`, orderID).Scan(&from, &customer, &owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return Change{}, ErrNotFound
	}
	if err != nil {
		return Change{}, fmt.Errorf("lock order: %w", err)
	}
	// Someone else's order looks the same as no order: we don't confirm it
	// exists to people who can't see it.
	if !canSee(by, customer, owner) {
		return Change{}, ErrNotFound
	}

	to, reason, err := next(from, a, by.Role)
	if err != nil {
		return Change{}, err
	}

	c := Change{OrderID: orderID, From: from, To: to, Reason: reason}
	err = tx.QueryRow(ctx, `
		UPDATE orders SET status = $2, cancel_reason = $3, updated_at = now()
		WHERE id = $1
		RETURNING updated_at`,
		orderID, to, nullIfEmpty(reason)).Scan(&c.At)
	if err != nil {
		return Change{}, fmt.Errorf("update order: %w", err)
	}
	if err := writeHistory(ctx, tx, c, by); err != nil {
		return Change{}, err
	}
	return c, nil
}

// canSee says whether the actor may act on an order with this customer and this
// restaurant owner. Drivers get orders assigned in phase 3; until then they
// see none.
func canSee(by Actor, customer, owner uuid.UUID) bool {
	switch by.Role {
	case RoleSystem:
		return true
	case users.RoleCustomer:
		return by.ProfileID == customer
	case users.RoleRestaurant:
		return by.ProfileID == owner
	default:
		return false
	}
}

// writeHistory records a move. A move with no From is the order being placed.
func writeHistory(ctx context.Context, tx pgx.Tx, c Change, by Actor) error {
	var profile *uuid.UUID
	if by.Role != RoleSystem {
		profile = &by.ProfileID
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO order_status_history
			(id, order_id, from_status, to_status, cancel_reason, by_role, by_profile_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		uuid.Must(uuid.NewV7()), c.OrderID, nullIfEmpty(c.From), c.To, nullIfEmpty(c.Reason),
		by.Role, profile, c.At)
	if err != nil {
		return fmt.Errorf("write history: %w", err)
	}
	return nil
}

// nullIfEmpty turns "" into NULL for the database.
func nullIfEmpty[T ~string](v T) *T {
	if v == "" {
		return nil
	}
	return &v
}

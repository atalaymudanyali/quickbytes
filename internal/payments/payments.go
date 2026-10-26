// Package payments keeps the API's side of each order's payment: what we know
// about it (its status) and how we learned it (an append-only ledger of
// events). The money itself is at the payment provider; this package talks to
// it only through the Provider interface.
package payments

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Status string

// Our payment statuses (the migration explains each). The provider client
// translates the provider's own words into these.
const (
	Pending    Status = "pending"
	Unknown    Status = "unknown"
	Processing Status = "processing"
	Authorized Status = "authorized"
	Captured   Status = "captured"
	Voided     Status = "voided"
	Refunded   Status = "refunded"
	Failed     Status = "failed"
)

// Decline codes: why a failed payment failed.
const (
	CardDeclined      = "card_declined"
	InsufficientFunds = "insufficient_funds"
	CardRejected      = "card_rejected"
	NoAnswer          = "no_answer"
)

// Via says how we learned about a change.
type Via string

const (
	ViaRequest   Via = "request"   // the answer to our own call
	ViaReconcile Via = "reconcile" // the reconcile loop asked the provider later
)

var ErrNotFound = errors.New("payment not found")

// Payment is what we know about one order's payment.
type Payment struct {
	ID                uuid.UUID `json:"-"`
	OrderID           uuid.UUID `json:"-"`
	AmountKurus       int       `json:"amount_kurus"`
	Status            Status    `json:"status"`
	ProviderPaymentID *string   `json:"-"`
	CardLast4         string    `json:"card_last4"`
	DeclineCode       *string   `json:"decline_code"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// Event is one line of the ledger: a status the payment reached.
type Event struct {
	Status      Status    `json:"status"`
	DeclineCode *string   `json:"decline_code"`
	Via         Via       `json:"via"`
	At          time.Time `json:"at"`
}

// paymentColumns is the column list every query reads a Payment with, in the
// order scanPayment expects.
const paymentColumns = `id, order_id, amount_kurus, status, provider_payment_id, card_last4, decline_code, created_at, updated_at`

func scanPayment(row pgx.Row) (Payment, error) {
	var p Payment
	err := row.Scan(&p.ID, &p.OrderID, &p.AmountKurus, &p.Status, &p.ProviderPaymentID,
		&p.CardLast4, &p.DeclineCode, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// CreateIn writes a new payment for an order as pending, with its first
// ledger line, inside tx (checkout's transaction). It happens before the
// provider is asked anything: if we crash while waiting for the answer, this
// row is how we find out later that there may be money to look for.
//
// nextCheck is when the reconcile loop may first look at it. Checkout sets it
// past its own call's timeout, so the loop doesn't go looking while the
// request is still waiting for the answer.
func CreateIn(ctx context.Context, tx pgx.Tx, orderID uuid.UUID, amountKurus int, cardLast4 string, nextCheck time.Duration) (Payment, error) {
	p, err := scanPayment(tx.QueryRow(ctx, `
		INSERT INTO payments (id, order_id, amount_kurus, card_last4, next_check_at)
		VALUES ($1, $2, $3, $4, now() + make_interval(secs => $5))
		RETURNING `+paymentColumns,
		uuid.Must(uuid.NewV7()), orderID, amountKurus, cardLast4, nextCheck.Seconds()))
	if err != nil {
		return Payment{}, fmt.Errorf("insert payment: %w", err)
	}
	if err := addEvent(ctx, tx, p.ID, p.Status, nil, ViaRequest); err != nil {
		return Payment{}, err
	}
	return p, nil
}

// Change is what we learned about a payment: its new status, and the
// provider's ID and the decline code when we know them.
type Change struct {
	Status            Status
	ProviderPaymentID *string
	DeclineCode       *string
}

// updateIn records a change to a payment inside tx and returns the payment as
// it is now. A ledger line is added only when the status really changes: the
// loop asking twice and hearing "authorized" twice is one event, not two.
//
// The payment row is locked (FOR UPDATE) first, so two writers that learned
// something at the same moment (a request and the loop) take turns.
func updateIn(ctx context.Context, tx pgx.Tx, id uuid.UUID, c Change, via Via) (Payment, error) {
	before, err := scanPayment(tx.QueryRow(ctx,
		`SELECT `+paymentColumns+` FROM payments WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Payment{}, ErrNotFound
	}
	if err != nil {
		return Payment{}, fmt.Errorf("lock payment: %w", err)
	}
	if c.ProviderPaymentID == nil {
		c.ProviderPaymentID = before.ProviderPaymentID // what we knew stays known
	}
	after, err := scanPayment(tx.QueryRow(ctx, `
		UPDATE payments
		SET status = $2, provider_payment_id = $3, decline_code = $4, updated_at = now()
		WHERE id = $1
		RETURNING `+paymentColumns,
		id, c.Status, c.ProviderPaymentID, c.DeclineCode))
	if err != nil {
		return Payment{}, fmt.Errorf("update payment: %w", err)
	}
	if after.Status != before.Status {
		if err := addEvent(ctx, tx, id, after.Status, after.DeclineCode, via); err != nil {
			return Payment{}, err
		}
	}
	return after, nil
}

func addEvent(ctx context.Context, tx pgx.Tx, paymentID uuid.UUID, s Status, declineCode *string, via Via) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO payment_events (payment_id, status, decline_code, via) VALUES ($1, $2, $3, $4)`,
		paymentID, s, declineCode, via)
	if err != nil {
		return fmt.Errorf("add payment event: %w", err)
	}
	return nil
}

// ForOrder returns an order's payment. ErrNotFound if it has none.
func (s *Store) ForOrder(ctx context.Context, orderID uuid.UUID) (Payment, error) {
	p, err := scanPayment(s.pool.QueryRow(ctx,
		`SELECT `+paymentColumns+` FROM payments WHERE order_id = $1`, orderID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Payment{}, ErrNotFound
	}
	if err != nil {
		return Payment{}, fmt.Errorf("get payment: %w", err)
	}
	return p, nil
}

// Events returns a payment's ledger, oldest first.
func (s *Store) Events(ctx context.Context, paymentID uuid.UUID) ([]Event, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT status, decline_code, via, created_at FROM payment_events
		WHERE payment_id = $1 ORDER BY id`, paymentID)
	if err != nil {
		return nil, fmt.Errorf("list payment events: %w", err)
	}
	events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Event, error) {
		var e Event
		err := row.Scan(&e.Status, &e.DeclineCode, &e.Via, &e.At)
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("list payment events: %w", err)
	}
	return events, nil
}

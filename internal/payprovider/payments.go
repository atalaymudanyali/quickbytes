package payprovider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Payment statuses. The moves between them:
//
//	processing → authorized | failed     (the bank answers; slow card only)
//	authorized → captured                (take the held money)
//	authorized → voided                  (drop the hold, nothing taken)
//	captured   → refunded                (give the taken money back)
//
// failed, voided and refunded are final.
const (
	StatusProcessing = "processing"
	StatusAuthorized = "authorized"
	StatusCaptured   = "captured"
	StatusVoided     = "voided"
	StatusRefunded   = "refunded"
	StatusFailed     = "failed"
)

// Decline codes: why a failed payment failed.
const (
	DeclineCardDeclined      = "card_declined"
	DeclineInsufficientFunds = "insufficient_funds"
)

// MaxAmountKurus is the largest payment accepted: 100.000,00 TL. It keeps
// amounts far from the database's integer limit and catches a caller that
// sends lira instead of kuruş the other way round (100 times too much).
const MaxAmountKurus = 10_000_000

var (
	ErrNotFound = errors.New("payment not found")
	// ErrInvalidAmount: the amount is not 1 to MaxAmountKurus kuruş.
	ErrInvalidAmount = errors.New("amount must be between 1 and 10000000 kuruş")
	// ErrUnknownCard: the number is not one of the test cards.
	ErrUnknownCard = errors.New("not a test card")
	// ErrInvalidReference: the reference is longer than 255 characters.
	ErrInvalidReference = errors.New("reference must be at most 255 characters")
)

// StateError says a move isn't allowed from the payment's current status, such
// as capturing a voided payment.
type StateError struct {
	Action string // capture, void, refund
	Status string // the payment's status
}

func (e *StateError) Error() string {
	return fmt.Sprintf("cannot %s a payment that is %s", e.Action, e.Status)
}

// Payment is one amount a merchant asked to take from a card. It's also the
// JSON the provider answers with.
type Payment struct {
	ID          uuid.UUID `json:"id"`
	AmountKurus int       `json:"amount_kurus"`
	Currency    string    `json:"currency"`
	Status      string    `json:"status"`
	DeclineCode *string   `json:"decline_code,omitempty"`
	CardLast4   string    `json:"card_last4"`
	Reference   *string   `json:"reference,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// paymentColumns is the column list every query reads a Payment with, in the
// order scanPayment expects.
const paymentColumns = `id, amount_kurus, currency, status, decline_code, card_last4, reference, created_at, updated_at`

func scanPayment(row pgx.Row) (Payment, error) {
	var p Payment
	err := row.Scan(&p.ID, &p.AmountKurus, &p.Currency, &p.Status, &p.DeclineCode,
		&p.CardLast4, &p.Reference, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

type Payments struct {
	pool *pgxpool.Pool
	// slowDelay is how long a slow card's payment stays processing.
	slowDelay time.Duration
}

func NewPayments(pool *pgxpool.Pool, slowDelay time.Duration) *Payments {
	return &Payments{pool: pool, slowDelay: slowDelay}
}

// AuthorizeInput is what a merchant sends to start a payment.
type AuthorizeInput struct {
	AmountKurus int
	CardNumber  string
	// Reference is the merchant's own label (QuickBytes: the order ID).
	// Optional.
	Reference *string
}

// Authorize asks to hold an amount on a card. The test card decides what
// happens (see Outcome), and the payment is stored whatever the answer: a
// declined payment is kept too, as failed, so there's a record of every try.
//
// A declined card is not an error here: the payment comes back with status
// failed and a decline code. Errors are for requests that can't become a
// payment at all (no such card, a bad amount).
func (p *Payments) Authorize(ctx context.Context, merchantID uuid.UUID, in AuthorizeInput) (Payment, error) {
	if in.AmountKurus < 1 || in.AmountKurus > MaxAmountKurus {
		return Payment{}, ErrInvalidAmount
	}
	if in.Reference != nil && (len(*in.Reference) == 0 || len(*in.Reference) > 255) {
		return Payment{}, ErrInvalidReference
	}
	outcome, ok := TestCard(in.CardNumber)
	if !ok {
		return Payment{}, ErrUnknownCard
	}

	status, declineCode := StatusAuthorized, (*string)(nil)
	var settleIn *float64 // seconds until a processing payment settles
	switch outcome {
	case Decline:
		status, declineCode = StatusFailed, ptr(DeclineCardDeclined)
	case InsufficientFunds:
		status, declineCode = StatusFailed, ptr(DeclineInsufficientFunds)
	case Slow:
		status, settleIn = StatusProcessing, ptr(p.slowDelay.Seconds())
	}

	// The payment and its first event are written together, so the history
	// never misses a payment.
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Payment{}, fmt.Errorf("begin authorize: %w", err)
	}
	defer tx.Rollback(ctx)

	pay, err := scanPayment(tx.QueryRow(ctx, `
		INSERT INTO payments (id, merchant_id, amount_kurus, card_last4, reference, status, decline_code, settle_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, now() + make_interval(secs => $8))
		RETURNING `+paymentColumns,
		uuid.Must(uuid.NewV7()), merchantID, in.AmountKurus, in.CardNumber[len(in.CardNumber)-4:],
		in.Reference, status, declineCode, settleIn))
	if err != nil {
		return Payment{}, fmt.Errorf("insert payment: %w", err)
	}
	if err := addEvent(ctx, tx, pay.ID, pay.Status); err != nil {
		return Payment{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Payment{}, fmt.Errorf("commit authorize: %w", err)
	}
	return pay, nil
}

// Capture takes the held amount of an authorized payment.
func (p *Payments) Capture(ctx context.Context, merchantID, id uuid.UUID) (Payment, error) {
	return p.move(ctx, merchantID, id, "capture", StatusAuthorized, StatusCaptured)
}

// Void drops the hold of an authorized payment; no money is ever taken.
func (p *Payments) Void(ctx context.Context, merchantID, id uuid.UUID) (Payment, error) {
	return p.move(ctx, merchantID, id, "void", StatusAuthorized, StatusVoided)
}

// Refund gives back the whole amount of a captured payment.
func (p *Payments) Refund(ctx context.Context, merchantID, id uuid.UUID) (Payment, error) {
	return p.move(ctx, merchantID, id, "refund", StatusCaptured, StatusRefunded)
}

// move changes a payment from one status to the next, and records the event.
//
// The UPDATE only matches while the payment is still in the from status. That
// check and the change are one statement, so two captures arriving together
// can't both succeed: the database lets the first change the row and makes
// the second wait for it; the second then sees "captured" and matches nothing.
// (Checking with a SELECT first and updating after would leave a gap between
// the two where both requests see "authorized".)
func (p *Payments) move(ctx context.Context, merchantID, id uuid.UUID, action, from, to string) (Payment, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Payment{}, fmt.Errorf("begin %s: %w", action, err)
	}
	defer tx.Rollback(ctx)

	pay, err := scanPayment(tx.QueryRow(ctx, `
		UPDATE payments SET status = $4, updated_at = now()
		WHERE id = $1 AND merchant_id = $2 AND status = $3
		RETURNING `+paymentColumns,
		id, merchantID, from, to))
	if errors.Is(err, pgx.ErrNoRows) {
		// Either there's no such payment (for this merchant), or it's in
		// another status. Find out which, to answer 404 or 409.
		current, err := p.Get(ctx, merchantID, id)
		if err != nil {
			return Payment{}, err
		}
		return Payment{}, &StateError{Action: action, Status: current.Status}
	}
	if err != nil {
		return Payment{}, fmt.Errorf("%s payment: %w", action, err)
	}
	if err := addEvent(ctx, tx, pay.ID, pay.Status); err != nil {
		return Payment{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Payment{}, fmt.Errorf("commit %s: %w", action, err)
	}
	return pay, nil
}

// Get returns one of the merchant's payments. Another merchant's payment is
// ErrNotFound, as if it didn't exist.
func (p *Payments) Get(ctx context.Context, merchantID, id uuid.UUID) (Payment, error) {
	pay, err := scanPayment(p.pool.QueryRow(ctx,
		`SELECT `+paymentColumns+` FROM payments WHERE id = $1 AND merchant_id = $2`,
		id, merchantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Payment{}, ErrNotFound
	}
	if err != nil {
		return Payment{}, fmt.Errorf("get payment: %w", err)
	}
	return pay, nil
}

// addEvent appends the status a payment just reached to its history.
func addEvent(ctx context.Context, tx pgx.Tx, paymentID uuid.UUID, status string) error {
	_, err := tx.Exec(ctx,
		"INSERT INTO payment_events (payment_id, status) VALUES ($1, $2)", paymentID, status)
	if err != nil {
		return fmt.Errorf("add payment event: %w", err)
	}
	return nil
}

func ptr[T any](v T) *T { return &v }

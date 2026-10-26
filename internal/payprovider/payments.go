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
	Action  Action
	Status  string  // the payment's status
	Payment Payment // the payment as it is, to send back with the refusal
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

// Action is a move a merchant asks for on an existing payment.
type Action string

const (
	ActionCapture Action = "capture"
	ActionVoid    Action = "void"
	ActionRefund  Action = "refund"
)

// moves says, for each action, which status it starts from and which it
// reaches.
var moves = map[Action]struct{ from, to string }{
	ActionCapture: {StatusAuthorized, StatusCaptured},
	ActionVoid:    {StatusAuthorized, StatusVoided},
	ActionRefund:  {StatusCaptured, StatusRefunded},
}

// Finish is work that must happen together with a change to a payment: it runs
// last inside the change's transaction, and if it fails, the change is undone.
// The HTTP layer stores its idempotent answer this way, so money never moves
// without the answer that describes it being stored too (a crash in between
// would otherwise let a retry move it again).
type Finish func(ctx context.Context, tx pgx.Tx, p Payment) error

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
	return p.AuthorizeAnd(ctx, merchantID, in, nil)
}

// AuthorizeAnd is Authorize, with finish (if not nil) run as the last step of
// the payment's transaction.
func (p *Payments) AuthorizeAnd(ctx context.Context, merchantID uuid.UUID, in AuthorizeInput, finish Finish) (Payment, error) {
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
	if finish != nil {
		if err := finish(ctx, tx, pay); err != nil {
			return Payment{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Payment{}, fmt.Errorf("commit authorize: %w", err)
	}
	return pay, nil
}

// Capture takes the held amount of an authorized payment.
func (p *Payments) Capture(ctx context.Context, merchantID, id uuid.UUID) (Payment, error) {
	return p.MoveAnd(ctx, merchantID, id, ActionCapture, nil)
}

// Void drops the hold of an authorized payment; no money is ever taken.
func (p *Payments) Void(ctx context.Context, merchantID, id uuid.UUID) (Payment, error) {
	return p.MoveAnd(ctx, merchantID, id, ActionVoid, nil)
}

// Refund gives back the whole amount of a captured payment.
func (p *Payments) Refund(ctx context.Context, merchantID, id uuid.UUID) (Payment, error) {
	return p.MoveAnd(ctx, merchantID, id, ActionRefund, nil)
}

// MoveAnd changes a payment from one status to the next (see moves), records
// the event, and runs finish (if not nil) last in the same transaction.
//
// The UPDATE only matches while the payment is still in the from status. That
// check and the change are one statement, so two captures arriving together
// can't both succeed: the database lets the first change the row and makes
// the second wait for it; the second then sees "captured" and matches nothing.
// (Checking with a SELECT first and updating after would leave a gap between
// the two where both requests see "authorized".)
func (p *Payments) MoveAnd(ctx context.Context, merchantID, id uuid.UUID, action Action, finish Finish) (Payment, error) {
	m, ok := moves[action]
	if !ok {
		return Payment{}, fmt.Errorf("unknown payment action %q", action)
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Payment{}, fmt.Errorf("begin %s: %w", action, err)
	}
	defer tx.Rollback(ctx)

	pay, err := scanPayment(tx.QueryRow(ctx, `
		UPDATE payments SET status = $4, updated_at = now()
		WHERE id = $1 AND merchant_id = $2 AND status = $3
		RETURNING `+paymentColumns,
		id, merchantID, m.from, m.to))
	if errors.Is(err, pgx.ErrNoRows) {
		// Either there's no such payment (for this merchant), or it's in
		// another status. Find out which, to answer 404 or 409.
		current, err := p.Get(ctx, merchantID, id)
		if err != nil {
			return Payment{}, err
		}
		return Payment{}, &StateError{Action: action, Status: current.Status, Payment: current}
	}
	if err != nil {
		return Payment{}, fmt.Errorf("%s payment: %w", action, err)
	}
	if err := addEvent(ctx, tx, pay.ID, pay.Status); err != nil {
		return Payment{}, err
	}
	if finish != nil {
		if err := finish(ctx, tx, pay); err != nil {
			return Payment{}, err
		}
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

// ByReference returns the merchant's payments carrying this reference,
// newest first. It's how a merchant finds a payment it never got the ID of:
// its authorize call timed out, so the answer (with the ID) never arrived.
// Usually there's at most one, but nothing stops a merchant from reusing a
// reference, so it's a list.
func (p *Payments) ByReference(ctx context.Context, merchantID uuid.UUID, reference string) ([]Payment, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT `+paymentColumns+` FROM payments
		WHERE merchant_id = $1 AND reference = $2
		ORDER BY id DESC
		LIMIT 100`, merchantID, reference)
	if err != nil {
		return nil, fmt.Errorf("find payments by reference: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Payment, error) { return scanPayment(row) })
	if err != nil {
		return nil, fmt.Errorf("find payments by reference: %w", err)
	}
	return list, nil
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

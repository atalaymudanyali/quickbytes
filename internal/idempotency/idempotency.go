// Package idempotency makes retried requests safe: the client sends the same
// key with every attempt of one action, the first attempt does the work and
// stores its answer, and later attempts get that stored answer back instead of
// doing the work again. Like a coat-check ticket: hand in ticket 42 twice and
// you get the same coat, not a second one.
package idempotency

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// Lifetime is how long a key is remembered. Retries come within seconds or
	// minutes; after a day the same key starts a new action.
	Lifetime = 24 * time.Hour
	// AbandonedAfter is how long a key may stay in progress before another
	// attempt may take it over: the request holding it must have died (no
	// request runs this long).
	AbandonedAfter = time.Minute
	// cleanupBatch caps how many keys one DeleteExpired call removes.
	cleanupBatch = 1000
)

var (
	// ErrInProgress: another attempt with this key is still running.
	ErrInProgress = errors.New("a request with this idempotency key is in progress")
	// ErrKeyReused: the key was used before for a different request.
	ErrKeyReused = errors.New("idempotency key was used for a different request")
	// ErrClaimLost: the key was taken over (or deleted) while this attempt ran,
	// so this attempt's answer is not the one to keep.
	ErrClaimLost = errors.New("idempotency key no longer held by this request")
)

// MaxKeyLength is the longest key accepted, like Stripe's.
const MaxKeyLength = 255

// ValidKey says whether a client's key can be used: 1 to MaxKeyLength
// printable ASCII characters, no spaces. A UUID fits; so does anything else
// random enough, which is the client's job.
func ValidKey(key string) bool {
	if len(key) == 0 || len(key) > MaxKeyLength {
		return false
	}
	for i := range len(key) {
		if key[i] < '!' || key[i] > '~' {
			return false
		}
	}
	return true
}

// Fingerprint is the SHA-256 of a request: what "the same request" means.
type Fingerprint [sha256.Size]byte

// FingerprintOf hashes a request's method, path and body. Method and path never
// contain a newline, so the three parts can't run into each other.
func FingerprintOf(method, path string, body []byte) Fingerprint {
	h := sha256.New()
	h.Write([]byte(method + "\n" + path + "\n"))
	h.Write(body)
	return Fingerprint(h.Sum(nil))
}

// Response is a stored answer: what the first attempt sent back.
type Response struct {
	Status int
	Body   []byte
}

// Claim is a key held by one attempt. Only the attempt that holds it may store
// the answer (Save) or give it back (Release).
type Claim struct {
	UserID  uuid.UUID
	Key     string
	attempt uuid.UUID
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Begin looks the key up and says what to do with this attempt:
//
//   - a new key: it's claimed (in progress) for this attempt, which should now
//     do the work and then Save its answer (or Release the key if it failed in
//     a way worth retrying). Begin returns the Claim and a nil Response.
//   - a finished key with the same fingerprint: Begin returns the stored
//     Response, to send back as it is.
//   - a key still in progress: ErrInProgress.
//   - a key used for a different request: ErrKeyReused.
//
// A key older than Lifetime counts as new. A key in progress for longer than
// AbandonedAfter is taken over by this attempt, if the request is the same.
//
// The claim is a single INSERT, so two attempts arriving together can't both
// win: the database lets one insert the row, and the other then finds it.
func (s *Store) Begin(ctx context.Context, userID uuid.UUID, key string, fp Fingerprint) (Claim, *Response, error) {
	c := Claim{UserID: userID, Key: key, attempt: uuid.Must(uuid.NewV7())}
	// The row may disappear between the two queries (the cleanup deleting an
	// expired key); then the next round inserts it.
	for range 3 {
		claimed, err := s.claim(ctx, c, fp)
		if err != nil {
			return Claim{}, nil, err
		}
		if claimed {
			return c, nil, nil
		}

		var (
			storedFP   []byte
			status     string
			respStatus *int
			respBody   []byte
		)
		err = s.pool.QueryRow(ctx, `
			SELECT fingerprint, status, response_status, response_body
			FROM idempotency_keys WHERE user_id = $1 AND key = $2`,
			userID, key).Scan(&storedFP, &status, &respStatus, &respBody)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return Claim{}, nil, fmt.Errorf("read idempotency key: %w", err)
		}
		switch {
		case Fingerprint(storedFP) != fp:
			return Claim{}, nil, ErrKeyReused
		case status == "in_progress":
			return Claim{}, nil, ErrInProgress
		default:
			return Claim{}, &Response{Status: *respStatus, Body: respBody}, nil
		}
	}
	return Claim{}, nil, errors.New("idempotency key kept disappearing")
}

// claim inserts the key as in progress for c's attempt, or takes over an
// existing row that is expired (any request) or abandoned (same request only).
// It reports whether c now holds the key.
func (s *Store) claim(ctx context.Context, c Claim, fp Fingerprint) (bool, error) {
	// A conflicting row is updated only when the WHERE allows it; otherwise
	// nothing comes back and the caller reads the row to see why.
	rows, err := s.pool.Query(ctx, `
		INSERT INTO idempotency_keys (user_id, key, fingerprint, attempt)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, key) DO UPDATE SET
			fingerprint = EXCLUDED.fingerprint,
			attempt = EXCLUDED.attempt,
			status = 'in_progress',
			response_status = NULL,
			response_body = NULL,
			-- An expired key starts a new life; a taken-over one keeps its age.
			created_at = CASE WHEN idempotency_keys.created_at < now() - make_interval(secs => $5)
				THEN now() ELSE idempotency_keys.created_at END,
			claimed_at = now()
		WHERE idempotency_keys.created_at < now() - make_interval(secs => $5)
			OR (idempotency_keys.status = 'in_progress'
				AND idempotency_keys.claimed_at < now() - make_interval(secs => $6)
				AND idempotency_keys.fingerprint = EXCLUDED.fingerprint)
		RETURNING 1`,
		c.UserID, c.Key, fp[:], c.attempt, Lifetime.Seconds(), AbandonedAfter.Seconds())
	if err != nil {
		return false, fmt.Errorf("claim idempotency key: %w", err)
	}
	n, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		return false, fmt.Errorf("claim idempotency key: %w", err)
	}
	return len(n) == 1, nil
}

// Save stores the answer of the attempt holding c and marks the key done, so
// later attempts replay it. ErrClaimLost if c no longer holds the key.
func (s *Store) Save(ctx context.Context, c Claim, r Response) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE idempotency_keys
		SET status = 'done', response_status = $4, response_body = $5
		WHERE user_id = $1 AND key = $2 AND attempt = $3 AND status = 'in_progress'`,
		c.UserID, c.Key, c.attempt, r.Status, r.Body)
	if err != nil {
		return fmt.Errorf("save idempotent response: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrClaimLost
	}
	return nil
}

// Release gives the key back without an answer, so the next attempt runs the
// work again. For failures where nothing happened (a server error inside a
// transaction that rolled back). Releasing a key c no longer holds does
// nothing.
func (s *Store) Release(ctx context.Context, c Claim) error {
	_, err := s.pool.Exec(ctx, `
		DELETE FROM idempotency_keys
		WHERE user_id = $1 AND key = $2 AND attempt = $3 AND status = 'in_progress'`,
		c.UserID, c.Key, c.attempt)
	if err != nil {
		return fmt.Errorf("release idempotency key: %w", err)
	}
	return nil
}

// DeleteExpired removes up to cleanupBatch keys older than Lifetime and returns
// how many it removed. Begin already ignores expired keys; this only keeps the
// table small.
func (s *Store) DeleteExpired(ctx context.Context) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM idempotency_keys
		WHERE (user_id, key) IN (
			SELECT user_id, key FROM idempotency_keys
			WHERE created_at < now() - make_interval(secs => $1)
			LIMIT $2)`,
		Lifetime.Seconds(), cleanupBatch)
	if err != nil {
		return 0, fmt.Errorf("delete expired idempotency keys: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

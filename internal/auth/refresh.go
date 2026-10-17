package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrInvalidRefresh = errors.New("invalid, expired or revoked refresh token")
	// ErrRefreshReused means an already-used token came back: either it was stolen,
	// or the app sent the same refresh twice. The whole family is revoked either way.
	ErrRefreshReused = errors.New("refresh token reused")
)

// RefreshStore keeps refresh tokens in PostgreSQL. Unlike access tokens they're
// checked against the database on every use, which is what makes logout and
// revocation possible.
type RefreshStore struct {
	pool *pgxpool.Pool
	ttl  time.Duration
}

func NewRefreshStore(pool *pgxpool.Pool, ttl time.Duration) *RefreshStore {
	return &RefreshStore{pool: pool, ttl: ttl}
}

// Start begins a new token family, at login or sign-up.
func (s *RefreshStore) Start(ctx context.Context, userID uuid.UUID) (string, error) {
	return s.insert(ctx, s.pool, userID, uuid.Must(uuid.NewV7()))
}

// Rotate exchanges a valid refresh token for a new one in the same family and
// returns the user it belongs to. On ErrRefreshReused the user is still returned,
// for logging.
func (s *RefreshStore) Rotate(ctx context.Context, token string) (uuid.UUID, string, error) {
	var (
		userID   uuid.UUID
		newToken string
		reused   bool
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var (
			id, familyID      uuid.UUID
			expiresAt         time.Time
			usedAt, revokedAt *time.Time
		)
		// FOR UPDATE locks this row until the transaction ends. If the same token
		// arrives twice at the same moment, the second request waits here, then
		// sees used_at already set, instead of both getting a new token.
		err := tx.QueryRow(ctx, `
			SELECT id, user_id, family_id, expires_at, used_at, revoked_at
			FROM refresh_tokens WHERE token_hash = $1 FOR UPDATE`,
			hashToken(token),
		).Scan(&id, &userID, &familyID, &expiresAt, &usedAt, &revokedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidRefresh
		}
		if err != nil {
			return fmt.Errorf("find refresh token: %w", err)
		}

		switch {
		case revokedAt != nil || time.Now().After(expiresAt):
			return ErrInvalidRefresh
		case usedAt != nil:
			// Revoke inside this transaction and let it commit; the error is
			// reported after commit.
			reused = true
			return revokeFamily(ctx, tx, familyID)
		}

		if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET used_at = now() WHERE id = $1`, id); err != nil {
			return fmt.Errorf("mark refresh token used: %w", err)
		}
		newToken, err = s.insert(ctx, tx, userID, familyID)
		return err
	})
	if err != nil {
		return uuid.Nil, "", err
	}
	if reused {
		return userID, "", ErrRefreshReused
	}
	return userID, newToken, nil
}

// Revoke ends the family a token belongs to (logout). Unknown tokens are ignored,
// so logging out twice is harmless.
func (s *RefreshStore) Revoke(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE refresh_tokens SET revoked_at = now()
		WHERE family_id = (SELECT family_id FROM refresh_tokens WHERE token_hash = $1)
		  AND revoked_at IS NULL`,
		hashToken(token))
	if err != nil {
		return fmt.Errorf("revoke refresh tokens: %w", err)
	}
	return nil
}

// queryRower is satisfied by both the pool and a transaction, so insert works
// inside or outside one.
type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *RefreshStore) insert(ctx context.Context, db queryRower, userID, familyID uuid.UUID) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate refresh token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	var id uuid.UUID
	err := db.QueryRow(ctx, `
		INSERT INTO refresh_tokens (id, user_id, family_id, token_hash, expires_at)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		uuid.Must(uuid.NewV7()), userID, familyID, hashToken(token), time.Now().Add(s.ttl),
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("insert refresh token: %w", err)
	}
	return token, nil
}

func revokeFamily(ctx context.Context, tx pgx.Tx, familyID uuid.UUID) error {
	_, err := tx.Exec(ctx, `
		UPDATE refresh_tokens SET revoked_at = now()
		WHERE family_id = $1 AND revoked_at IS NULL`, familyID)
	if err != nil {
		return fmt.Errorf("revoke token family: %w", err)
	}
	return nil
}

// hashToken uses plain SHA-256, not argon2: the token is 32 random bytes, so
// there's nothing to guess, and a slow hash would only slow down every refresh.
func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

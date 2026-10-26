// Package payprovider is the mock payment provider: a stand-in for an outside
// company such as Stripe or iyzico. It runs as its own service with its own
// database, so the API can only reach it over the network, where calls can be
// slow, fail or arrive twice.
package payprovider

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrUnknownKey: no merchant has this API key.
var ErrUnknownKey = errors.New("unknown API key")

// Merchant is a company that takes payments through the provider.
type Merchant struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

type Merchants struct {
	pool *pgxpool.Pool
}

func NewMerchants(pool *pgxpool.Pool) *Merchants {
	return &Merchants{pool: pool}
}

// hashKey is what's stored instead of the key itself: someone who reads the
// table can't turn the hash back into a key that works. A plain SHA-256 is
// enough here (passwords need a slow hash like Argon2 because people pick
// guessable ones; these keys are long random strings, so there's nothing to
// guess).
func hashKey(key string) []byte {
	h := sha256.Sum256([]byte(key))
	return h[:]
}

// Ensure creates the merchant called name with this key, or gives an existing
// one the key (replacing the old key, which stops working). It returns the
// merchant. Running it on every startup makes the environment the place where
// the key is set.
func (m *Merchants) Ensure(ctx context.Context, name, key string) (Merchant, error) {
	var mer Merchant
	err := m.pool.QueryRow(ctx, `
		INSERT INTO merchants (id, name, api_key_hash) VALUES ($1, $2, $3)
		ON CONFLICT (name) DO UPDATE SET api_key_hash = EXCLUDED.api_key_hash
		RETURNING id, name`,
		uuid.Must(uuid.NewV7()), name, hashKey(key)).Scan(&mer.ID, &mer.Name)
	if err != nil {
		return Merchant{}, fmt.Errorf("ensure merchant: %w", err)
	}
	return mer, nil
}

// ByKey finds the merchant an API key belongs to.
//
// The key is hashed before the lookup, so the database compares hashes: an
// attacker timing the answers learns nothing useful about real keys, because
// changing one character of a guess changes its whole hash.
func (m *Merchants) ByKey(ctx context.Context, key string) (Merchant, error) {
	var mer Merchant
	err := m.pool.QueryRow(ctx,
		"SELECT id, name FROM merchants WHERE api_key_hash = $1",
		hashKey(key)).Scan(&mer.ID, &mer.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return Merchant{}, ErrUnknownKey
	}
	if err != nil {
		return Merchant{}, fmt.Errorf("find merchant by key: %w", err)
	}
	return mer, nil
}

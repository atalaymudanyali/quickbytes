package payprovider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atalaymudanyali/quickbytes/internal/testutil"
)

// randomKey returns a key no other test uses (tests share one database).
func randomKey() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return "sk_test_" + hex.EncodeToString(b)
}

// newMerchant adds a merchant with a random name and key.
func newMerchant(t *testing.T, pool *pgxpool.Pool) (Merchant, string) {
	t.Helper()
	key := randomKey()
	m, err := NewMerchants(pool).Ensure(context.Background(), "test-"+key[8:20], key)
	if err != nil {
		t.Fatalf("ensure merchant: %v", err)
	}
	return m, key
}

func TestMerchantKeys(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PayProviderDB(t)
	merchants := NewMerchants(pool)
	m, key := newMerchant(t, pool)

	got, err := merchants.ByKey(ctx, key)
	if err != nil || got != m {
		t.Fatalf("ByKey = %v, %v; want %v", got, err, m)
	}
	if _, err := merchants.ByKey(ctx, randomKey()); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("unknown key: got %v, want ErrUnknownKey", err)
	}

	// The key itself is never stored, only its hash.
	var stored []byte
	if err := pool.QueryRow(ctx, "SELECT api_key_hash FROM merchants WHERE id = $1", m.ID).Scan(&stored); err != nil {
		t.Fatalf("read hash: %v", err)
	}
	if string(stored) == key {
		t.Error("the key was stored as it is")
	}

	// Ensure with a new key: same merchant, the old key stops working.
	newKey := randomKey()
	again, err := merchants.Ensure(ctx, m.Name, newKey)
	if err != nil {
		t.Fatalf("ensure again: %v", err)
	}
	if again.ID != m.ID {
		t.Errorf("ensure again made a new merchant: %v, want %v", again.ID, m.ID)
	}
	if _, err := merchants.ByKey(ctx, key); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("old key after rotation: got %v, want ErrUnknownKey", err)
	}
	if got, err := merchants.ByKey(ctx, newKey); err != nil || got.ID != m.ID {
		t.Errorf("new key after rotation: %v, %v", got, err)
	}
}

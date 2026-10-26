// An external test package (database_test), because testutil imports database.
package database_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atalaymudanyali/quickbytes/internal/database"
	"github.com/atalaymudanyali/quickbytes/internal/testutil"
	providermigrations "github.com/atalaymudanyali/quickbytes/migrations/payprovider"
)

// The provider's migrations build its own database, and nothing of the API's.
func TestPayProviderMigrations(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PayProviderDB(t) // migrates once

	// Running again must be safe: nothing new to apply.
	if err := database.Migrate(ctx, pool, providermigrations.FS); err != nil {
		t.Fatalf("migrate again: %v", err)
	}

	for table, want := range map[string]bool{
		"merchants":        true,
		"payments":         true,
		"payment_events":   true,
		"idempotency_keys": true,
		// The API's tables live in another database.
		"users":  false,
		"orders": false,
	} {
		var exists bool
		err := pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists)
		if err != nil {
			t.Fatalf("look up %s: %v", table, err)
		}
		if exists != want {
			t.Errorf("table %s exists = %v, want %v", table, exists, want)
		}
	}
}

// newMerchant adds a merchant with a random name and key hash, so tests running
// at the same time never collide.
func newMerchant(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	hash := make([]byte, 32)
	copy(hash, id[:]) // unique, because the ID is
	_, err := pool.Exec(context.Background(),
		"INSERT INTO merchants (id, name, api_key_hash) VALUES ($1, $2, $3)",
		id, "test-"+id.String(), hash)
	if err != nil {
		t.Fatalf("insert merchant: %v", err)
	}
	return id
}

// The database itself refuses payments that make no sense, whatever the Go code
// does.
func TestPaymentRowChecks(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PayProviderDB(t)
	merchant := newMerchant(t, pool)

	tests := []struct {
		name        string
		status      string
		declineCode *string
		settleIn    *string // an interval from now, for settle_at
		ok          bool
	}{
		{"authorized", "authorized", nil, nil, true},
		{"failed with a reason", "failed", ptr("card_declined"), nil, true},
		{"failed without a reason", "failed", nil, nil, false},
		{"reason on a payment that didn't fail", "authorized", ptr("card_declined"), nil, false},
		{"unknown reason", "failed", ptr("bad_luck"), nil, false},
		{"processing with a settle time", "processing", nil, ptr("10 seconds"), true},
		{"processing without a settle time", "processing", nil, nil, false},
		{"settle time on a finished payment", "authorized", nil, ptr("10 seconds"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := pool.Exec(ctx, `
				INSERT INTO payments (id, merchant_id, amount_kurus, card_last4, status, decline_code, settle_at)
				VALUES ($1, $2, 32000, '4242', $3, $4, now() + $5::interval)`,
				uuid.Must(uuid.NewV7()), merchant, tt.status, tt.declineCode, tt.settleIn)
			if tt.ok && err != nil {
				t.Errorf("want accepted, got %v", err)
			}
			if !tt.ok && err == nil {
				t.Error("want refused, but the row was inserted")
			}
		})
	}
}

// A payment's history can only grow: changing or deleting an event is refused.
func TestPaymentEventsAreAppendOnly(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PayProviderDB(t)
	merchant := newMerchant(t, pool)

	payment := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO payments (id, merchant_id, amount_kurus, card_last4, status)
		VALUES ($1, $2, 32000, '4242', 'authorized')`, payment, merchant); err != nil {
		t.Fatalf("insert payment: %v", err)
	}
	var event int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO payment_events (payment_id, status) VALUES ($1, 'authorized')
		RETURNING id`, payment).Scan(&event); err != nil {
		t.Fatalf("insert event: %v", err)
	}

	if _, err := pool.Exec(ctx,
		"UPDATE payment_events SET status = 'captured' WHERE id = $1", event); err == nil {
		t.Error("UPDATE of an event was allowed")
	}
	if _, err := pool.Exec(ctx, "DELETE FROM payment_events WHERE id = $1", event); err == nil {
		t.Error("DELETE of an event was allowed")
	}
}

func ptr(s string) *string { return &s }

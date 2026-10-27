package payprovider

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Endpoint is where a merchant wants its webhooks: notifications the provider
// sends when one of its payments changes, so the merchant doesn't have to
// keep asking.
type Endpoint struct {
	ID         uuid.UUID
	MerchantID uuid.UUID
	URL        string
	Secret     string // signs every webhook; the merchant checks with the same one
	Enabled    bool
}

// queueWebhooks ends a statement that has just added events, in a CTE named
// ev (id, payment_id, status): it queues one delivery per event per enabled
// endpoint of the payment's merchant. processing is not queued: "not decided
// yet" is not news, and the webhook comes when it is decided.
const queueWebhooks = `
	INSERT INTO webhook_deliveries (payment_event_id, endpoint_id)
	SELECT ev.id, e.id FROM ev
	JOIN payments p ON p.id = ev.payment_id
	JOIN webhook_endpoints e ON e.merchant_id = p.merchant_id AND e.enabled
	WHERE ev.status <> 'processing'`

type Endpoints struct {
	pool *pgxpool.Pool
}

func NewEndpoints(pool *pgxpool.Pool) *Endpoints {
	return &Endpoints{pool: pool}
}

// Ensure sets the merchant's endpoint to url and secret, creating it if
// missing, and turns it on. Like Merchants.Ensure, it runs at startup, so the
// environment is where the URL and secret are set. Deliveries still waiting go
// to the new URL, signed with the new secret.
func (e *Endpoints) Ensure(ctx context.Context, merchantID uuid.UUID, url, secret string) (Endpoint, error) {
	ep := Endpoint{MerchantID: merchantID}
	err := e.pool.QueryRow(ctx, `
		INSERT INTO webhook_endpoints (id, merchant_id, url, secret) VALUES ($1, $2, $3, $4)
		ON CONFLICT (merchant_id) DO UPDATE
		SET url = EXCLUDED.url, secret = EXCLUDED.secret, enabled = true, updated_at = now()
		RETURNING id, url, secret, enabled`,
		uuid.Must(uuid.NewV7()), merchantID, url, secret).Scan(&ep.ID, &ep.URL, &ep.Secret, &ep.Enabled)
	if err != nil {
		return Endpoint{}, fmt.Errorf("ensure webhook endpoint: %w", err)
	}
	return ep, nil
}

// Disable turns the merchant's webhooks off: new events queue nothing, and
// deliveries still waiting aren't sent. The endpoint and its history stay.
func (e *Endpoints) Disable(ctx context.Context, merchantID uuid.UUID) error {
	_, err := e.pool.Exec(ctx,
		`UPDATE webhook_endpoints SET enabled = false, updated_at = now() WHERE merchant_id = $1 AND enabled`, merchantID)
	if err != nil {
		return fmt.Errorf("disable webhook endpoint: %w", err)
	}
	return nil
}

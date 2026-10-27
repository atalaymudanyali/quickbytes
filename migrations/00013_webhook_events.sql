-- +goose Up

-- A third way to learn about a payment: the provider telling us (a webhook).
ALTER TABLE payment_events DROP CONSTRAINT payment_events_via_check,
    ADD CONSTRAINT payment_events_via_check CHECK (via IN ('request', 'reconcile', 'webhook'));

-- Every webhook we've handled: a guest book. The provider sends each one
-- "at least once", so the same event can arrive again (its answer to our 200
-- was lost, or its test switch sends everything twice). The primary key on
-- the provider's event ID is what turns a repeat into "seen it, nothing to
-- do".
--
-- event_id is text, not uuid: it's the provider's ID, and another provider's
-- needn't be UUIDs (Stripe's look like evt_1Nx...).
--
-- The row is written in the same transaction as the change the webhook
-- makes. If applying it fails, the row is undone too, so the provider's
-- retry is handled for real instead of being skipped as a repeat.
--
-- outcome is what we did with it:
--   applied:   it changed the payment (and maybe the order).
--   stale:     nothing new, or older than what we know (it came out of
--              order), or it contradicts what we know: changed nothing.
--   unmatched: not about any payment of ours.
--   mismatch:  about our payment, but for another amount: not trusted.
CREATE TABLE webhook_events (
    event_id    text PRIMARY KEY CHECK (char_length(event_id) BETWEEN 1 AND 255),
    type        text NOT NULL,
    payment_id  uuid REFERENCES payments (id) ON DELETE SET NULL,
    outcome     text NOT NULL CHECK (outcome IN ('applied', 'stale', 'unmatched', 'mismatch')),
    received_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX webhook_events_payment_id_idx ON webhook_events (payment_id);

-- +goose Down
DROP TABLE webhook_events;
-- Ledger lines learned by webhook can't be deleted (append-only), so the old
-- rule only applies to new rows (NOT VALID skips checking the existing ones).
ALTER TABLE payment_events DROP CONSTRAINT payment_events_via_check,
    ADD CONSTRAINT payment_events_via_check CHECK (via IN ('request', 'reconcile')) NOT VALID;

-- +goose Up

-- Where a merchant wants its webhooks (notifications about its payments), and
-- the secret they're signed with (see internal/webhook).
--
-- Unlike the API key, the secret is kept as it is, not hashed: a hash is only
-- good for checking a key someone shows us, while signing needs the secret
-- itself, to mix into every signature.
--
-- One endpoint per merchant for now (UNIQUE merchant_id), set from the
-- environment at startup like the merchant's key. Deliveries are already per
-- endpoint, so several endpoints only need that UNIQUE gone. enabled = false
-- turns webhooks off without losing the endpoint's delivery history.
CREATE TABLE webhook_endpoints (
    id          uuid PRIMARY KEY,
    merchant_id uuid NOT NULL UNIQUE REFERENCES merchants (id),
    url         text NOT NULL CHECK (url ~ '^https?://'),
    secret      text NOT NULL CHECK (char_length(secret) >= 32),
    enabled     boolean NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- The event's public ID: the one sent in webhooks, which the merchant dedupes
-- on. Not the bigint id: reset this database and those count from 1 again, and
-- a merchant that remembers event 1 would drop the new event 1 as a repeat.
-- A random UUID never repeats. (Not UUIDv7 like our other IDs: Postgres 17 has
-- no built-in uuidv7(), the settler writes events in plain SQL, and this is a
-- label, not a primary key, so v7's index order doesn't matter.)
--
-- Adding a column doesn't count as an UPDATE, so the append-only trigger
-- doesn't stop it; the old rows get their IDs here.
ALTER TABLE payment_events ADD COLUMN public_id uuid NOT NULL UNIQUE DEFAULT gen_random_uuid();

-- pending:   waiting to be sent, or to be sent again after a failed try.
-- delivered: the endpoint answered 2xx.
-- failed:    every try failed; no more are made.
CREATE TYPE webhook_delivery_status AS ENUM ('pending', 'delivered', 'failed');

-- One webhook to send: one event to one endpoint (UNIQUE). It's written in the
-- same transaction as the event, so a payment can't change without its
-- webhook being queued, and no webhook is queued for a change that was undone.
CREATE TABLE webhook_deliveries (
    id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    payment_event_id bigint NOT NULL REFERENCES payment_events (id),
    endpoint_id      uuid NOT NULL REFERENCES webhook_endpoints (id),
    status           webhook_delivery_status NOT NULL DEFAULT 'pending',
    -- How many tries have been made, and when the next one is due.
    attempts         integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at  timestamptz NOT NULL DEFAULT now(),
    -- What went wrong last time ("500 Internal Server Error", "timeout"...).
    last_error       text,
    delivered_at     timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (payment_event_id, endpoint_id),
    CONSTRAINT webhook_deliveries_delivered_has_time CHECK ((status = 'delivered') = (delivered_at IS NOT NULL))
);

-- Serves the sender's "which deliveries are due?". Partial: delivered and
-- failed ones are finished and never in it.
CREATE INDEX webhook_deliveries_due_idx ON webhook_deliveries (next_attempt_at) WHERE status = 'pending';

-- +goose Down
DROP TABLE webhook_deliveries;
DROP TYPE webhook_delivery_status;
ALTER TABLE payment_events DROP COLUMN public_id;
DROP TABLE webhook_endpoints;

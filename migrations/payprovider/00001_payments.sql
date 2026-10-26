-- +goose Up

-- The companies that take payments through the provider. QuickBytes is one.
-- A merchant proves who it is with a secret API key; only the key's SHA-256 is
-- kept, so someone who reads this table still can't call the provider.
CREATE TABLE merchants (
    id           uuid PRIMARY KEY,
    name         text NOT NULL UNIQUE CHECK (char_length(name) BETWEEN 1 AND 100),
    api_key_hash bytea NOT NULL UNIQUE CHECK (octet_length(api_key_hash) = 32),
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- processing: the card company hasn't answered yet (the slow test card).
-- authorized: the amount is held on the card, not taken yet.
-- captured:   the held amount was taken.
-- voided:     the hold was dropped before capture; no money ever moved.
-- refunded:   a captured amount was given back.
-- failed:     the card was declined; decline_code says why.
CREATE TYPE payment_status AS ENUM
    ('processing', 'authorized', 'captured', 'voided', 'refunded', 'failed');

-- One payment: an amount a merchant asked to take from a card.
--
-- Only the card's last four digits are kept, like a real provider: a full card
-- number is the most protected data in payments, and nothing here needs it.
--
-- reference is the merchant's own label (QuickBytes will send the order ID), so
-- a payment can be matched to what it paid for.
--
-- settle_at is when a processing payment gets its final answer. It's set
-- exactly while the payment is processing.
CREATE TABLE payments (
    id           uuid PRIMARY KEY,
    merchant_id  uuid NOT NULL REFERENCES merchants (id),
    amount_kurus integer NOT NULL CHECK (amount_kurus > 0),
    currency     text NOT NULL DEFAULT 'TRY' CHECK (currency = 'TRY'),
    card_last4   text NOT NULL CHECK (card_last4 ~ '^[0-9]{4}$'),
    reference    text CHECK (char_length(reference) BETWEEN 1 AND 255),
    status       payment_status NOT NULL,
    decline_code text CHECK (decline_code IN ('card_declined', 'insufficient_funds')),
    settle_at    timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    -- A reason is given exactly when the payment failed.
    CONSTRAINT payments_failed_has_reason CHECK ((status = 'failed') = (decline_code IS NOT NULL)),
    CONSTRAINT payments_processing_has_settle_at CHECK ((status = 'processing') = (settle_at IS NOT NULL))
);

-- Serves the loop that finishes processing payments once their time comes.
-- Partial: only the few processing payments are in it, not every payment ever.
CREATE INDEX payments_settle_at_idx ON payments (settle_at) WHERE status = 'processing';

-- Every status a payment reaches, in order: the payment's history. Rows are
-- only ever added (append-only). The trigger below refuses changes and
-- deletions, so the history can be trusted like a ledger.
--
-- Webhooks (a later issue) will be sent from these rows: each one is a
-- "payment.<status>" notification for the merchant.
CREATE TABLE payment_events (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    payment_id uuid NOT NULL REFERENCES payments (id),
    status     payment_status NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX payment_events_payment_id_idx ON payment_events (payment_id, id);

-- +goose StatementBegin
CREATE FUNCTION payment_events_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'payment_events is append-only: % is not allowed', TG_OP;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER payment_events_append_only
    BEFORE UPDATE OR DELETE ON payment_events
    FOR EACH ROW EXECUTE FUNCTION payment_events_append_only();

-- The provider's own copy of the API's idempotency_keys table (see the API's
-- migration 00011), so the same Go package (internal/idempotency) serves both.
-- Here the owner of a key is a merchant: the column keeps the name user_id
-- because that package's SQL uses it, but it points at merchants.
CREATE TYPE idempotency_status AS ENUM ('in_progress', 'done');

CREATE TABLE idempotency_keys (
    user_id         uuid NOT NULL REFERENCES merchants (id),
    key             text NOT NULL CHECK (char_length(key) BETWEEN 1 AND 255),
    fingerprint     bytea NOT NULL CHECK (octet_length(fingerprint) = 32),
    attempt         uuid NOT NULL,
    status          idempotency_status NOT NULL DEFAULT 'in_progress',
    response_status integer,
    response_body   bytea,
    created_at      timestamptz NOT NULL DEFAULT now(),
    claimed_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, key),
    CHECK ((status = 'done') = (response_status IS NOT NULL AND response_body IS NOT NULL))
);

CREATE INDEX idempotency_keys_created_at_idx ON idempotency_keys (created_at);

-- +goose Down
DROP TABLE idempotency_keys;
DROP TYPE idempotency_status;
DROP TABLE payment_events;
DROP FUNCTION payment_events_append_only();
DROP TABLE payments;
DROP TYPE payment_status;
DROP TABLE merchants;

-- +goose Up

-- What we know about an order's payment. The provider has its own list; these
-- are ours, and the provider client translates its words into them.
--
-- pending:    written just before we ask the provider to authorize. If we
--             crash before hearing back, the row is still here to be checked.
-- unknown:    we asked and got no answer (a timeout, a dropped connection, a
--             5xx). The money may or may not be held; the reconcile loop asks
--             the provider until we know. We never authorize again.
-- processing: the provider answered "not decided yet" (the slow card).
-- authorized: the amount is held on the card, not taken yet.
-- captured:   the held amount was taken (when the restaurant accepted).
-- voided:     the hold was dropped; no money ever moved.
-- refunded:   a captured amount was given back.
-- failed:     no money is held and none will be; decline_code says why.
CREATE TYPE payment_status AS ENUM
    ('pending', 'unknown', 'processing', 'authorized', 'captured', 'voided', 'refunded', 'failed');

-- One payment per order (UNIQUE order_id): a declined card cancels the order,
-- it doesn't try another card.
--
-- Only the card's last four digits are kept. The full number is in memory for
-- the one authorize call at checkout and never written anywhere, which is also
-- why nothing can ever authorize the same order a second time later.
--
-- provider_payment_id is the provider's own ID for the payment (text, since
-- another provider's IDs needn't be UUIDs). It's missing while we don't know
-- the provider's answer, and on a payment that failed without ever reaching
-- the provider.
--
-- next_check_at and attempts serve the reconcile loop: when it should look at
-- this payment next, and how many tries in a row have gone without an answer
-- (each one waits longer: backoff).
CREATE TABLE payments (
    id                  uuid PRIMARY KEY,
    order_id            uuid NOT NULL UNIQUE REFERENCES orders (id) ON DELETE CASCADE,
    amount_kurus        integer NOT NULL CHECK (amount_kurus > 0),
    status              payment_status NOT NULL DEFAULT 'pending',
    provider_payment_id text UNIQUE CHECK (char_length(provider_payment_id) BETWEEN 1 AND 255),
    card_last4          text NOT NULL CHECK (card_last4 ~ '^[0-9]{4}$'),
    -- card_declined, insufficient_funds: the card's bank said no.
    -- card_rejected: the provider refused the request itself (not a card it accepts).
    -- no_answer: the authorize never reached the provider.
    decline_code        text CHECK (decline_code IN ('card_declined', 'insufficient_funds', 'card_rejected', 'no_answer')),
    attempts            integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_check_at       timestamptz NOT NULL DEFAULT now(),
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    -- A reason exactly when failed.
    CONSTRAINT payments_failed_has_reason CHECK ((status = 'failed') = (decline_code IS NOT NULL)),
    -- Once the provider has told us about the payment, we know its ID.
    CONSTRAINT payments_known_has_provider_id
        CHECK (status IN ('pending', 'unknown', 'failed') OR provider_payment_id IS NOT NULL)
);

-- Serves the reconcile loop: "which payments may need work, and are due?".
-- Partial: voided, refunded and failed payments are finished and never in it.
CREATE INDEX payments_next_check_at_idx ON payments (next_check_at)
    WHERE status IN ('pending', 'unknown', 'processing', 'authorized', 'captured');

-- Every status a payment reaches, in order: our own ledger of the money,
-- separate from the provider's. Rows are only ever added (append-only); the
-- trigger below refuses changes and deletions.
--
-- via says how we learned it: 'request' (the answer to our own call) or
-- 'reconcile' (the loop asked the provider later). Webhooks (#21) add a third.
CREATE TABLE payment_events (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    payment_id   uuid NOT NULL REFERENCES payments (id) ON DELETE CASCADE,
    status       payment_status NOT NULL,
    decline_code text,
    via          text NOT NULL CHECK (via IN ('request', 'reconcile')),
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX payment_events_payment_id_idx ON payment_events (payment_id, id);

-- +goose StatementBegin
CREATE FUNCTION payment_events_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    -- A cascade from deleting the order (only the demo reset does) may take
    -- the events with it; nothing else may touch them.
    IF TG_OP = 'DELETE' AND NOT EXISTS (SELECT 1 FROM payments WHERE id = OLD.payment_id) THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'payment_events is append-only: % is not allowed', TG_OP;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER payment_events_append_only
    BEFORE UPDATE OR DELETE ON payment_events
    FOR EACH ROW EXECUTE FUNCTION payment_events_append_only();

-- +goose Down
DROP TABLE payment_events;
DROP FUNCTION payment_events_append_only();
DROP TABLE payments;
DROP TYPE payment_status;

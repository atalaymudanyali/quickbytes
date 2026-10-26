-- +goose Up

-- in_progress: a request with this key is running (or died while running).
-- done: it finished, and its answer is stored for replaying.
CREATE TYPE idempotency_status AS ENUM ('in_progress', 'done');

-- The ticket stubs of retried requests. The app sends the same random key with
-- every attempt of one action; the first attempt does the work and stores its
-- answer here, the others get that answer back instead of doing it again.
--
-- Keys belong to a user (the primary key starts with user_id), so two users who
-- happen to pick the same key never see each other's answers.
--
-- fingerprint is the SHA-256 of the request (method, path, body): the same key
-- arriving with a different request is a client bug, not a retry.
--
-- attempt names the request that holds the key right now. Only that request may
-- store the answer, so a request that was taken over (see AbandonedAfter in
-- internal/idempotency) can't overwrite the newer one's answer.
CREATE TABLE idempotency_keys (
    user_id         uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    key             text NOT NULL CHECK (char_length(key) BETWEEN 1 AND 255),
    fingerprint     bytea NOT NULL CHECK (octet_length(fingerprint) = 32),
    attempt         uuid NOT NULL,
    status          idempotency_status NOT NULL DEFAULT 'in_progress',
    response_status integer,
    response_body   bytea,
    created_at      timestamptz NOT NULL DEFAULT now(),
    claimed_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, key),
    -- An answer is stored exactly when the key is done.
    CHECK ((status = 'done') = (response_status IS NOT NULL AND response_body IS NOT NULL))
);

-- Serves the cleanup of keys older than their 24-hour lifetime.
CREATE INDEX idempotency_keys_created_at_idx ON idempotency_keys (created_at);

-- +goose Down
DROP TABLE idempotency_keys;
DROP TYPE idempotency_status;

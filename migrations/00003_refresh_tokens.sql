-- +goose Up
-- Each login starts a "family" of refresh tokens. Every refresh marks the current
-- token used and adds the next one to the same family. If a used token is ever
-- presented again, someone copied it, and the whole family is revoked.
CREATE TABLE refresh_tokens (
    id         uuid PRIMARY KEY,
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    family_id  uuid NOT NULL,
    -- SHA-256 of the token. The token itself is never stored, so a leaked copy of
    -- this table can't be used to log in.
    token_hash bytea NOT NULL UNIQUE,
    expires_at timestamptz NOT NULL,
    used_at    timestamptz,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Revoking a family updates all its tokens at once.
CREATE INDEX refresh_tokens_family_id_idx ON refresh_tokens (family_id);
-- PostgreSQL doesn't index foreign keys automatically. Without this, deleting a
-- user would scan the whole table to find their tokens.
CREATE INDEX refresh_tokens_user_id_idx ON refresh_tokens (user_id);

-- +goose Down
DROP TABLE refresh_tokens;

-- +goose Up

-- One row per driver profile: what they ride and whether they're working. This is
-- the truth about who is free. Their position changes every few seconds, so it
-- lives in Redis, not here (phase 3, location ingest).
--
-- The profile's ID is the driver's ID too, so a profile can't have two rows.
--
-- status: offline (light off), available (light on, can be offered orders),
-- on_delivery (carrying food, can't end the shift).
CREATE TABLE drivers (
    profile_id uuid PRIMARY KEY REFERENCES profiles (id) ON DELETE CASCADE,
    vehicle    text NOT NULL CHECK (vehicle IN ('motorcycle', 'bicycle', 'car')),
    status     text NOT NULL DEFAULT 'offline' CHECK (status IN ('offline', 'available', 'on_delivery')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE drivers;

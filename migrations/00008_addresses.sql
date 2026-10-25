-- +goose Up

-- A customer's saved delivery addresses: "Home", "Work". The text is for the
-- courier to read; the point is where the map pin goes. The app sends both, since
-- turning text into coordinates (geocoding) needs a paid service.
CREATE TABLE addresses (
    id                  uuid PRIMARY KEY,
    customer_profile_id uuid NOT NULL REFERENCES profiles (id) ON DELETE CASCADE,
    label               text NOT NULL CHECK (length(label) BETWEEN 1 AND 30),
    address_line        text NOT NULL CHECK (length(address_line) BETWEEN 1 AND 300),
    details             text NOT NULL DEFAULT '' CHECK (length(details) <= 200),
    location            geography(Point, 4326) NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);

-- Every read is "this customer's addresses, oldest first".
CREATE INDEX addresses_customer_profile_id_idx ON addresses (customer_profile_id, created_at);

-- +goose Down
DROP TABLE addresses;

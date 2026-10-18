-- +goose Up
CREATE TABLE restaurants (
    id                uuid PRIMARY KEY,
    -- The restaurant profile that owns it. One profile can own several branches.
    owner_profile_id  uuid NOT NULL REFERENCES profiles (id) ON DELETE CASCADE,
    name              text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    address           text NOT NULL CHECK (length(address) BETWEEN 1 AND 300),
    -- geography (not geometry) measures on the curved Earth and works in metres,
    -- so "within 3000" means 3 km without any unit conversion. SRID 4326 is the
    -- standard GPS coordinate system (WGS 84).
    location          geography(Point, 4326) NOT NULL,
    -- How far the restaurant delivers. Search returns a restaurant only to
    -- customers inside this circle.
    delivery_radius_m integer NOT NULL CHECK (delivery_radius_m BETWEEN 500 AND 10000),
    accepting_orders  boolean NOT NULL DEFAULT true,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

-- "My restaurants" for an owner, and fast deletes when a profile is removed.
CREATE INDEX restaurants_owner_profile_id_idx ON restaurants (owner_profile_id);

-- Spatial index for "near me". GiST groups nearby points into boxes, so a search
-- only looks inside boxes that overlap the search area instead of checking every
-- restaurant.
CREATE INDEX restaurants_location_idx ON restaurants USING gist (location);

-- +goose Down
DROP TABLE restaurants;

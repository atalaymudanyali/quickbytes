-- +goose Up
-- PostGIS adds location types (points, polygons) and spatial indexes.
CREATE EXTENSION IF NOT EXISTS postgis;
-- citext is a case-insensitive text type, used later for email addresses.
CREATE EXTENSION IF NOT EXISTS citext;

-- +goose Down
DROP EXTENSION IF EXISTS citext;
DROP EXTENSION IF EXISTS postgis;

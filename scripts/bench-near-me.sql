-- Measures the near-me query on 100,000 generated restaurants:
--   1. without any spatial index
--   2. with the GiST index (the real setup)
--   3. with the index, but only the per-restaurant radius check,
--      to show why the query also has a fixed 10 km check
--
-- Run with: make bench-near-me
-- Everything happens in a temporary table inside a transaction that is rolled
-- back, so real data is never touched.

\timing off
\set ON_ERROR_STOP on

BEGIN;

CREATE TEMP TABLE bench_restaurants (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    location          geography(Point, 4326) NOT NULL,
    delivery_radius_m integer NOT NULL
);

-- 100k restaurants at random spots inside Istanbul's bounding box, delivering
-- 1 to 8 km. setseed makes every run generate the same restaurants.
\o /dev/null
SELECT setseed(0.42);
\o
INSERT INTO bench_restaurants (location, delivery_radius_m)
SELECT ST_SetSRID(ST_MakePoint(27.95 + random() * 2.0, 40.80 + random() * 0.8), 4326)::geography,
       1000 + (random() * 7000)::int
FROM generate_series(1, 100000);
ANALYZE bench_restaurants;

-- The customer: Kadıköy ferry pier. Same query as internal/restaurants/nearby.go
-- (first page, 20 results + 1 to detect a next page).
\set near_me 'WITH here AS (SELECT ST_SetSRID(ST_MakePoint(29.0233, 40.9917), 4326)::geography AS p) SELECT b.id, ST_Distance(b.location, here.p) AS distance_m FROM bench_restaurants b, here WHERE ST_DWithin(b.location, here.p, 10000) AND ST_DWithin(b.location, here.p, b.delivery_radius_m) ORDER BY distance_m, b.id LIMIT 21'

\set radius_only 'WITH here AS (SELECT ST_SetSRID(ST_MakePoint(29.0233, 40.9917), 4326)::geography AS p) SELECT b.id, ST_Distance(b.location, here.p) AS distance_m FROM bench_restaurants b, here WHERE ST_DWithin(b.location, here.p, b.delivery_radius_m) ORDER BY distance_m, b.id LIMIT 21'

\echo
\echo '=== 1. Without a spatial index ==='
-- Warm-up run, output hidden, so caches are filled before measuring.
\o /dev/null
:near_me;
\o
EXPLAIN (ANALYZE, COSTS OFF) :near_me;

CREATE INDEX ON bench_restaurants USING gist (location);
ANALYZE bench_restaurants;

\echo
\echo '=== 2. With the GiST index ==='
-- Warm-up run, output hidden, so caches are filled before measuring.
\o /dev/null
:near_me;
\o
EXPLAIN (ANALYZE, COSTS OFF) :near_me;

\echo
\echo '=== 3. With the index, but only the per-restaurant radius check ==='
-- Warm-up run, output hidden, so caches are filled before measuring.
\o /dev/null
:radius_only;
\o
EXPLAIN (ANALYZE, COSTS OFF) :radius_only;

ROLLBACK;

// Package restaurants stores restaurants and finds the ones that deliver to a
// location.
package restaurants

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Restaurant struct {
	ID              uuid.UUID `json:"id"`
	OwnerProfileID  uuid.UUID `json:"-"`
	Name            string    `json:"name"`
	Address         string    `json:"address"`
	Lat             float64   `json:"lat"`
	Lng             float64   `json:"lng"`
	DeliveryRadiusM int       `json:"delivery_radius_m"`
	AcceptingOrders bool      `json:"accepting_orders"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// New holds the fields needed to create a restaurant.
type New struct {
	Name            string
	Address         string
	Lat, Lng        float64
	DeliveryRadiusM int
}

// Patch holds the fields an owner can change. A nil field is left as it is.
// Lat and Lng must be set together.
type Patch struct {
	Name            *string
	Address         *string
	Lat, Lng        *float64
	DeliveryRadiusM *int
	AcceptingOrders *bool
}

const (
	MinDeliveryRadiusM     = 500
	MaxDeliveryRadiusM     = 10_000
	DefaultDeliveryRadiusM = 3_000
)

// Istanbul's bounding box: the rectangle that contains the whole province. Points
// outside it are almost certainly a client bug (swapped lat/lng, or 0,0).
const (
	minLat, maxLat = 40.80, 41.60
	minLng, maxLng = 27.95, 29.95
)

func InIstanbul(lat, lng float64) bool {
	return lat >= minLat && lat <= maxLat && lng >= minLng && lng <= maxLng
}

var ErrNotFound = errors.New("restaurant not found")

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// columns is the SELECT list matching scanRestaurant. PostGIS keeps the point as
// one value; ST_Y and ST_X read its latitude and longitude back out.
const columns = `id, owner_profile_id, name, address,
	ST_Y(location::geometry), ST_X(location::geometry),
	delivery_radius_m, accepting_orders, created_at, updated_at`

func scanRestaurant(row pgx.Row) (Restaurant, error) {
	var r Restaurant
	err := row.Scan(&r.ID, &r.OwnerProfileID, &r.Name, &r.Address, &r.Lat, &r.Lng,
		&r.DeliveryRadiusM, &r.AcceptingOrders, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// point builds a PostGIS point from two query parameters. Note the order:
// ST_MakePoint takes longitude (x) first, then latitude (y).
func point(latParam, lngParam string) string {
	return fmt.Sprintf("ST_SetSRID(ST_MakePoint(%s, %s), 4326)::geography", lngParam, latParam)
}

func (s *Store) Create(ctx context.Context, ownerProfileID uuid.UUID, n New) (Restaurant, error) {
	r, err := scanRestaurant(s.pool.QueryRow(ctx, `
		INSERT INTO restaurants (id, owner_profile_id, name, address, location, delivery_radius_m)
		VALUES ($1, $2, $3, $4, `+point("$5::float8", "$6::float8")+`, $7)
		RETURNING `+columns,
		uuid.Must(uuid.NewV7()), ownerProfileID, n.Name, n.Address, n.Lat, n.Lng, n.DeliveryRadiusM,
	))
	if err != nil {
		return Restaurant{}, fmt.Errorf("insert restaurant: %w", err)
	}
	return r, nil
}

func (s *Store) Get(ctx context.Context, id uuid.UUID) (Restaurant, error) {
	r, err := scanRestaurant(s.pool.QueryRow(ctx, `SELECT `+columns+` FROM restaurants WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Restaurant{}, ErrNotFound
	}
	if err != nil {
		return Restaurant{}, fmt.Errorf("get restaurant: %w", err)
	}
	return r, nil
}

func (s *Store) ListByOwner(ctx context.Context, ownerProfileID uuid.UUID) ([]Restaurant, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+columns+` FROM restaurants
		WHERE owner_profile_id = $1 ORDER BY created_at`, ownerProfileID)
	if err != nil {
		return nil, fmt.Errorf("list restaurants: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Restaurant, error) {
		return scanRestaurant(row)
	})
}

// Update applies a patch, but only if ownerProfileID owns the restaurant. Owner
// and ID are checked in the same statement, so there's no gap between "check" and
// "change" for another request to slip into. Not owning it and not existing give
// the same ErrNotFound.
func (s *Store) Update(ctx context.Context, id, ownerProfileID uuid.UUID, p Patch) (Restaurant, error) {
	r, err := scanRestaurant(s.pool.QueryRow(ctx, `
		UPDATE restaurants SET
			name              = COALESCE($3, name),
			address           = COALESCE($4, address),
			location          = CASE WHEN $5::float8 IS NULL THEN location
			                         ELSE `+point("$5::float8", "$6::float8")+` END,
			delivery_radius_m = COALESCE($7, delivery_radius_m),
			accepting_orders  = COALESCE($8, accepting_orders),
			updated_at        = now()
		WHERE id = $1 AND owner_profile_id = $2
		RETURNING `+columns,
		id, ownerProfileID, p.Name, p.Address, p.Lat, p.Lng, p.DeliveryRadiusM, p.AcceptingOrders,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return Restaurant{}, ErrNotFound
	}
	if err != nil {
		return Restaurant{}, fmt.Errorf("update restaurant: %w", err)
	}
	return r, nil
}

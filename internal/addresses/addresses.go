// Package addresses stores the places a customer has food delivered to.
package addresses

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Address struct {
	ID                uuid.UUID `json:"id"`
	CustomerProfileID uuid.UUID `json:"-"`
	Label             string    `json:"label"`
	AddressLine       string    `json:"address_line"`
	// Details is the part a map can't show: building, floor, door, "ring twice".
	Details   string    `json:"details"`
	Lat       float64   `json:"lat"`
	Lng       float64   `json:"lng"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// New holds the fields needed to save an address.
type New struct {
	Label, AddressLine, Details string
	Lat, Lng                    float64
}

// Patch holds the fields a customer can change. A nil field is left as it is.
// Lat and Lng must be set together.
type Patch struct {
	Label, AddressLine, Details *string
	Lat, Lng                    *float64
}

// MaxPerCustomer caps how many addresses one customer can save. Real people have
// a handful; the cap stops a script from filling the table.
const MaxPerCustomer = 10

var (
	ErrNotFound     = errors.New("address not found")
	ErrLimitReached = errors.New("address limit reached")
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// columns is the SELECT list matching scanAddress. ST_Y and ST_X read latitude and
// longitude back out of the PostGIS point.
const columns = `id, customer_profile_id, label, address_line, details,
	ST_Y(location::geometry), ST_X(location::geometry), created_at, updated_at`

func scanAddress(row pgx.Row) (Address, error) {
	var a Address
	err := row.Scan(&a.ID, &a.CustomerProfileID, &a.Label, &a.AddressLine, &a.Details,
		&a.Lat, &a.Lng, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

// point builds a PostGIS point; ST_MakePoint takes longitude (x) first.
func point(latParam, lngParam string) string {
	return fmt.Sprintf("ST_SetSRID(ST_MakePoint(%s, %s), 4326)::geography", lngParam, latParam)
}

// Create saves a new address, unless the customer already has MaxPerCustomer.
//
// "Count, then insert" has a gap: two requests at once could both count 9 and
// both insert, ending at 11. Locking the customer's profile row first closes it.
// The second request waits at the lock until the first has committed, and then
// counts 10.
func (s *Store) Create(ctx context.Context, customerProfileID uuid.UUID, n New) (Address, error) {
	var a Address
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM profiles WHERE id = $1 FOR UPDATE`, customerProfileID); err != nil {
			return fmt.Errorf("lock profile: %w", err)
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM addresses WHERE customer_profile_id = $1`,
			customerProfileID).Scan(&count); err != nil {
			return fmt.Errorf("count addresses: %w", err)
		}
		if count >= MaxPerCustomer {
			return ErrLimitReached
		}
		var err error
		a, err = scanAddress(tx.QueryRow(ctx, `
			INSERT INTO addresses (id, customer_profile_id, label, address_line, details, location)
			VALUES ($1, $2, $3, $4, $5, `+point("$6::float8", "$7::float8")+`)
			RETURNING `+columns,
			uuid.Must(uuid.NewV7()), customerProfileID, n.Label, n.AddressLine, n.Details, n.Lat, n.Lng,
		))
		if err != nil {
			return fmt.Errorf("insert address: %w", err)
		}
		return nil
	})
	return a, err
}

// List returns the customer's addresses, oldest first.
func (s *Store) List(ctx context.Context, customerProfileID uuid.UUID) ([]Address, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+columns+` FROM addresses
		WHERE customer_profile_id = $1 ORDER BY created_at, id`, customerProfileID)
	if err != nil {
		return nil, fmt.Errorf("list addresses: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Address, error) {
		return scanAddress(row)
	})
}

// Update applies a patch to one of the customer's addresses. Like restaurants,
// the owner is part of the WHERE, so "is it yours?" and "change it" are one step.
func (s *Store) Update(ctx context.Context, id, customerProfileID uuid.UUID, p Patch) (Address, error) {
	a, err := scanAddress(s.pool.QueryRow(ctx, `
		UPDATE addresses SET
			label        = COALESCE($3, label),
			address_line = COALESCE($4, address_line),
			details      = COALESCE($5, details),
			location     = CASE WHEN $6::float8 IS NULL THEN location
			                    ELSE `+point("$6::float8", "$7::float8")+` END,
			updated_at   = now()
		WHERE id = $1 AND customer_profile_id = $2
		RETURNING `+columns,
		id, customerProfileID, p.Label, p.AddressLine, p.Details, p.Lat, p.Lng,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return Address{}, ErrNotFound
	}
	if err != nil {
		return Address{}, fmt.Errorf("update address: %w", err)
	}
	return a, nil
}

// Delete removes one of the customer's addresses. Orders (issue #17) will copy
// the address they were sent to, so deleting it later never changes an order.
func (s *Store) Delete(ctx context.Context, id, customerProfileID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM addresses WHERE id = $1 AND customer_profile_id = $2`,
		id, customerProfileID)
	if err != nil {
		return fmt.Errorf("delete address: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

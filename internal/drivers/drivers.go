// Package drivers stores who drives what and who is working right now.
//
// A shift is like a taxi's roof light: on means "offer me work", off means "I'm
// done". Each status change is one conditional UPDATE, not a read followed by a
// write, so nothing can change the status between the check and the change.
package drivers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Vehicle string

const (
	VehicleMotorcycle Vehicle = "motorcycle"
	VehicleBicycle    Vehicle = "bicycle"
	VehicleCar        Vehicle = "car"
)

type Status string

const (
	StatusOffline    Status = "offline"
	StatusAvailable  Status = "available"
	StatusOnDelivery Status = "on_delivery"
)

type Driver struct {
	// ID is the driver profile's ID.
	ID        uuid.UUID `json:"id"`
	Vehicle   Vehicle   `json:"vehicle"`
	Status    Status    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

var (
	// ErrNotFound: this profile hasn't saved its driver details yet.
	ErrNotFound = errors.New("driver not found")
	// ErrOnDelivery: a shift can't end while carrying someone's food.
	ErrOnDelivery = errors.New("driver is on a delivery")
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// returning is the column list matching scanDriver.
const returning = `profile_id, vehicle, status, created_at, updated_at`

func scanDriver(row pgx.Row) (Driver, error) {
	var d Driver
	err := row.Scan(&d.ID, &d.Vehicle, &d.Status, &d.CreatedAt, &d.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Driver{}, ErrNotFound
	}
	return d, err
}

// Get returns the driver of this profile.
func (s *Store) Get(ctx context.Context, profileID uuid.UUID) (Driver, error) {
	d, err := scanDriver(s.pool.QueryRow(ctx,
		`SELECT `+returning+` FROM drivers WHERE profile_id = $1`, profileID))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Driver{}, fmt.Errorf("get driver: %w", err)
	}
	return d, err
}

// Save creates the driver's details, or changes them if they exist. The status is
// left alone: changing vehicle doesn't start or end a shift.
func (s *Store) Save(ctx context.Context, profileID uuid.UUID, vehicle Vehicle) (Driver, error) {
	d, err := scanDriver(s.pool.QueryRow(ctx, `
		INSERT INTO drivers (profile_id, vehicle) VALUES ($1, $2)
		ON CONFLICT (profile_id) DO UPDATE SET vehicle = EXCLUDED.vehicle, updated_at = now()
		RETURNING `+returning, profileID, vehicle))
	if err != nil {
		return Driver{}, fmt.Errorf("save driver: %w", err)
	}
	return d, nil
}

// StartShift turns the light on. Only offline becomes available: starting twice
// changes nothing, and a driver on a delivery stays on_delivery, or they'd look
// free while carrying food and be offered a second order.
func (s *Store) StartShift(ctx context.Context, profileID uuid.UUID) (Driver, error) {
	d, err := scanDriver(s.pool.QueryRow(ctx, `
		UPDATE drivers
		SET status = CASE WHEN status = 'offline' THEN 'available' ELSE status END, updated_at = now()
		WHERE profile_id = $1
		RETURNING `+returning, profileID))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Driver{}, fmt.Errorf("start shift: %w", err)
	}
	return d, err
}

// EndShift turns the light off, unless the driver is on a delivery. Ending twice
// changes nothing.
func (s *Store) EndShift(ctx context.Context, profileID uuid.UUID) (Driver, error) {
	d, err := scanDriver(s.pool.QueryRow(ctx, `
		UPDATE drivers SET status = 'offline', updated_at = now()
		WHERE profile_id = $1 AND status <> 'on_delivery'
		RETURNING `+returning, profileID))
	if errors.Is(err, ErrNotFound) {
		// No row changed: either there's no driver, or they're on a delivery.
		if _, err := s.Get(ctx, profileID); err != nil {
			return Driver{}, err
		}
		return Driver{}, ErrOnDelivery
	}
	if err != nil {
		return Driver{}, fmt.Errorf("end shift: %w", err)
	}
	return d, nil
}

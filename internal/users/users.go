// Package users stores accounts and the profiles (roles) attached to them.
package users

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Role string

const (
	RoleCustomer   Role = "customer"
	RoleRestaurant Role = "restaurant"
	RoleDriver     Role = "driver"
)

func (r Role) Valid() bool {
	switch r {
	case RoleCustomer, RoleRestaurant, RoleDriver:
		return true
	}
	return false
}

type User struct {
	ID           uuid.UUID `json:"id"`
	Email        string    `json:"email"`
	PasswordHash string    `json:"-"` // never sent to clients
	FullName     string    `json:"full_name"`
	CreatedAt    time.Time `json:"created_at"`
}

type Profile struct {
	ID        uuid.UUID `json:"id"`
	UserID    uuid.UUID `json:"-"`
	Role      Role      `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

var (
	ErrNotFound      = errors.New("not found")
	ErrEmailTaken    = errors.New("email already registered")
	ErrProfileExists = errors.New("user already has a profile with this role")
)

// uniqueViolation is PostgreSQL's error code for "this would break a UNIQUE rule".
const uniqueViolation = "23505"

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Create inserts a user together with their customer profile. Both inserts run in
// one transaction: either the account exists with its profile, or nothing does.
func (s *Store) Create(ctx context.Context, email, passwordHash, fullName string) (User, Profile, error) {
	u := User{ID: mustV7(), Email: email, PasswordHash: passwordHash, FullName: fullName}
	p := Profile{ID: mustV7(), UserID: u.ID, Role: RoleCustomer}

	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO users (id, email, password_hash, full_name)
			VALUES ($1, $2, $3, $4)
			RETURNING created_at`,
			u.ID, u.Email, u.PasswordHash, u.FullName,
		).Scan(&u.CreatedAt)
		if isUniqueViolation(err) {
			return ErrEmailTaken
		}
		if err != nil {
			return fmt.Errorf("insert user: %w", err)
		}

		err = tx.QueryRow(ctx, `
			INSERT INTO profiles (id, user_id, role) VALUES ($1, $2, $3)
			RETURNING created_at`,
			p.ID, p.UserID, p.Role,
		).Scan(&p.CreatedAt)
		if err != nil {
			return fmt.Errorf("insert profile: %w", err)
		}
		return nil
	})
	if err != nil {
		return User{}, Profile{}, err
	}
	return u, p, nil
}

func (s *Store) GetByEmail(ctx context.Context, email string) (User, error) {
	return s.getUser(ctx, `WHERE email = $1`, email)
}

func (s *Store) GetByID(ctx context.Context, id uuid.UUID) (User, error) {
	return s.getUser(ctx, `WHERE id = $1`, id)
}

func (s *Store) getUser(ctx context.Context, where string, arg any) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx, `
		SELECT id, email, password_hash, full_name, created_at FROM users `+where, arg,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.FullName, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("get user: %w", err)
	}
	return u, nil
}

// AddProfile gives an existing user another role.
func (s *Store) AddProfile(ctx context.Context, userID uuid.UUID, role Role) (Profile, error) {
	p := Profile{ID: mustV7(), UserID: userID, Role: role}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO profiles (id, user_id, role) VALUES ($1, $2, $3)
		RETURNING created_at`,
		p.ID, p.UserID, p.Role,
	).Scan(&p.CreatedAt)
	if isUniqueViolation(err) {
		return Profile{}, ErrProfileExists
	}
	if err != nil {
		return Profile{}, fmt.Errorf("insert profile: %w", err)
	}
	return p, nil
}

// ProfileByRole finds the user's profile for a role. Each user has at most one per
// role, so the pair identifies it.
func (s *Store) ProfileByRole(ctx context.Context, userID uuid.UUID, role Role) (Profile, error) {
	var p Profile
	err := s.pool.QueryRow(ctx, `
		SELECT id, user_id, role, created_at FROM profiles
		WHERE user_id = $1 AND role = $2`, userID, role,
	).Scan(&p.ID, &p.UserID, &p.Role, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Profile{}, ErrNotFound
	}
	if err != nil {
		return Profile{}, fmt.Errorf("get profile: %w", err)
	}
	return p, nil
}

func (s *Store) ListProfiles(ctx context.Context, userID uuid.UUID) ([]Profile, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, role, created_at FROM profiles
		WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, fmt.Errorf("list profiles: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Profile, error) {
		var p Profile
		err := row.Scan(&p.ID, &p.UserID, &p.Role, &p.CreatedAt)
		return p, err
	})
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation
}

func mustV7() uuid.UUID {
	return uuid.Must(uuid.NewV7())
}

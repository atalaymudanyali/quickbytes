package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/atalaymudanyali/quickbytes/internal/users"
)

const issuer = "quickbytes"

var ErrInvalidToken = errors.New("invalid or expired token")

// Claims are the facts an access token carries. Anyone can read them (a JWT is
// only signed, not encrypted), so they must never hold secrets.
type Claims struct {
	jwt.RegisteredClaims
	// Roles lists the profiles the user had when the token was issued. Routes check
	// it without a database lookup. A newly added profile appears after the next
	// refresh.
	Roles []users.Role `json:"roles"`
}

func (c Claims) UserID() (uuid.UUID, error) {
	return uuid.Parse(c.Subject)
}

func (c Claims) HasRole(role users.Role) bool {
	for _, r := range c.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// TokenIssuer signs and checks access tokens with HMAC-SHA256: the same secret
// signs and verifies, which is fine while only this service checks them.
type TokenIssuer struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time
}

const minSecretLen = 32

func NewTokenIssuer(secret []byte, ttl time.Duration) (*TokenIssuer, error) {
	if len(secret) < minSecretLen {
		return nil, fmt.Errorf("token secret must be at least %d bytes", minSecretLen)
	}
	return &TokenIssuer{secret: secret, ttl: ttl, now: time.Now}, nil
}

func (t *TokenIssuer) Issue(userID uuid.UUID, roles []users.Role) (string, time.Time, error) {
	now := t.now()
	expires := now.Add(t.ttl)
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			Subject:   userID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expires),
		},
		Roles: roles,
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign token: %w", err)
	}
	return signed, expires, nil
}

// Parse checks the signature, issuer and expiry, and returns the claims.
func (t *TokenIssuer) Parse(token string) (Claims, error) {
	var claims Claims
	_, err := jwt.ParseWithClaims(token, &claims,
		func(*jwt.Token) (any, error) { return t.secret, nil },
		// Only accept the one algorithm we sign with. Otherwise a forged token
		// could claim "alg": "none" (no signature) and be trusted.
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(issuer),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(t.now),
	)
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	if _, err := claims.UserID(); err != nil {
		return Claims{}, ErrInvalidToken
	}
	return claims, nil
}

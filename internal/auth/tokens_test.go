package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/atalaymudanyali/quickbytes/internal/users"
)

var testSecret = []byte(strings.Repeat("s", 32))

func newTestIssuer(t *testing.T) *TokenIssuer {
	t.Helper()
	ti, err := NewTokenIssuer(testSecret, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return ti
}

func TestIssueAndParse(t *testing.T) {
	ti := newTestIssuer(t)
	userID := uuid.Must(uuid.NewV7())

	token, _, err := ti.Issue(userID, []users.Role{users.RoleCustomer, users.RoleDriver})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	claims, err := ti.Parse(token)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got, _ := claims.UserID(); got != userID {
		t.Fatalf("user ID = %v, want %v", got, userID)
	}
	if !claims.HasRole(users.RoleDriver) || claims.HasRole(users.RoleRestaurant) {
		t.Fatalf("roles = %v, want customer and driver only", claims.Roles)
	}
}

func TestRejectsBadTokens(t *testing.T) {
	ti := newTestIssuer(t)
	good, _, _ := ti.Issue(uuid.Must(uuid.NewV7()), []users.Role{users.RoleCustomer})

	expired := newTestIssuer(t)
	expired.now = func() time.Time { return time.Now().Add(-time.Hour) }
	expiredToken, _, _ := expired.Issue(uuid.Must(uuid.NewV7()), nil)

	other, _ := NewTokenIssuer([]byte(strings.Repeat("x", 32)), time.Minute)
	otherToken, _, _ := other.Issue(uuid.Must(uuid.NewV7()), nil)

	// "alg: none" means no signature at all. It must never be accepted.
	unsigned, _ := jwt.NewWithClaims(jwt.SigningMethodNone, Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: issuer, Subject: uuid.Must(uuid.NewV7()).String(),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Roles: []users.Role{users.RoleRestaurant},
	}).SignedString(jwt.UnsafeAllowNoneSignatureType)

	// Change one character in the middle of the payload (the token's middle part):
	// the signature no longer matches.
	parts := strings.Split(good, ".")
	mid := len(parts[1]) / 2
	parts[1] = parts[1][:mid] + flip(parts[1][mid]) + parts[1][mid+1:]
	tampered := strings.Join(parts, ".")

	for name, token := range map[string]string{
		"expired":          expiredToken,
		"other secret":     otherToken,
		"no signature":     unsigned,
		"tampered payload": tampered,
		"garbage":          "not.a.token",
		"empty":            "",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ti.Parse(token); !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("err = %v, want ErrInvalidToken", err)
			}
		})
	}
}

func TestShortSecretRefused(t *testing.T) {
	if _, err := NewTokenIssuer([]byte("short"), time.Minute); err == nil {
		t.Fatal("a 5-byte secret was accepted")
	}
}

func flip(c byte) string {
	if c == 'A' {
		return "B"
	}
	return "A"
}

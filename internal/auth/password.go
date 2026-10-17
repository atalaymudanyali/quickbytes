// Package auth handles passwords and login tokens.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Params control how expensive one argon2id hash is. Memory is the main defence:
// an attacker guessing passwords on graphics cards can run many guesses in parallel,
// but each guess needs this much memory.
type Params struct {
	MemoryKiB uint32
	Time      uint32 // passes over the memory
	Threads   uint8
	SaltLen   uint32
	KeyLen    uint32
}

// DefaultParams follow the OWASP Password Storage Cheat Sheet's argon2id minimum
// (19 MiB, 2 passes, 1 thread). Higher memory is stronger, but every login in
// progress holds that much memory, so it also limits how many logins run at once.
var DefaultParams = Params{MemoryKiB: 19 * 1024, Time: 2, Threads: 1, SaltLen: 16, KeyLen: 32}

// FastParams are for tests only: same algorithm, tiny cost.
var FastParams = Params{MemoryKiB: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

var ErrMalformedHash = errors.New("malformed password hash")

type Hasher struct {
	params Params
}

func NewHasher(p Params) *Hasher {
	return &Hasher{params: p}
}

// Hash returns the password in the standard argon2 text format, which records the
// algorithm, cost settings and salt next to the hash:
//
//	$argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>
//
// Storing the settings with each hash means the defaults can be raised later and old
// hashes still verify.
func (h *Hasher) Hash(password string) (string, error) {
	salt := make([]byte, h.params.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	p := h.params
	key := argon2.IDKey([]byte(password), salt, p.Time, p.MemoryKiB, p.Threads, p.KeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.MemoryKiB, p.Time, p.Threads,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// Verify reports whether password matches the stored hash.
func (h *Hasher) Verify(password, encoded string) (bool, error) {
	p, salt, want, err := decode(encoded)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, p.Time, p.MemoryKiB, p.Threads, uint32(len(want)))
	// Constant-time comparison: takes the same time whether the first byte or the
	// last byte differs, so timing reveals nothing about the hash.
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

var b64 = base64.RawStdEncoding

func decode(encoded string) (Params, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return Params{}, nil, nil, ErrMalformedHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return Params{}, nil, nil, ErrMalformedHash
	}
	var p Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.MemoryKiB, &p.Time, &p.Threads); err != nil {
		return Params{}, nil, nil, ErrMalformedHash
	}
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return Params{}, nil, nil, ErrMalformedHash
	}
	key, err := b64.DecodeString(parts[5])
	if err != nil || len(key) == 0 {
		return Params{}, nil, nil, ErrMalformedHash
	}
	return p, salt, key, nil
}

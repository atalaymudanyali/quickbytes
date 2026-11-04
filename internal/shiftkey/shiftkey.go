// Package shiftkey makes and checks the credentials drivers' phones sign their
// location messages with.
//
// Think of couriers sending postcards from the road. Anyone handling a postcard
// can read it, and a UDP message is just as open. So the postcard can't carry a
// password: whoever read it could copy the password and pretend to be that
// courier. Instead, each courier gets a personal stamp at shift start, over the
// secure API, and every postcard carries only the stamp's imprint:
//
//	stamp   = HMAC(server secret, driver ID + expiry)
//	imprint = HMAC(stamp, message)
//
// The postcard also says who sent it and when the stamp expires, so the checker
// needs no list of couriers: it works out the stamp again from the server secret
// and compares the imprints. At 2,000 location messages a second, that saves
// 2,000 database lookups a second. Changing the expiry on a postcard gives a
// different stamp, so the imprint stops matching.
//
// The cost of storing nothing: a stamp can't be cancelled, so it keeps working
// until it expires, even after the shift ends.
package shiftkey

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	// ErrExpired: the key's expiry has passed.
	ErrExpired = errors.New("shift key has expired")
	// ErrBadSignature: the signature doesn't match the message, driver and
	// expiry. It may be a forgery, or a key made with another server secret.
	ErrBadSignature = errors.New("shift key signature doesn't match")
)

// Key is what a driver receives at shift start. Secret goes to the driver's
// phone once, over the API, and never travels in a location message.
type Key struct {
	DriverID  uuid.UUID
	ExpiresAt time.Time
	Secret    []byte
}

// Issue makes a key for driverID that lasts ttl from now. Expiry is kept in whole
// seconds, the precision a location message carries.
func Issue(serverSecret string, driverID uuid.UUID, now time.Time, ttl time.Duration) Key {
	expiresAt := now.Add(ttl).Truncate(time.Second)
	return Key{
		DriverID:  driverID,
		ExpiresAt: expiresAt,
		Secret:    derive(serverSecret, driverID, expiresAt),
	}
}

// derive is the stamp. The driver ID and expiry both have a fixed length, so two
// different pairs can never run together into the same input.
func derive(serverSecret string, driverID uuid.UUID, expiresAt time.Time) []byte {
	h := hmac.New(sha256.New, []byte(serverSecret))
	h.Write(driverID[:])
	h.Write(binary.BigEndian.AppendUint64(nil, uint64(expiresAt.Unix())))
	return h.Sum(nil)
}

// Sign returns the signature of message: what the phone does with its key.
func Sign(secret, message []byte) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write(message)
	return h.Sum(nil)
}

// Verify checks that signature was made for message with the key of this driver
// and expiry, and that the key is still valid at now. driverID and expiresAt come
// from the message itself; they're only trusted once the signature matches.
func Verify(serverSecret string, driverID uuid.UUID, expiresAt time.Time, message, signature []byte, now time.Time) error {
	if !now.Before(expiresAt) {
		return ErrExpired
	}
	// hmac.Equal takes the same time wherever the first difference is, so timing
	// answers can't reveal a correct signature piece by piece.
	if !hmac.Equal(signature, Sign(derive(serverSecret, driverID, expiresAt), message)) {
		return ErrBadSignature
	}
	return nil
}

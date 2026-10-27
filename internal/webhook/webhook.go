// Package webhook signs and checks the messages the payment provider sends to
// the API (webhooks), the way Stripe does.
//
// A webhook is a letter posted to a public address: anyone can send one and
// claim "order 42 is paid". So each letter carries a seal only the real sender
// can make. Both sides know one shared secret; the sender mixes the secret
// with the letter through HMAC-SHA256 and sends the result in a header:
//
//	QB-Signature: t=1792927973,v1=5257a869e7ecebeda32affa62cdca3fa51cad7e77a0e56ff536d0ce8e108d8bd
//
// t is when it was signed (Unix seconds) and v1 the seal (hex). The receiver
// makes the same mix itself and compares. Without the secret, nobody can make
// a seal that matches, and changing one byte of the letter changes the whole
// seal.
//
// The time is sealed too, and a letter more than Tolerance old is refused:
// someone who copied a real, sealed letter can't post it again next week (a
// replay).
//
// Both services use this package, the way a real provider ships a small
// library that merchants use to check its webhooks.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Header is the HTTP header the signature travels in.
const Header = "QB-Signature"

// Tolerance is how far a signature's time may be from the receiver's clock,
// either way: old letters are replays, and a little ahead is allowed because
// two machines' clocks never agree exactly.
const Tolerance = 5 * time.Minute

var (
	// ErrNoSignature: the header is missing or empty.
	ErrNoSignature = errors.New("no webhook signature")
	// ErrBadHeader: the header isn't "t=<unix>,v1=<hex>".
	ErrBadHeader = errors.New("webhook signature header is malformed")
	// ErrTooOld: the signature's time is more than Tolerance from now.
	ErrTooOld = errors.New("webhook signature is too old (or from the future)")
	// ErrBadSignature: no v1 seal matches the body and the secret.
	ErrBadSignature = errors.New("webhook signature doesn't match")
)

// Sign returns the header value for body, signed with secret at time t.
func Sign(secret string, t time.Time, body []byte) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac(secret, ts, body))
}

// mac is the seal: HMAC-SHA256 of "<t>.<body>" with the secret as the key.
// The time goes in with the body, so it can't be changed without breaking
// the seal.
func mac(secret, ts string, body []byte) []byte {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(ts))
	h.Write([]byte("."))
	h.Write(body)
	return h.Sum(nil)
}

// Verify checks that header is a valid signature of body with secret, made
// within Tolerance of now. body must be the exact bytes received, before any
// JSON parsing: parsing and writing JSON back can change spaces or key order,
// and then the seal no longer matches.
//
// The header may carry several v1 values (the sender signing with an old and
// a new secret while a secret is being replaced); one matching is enough.
// Other schemes (v0=...) are skipped, so a sender can add new ones later.
func Verify(secret, header string, body []byte, now time.Time) error {
	if header == "" {
		return ErrNoSignature
	}
	var ts string
	var seals [][]byte
	for part := range strings.SplitSeq(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return ErrBadHeader
		}
		switch k {
		case "t":
			if ts != "" {
				return ErrBadHeader // two times: which one was signed?
			}
			ts = v
		case "v1":
			seal, err := hex.DecodeString(v)
			if err != nil {
				return ErrBadHeader
			}
			seals = append(seals, seal)
		}
	}
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || len(seals) == 0 {
		return ErrBadHeader
	}

	// The time is checked before the seal, but both answers are the same
	// 400 to the sender, so the order tells an attacker nothing.
	if d := now.Sub(time.Unix(unix, 0)); d > Tolerance || d < -Tolerance {
		return ErrTooOld
	}

	want := mac(secret, ts, body)
	for _, seal := range seals {
		// hmac.Equal takes the same time whether the first byte differs or
		// the last. A plain == stops at the first difference, so by timing
		// many tries an attacker could find a valid seal byte by byte.
		if hmac.Equal(seal, want) {
			return nil
		}
	}
	return ErrBadSignature
}

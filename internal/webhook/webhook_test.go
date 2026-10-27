package webhook

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const secret = "whsec_test_secret"

var (
	body   = []byte(`{"id":"evt_1"}`)
	signed = time.Unix(1792927973, 0)
)

// The seal worked out by a different tool, so Sign isn't only checked
// against itself:
//
//	printf '%s' '1792927973.{"id":"evt_1"}' | openssl dgst -sha256 -hmac 'whsec_test_secret'
func TestSignMatchesOpenSSL(t *testing.T) {
	want := "t=1792927973,v1=5fcc867543df15ab2d73a277a81f74cfee9688f3b0312ba724e693e4bad98cc6"
	if got := Sign(secret, signed, body); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestVerify(t *testing.T) {
	good := Sign(secret, signed, body)
	_, goodSeal, _ := strings.Cut(good, ",")
	otherSeal := "v1=" + strings.Repeat("ab", 32)

	tests := []struct {
		name   string
		secret string
		header string
		body   []byte
		now    time.Time
		want   error
	}{
		{"valid", secret, good, body, signed, nil},
		{"4 minutes later", secret, good, body, signed.Add(4 * time.Minute), nil},
		{"clock a little behind the sender's", secret, good, body, signed.Add(-time.Minute), nil},
		{"6 minutes later: a replay", secret, good, body, signed.Add(6 * time.Minute), ErrTooOld},
		{"far in the future", secret, good, body, signed.Add(-time.Hour), ErrTooOld},
		{"wrong secret", "whsec_someone_else", good, body, signed, ErrBadSignature},
		{"body changed by one byte", secret, good, []byte(`{"id":"evt_2"}`), signed, ErrBadSignature},
		// Moving the time forward to dodge the 5-minute check breaks the seal:
		// the time is part of what was signed.
		{"time changed", secret, "t=1792928273," + goodSeal, body, signed.Add(5 * time.Minute), ErrBadSignature},
		{"old secret's seal and new one's", secret, "t=1792927973," + otherSeal + "," + goodSeal, body, signed, nil},
		{"unknown scheme skipped", secret, good + ",v0=whatever", body, signed, nil},
		{"spaces after commas", secret, strings.ReplaceAll(good, ",", ", "), body, signed, nil},
		{"missing", secret, "", body, signed, ErrNoSignature},
		{"no seal", secret, "t=1792927973", body, signed, ErrBadHeader},
		{"no time", secret, goodSeal, body, signed, ErrBadHeader},
		{"two times", secret, "t=1792927973,t=1792927974," + goodSeal, body, signed, ErrBadHeader},
		{"time not a number", secret, "t=yesterday," + goodSeal, body, signed, ErrBadHeader},
		{"seal not hex", secret, "t=1792927973,v1=zz", body, signed, ErrBadHeader},
		{"no equals sign", secret, "garbage", body, signed, ErrBadHeader},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := Verify(tt.secret, tt.header, tt.body, tt.now); !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
		})
	}
}

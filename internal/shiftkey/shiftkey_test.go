package shiftkey

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

const testSecret = "test-only-shift-secret-0123456789abcdef"

var (
	testNow = time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	testMsg = []byte("41.0370,28.9850,counter=7")
)

func TestVerify(t *testing.T) {
	driver := uuid.New()
	key := Issue(testSecret, driver, testNow, 12*time.Hour)
	sig := Sign(key.Secret, testMsg)

	tests := []struct {
		name      string
		secret    string
		driver    uuid.UUID
		expiresAt time.Time
		message   []byte
		signature []byte
		now       time.Time
		want      error
	}{
		{"valid", testSecret, driver, key.ExpiresAt, testMsg, sig, testNow, nil},
		{"valid until the last moment", testSecret, driver, key.ExpiresAt, testMsg, sig, key.ExpiresAt.Add(-time.Second), nil},
		{"expired at exactly its expiry", testSecret, driver, key.ExpiresAt, testMsg, sig, key.ExpiresAt, ErrExpired},
		{"expired", testSecret, driver, key.ExpiresAt, testMsg, sig, key.ExpiresAt.Add(time.Hour), ErrExpired},
		{"wrong server secret", "another-secret-0123456789abcdef-0123", driver, key.ExpiresAt, testMsg, sig, testNow, ErrBadSignature},
		{"another driver", testSecret, uuid.New(), key.ExpiresAt, testMsg, sig, testNow, ErrBadSignature},
		{"expiry pushed later", testSecret, driver, key.ExpiresAt.Add(24 * time.Hour), testMsg, sig, testNow, ErrBadSignature},
		{"changed message", testSecret, driver, key.ExpiresAt, []byte("41.0370,28.9850,counter=8"), sig, testNow, ErrBadSignature},
		{"no signature", testSecret, driver, key.ExpiresAt, testMsg, nil, testNow, ErrBadSignature},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Verify(tt.secret, tt.driver, tt.expiresAt, tt.message, tt.signature, tt.now)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestIssue(t *testing.T) {
	driver := uuid.New()
	key := Issue(testSecret, driver, testNow.Add(500*time.Millisecond), time.Hour)

	if want := testNow.Add(time.Hour); !key.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v in whole seconds", key.ExpiresAt, want)
	}
	if other := Issue(testSecret, uuid.New(), testNow, time.Hour); bytes.Equal(key.Secret, other.Secret) {
		t.Error("two drivers got the same key")
	}
	if later := Issue(testSecret, driver, testNow, 2*time.Hour); bytes.Equal(key.Secret, later.Secret) {
		t.Error("two expiries gave the same key")
	}
	if bytes.Contains(key.Secret, []byte(testSecret)) {
		t.Error("the key contains the server secret")
	}
}

// BenchmarkVerify is the cost of checking one location message.
func BenchmarkVerify(b *testing.B) {
	key := Issue(testSecret, uuid.New(), testNow, 12*time.Hour)
	sig := Sign(key.Secret, testMsg)
	for b.Loop() {
		if err := Verify(testSecret, key.DriverID, key.ExpiresAt, testMsg, sig, testNow); err != nil {
			b.Fatal(err)
		}
	}
}

// The key and signature worked out by a different tool, so they aren't only
// checked against this package itself. The input is the driver's 16 ID bytes then
// the expiry as 8 bytes:
//
//	printf '01900000000070008000000000000001%016x' 1792927973 | xxd -r -p | openssl dgst -sha256 -hmac 'test-only-shift-secret-0123456789abcdef'
//	printf '%s' '41.0370,28.9850,counter=7' | openssl dgst -sha256 -mac HMAC -macopt hexkey:<the key above>
func TestMatchesOpenSSL(t *testing.T) {
	const (
		wantKey = "3c2684a79a063f298517bf4fd52065a5136eab9558efeb91a84d7b45009239f6"
		wantSig = "b06544213050b7be840c2ab1cfad424e3a10e59960fd429744829584335f86f6"
	)
	driver := uuid.MustParse("01900000-0000-7000-8000-000000000001")
	key := Issue(testSecret, driver, time.Unix(1792927973, 0), 0)
	if got := hex.EncodeToString(key.Secret); got != wantKey {
		t.Errorf("key\ngot  %s\nwant %s", got, wantKey)
	}
	if got := hex.EncodeToString(Sign(key.Secret, testMsg)); got != wantSig {
		t.Errorf("signature\ngot  %s\nwant %s", got, wantSig)
	}
}

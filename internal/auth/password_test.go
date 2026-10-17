package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestHashAndVerify(t *testing.T) {
	h := NewHasher(FastParams)

	encoded, err := h.Hash("correct horse")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$v=19$m=64,t=1,p=1$") {
		t.Fatalf("hash %q doesn't record its algorithm and settings", encoded)
	}
	if strings.Contains(encoded, "correct horse") {
		t.Fatal("hash contains the password")
	}

	if ok, err := h.Verify("correct horse", encoded); !ok || err != nil {
		t.Fatalf("right password: ok=%v err=%v", ok, err)
	}
	if ok, _ := h.Verify("correct horse ", encoded); ok {
		t.Fatal("wrong password accepted")
	}
}

func TestSamePasswordDifferentHashes(t *testing.T) {
	h := NewHasher(FastParams)
	a, _ := h.Hash("same")
	b, _ := h.Hash("same")
	if a == b {
		t.Fatal("two hashes of the same password are equal: salt isn't random")
	}
}

func TestVerifyUsesStoredSettings(t *testing.T) {
	// A hash made with other settings must still verify after defaults change.
	old, _ := NewHasher(Params{MemoryKiB: 128, Time: 2, Threads: 1, SaltLen: 8, KeyLen: 16}).Hash("pw")
	if ok, err := NewHasher(FastParams).Verify("pw", old); !ok || err != nil {
		t.Fatalf("old-settings hash: ok=%v err=%v", ok, err)
	}
}

func TestVerifyMalformed(t *testing.T) {
	h := NewHasher(FastParams)
	for _, bad := range []string{"", "plain", "$bcrypt$x$y$z$w", "$argon2id$v=19$m=x$a$b"} {
		if _, err := h.Verify("pw", bad); !errors.Is(err, ErrMalformedHash) {
			t.Errorf("Verify(%q) err = %v, want ErrMalformedHash", bad, err)
		}
	}
}

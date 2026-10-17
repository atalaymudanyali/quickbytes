package auth

import "testing"

// go test -bench Hash -benchmem ./internal/auth
func BenchmarkHashDefault(b *testing.B) {
	h := NewHasher(DefaultParams)
	for b.Loop() {
		_, _ = h.Hash("correct horse battery staple")
	}
}

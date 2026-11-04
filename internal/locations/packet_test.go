package locations

import (
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atalaymudanyali/quickbytes/internal/shiftkey"
)

const testSecret = "test-only-shift-secret-0123456789abcdef"

var testNow = time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)

// testPacket returns a valid packet and the ping and key behind it.
func testPacket() ([]byte, Ping, shiftkey.Key) {
	key := shiftkey.Issue(testSecret, uuid.New(), testNow, 12*time.Hour)
	p := Ping{
		DriverID:     key.DriverID,
		KeyExpiresAt: key.ExpiresAt,
		Counter:      uint64(testNow.UnixMilli()),
		Lat:          41.0370,
		Lng:          28.9850,
	}
	return Seal(p, key.Secret), p, key
}

func TestSealThenOpen(t *testing.T) {
	packet, want, _ := testPacket()
	if len(packet) != PacketSize {
		t.Fatalf("packet is %d bytes, want %d", len(packet), PacketSize)
	}
	got, err := Open(testSecret, packet, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if got.DriverID != want.DriverID || !got.KeyExpiresAt.Equal(want.KeyExpiresAt) ||
		got.Counter != want.Counter || got.Lat != want.Lat || got.Lng != want.Lng {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestCoordinatesSurviveTheTrip(t *testing.T) {
	key := shiftkey.Issue(testSecret, uuid.New(), testNow, time.Hour)
	tests := []struct {
		name     string
		lat, lng float64
	}{
		{"istanbul", 41.0369999, 28.9850001},
		{"needs rounding, or it lands one unit short", 41.0000040, 41.0000046},
		{"south and west are negative", -33.8688197, -151.2092955},
		{"the very edge of the map", maxLatitude, maxLongitude},
		{"the other edge", -maxLatitude, -maxLongitude},
		{"zero", 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := Ping{DriverID: key.DriverID, KeyExpiresAt: key.ExpiresAt, Lat: tt.lat, Lng: tt.lng}
			got, err := Open(testSecret, Seal(p, key.Secret), testNow)
			if err != nil {
				t.Fatal(err)
			}
			if got.Lat != tt.lat || got.Lng != tt.lng {
				t.Fatalf("got %v,%v, want %v,%v", got.Lat, got.Lng, tt.lat, tt.lng)
			}
		})
	}
}

// Every byte is protected: the driver, the expiry, the counter, the position, the
// version and the signature itself.
func TestChangingAnyByteIsRefused(t *testing.T) {
	packet, _, _ := testPacket()
	for i := range packet {
		changed := append([]byte(nil), packet...)
		changed[i] ^= 0x01
		if _, err := Open(testSecret, changed, testNow); err == nil {
			t.Errorf("byte %d changed and the packet was still accepted", i)
		}
	}
}

func TestOpenRefuses(t *testing.T) {
	packet, ping, key := testPacket()

	otherKey := shiftkey.Issue(testSecret, uuid.New(), testNow, time.Hour)
	forged := Seal(ping, otherKey.Secret)

	unsigned := append([]byte(nil), packet...)
	clear(unsigned[signedSize:])

	wrongVersion := append([]byte(nil), packet...)
	wrongVersion[0] = 2

	offMap := ping
	offMap.Lat = 89
	offLng := ping
	offLng.Lng = 181

	tests := []struct {
		name   string
		packet []byte
		now    time.Time
		want   error
	}{
		{"another driver's key", forged, testNow, shiftkey.ErrBadSignature},
		{"no real signature", unsigned, testNow, shiftkey.ErrBadSignature},
		{"expired key", packet, key.ExpiresAt, shiftkey.ErrExpired},
		{"empty", nil, testNow, ErrMalformed},
		{"too short", packet[:PacketSize-1], testNow, ErrMalformed},
		{"too long", append(append([]byte(nil), packet...), 0), testNow, ErrMalformed},
		{"unknown version", wrongVersion, testNow, ErrMalformed},
		{"latitude Redis can't store", Seal(offMap, key.Secret), testNow, ErrMalformed},
		{"longitude off the map", Seal(offLng, key.Secret), testNow, ErrMalformed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Open(testSecret, tt.packet, tt.now); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

// BenchmarkOpen is the cost of reading and checking one packet.
func BenchmarkOpen(b *testing.B) {
	packet, _, _ := testPacket()
	for b.Loop() {
		if _, err := Open(testSecret, packet, testNow); err != nil {
			b.Fatal(err)
		}
	}
}

// The whole packet worked out by a different tool, so the layout is pinned down and
// a phone written in another language has something to match:
//
//	BODY=$(printf '01%s%016x%016x%08x%08x' 01900000000070008000000000000001 1792927973 1792884000123 410370000 289850000)
//	KEY=$(printf '01900000000070008000000000000001%016x' 1792927973 | xxd -r -p | openssl dgst -sha256 -hmac 'test-only-shift-secret-0123456789abcdef' | awk '{print $NF}')
//	printf '%s' "$BODY" | xxd -r -p | openssl dgst -sha256 -mac HMAC -macopt hexkey:$KEY
func TestMatchesOpenSSL(t *testing.T) {
	const want = "01" + // version
		"01900000000070008000000000000001" + // driver
		"000000006adde8e5" + // expiry
		"000001a17036c57b" + // counter
		"1875bfd0" + // latitude
		"1146c290" + // longitude
		"55a1e39c2b1e11c9aea6201a668cec9139b42f766437f062b423c2d0b80006e7" // signature

	driver := uuid.MustParse("01900000-0000-7000-8000-000000000001")
	key := shiftkey.Issue(testSecret, driver, time.Unix(1792927973, 0), 0)
	p := Ping{DriverID: driver, KeyExpiresAt: key.ExpiresAt, Counter: 1792884000123, Lat: 41.0370, Lng: 28.9850}

	if got := hex.EncodeToString(Seal(p, key.Secret)); got != want {
		t.Fatalf("packet\ngot  %s\nwant %s", got, want)
	}
}

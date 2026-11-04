// Package locations keeps the live map of where drivers are.
//
// This file is the postcard a phone sends every 5 seconds: "this driver is here".
// Both doors of the location service (WebSocket and UDP) carry exactly these
// bytes, so there's one format to read and one security check, whichever way a
// postcard arrives.
//
// A UDP message has no login and anyone handling it can read it, so a postcard
// proves itself: it ends with an imprint of the driver's shift key (see the
// shiftkey package) over everything before it. Change one byte and the imprint
// stops matching. Nothing is hidden, only protected from being changed.
package locations

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"

	"github.com/atalaymudanyali/quickbytes/internal/shiftkey"
)

// The layout, in order, with numbers big-endian (largest byte first):
//
//	offset  size  field
//	     0     1  version
//	     1    16  driver ID
//	    17     8  key expiry, whole Unix seconds
//	    25     8  counter
//	    33     4  latitude,  in units of 0.0000001 degrees
//	    37     4  longitude, in units of 0.0000001 degrees
//	    41    32  signature of the 41 bytes before it
//
// Coordinates are whole numbers because floating point rounds differently from
// place to place, and 0.0000001 degrees is about 1 cm, far finer than GPS.
const (
	version       = 1
	signedSize    = 41
	PacketSize    = signedSize + 32
	degreesToUnit = 1e7

	// Redis can't store points closer to the poles than 85.05112878 (its map
	// projection stops there), so they're refused here, before they reach it. This
	// is the last value below that with the 7 decimals a packet holds.
	maxLatitude  = 85.0511287
	maxLongitude = 180
)

// ErrMalformed: the bytes aren't a location packet this service can read, or they
// hold a position that isn't on the map.
var ErrMalformed = errors.New("malformed location packet")

// Ping is one position report.
type Ping struct {
	DriverID uuid.UUID
	// KeyExpiresAt is the expiry of the shift key that signed this ping.
	KeyExpiresAt time.Time
	// Counter is the phone's clock in Unix milliseconds. It only has to keep
	// growing, so a late packet can be spotted, and a phone that restarts
	// carries on with nothing saved.
	Counter uint64
	Lat     float64
	Lng     float64
}

// Seal makes the signed packet for a ping: what a phone does with its shift key.
func Seal(p Ping, keySecret []byte) []byte {
	b := make([]byte, 0, PacketSize)
	b = append(b, version)
	b = append(b, p.DriverID[:]...)
	b = binary.BigEndian.AppendUint64(b, uint64(p.KeyExpiresAt.Unix()))
	b = binary.BigEndian.AppendUint64(b, p.Counter)
	b = binary.BigEndian.AppendUint32(b, uint32(int32(math.Round(p.Lat*degreesToUnit))))
	b = binary.BigEndian.AppendUint32(b, uint32(int32(math.Round(p.Lng*degreesToUnit))))
	return append(b, shiftkey.Sign(keySecret, b)...)
}

// Open reads a packet and checks it. It returns ErrMalformed, or shiftkey's
// ErrExpired or ErrBadSignature, and a ping only if all the checks pass: nothing in
// a packet is believed before its signature matches.
func Open(serverSecret string, packet []byte, now time.Time) (Ping, error) {
	if len(packet) != PacketSize {
		return Ping{}, fmt.Errorf("%w: %d bytes, want %d", ErrMalformed, len(packet), PacketSize)
	}
	if packet[0] != version {
		return Ping{}, fmt.Errorf("%w: version %d", ErrMalformed, packet[0])
	}

	driverID := uuid.UUID(packet[1:17])
	expiresAt := time.Unix(int64(binary.BigEndian.Uint64(packet[17:25])), 0)
	message, signature := packet[:signedSize], packet[signedSize:]
	if err := shiftkey.Verify(serverSecret, driverID, expiresAt, message, signature, now); err != nil {
		return Ping{}, err
	}

	p := Ping{
		DriverID:     driverID,
		KeyExpiresAt: expiresAt,
		Counter:      binary.BigEndian.Uint64(packet[25:33]),
		Lat:          float64(int32(binary.BigEndian.Uint32(packet[33:37]))) / degreesToUnit,
		Lng:          float64(int32(binary.BigEndian.Uint32(packet[37:41]))) / degreesToUnit,
	}
	if math.Abs(p.Lat) > maxLatitude || math.Abs(p.Lng) > maxLongitude {
		return Ping{}, fmt.Errorf("%w: position %v,%v is off the map", ErrMalformed, p.Lat, p.Lng)
	}
	return p, nil
}

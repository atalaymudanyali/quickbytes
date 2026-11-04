// Command locping plays a courier's phone by hand: it sends location pings to the
// location service, signed with the shift key from starting a shift. UDP never
// answers, so see what happened at http://localhost:8095/stats.
//
//	curl -s -X POST localhost:8080/driver/shift/start -H "Authorization: Bearer $TOKEN" > shift.json
//	go run ./cmd/locping -shift shift.json -lat 41.0370 -lng 28.9850
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/atalaymudanyali/quickbytes/internal/locations"
)

// shiftFile is the part of POST /driver/shift/start's answer a phone keeps.
type shiftFile struct {
	Driver struct {
		ID uuid.UUID `json:"id"`
	} `json:"driver"`
	ShiftKey struct {
		Key       []byte    `json:"key"`
		ExpiresAt time.Time `json:"expires_at"`
	} `json:"shift_key"`
}

func main() {
	var (
		shiftPath = flag.String("shift", "", "file holding the answer from POST /driver/shift/start (required)")
		// Not "localhost": that can mean IPv6's ::1 first, where Docker isn't
		// listening. TCP tries the next address when one fails, but UDP doesn't
		// find out, so every ping would quietly go nowhere.
		addr    = flag.String("addr", "127.0.0.1:8095", "where the location service is")
		lat     = flag.Float64("lat", 41.0370, "latitude")
		lng     = flag.Float64("lng", 28.9850, "longitude")
		counter = flag.Uint64("counter", 0, "counter to send, to imitate a late packet (default: the clock in milliseconds)")
		every   = flag.Duration("every", 0, "keep sending at this interval, like a real heartbeat: -every 5s (default: send once)")
		forge   = flag.Bool("forge", false, "sign with the wrong key, like an attacker would")
	)
	flag.Parse()
	if *shiftPath == "" {
		flag.Usage()
		os.Exit(2)
	}

	raw, err := os.ReadFile(*shiftPath)
	if err != nil {
		log.Fatal(err)
	}
	var shift shiftFile
	if err := json.Unmarshal(raw, &shift); err != nil {
		log.Fatalf("read %s: %v", *shiftPath, err)
	}
	if len(shift.ShiftKey.Key) == 0 {
		log.Fatalf("%s has no shift key: is it the answer from POST /driver/shift/start?", *shiftPath)
	}
	key := shift.ShiftKey.Key
	if *forge {
		key = []byte("not the real key")
	}

	conn, err := net.Dial("udp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	for {
		c := *counter
		if c == 0 {
			c = uint64(time.Now().UnixMilli())
		}
		packet := locations.Seal(locations.Ping{
			DriverID:     shift.Driver.ID,
			KeyExpiresAt: shift.ShiftKey.ExpiresAt,
			Counter:      c,
			Lat:          *lat,
			Lng:          *lng,
		}, key)
		if _, err := conn.Write(packet); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("sent %d bytes: driver %s at %.7f,%.7f, counter %d\n", len(packet), shift.Driver.ID, *lat, *lng, c)

		if *every == 0 {
			return
		}
		time.Sleep(*every)
	}
}

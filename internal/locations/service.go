package locations

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/atalaymudanyali/quickbytes/internal/shiftkey"
)

// redisLogEvery limits the "Redis is down" message. At 2,000 pings a second, one
// line per dropped ping would bury everything else in the log.
const redisLogEvery = 10 * time.Second

// DoorStats counts what happened to the packets that came through one door. They
// are the only feedback there is on UDP, which never answers.
type DoorStats struct {
	Received     atomic.Uint64
	Accepted     atomic.Uint64
	Stale        atomic.Uint64 // a same or newer counter had already arrived
	Malformed    atomic.Uint64
	BadSignature atomic.Uint64
	Expired      atomic.Uint64
	RedisErrors  atomic.Uint64 // good pings dropped because Redis didn't answer

	// arrivalAges counts accepted pings by how old they were on arrival, one
	// counter per entry of ArrivalAgeBuckets and one more for anything older.
	arrivalAges [len(ArrivalAgeBuckets) + 1]atomic.Uint64
}

// ArrivalAgeBuckets are the upper limits, in milliseconds, of the groups that
// accepted pings are counted in, by how old they were on arrival. The age is the
// server's clock minus the phone's, so it's only as good as those two agree: phones
// can be off by seconds, while a benchmark on one machine has one clock.
var ArrivalAgeBuckets = [...]int64{1, 2, 3, 5, 10, 20, 30, 40, 50, 60, 80, 100, 150, 200, 300, 400, 500, 750, 1000, 1500, 2000, 3000, 5000, 10000}

func (d *DoorStats) observeAge(ms int64) {
	for i, limit := range ArrivalAgeBuckets {
		if ms <= limit {
			d.arrivalAges[i].Add(1)
			return
		}
	}
	d.arrivalAges[len(ArrivalAgeBuckets)].Add(1)
}

// DoorCounts is one moment's reading of DoorStats.
type DoorCounts struct {
	Received     uint64 `json:"received"`
	Accepted     uint64 `json:"accepted"`
	Stale        uint64 `json:"stale"`
	Malformed    uint64 `json:"malformed"`
	BadSignature uint64 `json:"bad_signature"`
	Expired      uint64 `json:"expired"`
	RedisErrors  uint64 `json:"redis_errors"`
	// ArrivalAges lines up with Stats.ArrivalAgeBuckets, plus one for anything older.
	ArrivalAges []uint64 `json:"arrival_ages"`
}

// Counts reads the counters.
func (d *DoorStats) Counts() DoorCounts {
	ages := make([]uint64, len(d.arrivalAges))
	for i := range d.arrivalAges {
		ages[i] = d.arrivalAges[i].Load()
	}
	return DoorCounts{
		Received:     d.Received.Load(),
		Accepted:     d.Accepted.Load(),
		Stale:        d.Stale.Load(),
		Malformed:    d.Malformed.Load(),
		BadSignature: d.BadSignature.Load(),
		Expired:      d.Expired.Load(),
		RedisErrors:  d.RedisErrors.Load(),
		ArrivalAges:  ages,
	}
}

// Stats is what GET /stats answers.
type Stats struct {
	UDP       DoorCounts `json:"udp"`
	WebSocket DoorCounts `json:"websocket"`
	// Connections is how many WebSocket calls are open now, and SilentHangups how
	// many ended because nothing arrived for too long. UDP has neither, because
	// it has no calls.
	Connections   int64  `json:"websocket_connections"`
	SilentHangups uint64 `json:"websocket_silent_hangups"`
	// ArrivalAgeBuckets are the upper limits, in milliseconds, of each door's
	// arrival_ages.
	ArrivalAgeBuckets []int64 `json:"arrival_age_buckets_ms"`
	// MemoryBytes is what the service is holding right now, to see what open calls
	// cost.
	MemoryBytes uint64 `json:"memory_bytes"`
}

// Service is the back room behind both doors: every packet takes the same path
// from here, whichever way it arrived.
type Service struct {
	secret string
	store  *Store
	logger *slog.Logger
	// wsSilentAfter is how long a WebSocket call can go quiet before the service
	// hangs up.
	wsSilentAfter time.Duration

	UDP           DoorStats
	WebSocket     DoorStats
	Connections   atomic.Int64
	SilentHangups atomic.Uint64

	lastRedisLog atomic.Int64
}

// NewService returns a service that checks packets with secret and saves good ones
// to store.
func NewService(secret string, store *Store, logger *slog.Logger) *Service {
	return &Service{secret: secret, store: store, logger: logger, wsSilentAfter: SilentAfter}
}

// Handle takes one packet from a door. It returns an error only when the packet
// itself was refused. A good ping that Redis didn't take is dropped and counted but
// isn't the sender's fault, and the next one comes in 5 seconds anyway. That's what
// failing open means here: Redis being down never stops the service.
func (s *Service) Handle(ctx context.Context, door *DoorStats, packet []byte) error {
	door.Received.Add(1)
	now := time.Now()

	ping, err := Open(s.secret, packet, now)
	switch {
	case errors.Is(err, ErrMalformed):
		door.Malformed.Add(1)
		return err
	case errors.Is(err, shiftkey.ErrBadSignature):
		door.BadSignature.Add(1)
		return err
	case errors.Is(err, shiftkey.ErrExpired):
		door.Expired.Add(1)
		return err
	}

	saved, err := s.store.Save(ctx, ping, now)
	switch {
	case err != nil:
		door.RedisErrors.Add(1)
		s.logRedisError(now, err)
	case saved:
		door.Accepted.Add(1)
		door.observeAge(now.UnixMilli() - int64(ping.Counter))
	default:
		door.Stale.Add(1)
	}
	return nil
}

func (s *Service) logRedisError(now time.Time, err error) {
	last := s.lastRedisLog.Load()
	if now.UnixMilli()-last < redisLogEvery.Milliseconds() {
		return
	}
	// Only the door that wins this swap logs, if several notice at the same time.
	if s.lastRedisLog.CompareAndSwap(last, now.UnixMilli()) {
		s.logger.Warn("dropping pings: redis isn't answering", "err", err)
	}
}

// Routes serves the WebSocket door and the service's own pages.
func (s *Service) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", s.handleWebSocket)
	// Up as long as the process is: being up without Redis is on purpose, and the
	// redis_errors count shows it.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, _ *http.Request) {
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		writeJSON(w, Stats{
			UDP:               s.UDP.Counts(),
			WebSocket:         s.WebSocket.Counts(),
			Connections:       s.Connections.Load(),
			SilentHangups:     s.SilentHangups.Load(),
			ArrivalAgeBuckets: ArrivalAgeBuckets[:],
			MemoryBytes:       mem.HeapInuse + mem.StackInuse,
		})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

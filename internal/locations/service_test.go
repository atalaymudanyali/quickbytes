package locations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/atalaymudanyali/quickbytes/internal/cache"
	"github.com/atalaymudanyali/quickbytes/internal/shiftkey"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// sealedNow is a packet from a key valid right now, since Handle uses the real clock.
func sealedNow(driver uuid.UUID, counter uint64, lat, lng float64) []byte {
	key := shiftkey.Issue(testSecret, driver, time.Now(), time.Hour)
	return Seal(Ping{DriverID: driver, KeyExpiresAt: key.ExpiresAt, Counter: counter, Lat: lat, Lng: lng}, key.Secret)
}

func onMap(t *testing.T, s *Store, driver uuid.UUID) bool {
	t.Helper()
	err := s.redis.ZScore(context.Background(), s.geoKey, driver.String()).Err()
	if err != nil && !errors.Is(err, redis.Nil) {
		t.Fatal(err)
	}
	return err == nil
}

func TestHandleCountsEachOutcome(t *testing.T) {
	store := testStore(t)
	svc := NewService(testSecret, store, quiet)
	ctx := context.Background()

	driver := uuid.New()
	good := sealedNow(driver, 100, taksimLat, taksimLng)

	expiredKey := shiftkey.Issue(testSecret, driver, time.Now().Add(-2*time.Hour), time.Hour)
	expired := Seal(Ping{DriverID: driver, KeyExpiresAt: expiredKey.ExpiresAt, Counter: 200, Lat: taksimLat, Lng: taksimLng}, expiredKey.Secret)

	forged := append([]byte(nil), good...)
	forged[PacketSize-1] ^= 0x01

	tests := []struct {
		name    string
		packet  []byte
		want    error
		counter func(DoorCounts) uint64
	}{
		{"good", good, nil, func(c DoorCounts) uint64 { return c.Accepted }},
		{"the same packet again", good, nil, func(c DoorCounts) uint64 { return c.Stale }},
		{"older", sealedNow(driver, 99, kadikoyLat, kadikoyLng), nil, func(c DoorCounts) uint64 { return c.Stale - 1 }},
		{"bad signature", forged, shiftkey.ErrBadSignature, func(c DoorCounts) uint64 { return c.BadSignature }},
		{"expired", expired, shiftkey.ErrExpired, func(c DoorCounts) uint64 { return c.Expired }},
		{"garbage", []byte("hello"), ErrMalformed, func(c DoorCounts) uint64 { return c.Malformed }},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := svc.Handle(ctx, &svc.UDP, tt.packet); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			counts := svc.UDP.Counts()
			if got := tt.counter(counts); got != 1 {
				t.Errorf("its counter = %d, want 1: %+v", got, counts)
			}
			if counts.Received != uint64(i+1) {
				t.Errorf("received = %d, want %d", counts.Received, i+1)
			}
		})
	}

	found, err := store.Near(ctx, taksimLat, taksimLng, 100, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("only the good packet should count, and leave the driver at Taksim: got %+v", found)
	}
}

// Redis being down is counted, isn't blamed on the sender, and doesn't fill the log.
func TestRedisDownDropsThePingAndCarriesOn(t *testing.T) {
	c, err := cache.Open("redis://127.0.0.1:1/0") // port 1: nothing listens there
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var logged bytes.Buffer
	svc := NewService(testSecret, NewStore(c), slog.New(slog.NewTextHandler(&logged, nil)))

	const pings = 5
	for range pings {
		if err := svc.Handle(context.Background(), &svc.UDP, sealedNow(uuid.New(), 1, taksimLat, taksimLng)); err != nil {
			t.Fatalf("a good ping was refused: %v", err)
		}
	}
	if got := svc.UDP.Counts().RedisErrors; got != pings {
		t.Errorf("redis_errors = %d, want %d", got, pings)
	}
	if lines := strings.Count(logged.String(), "\n"); lines != 1 {
		t.Errorf("logged %d lines for %d drops, want 1:\n%s", lines, pings, logged.String())
	}
}

// startUDP opens a door on a free port and returns an address to send to, and a
// function that shuts it down and waits for it to finish.
func startUDP(t *testing.T, svc *Service) (net.Conn, func()) {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { svc.ServeUDP(ctx, conn) })
	shutdown := func() { cancel(); wg.Wait() }
	t.Cleanup(shutdown)

	sender, err := net.Dial("udp", conn.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sender.Close() })
	return sender, shutdown
}

// waitFor polls, because nothing tells a UDP sender when its packet has been handled.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestUDPDoor(t *testing.T) {
	store := testStore(t)
	svc := NewService(testSecret, store, quiet)
	sender, _ := startUDP(t, svc)

	// Junk of every shape first: it's counted, and the door stays open.
	forgedDriver := uuid.New()
	forged := sealedNow(forgedDriver, 1, taksimLat, taksimLng)
	forged[PacketSize-1] ^= 0x01
	oversized := append(sealedNow(uuid.New(), 1, taksimLat, taksimLng), make([]byte, 500)...)
	for _, junk := range [][]byte{[]byte("hello"), oversized, forged} {
		if _, err := sender.Write(junk); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "the junk to be counted", func() bool { return svc.UDP.Counts().Received == 3 })
	counts := svc.UDP.Counts()
	if counts.Malformed != 2 || counts.BadSignature != 1 || counts.Accepted != 0 {
		t.Errorf("after junk: %+v", counts)
	}
	if onMap(t, store, forgedDriver) {
		t.Error("a forged packet put a driver on the map")
	}

	driver := uuid.New()
	if _, err := sender.Write(sealedNow(driver, 1, taksimLat, taksimLng)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the ping to be saved", func() bool { return svc.UDP.Counts().Accepted == 1 })
	if !onMap(t, store, driver) {
		t.Error("the driver isn't on the map")
	}

	// The door never answers, to anyone.
	if err := sender.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if n, err := sender.Read(make([]byte, 100)); err == nil {
		t.Errorf("the door answered with %d bytes", n)
	}
}

func TestUDPDoorStopsWithItsContext(t *testing.T) {
	svc := NewService(testSecret, testStore(t), quiet)
	_, shutdown := startUDP(t, svc)

	done := make(chan struct{})
	go func() { shutdown(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the workers didn't stop")
	}
}

func TestStatsPage(t *testing.T) {
	svc := NewService(testSecret, testStore(t), quiet)
	_ = svc.Handle(context.Background(), &svc.UDP, []byte("hello"))

	srv := httptest.NewServer(svc.Routes())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var got map[string]DoorCounts
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["udp"].Received != 1 || got["udp"].Malformed != 1 {
		t.Errorf("stats = %+v", got)
	}

	health, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	health.Body.Close()
	if health.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d", health.StatusCode)
	}
}

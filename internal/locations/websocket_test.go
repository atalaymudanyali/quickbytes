package locations

import (
	"context"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/atalaymudanyali/quickbytes/internal/cache"
	"github.com/atalaymudanyali/quickbytes/internal/shiftkey"
)

// call opens a WebSocket to a test server running svc. Cancelling the returned
// function's context is the service shutting down.
func call(t *testing.T, svc *Service) (*websocket.Conn, func()) {
	t.Helper()
	ctx, shutdown := context.WithCancel(context.Background())
	t.Cleanup(shutdown)
	srv := httptest.NewUnstartedServer(svc.Routes())
	srv.Config.BaseContext = func(net.Listener) context.Context { return ctx }
	srv.Start()
	t.Cleanup(srv.CloseClientConnections)

	conn, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	waitFor(t, "the call to be open", func() bool { return svc.Connections.Load() == 1 })
	return conn, shutdown
}

func send(t *testing.T, conn *websocket.Conn, packet []byte) {
	t.Helper()
	if err := conn.Write(context.Background(), websocket.MessageBinary, packet); err != nil {
		t.Fatal(err)
	}
}

// closeStatus waits for the service to hang up and returns the reason it gave, or -1
// if the line just went dead.
func closeStatus(t *testing.T, conn *websocket.Conn) websocket.StatusCode {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err := conn.Read(ctx)
	if ctx.Err() != nil {
		t.Fatal("the service didn't hang up")
	}
	return websocket.CloseStatus(err)
}

func TestWebSocketDoorSavesPingsOnOneCall(t *testing.T) {
	store := testStore(t)
	svc := NewService(testSecret, store, quiet)
	conn, _ := call(t, svc)

	driver := uuid.New()
	send(t, conn, sealedNow(driver, 1, kadikoyLat, kadikoyLng))
	waitFor(t, "the first ping", func() bool { return svc.WebSocket.Counts().Accepted == 1 })
	send(t, conn, sealedNow(driver, 2, taksimLat, taksimLng))
	waitFor(t, "the second ping", func() bool { return svc.WebSocket.Counts().Accepted == 2 })

	found, err := store.Near(context.Background(), taksimLat, taksimLng, 100, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].DriverID != driver {
		t.Fatalf("got %+v, want %s at Taksim", found, driver)
	}
	if got := svc.UDP.Counts().Received; got != 0 {
		t.Errorf("the UDP door counted %d packets", got)
	}
}

// The doors share one back room: a late packet is late, whichever way either came.
func TestDoorsShareTheCounter(t *testing.T) {
	store := testStore(t)
	svc := NewService(testSecret, store, quiet)
	conn, _ := call(t, svc)
	sender, _ := startUDP(t, svc)
	driver := uuid.New()

	send(t, conn, sealedNow(driver, 100, taksimLat, taksimLng))
	waitFor(t, "the WebSocket ping", func() bool { return svc.WebSocket.Counts().Accepted == 1 })

	if _, err := sender.Write(sealedNow(driver, 99, kadikoyLat, kadikoyLng)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the late UDP ping", func() bool { return svc.UDP.Counts().Stale == 1 })

	found, err := store.Near(context.Background(), taksimLat, taksimLng, 100, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("the late packet moved the driver: %+v", found)
	}
}

func TestWebSocketDoorHangsUpOnBadPackets(t *testing.T) {
	driver := uuid.New()
	expiredKey := shiftkey.Issue(testSecret, driver, time.Now().Add(-2*time.Hour), time.Hour)
	expired := Seal(Ping{DriverID: driver, KeyExpiresAt: expiredKey.ExpiresAt, Lat: taksimLat, Lng: taksimLng}, expiredKey.Secret)

	forged := sealedNow(driver, 1, taksimLat, taksimLng)
	forged[PacketSize-1] ^= 0x01

	tests := []struct {
		name    string
		packet  []byte
		want    websocket.StatusCode
		counter func(DoorCounts) uint64
	}{
		{"expired key", expired, CloseKeyExpired, func(c DoorCounts) uint64 { return c.Expired }},
		{"bad signature", forged, CloseBadSignature, func(c DoorCounts) uint64 { return c.BadSignature }},
		{"garbage", []byte("hello"), websocket.StatusPolicyViolation, func(c DoorCounts) uint64 { return c.Malformed }},
		{"too long", make([]byte, PacketSize+1), websocket.StatusMessageTooBig, func(c DoorCounts) uint64 { return c.Malformed }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := testStore(t)
			svc := NewService(testSecret, store, quiet)
			conn, _ := call(t, svc)

			send(t, conn, tt.packet)
			if got := closeStatus(t, conn); got != tt.want {
				t.Errorf("closed with %v, want %v", got, tt.want)
			}
			waitFor(t, "the call to end", func() bool { return svc.Connections.Load() == 0 })
			counts := svc.WebSocket.Counts()
			if tt.counter(counts) != 1 || counts.Received != 1 || counts.Accepted != 0 {
				t.Errorf("counts = %+v", counts)
			}
			if onMap(t, store, driver) {
				t.Error("a refused packet put the driver on the map")
			}
		})
	}
}

// The tunnel: the line goes quiet with no goodbye, and the service hangs up.
func TestWebSocketDoorHangsUpOnSilence(t *testing.T) {
	svc := NewService(testSecret, testStore(t), quiet)
	svc.wsSilentAfter = 50 * time.Millisecond
	conn, _ := call(t, svc)

	closeStatus(t, conn)
	waitFor(t, "the call to end", func() bool { return svc.Connections.Load() == 0 })
	if got := svc.SilentHangups.Load(); got != 1 {
		t.Errorf("silent hang-ups = %d, want 1", got)
	}
}

func TestWebSocketDoorStaysOpenWhilePingsArrive(t *testing.T) {
	svc := NewService(testSecret, testStore(t), quiet)
	svc.wsSilentAfter = 200 * time.Millisecond
	conn, _ := call(t, svc)
	driver := uuid.New()

	// Longer in total than the silence limit, but never silent for that long.
	for i := range 10 {
		send(t, conn, sealedNow(driver, uint64(i+1), taksimLat, taksimLng))
		time.Sleep(50 * time.Millisecond)
	}
	if got := svc.Connections.Load(); got != 1 {
		t.Errorf("open calls = %d, want 1", got)
	}
	if got := svc.SilentHangups.Load(); got != 0 {
		t.Errorf("silent hang-ups = %d, want 0", got)
	}
}

// Redis being down isn't the phone's fault, so the call stays open for when it's back.
func TestWebSocketDoorStaysOpenWhenRedisIsDown(t *testing.T) {
	c, err := cache.Open("redis://127.0.0.1:1/0") // port 1: nothing listens there
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	svc := NewService(testSecret, NewStore(c), quiet)
	conn, _ := call(t, svc)

	// A second ping can only be counted if the call really is still open. Checking
	// the number of open calls wouldn't show it: a call being hung up still counts
	// as open until its goodbye has finished.
	for want := uint64(1); want <= 2; want++ {
		send(t, conn, sealedNow(uuid.New(), 1, taksimLat, taksimLng))
		waitFor(t, "the drop to be counted", func() bool { return svc.WebSocket.Counts().RedisErrors == want })
	}
}

func TestWebSocketDoorDropsCallsOnShutdown(t *testing.T) {
	svc := NewService(testSecret, testStore(t), quiet)
	conn, shutdown := call(t, svc)

	shutdown()
	closeStatus(t, conn)
	waitFor(t, "the call to end", func() bool { return svc.Connections.Load() == 0 })
	if got := svc.SilentHangups.Load(); got != 0 {
		t.Errorf("shutting down counted as %d silent hang-ups", got)
	}
}

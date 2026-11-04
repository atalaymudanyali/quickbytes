// Command locbench compares the location service's two doors, UDP and WebSocket, in
// three rounds: pretend couriers at a real heartbeat on a good network, the same on
// a bad one, and then as fast as each door will go.
//
// It sees what really happened from three places: the pings it sent, the service's
// /stats for what arrived and how old it was, and this container's network counters
// for every byte on the wire, including TCP's acknowledgements and resends.
//
// Run it with `make bench-locations`: it needs Docker's network, and NET_ADMIN to
// use tc.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/atalaymudanyali/quickbytes/internal/cache"
	"github.com/atalaymudanyali/quickbytes/internal/locations"
	"github.com/atalaymudanyali/quickbytes/internal/shiftkey"
)

const (
	door     = "eth0" // this container's only network card
	udpDoor  = "UDP"
	wsDoor   = "WebSocket"
	interval = 5 * time.Second // the heartbeat

	// How many calls to open at the same time.
	dialers = 200
	// How many senders to use flat out: as many as the UDP door has workers, so
	// each door handles the same number of pings at once.
	flatSenders = 32
)

type options struct {
	addr     string
	secret   string
	redisURL string
	couriers int
	duration time.Duration
	flat     time.Duration
	loss     string
	delay    string
}

// sender is one courier's way of sending a packet.
type sender func(packet []byte) error

func main() {
	opts := options{
		addr:     os.Getenv("LOCATIONS_ADDR"),
		secret:   os.Getenv("SHIFT_KEY_SECRET"),
		redisURL: os.Getenv("REDIS_URL"),
	}
	flag.IntVar(&opts.couriers, "couriers", 10_000, "how many pretend couriers")
	flag.DurationVar(&opts.duration, "duration", 30*time.Second, "how long each heartbeat round lasts")
	flag.DurationVar(&opts.flat, "flat", 10*time.Second, "how long each flat-out round lasts")
	flag.StringVar(&opts.loss, "loss", "5%", "packets lost in the bad-network round")
	flag.StringVar(&opts.delay, "delay", "40ms", "added delay in the bad-network round")
	flag.Parse()
	if opts.addr == "" || opts.secret == "" {
		log.Fatal("LOCATIONS_ADDR and SHIFT_KEY_SECRET are required: run this with make bench-locations")
	}

	bad := "loss " + opts.loss + ", delay " + opts.delay
	fmt.Printf("%d couriers, one ping every %v = %d pings a second, for %v per round.\n\n",
		opts.couriers, interval, opts.couriers*int(time.Second)/int(interval), opts.duration)

	var rows []heartbeatResult
	for _, network := range []string{"good", bad} {
		for _, d := range []string{udpDoor, wsDoor} {
			fmt.Printf("running: %s, network %s...\n", d, network)
			row, err := heartbeatRound(opts, d, network, network != "good")
			if err != nil {
				log.Fatalf("%s on %s network: %v", d, network, err)
			}
			rows = append(rows, row)
			clearMap(opts)
		}
	}

	var flats []flatResult
	for _, d := range []string{udpDoor, wsDoor} {
		fmt.Printf("running: %s, flat out...\n", d)
		row, err := flatOutRound(opts, d)
		if err != nil {
			log.Fatalf("%s flat out: %v", d, err)
		}
		flats = append(flats, row)
		clearMap(opts)
	}

	printResults(rows, flats)
}

// heartbeatResult is one heartbeat round.
type heartbeatResult struct {
	door, network           string
	sent, accepted, stale   uint64
	redisErrors, sendErrors uint64
	bytesPerPing            float64
	p50, p99, worst         string
	setupBytes, memoryBytes int64
}

// heartbeatRound has every courier send one ping per interval, spread out evenly,
// the way real ones would.
func heartbeatRound(opts options, doorName, network string, badNetwork bool) (heartbeatResult, error) {
	res := heartbeatResult{door: doorName, network: network}
	couriers := newCouriers(opts)

	memBefore, err := fetchStats(opts.addr)
	if err != nil {
		return res, err
	}
	devBefore, err := networkBytes()
	if err != nil {
		return res, err
	}
	send, closeAll, err := openDoor(opts, doorName, couriers)
	if err != nil {
		return res, err
	}
	defer closeAll()
	// Calls are opened on a good network, so this only compares what comes after.
	devOpen, err := networkBytes()
	if err != nil {
		return res, err
	}
	memOpen, err := fetchStats(opts.addr)
	if err != nil {
		return res, err
	}
	res.setupBytes = (devOpen - devBefore) / int64(len(couriers))
	res.memoryBytes = (int64(memOpen.MemoryBytes) - int64(memBefore.MemoryBytes)) / int64(len(couriers))

	if badNetwork {
		if err := badNetworkOn(opts); err != nil {
			return res, err
		}
		defer badNetworkOff()
	}

	before := doorCounts(memOpen, doorName)
	devStart, err := networkBytes()
	if err != nil {
		return res, err
	}

	var sent, sendErrors atomic.Uint64
	stop := time.Now().Add(opts.duration)
	var wg sync.WaitGroup
	for i := range couriers {
		wg.Go(func() {
			c := &couriers[i]
			time.Sleep(rand.N(interval))
			for time.Now().Before(stop) {
				if err := send[i](c.packet(uint64(time.Now().UnixMilli()))); err != nil {
					sendErrors.Add(1)
				} else {
					sent.Add(1)
				}
				time.Sleep(interval)
			}
		})
	}
	wg.Wait()

	after, err := waitUntilQuiet(opts.addr, doorName)
	if err != nil {
		return res, err
	}
	devEnd, err := networkBytes()
	if err != nil {
		return res, err
	}

	res.sent, res.sendErrors = sent.Load(), sendErrors.Load()
	res.accepted = after.counts.Accepted - before.Accepted
	res.stale = after.counts.Stale - before.Stale
	res.redisErrors = after.counts.RedisErrors - before.RedisErrors
	if res.sent > 0 {
		res.bytesPerPing = float64(devEnd-devStart) / float64(res.sent)
	}
	ages := subtract(after.counts.ArrivalAges, before.ArrivalAges)
	res.p50 = percentile(ages, after.stats.ArrivalAgeBuckets, 0.50)
	res.p99 = percentile(ages, after.stats.ArrivalAgeBuckets, 0.99)
	res.worst = percentile(ages, after.stats.ArrivalAgeBuckets, 1)
	return res, nil
}

// flatResult is one flat-out round.
type flatResult struct {
	door           string
	seconds        float64
	sent, accepted uint64
}

// flatOutRound sends as fast as each door will go. It's a different question from the
// heartbeat rounds: not "how does it behave at this load?" but "where's the ceiling,
// and what happens at it?"
func flatOutRound(opts options, doorName string) (flatResult, error) {
	res := flatResult{door: doorName}
	opts.couriers = flatSenders
	couriers := newCouriers(opts)
	send, closeAll, err := openDoor(opts, doorName, couriers)
	if err != nil {
		return res, err
	}
	defer closeAll()

	start, err := fetchStats(opts.addr)
	if err != nil {
		return res, err
	}
	before := doorCounts(start, doorName)

	var sent atomic.Uint64
	began := time.Now()
	stop := began.Add(opts.flat)
	base := uint64(began.UnixMilli())
	var wg sync.WaitGroup
	for i := range couriers {
		wg.Go(func() {
			c := &couriers[i]
			for n := uint64(1); time.Now().Before(stop); n++ {
				if err := send[i](c.packet(base + n)); err == nil {
					sent.Add(1)
				}
			}
		})
	}
	wg.Wait()
	res.seconds = time.Since(began).Seconds()

	after, err := waitUntilQuiet(opts.addr, doorName)
	if err != nil {
		return res, err
	}
	res.sent = sent.Load()
	res.accepted = after.counts.Accepted - before.Accepted
	return res, nil
}

type courier struct {
	key    shiftkey.Key
	lat    float64
	lng    float64
	secret []byte
}

func newCouriers(opts options) []courier {
	couriers := make([]courier, opts.couriers)
	now := time.Now()
	for i := range couriers {
		key := shiftkey.Issue(opts.secret, uuid.New(), now, time.Hour)
		couriers[i] = courier{
			key:    key,
			secret: key.Secret,
			lat:    40.9 + rand.Float64()*0.2,
			lng:    28.8 + rand.Float64()*0.4,
		}
	}
	return couriers
}

func (c *courier) packet(counter uint64) []byte {
	return locations.Seal(locations.Ping{
		DriverID:     c.key.DriverID,
		KeyExpiresAt: c.key.ExpiresAt,
		Counter:      counter,
		Lat:          c.lat,
		Lng:          c.lng,
	}, c.secret)
}

// openDoor returns one way of sending for each courier. On UDP it's a socket each,
// which costs the service nothing. On WebSocket it's a call each.
func openDoor(opts options, doorName string, couriers []courier) ([]sender, func(), error) {
	senders := make([]sender, len(couriers))
	closers := make([]func(), len(couriers))
	closeAll := func() {
		for _, c := range closers {
			if c != nil {
				c()
			}
		}
	}

	limit := make(chan struct{}, dialers)
	errs := make([]error, len(couriers))
	var wg sync.WaitGroup
	for i := range couriers {
		wg.Go(func() {
			limit <- struct{}{}
			defer func() { <-limit }()
			senders[i], closers[i], errs[i] = dial(opts.addr, doorName)
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		closeAll()
		return nil, nil, err
	}
	return senders, closeAll, nil
}

func dial(addr, doorName string) (sender, func(), error) {
	if doorName == udpDoor {
		conn, err := net.Dial("udp", addr)
		if err != nil {
			return nil, nil, err
		}
		send := func(packet []byte) error {
			_, err := conn.Write(packet)
			return err
		}
		return send, func() { conn.Close() }, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws://"+addr+"/ws", nil)
	if err != nil {
		return nil, nil, err
	}
	send := func(packet []byte) error {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return conn.Write(ctx, websocket.MessageBinary, packet)
	}
	return send, func() { conn.CloseNow() }, nil
}

func badNetworkOn(opts options) error {
	args := []string{"qdisc", "add", "dev", door, "root", "netem", "delay", opts.delay, "loss", opts.loss, "limit", "100000"}
	if out, err := exec.Command("tc", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("tc: %v: %s", err, out)
	}
	return nil
}

func badNetworkOff() {
	if out, err := exec.Command("tc", "qdisc", "del", "dev", door, "root").CombinedOutput(); err != nil {
		log.Printf("tc: %v: %s", err, out)
	}
}

// networkBytes is every byte this container has sent and received, from the operating
// system's own count. That includes what a program never sees: headers, TCP's
// acknowledgements and its resends.
func networkBytes() (int64, error) {
	raw, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return 0, err
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(name) != door {
			continue
		}
		fields := strings.Fields(rest)
		received, err1 := strconv.ParseInt(fields[0], 10, 64)
		sent, err2 := strconv.ParseInt(fields[8], 10, 64)
		if err := errors.Join(err1, err2); err != nil {
			return 0, err
		}
		return received + sent, nil
	}
	return 0, fmt.Errorf("no network card %q in /proc/net/dev", door)
}

func fetchStats(addr string) (locations.Stats, error) {
	var stats locations.Stats
	res, err := http.Get("http://" + addr + "/stats")
	if err != nil {
		return stats, err
	}
	defer res.Body.Close()
	return stats, json.NewDecoder(res.Body).Decode(&stats)
}

func doorCounts(s locations.Stats, doorName string) locations.DoorCounts {
	if doorName == udpDoor {
		return s.UDP
	}
	return s.WebSocket
}

type reading struct {
	stats  locations.Stats
	counts locations.DoorCounts
}

// waitUntilQuiet waits for what's still on its way: resent TCP packets, and whatever
// the service hasn't got to yet.
func waitUntilQuiet(addr, doorName string) (reading, error) {
	var last uint64
	for {
		time.Sleep(2 * time.Second)
		stats, err := fetchStats(addr)
		if err != nil {
			return reading{}, err
		}
		counts := doorCounts(stats, doorName)
		if counts.Received == last {
			return reading{stats, counts}, nil
		}
		last = counts.Received
	}
}

// clearMap empties the live map between rounds, so each starts the same.
func clearMap(opts options) {
	c, err := cache.Open(opts.redisURL)
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()
	if err := c.Del(context.Background(), "drivers:geo", "drivers:seen", "drivers:counter").Err(); err != nil {
		log.Fatal(err)
	}
}

func subtract(after, before []uint64) []uint64 {
	out := make([]uint64, len(after))
	for i := range after {
		out[i] = after[i] - before[i]
	}
	return out
}

// percentile finds the group a share of the pings fall in, and gives its upper limit:
// "99% arrived within 200 ms". counts has one more entry than limits, for anything
// older than the last limit.
func percentile(counts []uint64, limits []int64, share float64) string {
	var total uint64
	for _, n := range counts {
		total += n
	}
	if total == 0 {
		return "-"
	}
	want := uint64(float64(total)*share + 0.999999)
	var seen uint64
	for i, n := range counts {
		seen += n
		if seen >= want {
			if i == len(limits) {
				return fmt.Sprintf(">%d ms", limits[len(limits)-1])
			}
			return fmt.Sprintf("≤%d ms", limits[i])
		}
	}
	return "-"
}

func printResults(rows []heartbeatResult, flats []flatResult) {
	fmt.Println("\n## Heartbeat rounds")
	fmt.Println("\n| Door | Network | Sent | Arrived | Lost | Bytes per ping | Age p50 | Age p99 | Oldest |")
	fmt.Println("|---|---|---|---|---|---|---|---|---|")
	for _, r := range rows {
		lost := int64(r.sent) - int64(r.accepted)
		fmt.Printf("| %s | %s | %d | %d | %d (%.2f%%) | %.0f | %s | %s | %s |\n",
			r.door, r.network, r.sent, r.accepted, lost, 100*float64(lost)/float64(max(r.sent, 1)),
			r.bytesPerPing, r.p50, r.p99, r.worst)
	}
	for _, r := range rows {
		if r.stale+r.redisErrors+r.sendErrors > 0 {
			fmt.Printf("\nNote: %s on %s network had %d stale, %d Redis errors, %d send errors.\n",
				r.door, r.network, r.stale, r.redisErrors, r.sendErrors)
		}
	}

	fmt.Println("\n## What being connected costs, per courier")
	fmt.Println("\n| Door | Bytes to connect | Service memory |")
	fmt.Println("|---|---|---|")
	for _, r := range rows[:2] {
		fmt.Printf("| %s | %d | %.1f KB |\n", r.door, r.setupBytes, float64(r.memoryBytes)/1000)
	}

	fmt.Println("\n## Flat out")
	fmt.Println("\n| Door | Sent per second | Arrived per second | Lost |")
	fmt.Println("|---|---|---|---|")
	for _, f := range flats {
		lost := int64(f.sent) - int64(f.accepted)
		fmt.Printf("| %s | %.0f | %.0f | %.1f%% |\n", f.door, float64(f.sent)/f.seconds, float64(f.accepted)/f.seconds,
			100*float64(lost)/float64(max(f.sent, 1)))
	}
}

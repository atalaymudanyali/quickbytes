package locations

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/atalaymudanyali/quickbytes/internal/cache"
	"github.com/atalaymudanyali/quickbytes/internal/testutil"
)

// Three real places: Taksim is the search point, Beşiktaş about 2 km away and
// Kadıköy about 6.5 km.
const (
	taksimLat, taksimLng     = 41.0370, 28.9850
	besiktasLat, besiktasLng = 41.0430, 29.0090
	kadikoyLat, kadikoyLng   = 40.9903, 29.0291

	wideCircle = 20_000.0 // metres
)

// testStore is a store on its own keys, so tests don't see each other's drivers.
func testStore(t testing.TB) *Store {
	t.Helper()
	c := testutil.Redis(t)
	prefix := "test:" + uuid.NewString() + ":"
	s := &Store{redis: c, geoKey: prefix + "geo", seenKey: prefix + "seen", counterKey: prefix + "counter"}
	t.Cleanup(func() { c.Del(context.Background(), s.geoKey, s.seenKey, s.counterKey) })
	return s
}

func ping(driver uuid.UUID, counter uint64, lat, lng float64) Ping {
	return Ping{DriverID: driver, Counter: counter, Lat: lat, Lng: lng}
}

func mustSave(t *testing.T, s *Store, p Ping, now time.Time) {
	t.Helper()
	saved, err := s.Save(context.Background(), p, now)
	if err != nil {
		t.Fatal(err)
	}
	if !saved {
		t.Fatalf("ping with counter %d wasn't saved", p.Counter)
	}
}

func mustNear(t *testing.T, s *Store, now time.Time) []Nearby {
	t.Helper()
	found, err := s.Near(context.Background(), taksimLat, taksimLng, wideCircle, now)
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func TestRealKeyNames(t *testing.T) {
	s := NewStore(nil)
	if s.geoKey != "drivers:geo" || s.seenKey != "drivers:seen" {
		t.Fatalf("keys are %q and %q", s.geoKey, s.seenKey)
	}
}

func TestSavedPingIsSearchable(t *testing.T) {
	s, ctx, driver := testStore(t), context.Background(), uuid.New()
	mustSave(t, s, ping(driver, 1, taksimLat, taksimLng), testNow)

	// Redis's own search, not only the helper.
	found, err := s.redis.GeoSearch(ctx, s.geoKey, &redis.GeoSearchQuery{
		Latitude: taksimLat, Longitude: taksimLng, Radius: 100, RadiusUnit: "m",
	}).Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0] != driver.String() {
		t.Fatalf("GEOSEARCH found %v, want %s", found, driver)
	}
}

func TestNearIsClosestFirstAndKeepsToTheCircle(t *testing.T) {
	s := testStore(t)
	far, near := uuid.New(), uuid.New()
	mustSave(t, s, ping(far, 1, kadikoyLat, kadikoyLng), testNow) // saved first, on purpose
	mustSave(t, s, ping(near, 1, besiktasLat, besiktasLng), testNow)

	found := mustNear(t, s, testNow)
	if len(found) != 2 || found[0].DriverID != near || found[1].DriverID != far {
		t.Fatalf("got %+v, want %s then %s", found, near, far)
	}
	if d := found[0].DistanceMeters; d < 1500 || d > 3000 {
		t.Errorf("Beşiktaş is %.0f m from Taksim, want about 2 km", d)
	}

	inside, err := s.Near(context.Background(), taksimLat, taksimLng, 3000, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(inside) != 1 || inside[0].DriverID != near {
		t.Fatalf("in a 3 km circle got %+v, want only %s", inside, near)
	}
}

// A late or repeated packet changes nothing: not the position, and not the last
// seen time either, or a recording could keep a gone driver looking alive.
func TestOlderOrSameCounterChangesNothing(t *testing.T) {
	for _, tt := range []struct {
		name    string
		counter uint64
	}{{"older", 99}, {"same, so a recording sent again", 100}} {
		t.Run(tt.name, func(t *testing.T) {
			s, ctx, driver := testStore(t), context.Background(), uuid.New()
			mustSave(t, s, ping(driver, 100, taksimLat, taksimLng), testNow)

			saved, err := s.Save(ctx, ping(driver, tt.counter, kadikoyLat, kadikoyLng), testNow.Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if saved {
				t.Fatal("the ping was reported as saved")
			}

			pos, err := s.redis.GeoPos(ctx, s.geoKey, driver.String()).Result()
			if err != nil {
				t.Fatal(err)
			}
			if pos[0].Latitude < 41.03 {
				t.Errorf("the driver moved to %v,%v", pos[0].Latitude, pos[0].Longitude)
			}
			seen, err := s.redis.ZScore(ctx, s.seenKey, driver.String()).Result()
			if err != nil {
				t.Fatal(err)
			}
			if seen != float64(testNow.UnixMilli()) {
				t.Error("the last seen time changed")
			}
		})
	}
}

func TestNewerCounterMovesTheDriver(t *testing.T) {
	s, driver := testStore(t), uuid.New()
	mustSave(t, s, ping(driver, 100, kadikoyLat, kadikoyLng), testNow)
	mustSave(t, s, ping(driver, 101, besiktasLat, besiktasLng), testNow)

	found := mustNear(t, s, testNow)
	if len(found) != 1 || found[0].DistanceMeters > 3000 {
		t.Fatalf("got %+v, want one driver at Beşiktaş", found)
	}
}

func TestSilentDriverIsSkipped(t *testing.T) {
	s, driver := testStore(t), uuid.New()
	mustSave(t, s, ping(driver, 1, taksimLat, taksimLng), testNow)

	if found := mustNear(t, s, testNow.Add(SilentAfter)); len(found) != 1 {
		t.Fatalf("after exactly %v got %+v, want the driver still there", SilentAfter, found)
	}
	if found := mustNear(t, s, testNow.Add(SilentAfter+time.Millisecond)); len(found) != 0 {
		t.Fatalf("after %v got %+v, want nobody", SilentAfter, found)
	}

	// Coming back from the tunnel puts them on the map again.
	mustSave(t, s, ping(driver, 2, taksimLat, taksimLng), testNow.Add(time.Minute))
	if found := mustNear(t, s, testNow.Add(time.Minute)); len(found) != 1 {
		t.Fatalf("after coming back got %+v, want the driver", found)
	}
}

func TestSilentDriverDoesNotHideALiveOne(t *testing.T) {
	s := testStore(t)
	silent, live := uuid.New(), uuid.New()
	mustSave(t, s, ping(silent, 1, taksimLat, taksimLng), testNow)
	later := testNow.Add(time.Minute)
	mustSave(t, s, ping(live, 1, kadikoyLat, kadikoyLng), later)

	found := mustNear(t, s, later)
	if len(found) != 1 || found[0].DriverID != live {
		t.Fatalf("got %+v, want only %s", found, live)
	}
}

func TestRemoveTakesTheDriverOffAtOnce(t *testing.T) {
	s, ctx := testStore(t), context.Background()
	driver, other := uuid.New(), uuid.New()
	mustSave(t, s, ping(driver, 100, taksimLat, taksimLng), testNow)
	mustSave(t, s, ping(other, 100, besiktasLat, besiktasLng), testNow)

	if err := s.Remove(ctx, driver); err != nil {
		t.Fatal(err)
	}

	found := mustNear(t, s, testNow)
	if len(found) != 1 || found[0].DriverID != other {
		t.Fatalf("got %+v, want only %s", found, other)
	}
	for _, key := range []string{s.geoKey, s.seenKey} {
		if err := s.redis.ZScore(ctx, key, driver.String()).Err(); !errors.Is(err, redis.Nil) {
			t.Errorf("%s still holds the driver (err = %v)", key, err)
		}
	}
	if err := s.redis.HGet(ctx, s.counterKey, driver.String()).Err(); !errors.Is(err, redis.Nil) {
		t.Errorf("the counter is still there (err = %v)", err)
	}

	// A new shift starts clean, even from a smaller counter.
	mustSave(t, s, ping(driver, 5, taksimLat, taksimLng), testNow)

	if err := s.Remove(ctx, uuid.New()); err != nil {
		t.Errorf("removing a driver who isn't on the map: %v", err)
	}
}

// Many pings from one driver, in a random order, at the same moment: the newest must
// win. Each position says which counter it came with, so a position that doesn't
// match the final counter means two pings got mixed together.
func TestNewestWinsWhenPingsArriveTogether(t *testing.T) {
	s, ctx, driver := testStore(t), context.Background(), uuid.New()

	const pings = 200
	var wg sync.WaitGroup
	for _, i := range rand.Perm(pings) {
		wg.Go(func() {
			lat := 40 + float64(i)/100
			if _, err := s.Save(ctx, ping(driver, uint64(i), lat, taksimLng), testNow); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()

	counter, err := s.redis.HGet(ctx, s.counterKey, driver.String()).Result()
	if err != nil {
		t.Fatal(err)
	}
	if counter != "199" {
		t.Errorf("counter = %s, want 199", counter)
	}
	pos, err := s.redis.GeoPos(ctx, s.geoKey, driver.String()).Result()
	if err != nil {
		t.Fatal(err)
	}
	if lat := pos[0].Latitude; lat < 41.98 || lat > 42.00 {
		t.Errorf("latitude = %v, want the newest ping's 41.99", lat)
	}
}

// Redis forgets its scripts when it restarts. Saving must not need telling.
func TestSaveWorksAfterRedisForgetsTheScript(t *testing.T) {
	s, ctx := testStore(t), context.Background()
	if err := s.redis.ScriptFlush(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	mustSave(t, s, ping(uuid.New(), 1, taksimLat, taksimLng), testNow)
}

// With Redis down, every call answers with an error quickly and nothing panics. What
// to do about it is the caller's choice: the service drops the ping and carries on.
func TestRedisDownAnswersQuickly(t *testing.T) {
	c, err := cache.Open("redis://127.0.0.1:1/0") // port 1: nothing listens there
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s, ctx := NewStore(c), context.Background()

	start := time.Now()
	if _, err := s.Save(ctx, ping(uuid.New(), 1, taksimLat, taksimLng), testNow); err == nil {
		t.Error("Save: expected an error")
	}
	if _, err := s.Near(ctx, taksimLat, taksimLng, wideCircle, testNow); err == nil {
		t.Error("Near: expected an error")
	}
	if err := s.Remove(ctx, uuid.New()); err == nil {
		t.Error("Remove: expected an error")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %v", took)
	}
}

// BenchmarkSave is the cost of one ping in Redis, including the trip to it.
func BenchmarkSave(b *testing.B) {
	s := testStore(b)
	driver, ctx := uuid.New(), context.Background()
	counter := uint64(0)
	for b.Loop() {
		counter++
		if _, err := s.Save(ctx, ping(driver, counter, taksimLat, taksimLng), testNow); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkMemory measures the map at the imagined city-scale 10,000 drivers, to
// check the estimate. Run it with: -bench Memory -benchtime 1x
func BenchmarkMemory(b *testing.B) {
	s, ctx := testStore(b), context.Background()
	for b.Loop() {
		for range 10_000 {
			lat, lng := 40.9+rand.Float64()*0.2, 28.8+rand.Float64()*0.4
			if _, err := s.Save(ctx, ping(uuid.New(), 1, lat, lng), testNow); err != nil {
				b.Fatal(err)
			}
		}
		var total int64
		for _, key := range []string{s.geoKey, s.seenKey, s.counterKey} {
			used, err := s.redis.MemoryUsage(ctx, key, 0).Result()
			if err != nil {
				b.Fatal(err)
			}
			b.ReportMetric(float64(used)/1e6, key[len(key)-4:]+"_MB")
			total += used
		}
		b.ReportMetric(float64(total)/1e6, "total_MB")
		start := time.Now()
		found, err := s.Near(ctx, taksimLat, taksimLng, 3000, testNow)
		if err != nil {
			b.Fatal(err)
		}
		b.ReportMetric(float64(time.Since(start).Microseconds()), "near_3km_us")
		b.ReportMetric(float64(len(found)), "near_3km_drivers")
	}
}

package locations

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// SilentAfter is how long a driver can go quiet and still count as being on the
// map: six missed heartbeats. Losing six in a row by bad luck is very unlikely, so
// a driver who's this quiet has really gone: a dead phone, a tunnel, a closed app.
const SilentAfter = 30 * time.Second

// Store is the live map, kept in Redis as one latest position per driver. Nothing
// in it is precious: if Redis restarts, every driver's next heartbeat puts them
// back within 5 seconds.
type Store struct {
	redis *redis.Client
	// geoKey holds each driver's latest point, searchable by distance. seenKey is
	// a sorted set of when each driver was last heard from, because members of a
	// GEO set never expire by themselves. counterKey is a hash of the newest
	// counter from each driver.
	geoKey, seenKey, counterKey string
}

// NewStore returns the live map on this Redis.
func NewStore(c *redis.Client) *Store {
	return &Store{redis: c, geoKey: "drivers:geo", seenKey: "drivers:seen", counterKey: "drivers:counter"}
}

// saveScript is "is this newer? then save it" as one step. Redis runs a script
// from start to finish with nothing in between, so two pings from one driver can't
// both pass the check before either has written. Doing it from Go would leave that
// gap, like two people checking the same empty seat and both sitting down.
//
// The same counter is refused as well as an older one, so a recorded packet sent
// again changes nothing. Lua numbers lose precision above 2^53, which a clock in
// milliseconds won't reach for about 285,000 years.
var saveScript = redis.NewScript(`
local last = redis.call('HGET', KEYS[3], ARGV[1])
if last and tonumber(last) >= tonumber(ARGV[2]) then
	return 0
end
redis.call('GEOADD', KEYS[1], ARGV[3], ARGV[4], ARGV[1])
redis.call('ZADD', KEYS[2], ARGV[5], ARGV[1])
redis.call('HSET', KEYS[3], ARGV[1], ARGV[2])
return 1
`)

// Save puts a ping on the map, unless a ping with the same or a newer counter got
// there first. It reports whether the ping was newer. now is the server's clock, so
// a phone with a wrong clock can't make its driver look recently heard from.
func (s *Store) Save(ctx context.Context, p Ping, now time.Time) (bool, error) {
	keys := []string{s.geoKey, s.seenKey, s.counterKey}
	saved, err := saveScript.Run(ctx, s.redis, keys,
		p.DriverID.String(), p.Counter, p.Lng, p.Lat, now.UnixMilli()).Int()
	if err != nil {
		return false, fmt.Errorf("save position: %w", err)
	}
	return saved == 1, nil
}

// Nearby is a driver found by Near.
type Nearby struct {
	DriverID       uuid.UUID
	DistanceMeters float64
}

// Near returns the drivers within radius of a point, closest first, heard from in
// the last SilentAfter. It says who is close, not who is free: that's Postgres's
// answer, and a position can be up to a few seconds old.
//
// Silent drivers stay in the GEO set, so it asks for everyone in the circle and
// filters. Asking Redis for only the nearest few could return just silent drivers,
// and hide a live one behind them.
func (s *Store) Near(ctx context.Context, lat, lng, radiusMeters float64, now time.Time) ([]Nearby, error) {
	found, err := s.redis.GeoSearchLocation(ctx, s.geoKey, &redis.GeoSearchLocationQuery{
		GeoSearchQuery: redis.GeoSearchQuery{
			Latitude:   lat,
			Longitude:  lng,
			Radius:     radiusMeters,
			RadiusUnit: "m",
			Sort:       "ASC",
		},
		WithDist: true,
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("search positions: %w", err)
	}
	if len(found) == 0 {
		return nil, nil
	}

	members := make([]string, len(found))
	for i, f := range found {
		members[i] = f.Name
	}
	seen, err := s.redis.ZMScore(ctx, s.seenKey, members...).Result()
	if err != nil {
		return nil, fmt.Errorf("read last seen: %w", err)
	}

	// A driver removed between the two calls has a score of 0 here, so they're
	// skipped like a silent one.
	cutoff := float64(now.Add(-SilentAfter).UnixMilli())
	nearby := make([]Nearby, 0, len(found))
	for i, f := range found {
		if seen[i] < cutoff {
			continue
		}
		id, err := uuid.Parse(f.Name)
		if err != nil {
			return nil, fmt.Errorf("driver ID on the map: %w", err)
		}
		nearby = append(nearby, Nearby{DriverID: id, DistanceMeters: f.Dist})
	}
	return nearby, nil
}

// Remove takes a driver off the map when their shift ends. Their counter goes too,
// so a new shift starts clean even if the phone's clock has moved back since.
//
// The key can't be cancelled, so a ping still on its way can put them back. It
// only lasts until they count as silent, and dispatch asks Postgres who is free.
func (s *Store) Remove(ctx context.Context, driverID uuid.UUID) error {
	member := driverID.String()
	_, err := s.redis.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.ZRem(ctx, s.geoKey, member) // a GEO set is a sorted set underneath
		p.ZRem(ctx, s.seenKey, member)
		p.HDel(ctx, s.counterKey, member)
		return nil
	})
	if err != nil {
		return fmt.Errorf("remove position: %w", err)
	}
	return nil
}

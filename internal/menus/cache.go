package menus

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Cache keeps ready-made copies of public menus in Redis ("cache-aside"):
//
//   - Reading: look in Redis first. If the copy is there (a hit), use it. If not
//     (a miss), read the menu from PostgreSQL and put a copy in Redis.
//   - Changing: every committed menu change deletes the copy, so the next reader
//     rebuilds it from the database.
//
// It fails open: if Redis is missing or broken, menus are read from the database
// as if there were no cache.
type Cache struct {
	store  *Store
	redis  *redis.Client // nil: no cache
	ttl    time.Duration
	logger *slog.Logger
}

// Result says where a menu came from, for the X-Cache response header.
type Result string

const (
	Hit    Result = "HIT"    // served from Redis
	Miss   Result = "MISS"   // not in Redis: read from the database, then stored
	Bypass Result = "BYPASS" // no cache available: read from the database only
)

// DefaultTTL is how long a copy may live even if no change deletes it: the safety
// net for the rare case where a deletion is missed (e.g. Redis was briefly down
// during a change).
const DefaultTTL = 5 * time.Minute

// NewCache wraps the store and subscribes to its changes. client may be nil.
func NewCache(store *Store, client *redis.Client, ttl time.Duration, logger *slog.Logger) *Cache {
	c := &Cache{store: store, redis: client, ttl: ttl, logger: logger}
	store.onChange = c.invalidate
	return c
}

// Two keys per restaurant. The "v2" is the format of what's stored: if the Menu
// JSON ever changes shape, bump it, and old copies are simply never read again.
// (v1 had no option groups; its copies expire on their own.)
//
//	menu:v2:<id>          the cached menu (JSON)
//	menu:v2:<id>:version  the newest menu_version a change has announced
func menuKey(id uuid.UUID) string    { return "menu:v2:" + id.String() }
func versionKey(id uuid.UUID) string { return "menu:v2:" + id.String() + ":version" }

// PublicMenu returns the public menu, from Redis when possible.
func (c *Cache) PublicMenu(ctx context.Context, restaurantID uuid.UUID) (Menu, Result, error) {
	if c.redis == nil {
		m, err := c.store.Menu(ctx, restaurantID, View{})
		return m, Bypass, err
	}

	raw, err := c.redis.Get(ctx, menuKey(restaurantID)).Bytes()
	switch {
	case err == nil:
		var m Menu
		if jsonErr := json.Unmarshal(raw, &m); jsonErr == nil {
			return m, Hit, nil
		}
		// A copy we can't read is treated like no copy (it gets overwritten below).
	case !errors.Is(err, redis.Nil): // redis.Nil just means "no such key"
		c.logger.WarnContext(ctx, "menu cache unavailable, reading from the database", "err", err)
		m, err := c.store.Menu(ctx, restaurantID, View{})
		return m, Bypass, err
	}

	m, err := c.store.Menu(ctx, restaurantID, View{})
	if err != nil {
		return Menu{}, Miss, err
	}
	c.storeIfCurrent(ctx, m)
	return m, Miss, nil
}

// storeScript saves a menu copy only if no newer version has been announced.
// Redis runs a script as one step, with no other command in between, so the
// "check the version" and the "save" can't be separated by a change.
var storeScript = redis.NewScript(`
local latest = tonumber(redis.call('GET', KEYS[2]) or '0')
if tonumber(ARGV[2]) < latest then
  return 0
end
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[3])
return 1
`)

// invalidateScript records the new version and deletes the copy, in one step.
// The version is kept twice as long as copies live, so it outlasts any copy that
// a slow reader could still be about to save.
var invalidateScript = redis.NewScript(`
local latest = tonumber(redis.call('GET', KEYS[2]) or '0')
if tonumber(ARGV[1]) > latest then
  redis.call('SET', KEYS[2], ARGV[1], 'PX', ARGV[2])
end
redis.call('DEL', KEYS[1])
return 1
`)

// storeIfCurrent saves a copy of m, unless a change has meanwhile announced a
// newer version. That protects against this race:
//
//	reader: reads menu v7 from the database ... (slow)
//	owner:  saves v8, deletes the cached copy
//	reader: ... saves its v7 copy        ← would serve the old menu until the TTL
//
// With the check, the reader's v7 is refused because v8 was announced.
func (c *Cache) storeIfCurrent(ctx context.Context, m Menu) {
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	err = storeScript.Run(ctx, c.redis,
		[]string{menuKey(m.RestaurantID), versionKey(m.RestaurantID)},
		b, m.Version, c.ttl.Milliseconds()).Err()
	if err != nil {
		c.logger.WarnContext(ctx, "menu cache: could not store a copy", "err", err)
	}
}

// Forget drops everything cached for these restaurants, once they've been
// deleted. Without it, a copy would keep serving the menu of a restaurant that
// no longer exists until its TTL ran out. Like invalidate, a Redis failure is
// only a warning: the copies still expire on their own.
func (c *Cache) Forget(ctx context.Context, restaurantIDs ...uuid.UUID) {
	if c.redis == nil || len(restaurantIDs) == 0 {
		return
	}
	keys := make([]string, 0, 2*len(restaurantIDs))
	for _, id := range restaurantIDs {
		keys = append(keys, menuKey(id), versionKey(id))
	}
	if err := c.redis.Del(ctx, keys...).Err(); err != nil {
		c.logger.WarnContext(ctx, "menu cache: could not drop deleted restaurants; they expire on their own",
			"restaurants", len(restaurantIDs), "err", err)
	}
}

// invalidate is called by the store after each committed menu change.
func (c *Cache) invalidate(ctx context.Context, restaurantID uuid.UUID, version int64) {
	if c.redis == nil {
		return
	}
	err := invalidateScript.Run(ctx, c.redis,
		[]string{menuKey(restaurantID), versionKey(restaurantID)},
		version, (2 * c.ttl).Milliseconds()).Err()
	if err != nil {
		// The change itself is saved; only the cached copy may now be out of date,
		// until its TTL runs out. Worth a warning, not a failed request.
		c.logger.WarnContext(ctx, "menu cache: could not drop the old copy; it expires on its own",
			"restaurant_id", restaurantID, "err", err)
	}
}

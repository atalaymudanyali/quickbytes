// Package ratelimit counts attempts in Redis, to slow down people who try
// something too often (like guessing passwords).
//
// It uses fixed windows: a counter starts at the first attempt and expires by
// itself when the window ends, like a parking ticket that starts when you arrive.
// Until it expires, every attempt adds one; once it's over the limit, attempts are
// refused until the counter is gone.
package ratelimit

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// Rule is one limit: at most Limit attempts per Window. Name keeps the counters
// of different rules apart, e.g. "login:ip" and "login:email".
type Rule struct {
	Name   string
	Limit  int64
	Window time.Duration
}

// Result says whether an attempt may go ahead. When it may not, RetryAfter is how
// long until the counter expires and attempts are allowed again.
type Result struct {
	Allowed    bool
	RetryAfter time.Duration
}

var allowed = Result{Allowed: true}

// Limiter keeps counters in Redis. It fails open: if Redis is missing or broken,
// every attempt is allowed (and a warning is logged), so a Redis outage doesn't
// stop people from logging in. The password hashing still slows guessing down.
type Limiter struct {
	redis  *redis.Client // nil: no limits
	logger *slog.Logger
}

// New creates a limiter. client may be nil, which turns all limits off.
func New(client *redis.Client, logger *slog.Logger) *Limiter {
	return &Limiter{redis: client, logger: logger}
}

// key is where a rule's counter for one subject (an IP, an email hash...) lives:
// rl:<rule>:<subject>, e.g. rl:login:ip:203.0.113.7.
func key(rule Rule, subject string) string { return "rl:" + rule.Name + ":" + subject }

// hitScript adds one to the counter and returns {count, milliseconds left}. A new
// counter gets its expiry here, in the same step as the increment. Done as two
// separate commands, a crash in between could leave a counter that never expires,
// blocking that subject forever. Checking "no expiry" (instead of "count is 1")
// also repairs such a counter if one ever exists.
var hitScript = redis.NewScript(`
local count = redis.call('INCR', KEYS[1])
local ttl = redis.call('PTTL', KEYS[1])
if ttl < 0 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
  ttl = tonumber(ARGV[1])
end
return {count, ttl}
`)

// peekScript returns {count, milliseconds left} without counting. Reading both in
// one step means the counter can't expire between the two reads.
var peekScript = redis.NewScript(`
local count = tonumber(redis.call('GET', KEYS[1]) or '0')
return {count, redis.call('PTTL', KEYS[1])}
`)

// Hit counts one attempt and says whether it's within the limit. Use it when
// every attempt counts, e.g. "20 login attempts per minute per IP".
func (l *Limiter) Hit(ctx context.Context, rule Rule, subject string) Result {
	count, ttl, ok := l.run(ctx, hitScript, rule, subject, rule.Window.Milliseconds())
	if !ok || count <= rule.Limit {
		return allowed
	}
	return Result{RetryAfter: ttl}
}

// Check says whether the limit is already used up, without counting. Use it with
// Hit for rules that count only failures: Check before trying, Hit after a
// failure, e.g. "5 wrong passwords per 15 minutes".
func (l *Limiter) Check(ctx context.Context, rule Rule, subject string) Result {
	count, ttl, ok := l.run(ctx, peekScript, rule, subject)
	if !ok || count < rule.Limit {
		return allowed
	}
	return Result{RetryAfter: ttl}
}

// Reset deletes a counter, e.g. after a successful login.
func (l *Limiter) Reset(ctx context.Context, rule Rule, subject string) {
	if l.redis == nil {
		return
	}
	if err := l.redis.Del(ctx, key(rule, subject)).Err(); err != nil {
		l.logger.WarnContext(ctx, "rate limiter: could not reset a counter", "rule", rule.Name, "err", err)
	}
}

// run executes a script on the rule's counter. ok is false when there's no Redis
// or it failed, which callers treat as "allowed" (failing open).
func (l *Limiter) run(ctx context.Context, s *redis.Script, rule Rule, subject string, args ...any) (count int64, ttl time.Duration, ok bool) {
	if l.redis == nil {
		return 0, 0, false
	}
	vals, err := s.Run(ctx, l.redis, []string{key(rule, subject)}, args...).Int64Slice()
	if err != nil || len(vals) != 2 {
		l.logger.WarnContext(ctx, "rate limiter unavailable, allowing the request", "rule", rule.Name, "err", err)
		return 0, 0, false
	}
	return vals[0], time.Duration(vals[1]) * time.Millisecond, true
}

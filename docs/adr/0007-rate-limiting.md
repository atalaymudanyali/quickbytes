# 0007. Login rate limits: slow down, never lock

**Status:** Accepted · recorded 2026-10-22 (end of phase 1)

## Context

A login form answers every guess, so an attacker can try many passwords
(*brute force*). Password hashing (argon2id: 18.7 ms and 19 MB per check) already
slows each guess. But nothing stopped one machine from guessing nonstop, or many
machines from guessing one account. Locking accounts after N failures sounds safe,
but it hands a stranger a button that locks out the real owner.

## Decision

Fixed-window counters in Redis (`internal/ratelimit`). One Lua script increments and
sets the expiry in one step. Every check runs **before** the database lookup and
argon2:

| Counter | Limit | Stops |
|---|---|---|
| Every login attempt per IP | 20 / minute | one machine trying many emails |
| Failures per email + IP | 5 / 15 min (reset on success) | one attacker guessing one account |
| Failures per email, any IP | 50 / hour | a botnet guessing one account |
| Sign-ups per IP | 10 / hour | email probing and CPU abuse |

Over a limit: `429 Too Many Requests` with `Retry-After`. Limits **slow down and
reset on their own, they never lock**. A blocked attempt is refused even with the
right password. Emails are SHA-256 hashed in the Redis keys. `X-Forwarded-For` is
ignored (there's no proxy yet). **Fail open** if Redis is down.

## Alternatives

| Option | Why not |
|---|---|
| Lock the account after N failures | Anyone could lock out anyone. |
| Sliding window / token bucket | Smoother, but more complex, and `Retry-After` is no longer simply the key's TTL. |
| Fail closed while Redis is down | A Redis outage would lock out every customer. argon2 still slows guessing in the meantime. |

## Consequences

- Measured: a wrong password costs **~20 ms** (argon2), and a blocked attempt is
  refused in **~1 ms** (one Redis round trip). A blocked attacker costs the server
  about 20× less than a guess.
- A 429 looks and times the same for real and unknown emails, so it doesn't reveal
  who has an account.
- Fixed windows let up to 2× the limit through around a window's edge, and parallel
  guesses can slip a few past the failure counters.
- Behind a reverse proxy, the client IP must come from the proxy's header, trusted
  only from the proxy.

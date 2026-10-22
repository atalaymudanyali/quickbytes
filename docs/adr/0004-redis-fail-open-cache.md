# 0004. Redis as a fail-open cache, with ETag/304

**Status:** Accepted · recorded 2026-10-22 (end of phase 1)

## Context

A restaurant's public menu is read far more often than it changes. Each read costs
three PostgreSQL queries. Think of a bakery with a display case (a *vitrin*): plates
that are already made are handed over at once, and the kitchen cooks only what's
missing.

## Decision

- **Cache-aside** for the public menu only. Read Redis first (`X-Cache: HIT`). On a
  miss, read PostgreSQL and store a copy (`MISS`).
- **Delete on change.** After a menu change is committed, the cached copy is
  deleted. An atomic Lua script compares `menu_version`, so a slow reader can't put
  an old copy back. A 5-minute TTL is the safety net.
- **Fail open.** If Redis is missing, slow (200 ms timeout, no retries) or down,
  the database answers (`BYPASS`). `/readyz` doesn't check Redis.
- **ETag from `menu_version`.** A client that sends `If-None-Match` for an unchanged
  menu gets an empty `304 Not Modified`.
- The owner's own view always reads the database, so owners see their edits at once.

## Alternatives

| Option | Why not |
|---|---|
| Fail closed (error while Redis is down) | A cache is there to speed things up. It must never become a new way for the app to break. |
| Write-through (update the cache on every write) | More moving parts, and it caches menus nobody reads. |
| Update the cached copy in place | Deleting can never write a wrong menu. The next reader rebuilds it from the source of truth. |
| A hash of the whole menu as the ETag | The version number already changes on every edit, so the tag costs nothing to compute. |

## Consequences

Measured with k6 (`make load-menu`: 50 users × 30 s, median of 3 runs, one laptop):

| Setup | Requests/s | p95 |
|---|---|---|
| No cache | 8,475 | 7.12 ms |
| Redis cache, 100% hits | 27,107 | 4.65 ms |
| Cache + 304 | **34,767** | **3.17 ms** |

- About 3.2× the throughput with the cache, and 4.1× with 304s. The phone also
  downloads nothing on a 304.
- A missed delete can serve a stale menu for at most 5 minutes.
- A burst of misses on one menu (a *stampede*) isn't merged yet. That waits until a
  load test shows it matters.

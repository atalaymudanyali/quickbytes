# QuickBytes

[![CI](https://github.com/atalaymudanyali/quickbytes/actions/workflows/ci.yml/badge.svg)](https://github.com/atalaymudanyali/quickbytes/actions/workflows/ci.yml)

A food delivery platform built end to end, set in Istanbul: customers order from
restaurants, payments are checked by a mock payment provider, and a dispatch system
groups orders into trips for drivers and plans the fastest route through real
Istanbul streets.

One iPhone app holds all three roles (customer, restaurant, driver), switched from a
side menu.

> **Status:** phase 1 of 7 (foundation) is done: accounts, restaurants, menus,
> caching, rate limits, API docs and Istanbul demo data. Next: orders and payments.
> See the [roadmap](#roadmap).

## Quick start

Needs Docker and Go 1.27+.

```bash
make up        # start PostgreSQL, Redis and the API in Docker
make seed      # fill it with Istanbul demo data
```

Then open **http://localhost:8080/docs**, log in with
`demo@demo.quickbytes.test` / `quickbytes-demo` through `POST /auth/login`, paste the
`access_token` into **Authorize**, and try any endpoint. `make test` runs every test,
and `make down` stops everything.

## What's built (phase 1)

- **Accounts and login:** sign-up, argon2id password hashing, 15-minute access tokens
  and single-use refresh tokens (reusing one revokes the whole login). One account
  can hold customer, restaurant and driver profiles.
- **Restaurants and near-me search:** PostGIS locations, each restaurant with its own
  delivery radius, closest first, cursor pagination. Owners can edit only their own.
- **Menus:** sections and dishes in the owner's order, prices in kuruş, sold-out and
  archived items, partial updates.
- **Menu cache:** Redis in front of the public menu, with version-checked
  invalidation, `ETag` / `304 Not Modified`, and a k6 load test.
- **Rate limits** on login and sign-up: `429` with `Retry-After`, without ever
  locking an account.
- **API contract:** a hand-written OpenAPI spec, tests that keep code and spec in
  sync, and Swagger UI at `/docs`.
- **Istanbul demo data:** 13 fictional restaurants on real streets, 96 dishes and
  7 demo accounts, from `make seed`.
- **CI:** formatting, `go vet` and the full test suite (against real PostgreSQL and
  Redis) on every pull request.

## Measured

Every number here was measured on one laptop (Apple Silicon, everything in Docker)
and comes with the command that reproduces it.

### Near-me search: spatial index

`make bench-near-me` generates 100,000 restaurants across Istanbul and times the
first page of the near-me query from Kadıköy pier (median of 3 runs,
PostgreSQL 17 + PostGIS 3.5):

| Setup | How PostgreSQL searched | Time |
|---|---|---|
| No spatial index | Checked all 100,000 rows | 51 ms |
| GiST index + fixed 10 km filter (what the API does) | Index found 3,682 candidates in the 10 km box; 565 deliver here | **3.6 ms** |
| GiST index, per-restaurant radius only | Index unusable, checked all 100,000 rows | 50 ms |

The last row is why the query filters twice: the index needs one fixed search
distance, so a 10 km outer check (the maximum delivery radius) narrows the search
before each restaurant's own radius is applied.

### Public menu: Redis cache and ETag

`make load-menu` builds an 80-dish menu (8 sections × 10 dishes) through the API,
then has [k6](https://k6.io) send 50 users reading it nonstop for 30 seconds, three
times (median of 3 runs, Apple M5 Pro):

| Setup | Requests/s | p50 | p95 |
|---|---|---|---|
| No cache: every read goes to PostgreSQL (3 queries) | 8,475 | 5.78 ms | 7.12 ms |
| Redis cache: 100% hits | 27,107 | 1.35 ms | 4.65 ms |
| Redis cache + `If-None-Match`: empty `304 Not Modified` | **34,767** | **1.16 ms** | **3.17 ms** |

The cache serves about 3.2× as many menus per second, and 304s about 4.1×, while
also sparing the phone the download. k6 and the server share the same CPU here, so
these numbers compare the three setups; they don't predict a real server's capacity.

### Login: what a guess costs

| What | Time |
|---|---|
| One argon2id password hash (`go test -bench Hash ./internal/auth`) | 18.7 ms, 19 MB of memory |
| A wrong password, answered `401` | ~20 ms |
| A blocked attempt, answered `429` | **~1 ms** |

The rate-limit checks run before the database lookup and the password hash, so a
blocked attacker costs the server about 20 times less than a guess.

## Try it

### Demo data

`make seed` adds 13 restaurants across Kadıköy, Üsküdar, Beşiktaş, Şişli, Fatih and
Beyoğlu, with real street coordinates and Turkish menus priced in kuruş (96 dishes),
plus demo accounts. Running it again only adds what's missing, so IDs stay the same.
`make seed-reset` deletes the demo accounts and everything they own, then seeds
fresh. Other accounts are never touched. Both refuse any database that isn't local.

Every demo account's password is **`quickbytes-demo`**. They live on the reserved
`.test` domain, so they can never be real inboxes.

| Account | Profiles |
|---|---|
| `musteri@demo.quickbytes.test` | customer |
| `kurye@demo.quickbytes.test` | customer, driver |
| `demo@demo.quickbytes.test` | customer, restaurant, driver (owns Kuzguncuk Fırını) |
| `hasan@demo.quickbytes.test` | customer, restaurant (two branches of Hasan Usta Kebap) |
| `elif@`, `murat@`, `zeynep@demo.quickbytes.test` | customer, restaurant |

Things to try, in Swagger UI or with curl:

```bash
# Near me from Moda: three Kadıköy restaurants. Bahariye Lahmacun is paused
# ("accepting_orders": false). Fenerbahçe Dondurma is 1.7 km away but only
# delivers 1 km, so it's left out.
curl 'localhost:8080/restaurants?lat=40.9810&lng=29.0270'

# From Fenerbahçe it's the closest one.
curl 'localhost:8080/restaurants?lat=40.9712&lng=29.0405'

# Sultanahmet: Divanyolu Köfte, Karaköy Pide Salonu, Fatih Çorba Salonu.
curl 'localhost:8080/restaurants?lat=41.0055&lng=28.9768'

# A menu, twice: X-Cache says MISS, then HIT (served from Redis).
curl -si localhost:8080/restaurants/<id>/menu | grep X-Cache

# Log in as a demo owner, then use the token in Authorize.
curl -s localhost:8080/auth/login -H 'Content-Type: application/json' \
  -d '{"email": "zeynep@demo.quickbytes.test", "password": "quickbytes-demo"}'

# Wrong passwords for one email: the 6th try in 15 minutes gets 429 Too Many
# Requests. Use a made-up address, not a demo account you want to keep using.
for i in 1 2 3 4 5 6; do curl -s -o /dev/null -w '%{http_code} ' localhost:8080/auth/login \
  -H 'Content-Type: application/json' -d '{"email": "nobody@demo.quickbytes.test", "password": "wrong-password"}'; done
```

Şişli Mantı Evi's menu shows a sold-out dish (`"available": false`).

### API documentation

With the API running, open **http://localhost:8080/docs** for interactive docs
(Swagger UI): every endpoint, its inputs and responses, and "Try it out" to call it
from the browser. Log in with `POST /auth/login`, then paste the `access_token` into
**Authorize** to try the protected endpoints. The contract itself is
[`api/openapi.yaml`](api/openapi.yaml) (also served at `/openapi.yaml`).

The tests keep code and spec in sync: every served route must be in the spec and
the other way around, and every request and response in the HTTP tests is checked
against it.

<details>
<summary><b>All endpoints</b></summary>

| Endpoint | Does |
|---|---|
| `GET /healthz` | Is the process alive? Never checks dependencies. |
| `GET /readyz` | Can this copy serve traffic? Returns 503 if the database is unreachable. |
| `POST /auth/signup` | Create an account (with a customer profile) and log in. |
| `POST /auth/login` | Log in: returns a 15-minute access token and a refresh token. |
| `POST /auth/refresh` | Swap a refresh token for a new pair. Each refresh token works once. |
| `POST /auth/logout` | Revoke the refresh token and all tokens issued from the same login. |
| `GET /me` | The signed-in user and their profiles. Needs `Authorization: Bearer <token>`. |
| `POST /me/profiles` | Add a `restaurant` or `driver` profile. Refresh afterwards to use it. |
| `GET /restaurants?lat=…&lng=…` | Restaurants whose delivery zone covers that point, closest first. Public, paginated with `next_cursor`. |
| `GET /restaurants/{id}` | One restaurant. Public. |
| `POST /restaurants` | Create a restaurant (needs a restaurant profile). |
| `PATCH /restaurants/{id}` | Change only the fields sent. Owner only; anyone else gets 404. |
| `GET /me/restaurants` | The signed-in owner's restaurants. |
| `GET /restaurants/{id}/menu` | A restaurant's menu: sections and dishes in the owner's order, sold-out dishes marked `"available": false`, removed ones left out. Public. |
| `GET /me/restaurants/{id}/menu` | The owner's full menu, archived categories and items included (marked). |
| `POST /restaurants/{id}/menu/categories` | Add a menu section, e.g. "Kebaplar". Owner only. |
| `PATCH /restaurants/{id}/menu/categories/{category_id}` | Rename, archive (`"archived": true`) or restore it. |
| `POST /restaurants/{id}/menu/items` | Add a dish: category, name, description, price in kuruş (`32000` = 320 TL). |
| `PATCH /restaurants/{id}/menu/items/{item_id}` | Change any field, mark sold out (`"available": false`), move category, archive or restore. |
| `PUT /restaurants/{id}/menu/categories/order` | New section order: `{"category_ids": [...]}`, every visible section exactly once. |
| `PUT /restaurants/{id}/menu/categories/{category_id}/order` | New dish order within a section: `{"item_ids": [...]}`. |

Errors always look like
`{"error": {"code": "...", "message": "...", "request_id": "..."}}`, and every
response carries an `X-Request-ID` header that matches the server's log line.

</details>

## Tech stack

**In use now:** Go (`net/http`, pgx, goose) · PostgreSQL 17 + PostGIS 3.5 · Redis ·
OpenAPI 3.0 + Swagger UI · k6 · Docker Compose · GitHub Actions

**Coming in later phases:** Python + OR-Tools (route optimizer, over gRPC) ·
Redpanda (Kafka API) · OSRM with Istanbul OpenStreetMap data · SwiftUI + MapKit ·
Prometheus + Grafana · OpenTelemetry

Everything runs locally and costs nothing: no paid APIs or keys.

## Design decisions

The decisions that shaped the code are written up as short
[architecture decision records](docs/adr/): what was decided, what else was
considered, and what it costs.

- [0001](docs/adr/0001-go-with-a-python-optimizer.md) Go for the services, Python only for the route optimizer
- [0002](docs/adr/0002-modular-monolith.md) A modular monolith, not microservices
- [0003](docs/adr/0003-contract-first-openapi.md) Contract-first OpenAPI, checked by tests
- [0004](docs/adr/0004-redis-fail-open-cache.md) Redis as a fail-open cache, with ETag/304
- [0005](docs/adr/0005-money-in-kurus.md) Money as whole kuruş
- [0006](docs/adr/0006-uuidv7-ids.md) UUIDv7 for every ID
- [0007](docs/adr/0007-rate-limiting.md) Login rate limits: slow down, never lock

## Roadmap

Each phase ends in a working, demoable state with passing CI.

| Phase | Builds | Done when |
|---|---|---|
| ✅ **1. Foundation** | Accounts, restaurants, menus with caching, rate limits, API docs, demo data, CI | You can sign up, create a restaurant, add a menu, and read it back fast from cache. |
| **2. Orders and payments** | Cart, checkout, order state machine, mock payment service, webhooks, idempotency, outbox | An order can be placed and paid. Test cards produce declines and timeouts, and retries never double-charge. |
| **3. Drivers and basic dispatch** | Location ingest, live positions in Redis, nearest-driver assignment, accept/reject | A ready order is offered to the nearest free driver and reassigned if they reject. |
| **4. Routing and batching** | OSRM with Istanbul data, Python optimizer, gRPC, multi-order trips | Several orders are combined into one trip with a sensible stop order on real streets. |
| **5. Simulator and live dashboard** | Driver simulator, order generator, web map | You can watch 50 drivers deliver orders around Istanbul. |
| **6. iPhone app** | SwiftUI app with the profile switcher, MapKit tracking | You can order, cook and deliver from your own phone. |
| **7. Scale and polish** | Partitioning, load tests, Grafana dashboards, tracing | Measured numbers for every system design topic. |

## How it's built

Every feature goes through the same loop:

1. **An issue** with the scope, the open decisions and acceptance criteria.
2. **A branch, one commit at a time.** Each commit is small enough to explain, and
   is reviewed and tested by hand before the next one starts.
3. **A pull request** with what it adds, the measured numbers, the decisions taken
   and how to try it, then review, green CI and merge.

I build it with an AI pair programmer ([Claude Code](https://claude.com/claude-code)).
The AI writes code with me, and I decide the design, test every step, and must be able
to explain and defend every line. Commits it helped write carry a
`Co-Authored-By: Claude` trailer, so the history shows that openly.

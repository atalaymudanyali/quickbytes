# QuickBytes

A food delivery platform built end to end, set in Istanbul: customers order from
restaurants, payments are checked by a mock payment provider, and a dispatch system
groups orders into trips for drivers and plans the fastest route through real
Istanbul streets.

One iPhone app holds all three roles (customer, restaurant, driver), switched from a
side menu.

> **Status:** early development (phase 1 of 7: foundation).

## What it shows

- An order moving through a real backend: cart → payment → restaurant → driver → door.
- Mock payments that behave like a real provider: approvals, declines, timeouts,
  and notifications that can arrive twice without charging anyone twice.
- Dispatch that batches several orders into one driver trip, with routes planned on
  real OpenStreetMap road data.
- System design you can measure: indexes, partitioning, caching and load tests, with
  the numbers published here.

## Tech

Go · Python (route optimizer) · PostgreSQL + PostGIS · Redis · Redpanda (Kafka API) ·
OSRM · SwiftUI · Docker Compose · Prometheus + Grafana · GitHub Actions

Everything runs locally and costs nothing: no paid APIs or keys.

## Run it

Needs Docker and Go 1.27+.

```bash
make up        # start PostgreSQL + the API in Docker
curl localhost:8080/readyz
make test      # run all tests (starts the database if needed)
make down      # stop everything
```

### API documentation

With the API running, open **http://localhost:8080/docs** for interactive docs
(Swagger UI): every endpoint, its inputs and responses, and "Try it out" to call it
from the browser. Log in with `POST /auth/login`, then paste the `access_token` into
**Authorize** to try the protected endpoints. The contract itself is
[`api/openapi.yaml`](api/openapi.yaml) (also served at `/openapi.yaml`).

The tests keep code and spec in sync: every served route must be in the spec and
the other way around, and every request and response in the HTTP tests is checked
against it.

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

## Measured

### Near-me search: spatial index

`make bench-near-me` generates 100,000 restaurants across Istanbul and times the
first page of the near-me query from Kadıköy pier (median of 3 runs, Apple Silicon,
PostgreSQL 17 + PostGIS 3.5 in Docker):

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
times (median of 3 runs, Apple M5 Pro, everything in Docker on one laptop):

| Setup | Requests/s | p50 | p95 |
|---|---|---|---|
| No cache: every read goes to PostgreSQL (3 queries) | 8,475 | 5.78 ms | 7.12 ms |
| Redis cache: 100% hits | 27,107 | 1.35 ms | 4.65 ms |
| Redis cache + `If-None-Match`: empty `304 Not Modified` | **34,767** | **1.16 ms** | **3.17 ms** |

The cache serves about 3.2× as many menus per second, and 304s about 4.1×, while
also sparing the phone the download. k6 and the server share the same CPU here, so
these numbers compare the three setups; they don't predict a real server's capacity.

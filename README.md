# QuickBytes

[![CI](https://github.com/atalaymudanyali/quickbytes/actions/workflows/ci.yml/badge.svg)](https://github.com/atalaymudanyali/quickbytes/actions/workflows/ci.yml)

A food delivery platform built end to end, set in Istanbul: customers order from
restaurants, payments are checked by a mock payment provider, and a dispatch system
groups orders into trips for drivers and plans the fastest route through real
Istanbul streets.

One iPhone app holds all three roles (customer, restaurant, driver), switched from a
side menu.

> **Status:** phase 1 of 7 (foundation) is done: accounts, restaurants, menus,
> caching, rate limits, API docs and Istanbul demo data. Phase 2 (orders and payments)
> has started: dishes have options (sizes, extras), customers have saved delivery
> addresses and a server-side cart priced live from the menu, and checkout turns the
> cart into an order that moves through a state machine. Checkout is safe to retry
> (idempotency keys). A mock payment provider runs as its own service, with test
> cards for every outcome. Next: payments in the API (authorize at checkout, capture
> when the restaurant accepts). See the [roadmap](#roadmap).

## Quick start

Needs Docker and Go 1.27+.

```bash
make up        # start PostgreSQL, Redis, the API and the mock payment provider in Docker
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

Phase 2 so far:

- **Dish options:** option groups with priced choices ("Boy: pick exactly 1",
  "Ekstralar: up to 3"), each owner-set `min_picks` / `max_picks`, shown in the
  cached menu. One shared validator checks a customer's picks; the cart and
  checkout will reuse it.
- **Delivery addresses:** customers save up to 10 (label, street address, details
  such as floor and door, and a map pin). The limit holds even under simultaneous
  requests: each save locks the customer's profile row first.
- **Server-side cart:** one per customer in PostgreSQL, for one restaurant at a
  time (a dish from another gets `409 cart_restaurant_mismatch`). The cart stores no
  prices: every read works out the subtotal, the restaurant's delivery fee and the
  total from the live (cached) menu, so a price change shows up at once. Dishes
  that can no longer be ordered as they are stay in the cart, flagged, without a
  price. The database itself keeps every line in the cart's restaurant: both of a
  line's foreign keys share its `restaurant_id`.
- **Checkout and orders:** `POST /orders` turns the cart into an order. It refuses
  an empty cart, flagged lines, a paused restaurant and a basket below the minimum,
  then in one transaction locks the cart, checks it still holds what was priced,
  **copies** every name, option and price and the delivery address into the order,
  and empties the cart. Later menu or address changes never alter a past order, and
  a double-tapped "Place order" gives one order, not two.
- **Retry-safe checkout (idempotency keys):** `POST /orders` needs an
  `Idempotency-Key` header, one random value per tap on "Place order". A retry with
  the same key (the phone lost signal before the answer came) gets the first
  attempt's stored answer again, byte for byte, marked `Idempotent-Replayed: true`,
  instead of ordering again. While the first attempt still runs, retries get
  `409 idempotency_in_progress`; the same key with a different body is
  `422 idempotency_key_reused`. Keys live in PostgreSQL per user, with a SHA-256
  fingerprint of the request, for 24 hours. The answer is stored **in the order's
  own transaction**, so an order never exists without its answer. Ten simultaneous
  retries make exactly one order and ten identical answers.
- **Order state machine:** `placed → paid → accepted → ready → picked_up →
  delivered`, or `cancelled` with a reason (`payment_failed`, `rejected`,
  `by_customer`, `expired`). One function holds every allowed move and who may make
  it; it locks the order row (`SELECT … FOR UPDATE`), so two moves at once can't
  both win, and writes a status history row for each. Every other move gets
  `409 invalid_transition`. Customers cancel before the restaurant accepts;
  restaurants see paid orders in a kitchen queue and accept, reject or mark them
  ready; other people's orders are `404`.
- **Expiry:** a background loop cancels orders still unpaid after 15 minutes. It
  takes them with `FOR UPDATE SKIP LOCKED`, so several API copies share the work and
  an order being paid at that moment is skipped, not waited for. Until the payment
  issues land nothing marks an order paid, so every order not cancelled first
  ends up expired.
- **Mock payment provider:** a fake card processor, run as **its own service with
  its own database** (`payprovider`, in the same PostgreSQL container, sharing no
  tables), so the API will reach it only over HTTP, like Stripe or iyzico, where
  calls can be slow, fail or arrive twice. Merchants log in with an API key (only
  its SHA-256 is stored). It **authorizes** (holds an amount on a card), then
  **captures** (takes it), **voids** (drops the hold) or **refunds**; any other
  move is `409 invalid_state`, and two captures at once can't both win (the status
  check and the change are one `UPDATE`). Every write needs an `Idempotency-Key`,
  reusing the API's idempotency package, and the answer is stored in the payment's
  own transaction, so a retry never holds, takes or gives back money twice. Every
  status change is appended to an append-only history (a trigger refuses edits),
  which webhooks will be sent from. Test cards cover approval, decline,
  insufficient funds, a slow card (`processing`, finished later by a background
  loop) and a card that hangs for 30 seconds. Its own OpenAPI file,
  [`api/payprovider.yaml`](api/payprovider.yaml), is checked against every answer
  in its tests. The API doesn't call it yet.

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
Beyoğlu, with real street coordinates and Turkish menus priced in kuruş (96 dishes,
9 of them with options), each restaurant's delivery fee and minimum basket, and
demo accounts with delivery addresses near them. Running it again only adds what's
missing, so IDs stay the same.
`make seed-reset` deletes the demo accounts and everything they own, then seeds
fresh. Other accounts are never touched. Both refuse any database that isn't local.

Every demo account's password is **`quickbytes-demo`**. They live on the reserved
`.test` domain, so they can never be real inboxes.

| Account | Profiles |
|---|---|
| `musteri@demo.quickbytes.test` | customer (addresses: Ev in Moda, İş in Mecidiyeköy) |
| `kurye@demo.quickbytes.test` | customer, driver (Ev in Aksaray) |
| `demo@demo.quickbytes.test` | customer, restaurant, driver (owns Kuzguncuk Fırını; Ev in Beşiktaş, Annem in Üsküdar) |
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

Şişli Mantı Evi's menu shows a sold-out dish (`"available": false`). Dishes with
options include Nişantaşı Bistro's Burger (doneness, extras, a menu deal) and
Fenerbahçe Dondurma's Maraş Dondurması (scoops, cone or cup, toppings); Hasan Usta
Kadıköy's Kıymalı Pide has a switched-off extra.

To fill a cart, log in as `musteri@`, find a dish's `id` and its choice IDs in a
menu, and `POST /cart/items` with `{"menu_item_id": "...", "quantity": 2,
"choice_ids": ["..."]}`. Every cart call answers with the whole cart, priced now.
Then try adding a dish from a second restaurant (409), or log in as `hasan@` in
another tab, change the price of a dish in the cart, and `GET /cart` again.

To check out, find the customer's address IDs with `GET /me/addresses` and
`POST /orders` with `{"address_id": "..."}` and an `Idempotency-Key` header (any
new UUID, e.g. from `uuidgen`). The answer is the order with its history
(`placed`, by the customer), and the cart is now empty. Then:

- Send the exact same request again, same key: you get the same order back with
  `Idempotent-Replayed: true`, and `GET /orders` still shows one order. The same
  key with another `address_id` gets `422 idempotency_key_reused`.

- Change that dish's price as `hasan@`, and `GET /orders/{order_id}`: the order
  keeps the price it was placed at.
- `POST /orders/{order_id}/cancel` cancels it (`by_customer`); a second cancel gets
  `409 invalid_transition`.
- As `hasan@`, `GET /me/restaurants/{id}/orders` is the kitchen queue. It stays
  empty for now: only paid orders show up, and paying arrives with the payment
  issues.
- Watch an unpaid order expire: restart the API with a short window,
  `ORDER_PAY_WINDOW=1m ORDER_EXPIRY_EVERY=5s make up`, place an order and keep
  reading it. After about a minute it's `cancelled` (`expired`, by `system`).
  `make up` on its own goes back to 15 minutes.

### Mock payment provider

`make up` also starts the provider on **http://localhost:8090**, with QuickBytes as
its merchant. Its dev key is `sk_test_dev_only_quickbytes_0123456789abcdef`. Only
these test cards are accepted:

| Card number | What happens |
|---|---|
| `4242424242424242` | Approved: `201`, `authorized` |
| `4000000000000002` | Declined: `402`, `card_declined` (the failed payment comes with it) |
| `4000000000009995` | Not enough money: `402`, `insufficient_funds` |
| `4000000000007777` | Slow: `201`, `processing`; `authorized` about 10 seconds later (`SLOW_CARD_DELAY`), seen with `GET` |
| `4000000000003333` | Hang: authorized at once, but the answer comes after 30 seconds |

```bash
KEY='Authorization: Bearer sk_test_dev_only_quickbytes_0123456789abcdef'

# Hold 320,00 TL. Run it twice with the same Idempotency-Key: the second answer
# is the first one again (Idempotent-Replayed: true), still one payment.
curl -i localhost:8090/v1/payments -H "$KEY" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: demo-1' -d '{"amount_kurus": 32000, "card_number": "4242424242424242"}'

# Take it, then try to drop the hold: 409 invalid_state ("cannot void a payment
# that is captured"). Each new action gets a new key.
curl localhost:8090/v1/payments/<id>/capture -X POST -H "$KEY" -H 'Idempotency-Key: demo-2'
curl localhost:8090/v1/payments/<id>/void -X POST -H "$KEY" -H 'Idempotency-Key: demo-3'

# The hang card, with a caller that gives up after 5 seconds (like the API will).
# The call times out, yet GET shows the money is held: a timeout means "unknown",
# not "failed".
curl --max-time 5 localhost:8090/v1/payments -H "$KEY" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: demo-4' -d '{"amount_kurus": 5000, "card_number": "4000000000003333", "reference": "hang-demo"}'
docker compose exec postgres psql -U quickbytes -d payprovider \
  -c "SELECT id, status FROM payments WHERE reference = 'hang-demo'"
```

All provider endpoints: `POST /v1/payments` (authorize), `GET /v1/payments/{id}`,
`POST /v1/payments/{id}/capture`, `/void`, `/refund`, and `GET /v1/account` (whose
key is this?). The contract is [`api/payprovider.yaml`](api/payprovider.yaml).

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
| `GET /me/addresses` | The signed-in customer's delivery addresses. |
| `POST /me/addresses` | Save one: label, `address_line`, optional `details`, `lat`/`lng`. Up to 10 (`409 address_limit_reached`). |
| `PATCH /me/addresses/{address_id}` | Change only the fields sent. Someone else's address gets 404. |
| `DELETE /me/addresses/{address_id}` | Delete it. `204`. |
| `GET /cart` | The customer's cart, priced from the live menu: lines with their options, subtotal, delivery fee, total, and how far below the restaurant's minimum basket it is. Lines that can't be ordered as they are carry a `problem` and no price. |
| `POST /cart/items` | Add a dish: `{"menu_item_id", "quantity", "choice_ids"}`. The same dish with the same choices adds to that line. A dish from another restaurant gets `409 cart_restaurant_mismatch`. |
| `PATCH /cart/items/{item_id}` | Change a line's `quantity` (1–20) or `choice_ids`. |
| `DELETE /cart/items/{item_id}` | Take a line out; taking out the last one empties the cart. |
| `DELETE /cart` | Empty the cart, e.g. to start one at another restaurant. `204`. |
| `POST /orders` | Check out: `{"address_id"}`, with an `Idempotency-Key` header (retries with the same key replay the first answer). Copies the cart into a `placed` order and empties the cart. `409` for an empty cart (`cart_empty`), flagged lines (`cart_has_problems`), a cart changed mid-checkout (`cart_changed`), a paused restaurant or a basket below the minimum (`below_minimum`). |
| `GET /orders` | The customer's newest 50 orders, with their dishes. |
| `GET /orders/{order_id}` | One order with its status history (each move, who made it, when). Someone else's gets 404. |
| `POST /orders/{order_id}/cancel` | Cancel while `placed` or `paid`; later gets `409 invalid_transition`. |
| `GET /restaurants?lat=…&lng=…` | Restaurants whose delivery zone covers that point, closest first. Public, paginated with `next_cursor`. |
| `GET /restaurants/{id}` | One restaurant. Public. |
| `POST /restaurants` | Create a restaurant (needs a restaurant profile), optionally with `delivery_fee_kurus` and `min_basket_kurus` (both default to 0). |
| `PATCH /restaurants/{id}` | Change only the fields sent. Owner only; anyone else gets 404. |
| `GET /me/restaurants` | The signed-in owner's restaurants. |
| `GET /restaurants/{id}/menu` | A restaurant's menu: sections and dishes in the owner's order, each dish with its option groups and choices, sold-out dishes and switched-off choices marked `"available": false`, removed ones left out. Public. |
| `GET /me/restaurants/{id}/menu` | The owner's full menu, archived categories and items included (marked). |
| `GET /me/restaurants/{id}/orders` | The kitchen queue: the restaurant's paid, accepted and ready orders, oldest first. Owner only. |
| `POST /orders/{order_id}/accept` | Restaurant: accept a paid order. |
| `POST /orders/{order_id}/reject` | Restaurant: reject a paid or accepted order (`cancelled`, reason `rejected`). |
| `POST /orders/{order_id}/ready` | Restaurant: mark an accepted order ready for pickup. |
| `POST /restaurants/{id}/menu/categories` | Add a menu section, e.g. "Kebaplar". Owner only. |
| `PATCH /restaurants/{id}/menu/categories/{category_id}` | Rename, archive (`"archived": true`) or restore it. |
| `POST /restaurants/{id}/menu/items` | Add a dish: category, name, description, price in kuruş (`32000` = 320 TL). |
| `PATCH /restaurants/{id}/menu/items/{item_id}` | Change any field, mark sold out (`"available": false`), move category, archive or restore. |
| `PUT /restaurants/{id}/menu/categories/order` | New section order: `{"category_ids": [...]}`, every visible section exactly once. |
| `PUT /restaurants/{id}/menu/categories/{category_id}/order` | New dish order within a section: `{"item_ids": [...]}`. |
| `POST /restaurants/{id}/menu/items/{item_id}/option-groups` | Add an option group to a dish: `{"name": "Boy", "min_picks": 1, "max_picks": 1}` means "pick exactly one". |
| `PATCH /restaurants/{id}/menu/option-groups/{group_id}` | Rename it or change `min_picks` / `max_picks` (never min above max). |
| `DELETE /restaurants/{id}/menu/option-groups/{group_id}` | Delete it with its choices. `204`. |
| `POST /restaurants/{id}/menu/option-groups/{group_id}/choices` | Add a choice: `{"name": "Büyük", "extra_price_kurus": 4000}`; the price defaults to 0. |
| `PATCH /restaurants/{id}/menu/option-choices/{choice_id}` | Rename, reprice, or switch off for now (`"available": false`). |
| `DELETE /restaurants/{id}/menu/option-choices/{choice_id}` | Delete it. `204`. |

Errors always look like
`{"error": {"code": "...", "message": "...", "request_id": "..."}}` (option-picking
errors add the `group_id` or `choice_id` they're about), and every response carries
an `X-Request-ID` header that matches the server's log line.

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

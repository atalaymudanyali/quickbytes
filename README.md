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
> cards for every outcome; the API authorizes at checkout, captures when the
> restaurant accepts, and hears back by signed webhooks. Every order move is
> published as an event to Redpanda through a transactional outbox. Next: the
> phase 2 wrap-up. See the [roadmap](#roadmap).

## Quick start

Needs Docker and Go 1.27+.

```bash
make up        # start PostgreSQL, Redis, Redpanda, the API and the mock payment provider in Docker
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
  own transaction**, so an order never exists without its answer (a first answer
  before the card is asked, replaced by the final one after). Ten simultaneous
  retries make exactly one order and one card authorization.
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
  an order being paid at that moment is skipped, not waited for.
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
  which its webhooks are sent from. Test cards cover approval, decline,
  insufficient funds, a slow card (`processing`, finished later by a background
  loop) and a card that hangs for 30 seconds. Its own OpenAPI file,
  [`api/payprovider.yaml`](api/payprovider.yaml), is checked against every answer
  in its tests.
- **Payments in the API:** checkout **authorizes** the card (holds the money), the
  restaurant accepting **captures** it, a reject or cancel before that **voids**
  the hold, and a cancel after it is **refunded**. A decline cancels the order
  (`payment_failed`). The API reaches the provider only through a small Go
  interface (`payments.Provider`), with one HTTP client plugged into it, and the
  card number is never stored (only its last 4 digits). Every call carries an
  `Idempotency-Key` made from the payment and the step (`<payment>:capture`), so a
  retry reuses the same key and the provider moves the money once. A provider that
  doesn't answer within 5 seconds means **`unknown`, not failed**. A background
  reconcile loop then finds the payment at the provider by the order ID, and never
  authorizes again. If nothing turns up after a minute, the payment fails
  (`no_answer`). The same loop polls slow (`processing`) cards, and retries
  captures, voids and refunds that got no answer, waiting longer each time
  (exponential backoff with jitter). It decides what to do the thermostat way:
  look at the order and the payment, and take the one step that brings them in
  line. Every payment change is appended to `payment_events` (append-only, by
  trigger) with where it came from (`request`, `reconcile` or `webhook`). Several
  API copies can run the loop at once: each takes due payments with
  `FOR UPDATE SKIP LOCKED` and a one-minute lease. The customer sees the payment
  and its history on the order; the restaurant doesn't.
- **Payment webhooks:** instead of only asking, the API is **told**: the provider
  posts `payment.authorized`, `.failed`, `.captured`, `.voided` and `.refunded` to
  `POST /webhooks/payprovider`. Anyone could call that URL, so every webhook is
  **signed** like Stripe's (`QB-Signature: t=…,v1=…`, HMAC-SHA256 of the time and
  the raw body with a shared secret, compared in constant time, refused when more
  than 5 minutes old so it can't be replayed). The provider queues each webhook in
  the **same statement** as the payment change, and keeps trying while the API
  doesn't answer 2xx: 1 s, 2 s, 4 s… up to an hour, with random jitter, for about
  2 hours. So a webhook can arrive **twice** (the API remembers event IDs in a
  `webhook_events` table, written in the same transaction as the change it makes)
  and **out of order** (a payment's status only ever moves forward, so older news
  changes nothing). Webhooks go through the same code as the reconcile loop, which
  stays as the safety net and now waits 30 s before polling a slow card. A test
  switch (`WEBHOOK_SEND_TWICE`) sends every webhook twice.
- **Order events (transactional outbox):** every order move (placed, paid,
  accepted, ready, cancelled) is published to **Redpanda** (Kafka's protocol),
  topic `orders`, for dispatch to react to in phase 3. Saving the move in
  PostgreSQL and publishing to Redpanda can't be one all-or-nothing step, so the
  event is written into an `outbox` table **in the move's own transaction**: it
  exists if and only if the move committed. A relay in the API then publishes the
  waiting rows and stamps them, one transaction per round. Several relays can run
  (`FOR UPDATE SKIP LOCKED`), the message key is the order ID (one order's events
  stay on one partition, in order), and a relay only takes an order's oldest
  waiting event, so a second relay can't overtake the first. Delivery is at least
  once: each event carries an ID for consumers to drop repeats. While Redpanda is
  down, orders keep working, events pile up, and the relay backs off (1 s up to
  30 s) and sends them all once it's back. Published rows are deleted after
  7 days.

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

### Payments: timeouts and retries

`make test` runs these against the real provider over HTTP, with the API's client
timeout cut to 300 ms so the hang card really times out
([`internal/payments/reconcile_test.go`](internal/payments/reconcile_test.go)).
The live numbers are from `make up` with the default 5-second timeout and
5-second reconcile loop, measured before webhooks existed (today the same path runs
with `WEBHOOK_URL= make up`; see the next section for how webhooks changed them):

| What | Result |
|---|---|
| Hang card at checkout (live) | Answer after **5.03 s**: order `placed`, payment `unknown` |
| …then the reconcile loop (live) | `paid` about **9 s later**, found by the order ID; the provider holds **one** payment |
| Slow card (live) | `processing`, then `paid` about **12 s** after checkout |
| 10 simultaneous retries of one checkout | **1** order, **1** card authorization |
| The same authorize sent 4 times (hang card, real provider) | **1** provider payment |

Each safety net was also broken on purpose, to check a test catches it:

| Broken on purpose | What the tests said |
|---|---|
| A fresh random key on every authorize try | `the provider has 4 payments for the order, want 1` (a double charge) |
| No one-minute grace before "never arrived" | `young payment: failed, want still pending` |
| Provider calls stopped when the customer's request ends | `context canceled`: an answer about real money went unrecorded |
| The order move not marking the payment due | `payment due: false`: an accept whose capture failed would never be retried |

### Payments: webhooks

Live, with `make up` (the slow card's bank takes 10 s; the provider's settler and
sender each run every second). The times are from the API's own ledger
(`payment_events`), checkout to `authorized`:

| What | Result |
|---|---|
| Slow card, webhooks on (3 orders) | `paid` after **11.2–11.4 s**, learned `via webhook` |
| Slow card, webhooks off (`WEBHOOK_URL=`, 2 orders) | `paid` after **31.8–34.7 s**, `via reconcile` (the first poll waits 30 s for a webhook) |
| Hang card, webhooks on (3 orders) | the webhook lands **0.03–0.9 s** after checkout, while our call still waits; checkout answers **`paid`** after **5.1 s**, and no `unknown` is ever recorded |
| API stopped for 30 s after a slow-card checkout | delivered on the provider's **5th try**, 21 s after it was queued; order `paid`, `via webhook` |
| `WEBHOOK_SEND_TWICE=true` | 2 posts, both `200`; **1** `webhook_events` row, **1** status change |

`make test` covers the same cases with the real provider and its sender posting to
a real API over HTTP ([`internal/httpapi/webhooks_test.go`](internal/httpapi/webhooks_test.go)),
including an API that answers `500` twice before the third try gets through.

Broken on purpose:

| Broken on purpose | What the tests said |
|---|---|
| No ratchet (a status may go backwards) | `outcome "applied", want "stale"`, and `ledger = [pending, authorized via webhook, unknown via request]`: our own timeout wiped out the webhook's news |
| The guest-book row written outside the apply transaction | `the failed webhook is in the guest book: its retry would be skipped` |
| No 5-minute check on the signature's time | a 6-minute-old webhook got `200 {"outcome":"applied"}`, and `order paid after a refused webhook` |
| Redirects followed | the redirected webhook arrived as an empty `GET`: `webhook signature doesn't match` |

### Order events: Redpanda down, API killed

Live, with `make up` (relay polling every 500 ms, a 10 s limit per publish):

| What | Result |
|---|---|
| Place, pay, accept, ready | `order.placed`, `.paid`, `.accepted`, `.ready` on one partition, offsets 0–3, keyed by the order ID |
| Redpanda stopped, 2 checkouts (6 events) | both checkouts answer `paid`; the relay backs off (1 s, 2 s…) |
| …Redpanda started again | all 6 events published within **~5 s**, each order's in order |
| API killed (`docker compose kill`) while the relay held a batch | the events stayed waiting; after the restart they were published, **0** repeats |
| Two relays side by side (Docker API + a second API on :8081), 10 orders | **62** outbox rows → **62** messages, **0** repeats |

The relay's tests use a fake publisher and franz-go's in-memory Kafka (`kfake`),
so CI needs no Redpanda
([`internal/outbox`](internal/outbox)). Broken on purpose:

| Broken on purpose | What the tests said |
|---|---|
| No oldest-event rule | `relay B published [x2 y1], want only y1: x1 is A's, x2 must wait for x1` |
| `FOR UPDATE` without `SKIP LOCKED` | `relay B waited for relay A's rows instead of skipping them` |
| No row lock at all | `event … published 2 times, want 1` |
| Rows stamped even when publishing failed | `event … published 0 times, want 1` (lost) |
| franz-go's default "never cancel a sent message" | `want an error when the broker never answered in time`: the relay's transaction stayed open as long as the broker was silent |
| The move's event left out of the transaction path | `0 events, want 1` for every move |

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
`POST /orders` with `{"address_id": "...", "card_number": "4242424242424242"}` and
an `Idempotency-Key` header (any new UUID, e.g. from `uuidgen`). The card is one of
the payment provider's test cards (below). The answer is the order with its
history and its payment, and the cart is now empty. What the card does:

| Card | Order | Payment (ledger) |
|---|---|---|
| `4242424242424242` | `paid` | `pending` → `authorized` |
| `4000000000000002` | `cancelled` (`payment_failed`) | `pending` → `failed` (`card_declined`) |
| `4000000000007777` | `placed`, then `paid` about 11 seconds later | `pending` → `processing` → `authorized` (via `webhook`) |
| `4000000000003333` | `paid`, after a 5-second wait | `pending` → `authorized` (via `webhook`, while our call was still waiting) |

Keep reading the order (`GET /orders/{order_id}`) to watch the slow card settle:
the provider's webhook tells the API. With webhooks off (`WEBHOOK_URL= make up`),
the API's reconcile loop finds out instead: the slow card after about 30 seconds,
and the hang card goes `unknown` first, then `authorized` (via `reconcile`), found
by the order ID instead of charging again.

Then:

- Send the exact same request again, same key: you get the same order back with
  `Idempotent-Replayed: true`, and `GET /orders` still shows one order. The same
  key with another `address_id` gets `422 idempotency_key_reused`.

- Change that dish's price as `hasan@`, and `GET /orders/{order_id}`: the order
  keeps the price it was placed at.
- `POST /orders/{order_id}/cancel` cancels it (`by_customer`); a second cancel gets
  `409 invalid_transition`.
- As `hasan@`, `GET /me/restaurants/{id}/orders` is the kitchen queue: the paid
  orders (approved card) show up there.
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
| `4000000000007777` | Slow: `201`, `processing`; `authorized` about 10 seconds later (`SLOW_CARD_DELAY`), announced by a `payment.authorized` webhook (or seen with `GET`) |
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
# The call times out, so we never got the payment's ID. Looking it up by our own
# reference shows the money is held: a timeout means "unknown", not "failed".
curl --max-time 5 localhost:8090/v1/payments -H "$KEY" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: demo-4' -d '{"amount_kurus": 5000, "card_number": "4000000000003333", "reference": "hang-demo"}'
curl 'localhost:8090/v1/payments?reference=hang-demo' -H "$KEY"
```

All provider endpoints: `POST /v1/payments` (authorize), `GET /v1/payments?reference=`
(find by your own reference), `GET /v1/payments/{id}`,
`POST /v1/payments/{id}/capture`, `/void`, `/refund`, and `GET /v1/account` (whose
key is this?). The contract is [`api/payprovider.yaml`](api/payprovider.yaml).

### Webhooks

The provider posts its webhooks to the API (`WEBHOOK_URL` and `WEBHOOK_SECRET` in
`docker-compose.yml`). Place a slow-card order, then see what arrived and what the
API did with it:

```bash
docker compose exec -T postgres psql -U quickbytes -d quickbytes \
  -c "SELECT type, outcome, received_at FROM webhook_events ORDER BY received_at DESC LIMIT 5"
docker compose logs payprovider | grep webhook
```

- **A forged one** is refused with `400 invalid_webhook`, and changes nothing:
  `curl localhost:8080/webhooks/payprovider -H 'Content-Type: application/json' -H 'QB-Signature: t=1792927973,v1=00' -d '{"id": "evt_fake"}'`
- **The API down:** place a slow-card order, `docker compose stop api` within
  10 seconds, and watch the provider's log try again (1 s, 2 s, 4 s, 8 s…). After
  `docker compose start api` the next try gets through and the order is `paid`.
- **Twice:** `WEBHOOK_SEND_TWICE=true make up` posts every webhook twice. The API
  answers both `200`, the second with outcome `repeat`, and the order moves once.
- **Off:** `WEBHOOK_URL= make up` turns webhooks off; the reconcile loop still
  settles every payment, only later. `make up` turns them back on.

### Order events

Watch every order event live (`rpk topic consume` inside the Redpanda container),
then place and pay an order in another terminal:

```bash
make events     # partition, offset, key (the order ID), event-id / event-type, then the JSON
make console    # or browse topics and messages at http://localhost:8088
```

- **Redpanda down:** `docker compose stop redpanda`, place an order (checkout
  still answers), and see the events wait:
  `docker compose exec -T postgres psql -U quickbytes -d quickbytes -c "SELECT seq, type, published_at FROM outbox ORDER BY seq DESC LIMIT 5"`.
  `docker compose logs -f api | grep outbox` shows the relay backing off. After
  `docker compose start redpanda` they appear in `make events`, in order.
- **Relay off:** `OUTBOX_RELAY=false make up` lets events pile up in the outbox;
  `make up` sends them.

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
| `POST /orders` | Check out: `{"address_id", "card_number"}` (a test card), with an `Idempotency-Key` header (retries with the same key replay the first answer). Copies the cart into an order, empties the cart, and authorizes the card: `paid`, `cancelled` (`payment_failed`), or `placed` while the payment is `processing` or `unknown`. `409` for an empty cart (`cart_empty`), flagged lines (`cart_has_problems`), a cart changed mid-checkout (`cart_changed`), a paused restaurant or a basket below the minimum (`below_minimum`). |
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

**In use now:** Go (`net/http`, pgx, goose, franz-go) · PostgreSQL 17 + PostGIS 3.5 ·
Redis · Redpanda (Kafka API) · OpenAPI 3.0 + Swagger UI · k6 · Docker Compose ·
GitHub Actions

**Coming in later phases:** Python + OR-Tools (route optimizer, over gRPC) ·
OSRM with Istanbul OpenStreetMap data · SwiftUI + MapKit ·
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
- [0008](docs/adr/0008-payments-find-out-never-charge-again.md) Payments: a timeout is "unknown", found out later, never charged again
- [0009](docs/adr/0009-payment-webhooks.md) Payment webhooks: signed, sent at least once, applied by a ratchet
- [0010](docs/adr/0010-transactional-outbox.md) Order events: a transactional outbox, relayed to Redpanda

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

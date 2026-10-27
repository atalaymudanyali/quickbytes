# 0012. The cart lives in PostgreSQL and stores no prices

**Status:** Accepted · decided in the phase 2 plan, built in issue #16, recorded
2026-10-27 (issue #23)

## Context

A customer fills a basket before ordering. It has to survive closing the app or
switching phones, so it can't live only on the phone. Meanwhile the restaurant
can change a price, sell out a dish or switch off an option while the dish sits
in someone's basket. Two questions follow: where does the cart live, and does it
keep its own copy of the prices?

## Decision

- **One cart per customer, in PostgreSQL** (`carts`, `cart_items`), for one
  restaurant at a time. A dish from another restaurant gets
  `409 cart_restaurant_mismatch`.
- **No prices stored.** Each line holds the dish, the quantity and the picked
  choice IDs. Every read prices it **live** from the menu (served by the Redis
  cache from [0004](0004-redis-fail-open-cache.md)): subtotal, delivery fee,
  total, and how far below the minimum basket it is. The menu stays the one
  **source of truth**; a price change shows up at once.
- **Problem lines stay, flagged.** A dish sold out, removed, or whose options no
  longer fit keeps its line with a `problem` and no price, instead of vanishing.
  Checkout refuses a cart with problems.
- **The database keeps a cart in one restaurant.** A line's foreign keys to the
  cart and to the dish both include `restaurant_id`, so a mixed cart is refused
  even if a future bug lets one through.
- **Checkout copies; the cart points.** The order (see
  [0013](0013-orders-copy-and-one-state-machine.md)) is where names and prices
  are frozen.

## Alternatives

| Option | Why not |
|---|---|
| Cart in Redis | Redis here is a cache that fails open: when it's down, the app keeps working without it. A cart that vanishes with Redis breaks that promise. |
| Cart on the phone only | Lost when switching devices or reinstalling, and every price would have to be re-checked at checkout anyway. |
| Copy prices into the cart | A copy goes stale: the basket shows yesterday's price and checkout has to correct it. Two sources of truth to keep in line. |
| Drop lines that can't be ordered | A dish silently disappearing is worse than one shown as "no longer available". |

## Consequences

- A cart read costs a menu lookup, cheap because the menu is cached and served
  with ETags (27,107 requests/s from cache in the menu load test).
- Prices are checked twice: on every read and again at checkout, inside its
  transaction (`409 cart_changed` if the cart moved in between).
- Abandoned carts stay in the table; expiring them is in the upgrades list.

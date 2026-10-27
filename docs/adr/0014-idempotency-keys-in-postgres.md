# 0014. Idempotency keys in PostgreSQL, saved in the order's own transaction

**Status:** Accepted · decided in the phase 2 plan, built in issue #18, recorded
2026-10-27 (issue #23)

## Context

A phone taps "Place order", the request reaches the API, the order is placed and
the card is held, and then the answer is lost (a tunnel, a timeout). The app
can't tell "it worked" from "it never arrived", so it sends the request again.
Without help, the retry either places a second order and charges again, or gets
a confusing error ("your cart is empty") for an order that did go through.

## Decision

- **`POST /orders` requires an `Idempotency-Key`** header: one random value
  (a UUID) per tap on "Place order", the same value on every retry of it. No key
  is `400`: an optional key would be forgotten exactly where it matters.
- **The first answer is stored and replayed.** A retry with the same key gets the
  stored answer again, byte for byte, marked `Idempotent-Replayed: true`. While
  the first attempt still runs, retries get `409 idempotency_in_progress` with
  `Retry-After: 1`. The same key with a different body (a SHA-256 fingerprint of
  method, path and raw body) is `422 idempotency_key_reused`.
- **In PostgreSQL, per user,** kept 24 hours (`PRIMARY KEY (user_id, key)`).
- **Saved in the order's own transaction.** Checkout stores a first answer
  ("placed, payment pending") in the transaction that creates the order, and
  replaces it with the final one after the card is asked. An order never exists
  without an answer for its key.
- **2xx and 4xx are final and stored; a 5xx releases the key**, since nothing
  was committed and a retry may really run. Like Stripe.
- **Stuck keys are taken over** after a minute, by the same request only, and an
  `attempt` ID (a fencing token) stops an overtaken attempt from overwriting the
  newer answer.
- **A middleware wraps the route**, so any `POST` can be made retry-safe with one
  word. The mock provider ([0011](0011-payment-provider-own-service.md)) reuses
  the same package with its own table, and the API sends it one key per payment
  and step, so retries never move money twice there either.

## Alternatives

| Option | Why not |
|---|---|
| Keys in Redis | Faster, but the answer must be as durable as the order, and only PostgreSQL can commit both in one transaction. Redis here may also be down by design ([0004](0004-redis-fail-open-cache.md)). |
| Save the answer after the order commits | A crash between the two leaves an order whose retry runs again. |
| Rely on the cart lock alone | It stops a second order (a retry finds the cart empty), but the retry gets an error for an order that went through. |
| Dedupe by "same cart, same minute" | Guesswork: two real orders of the same basket are allowed. |
| Store every answer, 5xx included | A crash would be replayed for 24 hours instead of retried. |

## Consequences

- Measured with `make load-checkout`: 84 checkouts, each sent 10 times at once
  with one key (hang-card answers lost on purpose), gave **84 orders, 84
  payments at the provider, 0 double charges**. With a fresh key per try the
  run fails: still one order (the cart lock), but 9 errors per checkout.
- A retry during a slow card call gets the honest first answer at once, instead
  of waiting for the provider.
- One more table, cleaned every 10 minutes, and one more header for clients.

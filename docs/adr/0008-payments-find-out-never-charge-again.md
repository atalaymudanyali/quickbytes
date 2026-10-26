# 0008. Payments: a timeout is "unknown", found out later, never charged again

**Status:** Accepted · recorded 2026-10-26 (issue #20)

## Context

The API takes money through a payment provider it reaches over HTTP (the mock
`payprovider`, standing in for Stripe or iyzico). Any call can be slow, fail, or
succeed at the provider while the answer is lost on the way back. The hang card does
exactly that: it holds the money at once, then answers after 30 seconds, long past
our 5-second timeout. After a timeout we don't know whether the money moved. Both
easy reactions are wrong: calling it "failed" leaves a customer charged for a
cancelled order, and authorizing again can charge them twice.

Money also has to follow the order: held at checkout, taken when the restaurant
accepts, released on a reject, given back on a later cancel. That holds even when a
step's call gets no answer, or the API crashes between moving the order and calling
the provider.

## Decision

- **Three kinds of answer.** Every provider reply is either news about the payment
  (approved, declined, still processing), a final no (the provider judged the request
  and refused: 400, 404, 422), or **no answer** (timeout, connection error, 5xx, 429,
  "still busy with this key"). No answer never changes a payment: it's `unknown`, or
  the step is simply tried again later.
- **One key per payment and step** (`<payment_id>:authorize`, `:capture`, ...), sent as
  `Idempotency-Key` on every try. A retry repeats the same request with the same key,
  so the provider moves the money once.
- **Find out, never authorize again.** The order ID is sent as the payment's
  `reference`. A background **reconcile loop** looks an `unknown` payment up by it
  (`GET /v1/payments?reference=`), and polls `processing` ones by ID (until #21's
  webhooks). Authorizing again is impossible anyway: the card number is never
  stored. If the lookup finds nothing a minute after checkout, the authorize never
  arrived: the payment fails (`no_answer`) and the order is cancelled like a decline.
- **A thermostat, not a script.** A pure function `nextStep(order status, payment
  status)` names the one step that brings the money in line (capture, void, refund,
  look up, check, or nothing). The same `Sync` runs right after a move (the fast
  path) and in the loop. Order moves mark the payment due **in their own
  transaction**, so a crash right after an accept still leads to a capture.
- **The order doesn't wait for the money.** An accept stands even when its capture
  gets no answer; the loop retries with exponential backoff (2 s doubling to 5 min,
  plus up to 20% jitter).
- **An append-only ledger.** Every status change is a `payment_events` row (a trigger
  refuses edits), noting whether the request or the loop learned it.
- **A small interface as the wall socket.** The payments code talks to
  `payments.Provider`; the HTTP client is one plug, and tests use an in-memory fake
  or the real provider over `httptest`.

## Alternatives

| Option | Why not |
|---|---|
| Treat a timeout as a decline | The money may be held: the customer pays for an order we cancelled. |
| Retry authorize with a new key | Each try can create a new hold: a double charge. |
| Keep the card number to retry later | Storing card numbers brings card-data rules (PCI) and risk, for a retry the reference lookup makes unnecessary. |
| Call the provider inside the order's transaction | A row lock would be held across a 5-second network call, and a rollback can't undo money that moved. |
| Wait for webhooks only (#21) | Webhooks come later in the plan, and get lost too; a loop that asks is the safety net either way. |
| Capture at checkout | Money would be taken for orders the restaurant then rejects, and refunds are slower and cost more than voids. |

## Consequences

- A hang-card checkout answers `placed` + `unknown` after 5 s, and becomes `paid`
  about 9 s later with exactly one provider payment. Tests prove one charge for
  repeated authorizes, and that each safety net fails a test when removed.
- A customer sees `placed` for a few seconds on slow and hung cards: the app has to
  keep reading the order (webhooks in #21 make it faster, not different).
- The checkout answer can change once: a retry may get the first answer (payment
  pending) or the final one.
- Not covered yet: an authorize that reaches the provider after the minute's grace
  leaves an orphan hold that nothing voids, and a capture the provider
  refuses is only logged, with no one alerted. Both are in the upgrades list.
- The loop's work is spread over API copies by `FOR UPDATE SKIP LOCKED` and a
  one-minute lease, so a crash mid-run delays a payment by at most a minute.

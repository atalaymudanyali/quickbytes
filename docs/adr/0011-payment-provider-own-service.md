# 0011. The payment provider is its own service, with its own database

**Status:** Accepted · decided in the phase 2 plan, built in issue #19, recorded
2026-10-27 (issue #23)

## Context

Phase 2 takes money, and the hard part of payments is not the arithmetic. It's
that the money lives at **another company** (Stripe, iyzico), reached over the
network: a call can be slow, fail, arrive twice, or succeed while its answer is
lost. A project that fakes payments with a function call inside the API never
meets those problems, so it can't show how it handles them.

[0002](0002-modular-monolith.md) keeps QuickBytes one program unless there is "a
real reason to split". This is one.

## Decision

- **A separate program, `payprovider`** (`cmd/payprovider`), its own Docker
  service on `:8090`, with its own OpenAPI file (`api/payprovider.yaml`). The API
  reaches it **only over HTTP**, with an API key, like a merchant reaches Stripe.
- **Its own database** (`payprovider`) in the same PostgreSQL container, sharing
  no tables with the API. Neither side can read the other's rows, or join them in
  a transaction: our database can't "undo" money that moved there.
- **It behaves like a real provider:** authorize, then capture, void or refund;
  `Idempotency-Key` required on every write; a payment's moves checked in one
  guarded `UPDATE`, so two captures at once can't both win; an append-only
  history that its webhooks are sent from; only the card's last 4 digits stored.
- **Test cards for every outcome:** approve, decline, insufficient funds, a slow
  card (`processing`, finished ~10 s later) and a hang card (the money is held at
  once, the answer comes after 30 s, long past the API's 5 s timeout).
- **Inside the API, a small interface** (`payments.Provider`) is the wall
  socket. The HTTP client is one plug; tests plug in an in-memory fake, or the
  real provider over `httptest`.

## Alternatives

| Option | Why not |
|---|---|
| A package inside the API | The failures would be imaginary: no timeouts, no lost answers, no duplicate webhooks. |
| Stripe's or iyzico's test mode | Needs an account and keys (the project uses nothing paid or keyed), and can't hang on demand or be stopped to test a webhook retry. |
| A second PostgreSQL container | Separate data without a second server to run is enough; no tables are shared either way. |
| Share the API's tables | The provider could then be "consistent" with the API by accident, which no real provider is. |

## Consequences

- Every failure path is real and tested: the hang card made "a timeout is
  unknown, not failed" ([0008](0008-payments-find-out-never-charge-again.md)) and
  webhooks ([0009](0009-payment-webhooks.md)) necessary, not decorative.
- `make load-checkout` can ask the provider itself how many payments it holds per
  order (84 checkouts, 84 payments, 0 double charges), the way you'd check a bank
  statement rather than your own notes.
- Two programs to run, two migration sets and two OpenAPI files. `make up` hides
  most of that.
- Some HTTP basics (idempotency middleware, the spec checker) are copied between
  the two services for now; sharing them is in the upgrades list.

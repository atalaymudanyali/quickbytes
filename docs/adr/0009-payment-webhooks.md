# 0009. Payment webhooks: signed, sent at least once, applied by a ratchet

**Status:** Accepted · recorded 2026-10-27 (issue #21)

## Context

Until now the API learned about a slow payment only by asking the provider again and
again (the reconcile loop from [0008](0008-payments-find-out-never-charge-again.md)).
That's slow and wasteful. The usual fix is a **webhook**: the provider calls a URL on
our API when a payment changes. That raises three problems:

- The URL is public. Anyone can call it and claim "order 42 is paid".
- Our API can be down when the provider calls, so the provider has to try again, and
  a retry whose first try actually got through arrives twice.
- Retries overtake each other: an older "authorized" can arrive after a newer
  "captured". Our own timed-out checkout call can also report "no answer" after a
  webhook has already told us the money is held.

## Decision

- **Signed like Stripe.** `QB-Signature: t=<unix>,v1=<hex>`, where `v1` is
  HMAC-SHA256 of `<t>.<raw body>` with a shared secret. The API checks it before
  reading the JSON, compares in constant time (`hmac.Equal`), refuses a `t` more
  than 5 minutes from its clock (a replay), and accepts any one of several `v1`
  values (so a secret can be replaced without downtime). Both services share the
  small `internal/webhook` package.
- **A delivery queue at the provider, written with the change.** Each payment event
  queues one `webhook_deliveries` row per endpoint, **in the same statement** as the
  event. A change can't happen without its webhook queued, and an undone change
  queues nothing.
- **At least once, with backoff.** A sender claims due deliveries with a lease
  (`FOR UPDATE SKIP LOCKED`), posts them in parallel, and on anything but a 2xx
  (timeouts, refused connections and redirects included) tries again after 1 s, 2 s,
  4 s… up to an hour, plus up to 20% jitter. After 14 tries (about 2 hours) it gives
  up. Redirects are never followed. There is no promise about order.
- **A guest book at the API, in the same transaction.** `webhook_events` is keyed by
  the provider's event ID. The row is written in the transaction that applies the
  webhook, so a failure (a 500) undoes it too, and the provider's retry is handled
  for real instead of being skipped as a repeat. A repeat is answered `200` and does
  nothing.
- **A ratchet on every status change.** `updateIn`, the one function every payment
  change goes through, only moves a payment forward (`pending → unknown →
  processing → authorized|failed → captured|voided → refunded`), never out of a
  final status. Older news, from any source, changes nothing.
- **One way to act on news.** A webhook is applied through the same `recordIn` as
  checkout's answer and the reconcile loop: the order and payment locks, the ledger
  (`via webhook`), paying or cancelling the order. A money step it leads to (void a
  hold whose order was cancelled meanwhile) is left to the loop, so no provider call
  happens inside the webhook's request.
- **Webhooks are the fast path, polling is the safety net.** A `processing` payment's
  first poll waits 30 seconds, long enough for its webhook, and the loop catches any
  webhook that never comes.
- **Genuine but unknown event types get a 200.** An error would only make the
  provider retry the same webhook for hours.

## Alternatives

| Option | Why not |
|---|---|
| Treat a webhook as a doorbell and `GET` the payment | Always current, but one more call to the provider per webhook, and the ratchet is needed anyway for our own timed-out calls. |
| A shared token in a header instead of a signature | A copied request could be replayed forever, and nothing protects the body from being changed. |
| Deliver in order, one payment at a time | One failing webhook would hold back every later one for that payment, for up to two hours. |
| Mark the event as seen in its own step, before applying | If applying then fails, every retry is skipped as a repeat: the update is lost. |
| Webhooks only, drop the reconcile loop | A webhook that's given up, or sent while webhooks were off, would leave the order waiting forever. |
| Follow redirects | Signed payment data could end up at an address nobody configured. |
| Poll every 2 s alongside the webhook | Works, but the webhook would rarely be the one that settles a payment, and the loop would keep asking for nothing. |

## Consequences

- A slow-card order is `paid` about **11 s** after checkout (the bank's 10 s, plus
  one tick each of the settler and the sender), learned `via webhook`. With
  webhooks off it's about **32–35 s**: the price of letting the webhook go first.
- The hang card's webhook arrives while our call is still waiting, so checkout now
  answers `paid` after its 5-second timeout, instead of `placed` + `unknown`.
- The webhook endpoint has no login: the signature is the only gate, so the secret
  must stay secret. The provider stores endpoint secrets in plain text (it needs
  them to sign); a real one encrypts them at rest.
- The guest book grows forever, the endpoint isn't rate limited, and a merchant has
  one endpoint. These are in the upgrades list.
- The constant-time compare is reviewed, not tested: a timing leak doesn't show up
  in a unit test.

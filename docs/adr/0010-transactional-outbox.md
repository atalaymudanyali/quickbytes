# 0010. Order events: a transactional outbox, relayed to Redpanda

**Status:** Accepted · recorded 2026-10-27 (issue #22)

## Context

Other parts of the system need to hear when an order moves: dispatch (phase 3) must
learn that an order is paid or ready. We announce that as **events** on Redpanda
(Kafka's protocol), topic `orders`.

But an order move is saved in PostgreSQL, and the event goes to Redpanda: two
separate systems, with no way to save to both as one all-or-nothing step (the
**dual-write problem**):

- Save the move, then publish: a crash in between, or Redpanda being down, loses
  the event. The order is paid, and dispatch never hears about it.
- Publish, then save: a failed save leaves an event about a move that never
  happened.

## Decision

- **An `outbox` table, written in the move's own transaction.** Every move goes
  through one function (`record`, next to the status history), which also inserts
  the event. The event exists if and only if the move committed. `outbox.AddIn`
  only accepts a transaction, so it can't be called outside one.
- **A relay publishes the waiting rows.** It runs in the API (`OUTBOX_RELAY`).
  Each round is one transaction: take up to 100 waiting rows, publish them, stamp
  `published_at`, commit. If publishing fails, the round rolls back and the rows
  stay waiting. A crash after Redpanda stored a batch, but before the commit,
  publishes it again. Delivery is **at least once**: every event carries its own
  ID (in the body and as a Kafka header), and consumers drop repeats by it.
- **Several relays can run.** Rows are taken with `FOR UPDATE SKIP LOCKED`, so two
  relays never publish the same row, and neither waits for the other.
- **One order's events stay in order.** The message key is the order ID, so all its
  events land on one partition, in the order they were written. Rows are ordered
  by a database counter (`seq`), not by time, since machines' clocks can disagree.
  A relay takes only each order's **oldest** waiting event: otherwise a second
  relay could skip a locked "paid" and publish the same order's "accepted" first.
- **The whole relay backs off while Redpanda is down** (1 s doubling to 30 s,
  +20% jitter). Every event fails for the same reason then, so per-row retry
  counts would add nothing. Orders keep working: their events pile up.
- **A round's publish has a hard 10 s limit.** franz-go's idempotent producer, by
  default, never gives up on a message it has already sent (it can't tell "never
  arrived" from "stored, answer lost"). That would keep the round's transaction,
  and its row locks, open for the whole outage. `AllowIdempotentProduceCancellation`
  lets the limit win; the cost is a possible repeat, already allowed.
- **Events carry IDs and the change**, not the whole order: `{id, type, version,
  occurred_at, order_id, data: {from, to, cancel_reason, by_role, restaurant_id,
  customer_profile_id, total_kurus}}`. A consumer that needs more reads the order.
- **Topics are made on purpose** (`make topics`: `orders`, 3 partitions), with
  automatic topic creation off, so a misspelled topic fails instead of quietly
  collecting events nobody reads.
- **Published rows are kept 7 days**, then deleted in batches. Waiting rows are
  never deleted.

## Alternatives

| Option | Why not |
|---|---|
| Publish straight after the commit, retry on failure | A crash between commit and publish loses the event: the retry lives only in memory. |
| Change data capture (Debezium reading PostgreSQL's log) | No extra table, but another heavy service (Kafka Connect) to run; out of scope for #22. |
| Lease the rows (claim, commit, publish, then stamp), like the webhook sender in [0009](0009-payment-webhooks.md) | No transaction open during the publish, but more code and per-row retry state, built for a case where only one endpoint is down. Here the broker is down for everyone. |
| `SKIP LOCKED` without the oldest-event rule | Two relays could publish one order's events out of order. |
| One relay at a time (an advisory lock as leader) | Ordering for free, but no sharing of work and a leader to hand over when it dies. |
| Order by `created_at` | Two API machines' clocks can be milliseconds apart; a counter can't. |
| The full order in every event | Consumers never call back, but every change to the order's shape becomes a change to the event format. |

## Consequences

- An event reaches Redpanda within about half a second of its move (the relay's
  polling interval). After a Redpanda outage, the backlog drained within **~5 s**
  of it coming back, each order's events in order.
- Killing the API mid-publish lost nothing (0 repeats in that run, though a repeat
  is allowed). With two relays side by side, 62 events gave 62 messages, 0 repeats.
- Consumers must be idempotent: they will see a repeat sometimes, by design.
- An order with several waiting events needs one round per event after an outage.
- The relay polls the table even when there's nothing to send (a cheap check on a
  small partial index). `LISTEN/NOTIFY` could wake it instead; that's in the
  upgrades list, along with a poison-message limit and a dead-letter topic.

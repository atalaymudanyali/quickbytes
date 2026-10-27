# 0013. Orders copy what was bought, and move only through one state machine

**Status:** Accepted · decided in the phase 2 plan, built in issue #17, recorded
2026-10-27 (issue #23)

## Context

An order is a record of what was bought, at what price, sent where. The menu and
the address book keep changing afterwards. An order also moves through a life
(paid, accepted, ready, delivered, or cancelled), pushed by different people and
programs: the customer, the restaurant, the payment flow, an expiry timer and,
later, drivers. Two of them can press a button on the same order at the same
moment ("cancel" and "accept"), and only one may win.

## Decision

- **Checkout copies.** In one transaction it locks the cart, checks it still
  holds what was priced, **copies** every dish name, option, price and the
  delivery address into the order, and empties the cart. Later edits to the menu
  or the address never change a past order.
- **One list of statuses, plus a reason:** `placed → paid → accepted → ready →
  picked_up → delivered`, or `cancelled` with a `cancel_reason`
  (`payment_failed`, `rejected`, `by_customer`, `expired`). One cancelled status
  with a reason, not five kinds of cancelled.
- **Actions, not statuses.** Clients press buttons (`/cancel`, `/accept`,
  `/reject`, `/ready`); they never send `{"status": …}`. One rules table says
  where each action leads from which status, and which role may press it.
- **One function makes every move** (`transition`), expiry and payments
  included. It locks the order row (`SELECT … FOR UPDATE`), checks the rule and
  the owner under that lock, changes the status, and in the same transaction
  writes a history row, the order event for the outbox
  ([0010](0010-transactional-outbox.md)) and marks the payment due
  ([0008](0008-payments-find-out-never-charge-again.md)). Anything else gets
  `409 invalid_transition`.
- **Restaurants see paid orders only**, and may reject an accepted one ("we ran
  out after all"); the payment flow then refunds instead of voiding.
- **Expiry is a loop in the API**: orders still unpaid after 15 minutes are
  cancelled (`expired`), taken with `FOR UPDATE SKIP LOCKED` so several API
  copies share the work.

## Alternatives

| Option | Why not |
|---|---|
| Point at the menu and the address book | An order would change when the menu does: last week's receipt showing today's price. |
| `PATCH /orders/{id}` with a status | A client could ask for any status; the rules would be spread over every caller. |
| Each feature updating `status` itself | No single place to lock, check, record and announce a move; the history and the events would miss some. |
| Several cancelled statuses | Every "is it over?" check would list them all; a reason column says the same with one status. |
| Check the owner before locking | The order could change between the check and the move. |

## Consequences

- Two moves at once can't both win. A deterministic test holds the lock in one
  transaction and shows the second move waits, then gets `invalid_transition`;
  without the lock it wins too.
- Every move has a history row and an event, written together or not at all.
- Orders duplicate some data (names, prices, the address): the price of a record
  that never changes.
- `picked_up` and `delivered` exist in the rules but wait for drivers (phase 3).

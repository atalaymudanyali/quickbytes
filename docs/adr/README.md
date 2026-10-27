# Architecture decision records

An ADR is a short note about one decision that shapes the project. It records what
was decided, what else was considered, and what the choice costs. A ship's logbook
is a good comparison. It doesn't explain how to sail. It records why the captain
turned left at a certain point, so the next person on watch doesn't have to guess.

Each record is one screen: **Context** (the problem), **Decision**, **Alternatives**
(what else was on the table and why it lost) and **Consequences** (what we gain and
what we give up). Records aren't edited once accepted. When a decision changes, a new
record replaces the old one, and the old one is marked *Superseded*.

| # | Decision | Status |
|---|---|---|
| [0001](0001-go-with-a-python-optimizer.md) | Go for the services, Python only for the route optimizer | Accepted |
| [0002](0002-modular-monolith.md) | A modular monolith, not microservices | Accepted |
| [0003](0003-contract-first-openapi.md) | Contract-first OpenAPI, checked by tests | Accepted |
| [0004](0004-redis-fail-open-cache.md) | Redis as a fail-open cache, with ETag/304 | Accepted |
| [0005](0005-money-in-kurus.md) | Money as whole kuruş | Accepted |
| [0006](0006-uuidv7-ids.md) | UUIDv7 for every ID | Accepted |
| [0007](0007-rate-limiting.md) | Login rate limits: slow down, never lock | Accepted |
| [0008](0008-payments-find-out-never-charge-again.md) | Payments: a timeout is "unknown", found out later, never charged again | Accepted |
| [0009](0009-payment-webhooks.md) | Payment webhooks: signed, sent at least once, applied by a ratchet | Accepted |

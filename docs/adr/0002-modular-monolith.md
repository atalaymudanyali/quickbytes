# 0002. A modular monolith, not microservices

**Status:** Accepted · recorded 2026-10-22 (end of phase 1)

## Context

A food delivery system has many areas: users, restaurants, menus, orders, payments,
dispatch, locations. Big companies split these into many small services
(microservices). That lets separate teams ship separately, but every call between
services becomes a network call that can fail, and every service needs its own
deployment and monitoring. QuickBytes has one developer.

## Decision

The API is **one program with clearly separated modules** inside it. Each area has
its own package (`internal/users`, `internal/restaurants`, `internal/menus`,
`internal/ratelimit`, …). Modules talk through Go interfaces, not by reaching into
each other's tables. Compare it to one building with separate, labelled rooms, not
a street of separate buildings.

A part becomes a separate service only when it has a concrete reason:

| Service | Reason |
|---|---|
| Mock payment provider | It plays an outside company, so it must be reached over HTTP with webhooks, like a real one. |
| Location ingest | Very different traffic (many tiny pings per second, TCP and UDP). |
| Route optimizer | Another language ([0001](0001-go-with-a-python-optimizer.md)). |

## Alternatives

| Option | Why not |
|---|---|
| Microservices from day one | Network failures, deployments and tracing for every module, with no team that needs the independence. |
| One big package | Fast at first, but areas tangle together and can't be split out later. |

## Consequences

- One deploy, one database, and real transactions across modules (an order and its
  payment state can change together).
- Clear module edges keep a later split possible. The interfaces are where the
  network calls would go.
- Scaling is all or nothing for the API. That's acceptable at this size, and the
  hot paths (menus, near-me search) are covered by caching and indexes instead.

# QuickBytes

A food delivery platform built end to end, set in Istanbul: customers order from
restaurants, payments are checked by a mock payment provider, and a dispatch system
groups orders into trips for drivers and plans the fastest route through real
Istanbul streets.

One iPhone app holds all three roles (customer, restaurant, driver), switched from a
side menu.

> **Status:** early development (phase 1 of 7: foundation).

## What it shows

- An order moving through a real backend: cart → payment → restaurant → driver → door.
- Mock payments that behave like a real provider: approvals, declines, timeouts,
  and notifications that can arrive twice without charging anyone twice.
- Dispatch that batches several orders into one driver trip, with routes planned on
  real OpenStreetMap road data.
- System design you can measure: indexes, partitioning, caching and load tests, with
  the numbers published here.

## Tech

Go · Python (route optimizer) · PostgreSQL + PostGIS · Redis · Redpanda (Kafka API) ·
OSRM · SwiftUI · Docker Compose · Prometheus + Grafana · GitHub Actions

Everything runs locally and costs nothing: no paid APIs or keys.

## Run it

Needs Docker and Go 1.27+.

```bash
make up        # start PostgreSQL + the API in Docker
curl localhost:8080/readyz
make test      # run all tests (starts the database if needed)
make down      # stop everything
```

| Endpoint | Answers |
|---|---|
| `GET /healthz` | Is the process alive? Never checks dependencies. |
| `GET /readyz` | Can this copy serve traffic? Returns 503 if the database is unreachable. |

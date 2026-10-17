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

| Endpoint | Does |
|---|---|
| `GET /healthz` | Is the process alive? Never checks dependencies. |
| `GET /readyz` | Can this copy serve traffic? Returns 503 if the database is unreachable. |
| `POST /auth/signup` | Create an account (with a customer profile) and log in. |
| `POST /auth/login` | Log in: returns a 15-minute access token and a refresh token. |
| `POST /auth/refresh` | Swap a refresh token for a new pair. Each refresh token works once. |
| `POST /auth/logout` | Revoke the refresh token and all tokens issued from the same login. |
| `GET /me` | The signed-in user and their profiles. Needs `Authorization: Bearer <token>`. |
| `POST /me/profiles` | Add a `restaurant` or `driver` profile. Refresh afterwards to use it. |

Errors always look like
`{"error": {"code": "...", "message": "...", "request_id": "..."}}`, and every
response carries an `X-Request-ID` header that matches the server's log line.

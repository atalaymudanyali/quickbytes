# 0001. Go for the services, Python only for the route optimizer

**Status:** Accepted · recorded 2026-10-22 (end of phase 1)

## Context

QuickBytes needs an API, a mock payment provider, a dispatch worker, a location
ingest service and a driver simulator. Most of them spend their time waiting on the
network and juggling many connections at once, like thousands of drivers sending
their positions. One piece is different: choosing the best order of stops for a
trip (the *vehicle routing problem*) is a hard math problem, and good solvers are
rare.

## Decision

- **Go** for every service. It handles many tasks at once cheaply (goroutines),
  compiles to one small file that fits easily in Docker, and is widely used for
  backend and infrastructure work, including at Uber.
- **Python + Google OR-Tools** for the route optimizer only. It runs as its own
  small service, and the dispatch worker calls it over gRPC.

## Alternatives

| Option | Why not |
|---|---|
| Everything in Go | OR-Tools has no official Go support. Writing a routing solver by hand would be a project of its own. |
| Everything in Python | Weaker at serving many connections at once, and a less common choice for the backend roles this project targets. |
| A paid routing API | The project has a "nothing paid" rule. |

## Consequences

- One main language to learn and test, and one toolchain for CI.
- A second language and a gRPC contract appear in phase 4. That's a real cost, but
  it's limited to one small service with one job.
- The optimizer can be swapped later (another solver or language) without touching
  the Go services, as long as the gRPC contract stays the same.

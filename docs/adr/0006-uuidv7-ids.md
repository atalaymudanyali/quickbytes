# 0006. UUIDv7 for every ID

**Status:** Accepted · recorded 2026-10-22 (end of phase 1)

## Context

Every row needs an ID that also appears in URLs (`/restaurants/{id}`). Counting
numbers (1, 2, 3…) leak information: anyone can guess the next ID, and the newest
one tells a competitor how many users or orders exist. Random IDs (UUIDv4) fix that,
but they land at random places in the database index. That's like filing new folders
in random drawers instead of at the back of the cabinet, so the index gets slower
and larger as it grows.

## Decision

**UUIDv7** for every primary key, generated in Go (`uuid.NewV7()`) and stored in
PostgreSQL's native `uuid` type. A UUIDv7 starts with a millisecond timestamp,
followed by random bits:

- new IDs sort roughly by creation time, so they are added near the end of the index
  like counting numbers;
- the random part makes them impossible to guess or count.

## Alternatives

| Option | Why not |
|---|---|
| Auto-increment (`bigserial`) | Guessable, and reveals volumes. Also, IDs only exist after the insert. |
| UUIDv4 | Unguessable, but random inserts spread across the index. |
| ULID / Snowflake IDs | Similar idea, but not the standard `uuid` type, so they need custom handling in every layer. |

## Consequences

- IDs can be created before touching the database, which helps with batches and,
  later, with events in the outbox.
- 16 bytes per ID instead of 8. That's fine at this scale.
- The timestamp shows roughly when a row was created. That's acceptable here.
  Nothing secret is encoded in it.

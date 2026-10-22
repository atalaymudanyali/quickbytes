# 0003. Contract-first OpenAPI, checked by tests

**Status:** Accepted · recorded 2026-10-22 (end of phase 1)

## Context

The API needs a written contract: for people reading it (Swagger UI at `/docs`), and
for the iPhone app in phase 6, whose networking code Apple's
`swift-openapi-generator` will create from it. A contract that drifts from the code
is worse than none, because it lies with confidence.

There are two ways to get one. **Code-first** means generating the spec from typed
code. **Contract-first** means writing the spec by hand, like an architect's
blueprint, and making the code match it.

## Decision

**Contract-first.** The spec is hand-written in
[`api/openapi.yaml`](../../api/openapi.yaml) (OpenAPI 3.0.3), and the handlers stay
plain Go `func(w, r)`. Tests keep the two in sync:

- every served route is in the spec, and every route in the spec is served;
- every request and response in the HTTP tests is validated against the spec
  (with kin-openapi).

Swagger UI is embedded in the binary and its checksum is verified, so `/docs` works
offline and makes no third-party calls.

## Alternatives

| Option | Why not |
|---|---|
| **Huma** (a Go framework with typed handlers that generates the spec) | A good choice. But it meant rewriting all 21 handlers mid-project. **Starting fresh and wanting a generated spec, I'd pick Huma.** |
| `swaggo` comment annotations | Just as manual as YAML, clutters every handler, and drifts without anyone noticing. |
| Checking only that routes match | A response's shape could change without the spec noticing. |

## Consequences

- Writing the spec first forces the API to be designed on purpose: names, error
  codes, pagination.
- Each endpoint is described twice (code and YAML). The tests make that safe rather
  than fragile: a mismatch fails CI.
- The phase 6 iPhone client can be generated with confidence.

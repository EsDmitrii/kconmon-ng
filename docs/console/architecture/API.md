<!--
Status: current
Owner: @EsDmitrii
Source: extracted from root DESIGN.md §8 in M0 (2026-07-14); "Implemented in M3"
added from the as-built implementation (2026-08-06): internal/console/httpapi/{events,runs,audit,rbac,tokens,auth}.go.
This document is the source of truth for Console API. Update it (and the ADRs) in the same PR as any deviation.
-->

# Console API

## 8. Console API

REST under `/api/v1/*`, JSON, cursor pagination, RFC 7807 errors, durations
in **nanoseconds** (consistent with controller). The OpenAPI spec landed in
M4 as `docs/console-api.yaml`, with TypeScript **types** generated from it
into `web/src/lib/api-types.ts` — see "OpenAPI + codegen (landed M4)" below
for what shipped and what deliberately did not.

```
GET    /api/v1/topology?at=                  live or reconstructed @t        -- implemented (M1): live only, no ?at= yet
GET    /api/v1/matrix?protocol&plane&at=                                     -- implemented (M1): protocol=tcp|udp|icmp, plane=pod only, no ?at= yet
GET    /api/v1/events?filters&from&to        history for Live scrollback      -- implemented (M3)
POST   /api/v1/runs        GET /runs/{id}    diagnostics fan-out + results    -- implemented (M3)
GET    /api/v1/runs                          run history, paged               -- implemented (M3)
POST   /api/v1/runs/{id}/cancel              cancel an in-flight run          -- implemented (M4)
CRUD   /api/v1/targets|checks|schedules      external targets + saved specs   -- implemented (M4)
POST   /api/v1/checks/projection             cardinality preview              -- implemented (M4)
GET    /api/v1/mtr/paths?src&dst&from&to     ; GET /mtr/paths/diff?a&b
POST   /api/v1/investigations                assemble; GET result; save→incident
CRUD   /api/v1/incidents|annotations|maintenance|webhooks
CRUD   /api/v1/alert-rules (+ /{id}/preview, /{id}/sync)
POST   /api/v1/promql/query|query_range      guarded proxy                   -- implemented (M1)
GET    /api/v1/export      POST /api/v1/import
CRUD   /api/v1/rbac/roles|bindings                                           -- implemented (M3)
CRUD   /api/v1/tokens                                                        -- implemented (M3)
GET    /api/v1/audit                                                         -- implemented (M3)
POST   /api/v1/auth/login|logout ; GET /api/v1/auth/me|oidc/start|oidc/callback -- implemented (M3)
GET    /ws                                   authenticated WebSocket          -- implemented (M2, extended M3): see WEBSOCKET.md
```

`?at=` (Time Machine — historical topology/matrix reconstruction) is not
implemented yet; it lands with M5. `targets`/`checks`/`schedules` shipped in
M4 and are **not** on this list any more; every one of them needs
`database.mode=cnpg|external` and answers `503` otherwise (Decision 13), the
same way the M3 history endpoints do. `mtr/paths`, `investigations`,
`incidents`/`annotations`/`maintenance`/`webhooks`, `alert-rules`, and
`export`/`import` remain entirely unimplemented past M4.

WebSocket: single multiplexed socket at `/ws`, topic subscribe, messages
`{"topic","type":"snapshot|delta|event|error|closed","seq","data"}`, ping/pong
30s, resume by last-seen `seq` per topic. Implemented in M2 for the topics
`live`, `topology` and `matrix:{tcp,udp,icmp}:pod`; `run:{id}` is implemented
in M3 (ephemeral, opened per run — see WEBSOCKET.md); `mtr` is still deferred
to M5 and rejected with an error frame until then. Replay is a per-replica
in-memory ring, not a Valkey log; snapshot topics resubscribe without a resume
cursor and take a fresh whole state; and `delta` frames are not produced yet. The
full protocol — envelope, allowlist, sequence semantics, limits, origin check,
payloads — is [WEBSOCKET.md](WEBSOCKET.md). Pages keep their REST polling path as
the automatic fallback (see [FRONTEND.md](FRONTEND.md) "Data layer").

## Implemented in M1 (`internal/console/httpapi`)

M1 ships exactly four data endpoints, plus `/healthz`, `/readyz`, `/metrics`,
`/api/v1/version`, `/api/v1/config` (not detailed here). Every data endpoint
below answers `503 application/problem+json` when its upstream is
unconfigured — `controller.url` empty for topology, `prometheus.url` empty
for matrix/promql (Helm: `console.controller.url` / `console.prometheus.url`,
both empty by default).

### `GET /api/v1/topology`

Proxies the controller's own `GET /api/v1/topology`
(`internal/console/controllerclient`), live only. A non-leader controller
reply (`503`) is retried up to 3 times with exponential backoff; if every
attempt still 503s, the console answers `502 Bad Gateway`
(`controllerclient.ErrUnavailable`). Response body is the controller's
snapshot verbatim (`docs/api.md`):

```json
{
  "nodes": [{"name": "node-a", "zone": "zone-a", "ready": true}],
  "agents": [{"id": "...", "nodeName": "node-a", "podIP": "10.0.0.1", "zone": "zone-a"}],
  "timestamp": "2026-07-14T00:00:00Z"
}
```

### `GET /api/v1/matrix?protocol&plane`

`protocol` ∈ `tcp|udp|icmp` (default `tcp`; anything else → `400`). `plane`
accepts only `pod` (default; anything else → `400 "only plane=pod exists in
M1"`). Recomputed from Prometheus on every request
(`internal/console/matrix`) — no caching, no persistence. Fail ratio and RTT
p95 are 5m-rate-window queries; loss ratio (udp/icmp only) is an instant
average. A pair with no matching series gets a `null` field rather than a
zero.

```json
{
  "protocol": "tcp",
  "plane": "pod",
  "nodes": ["node-a", "node-b"],
  "cells": [
    {"source": "node-a", "destination": "node-b", "failRatio": 0, "rttP95": 1234000, "lossRatio": null}
  ],
  "timestamp": "2026-07-14T00:00:00Z"
}
```

`rttP95` is integer **nanoseconds** (repo-wide duration convention).

### `POST /api/v1/promql/query` and `POST /api/v1/promql/query_range`

Guarded passthrough to Prometheus's own `/api/v1/query` /
`/api/v1/query_range` (`internal/console/promql`). The response envelope —
success or Prometheus's own 4xx/5xx error body (e.g. a PromQL parse error) —
is forwarded byte-for-byte, never re-shaped.

Request bodies:

```json
// POST /api/v1/promql/query
{"query": "up", "time": "2026-07-14T00:00:00Z"}    // "time" optional (RFC3339); omitted = "now"

// POST /api/v1/promql/query_range
{"query": "up", "start": "2026-07-14T00:00:00Z", "end": "2026-07-14T01:00:00Z", "step": 15000000000}
```

`step` is an integer in **nanoseconds** in the request JSON (repo-wide
duration convention — note this differs from Prometheus's own HTTP API,
which takes `step` as seconds; the console client converts).

Guards (`internal/console/config` defaults; Helm `console.prometheus.*`):

| Guard | Default | On violation |
| --- | --- | --- |
| `queryTimeout` | 30s | HTTP client timeout to Prometheus (surfaces as `502`) |
| `maxRange` (`end - start`) | 24h | `422` with title "range exceeds maximum" (`promql.ErrRangeTooLarge`) |
| `maxResponseBytes` | 8 MiB | `422` with title "result too large" (`promql.ErrResponseTooLarge`) |
| bad `step`/range (`step<=0` or `end<=start`) or malformed JSON body | — | `400` (`promql.ErrBadRequest`) |

### Error format

Console-originated errors (as opposed to a forwarded Prometheus error body)
are RFC 7807 `application/problem+json`:

```json
{"type": "about:blank", "title": "prometheus not configured", "status": 503, "detail": "set prometheus.url in the console config (Helm: console.prometheus.url)"}
```

`type` is always `"about:blank"` in M1 (no per-error-type documentation URIs
yet).

## Implemented in M2

### `GET /ws`

The multiplexed WebSocket, registered at the **top level** (not under
`/api/v1/`). Rejected before the upgrade when `Origin` is present and
cross-origin (gorilla answers `403`); an absent `Origin` is allowed, so
non-browser clients still work. Protocol: [WEBSOCKET.md](WEBSOCKET.md).

The handler answers `503 application/problem+json` when the console was built
without a hub, matching how the M1 data endpoints answer for an unconfigured
upstream. In practice `cmd/console` constructs the hub unconditionally — snapshot
topics do not depend on the event pipeline — so that branch is reachable only for
an embedder that passes `nil`.

Two metrics traps come from the hijack, both documented in
[WEBSOCKET.md](WEBSOCKET.md) "Metrics": a successful upgrade records
`status="200"` (not 101), and the duration histogram for `path="/ws"` measures
connection lifetime.

### `GET /api/v1/version` — `capabilities` is now dynamic

```json
{"version": "1.5.0", "commit": "…", "capabilities": ["events"]}
```

(`version` is the console **binary's** version — `internal/version`, set at
build time from the git tag — not the Helm chart version; the two happen to
move together by convention, but this field is not a chart-version probe and
the example above is illustrative, not a promise that this string matches
any particular release.)

`capabilities` contains `"events"` only while **this replica's** event ingester
holds a proven gRPC stream to a controller that itself advertises `events` on its
own `/api/v1/version`; otherwise the array is empty (`[]`, never `null` — the
frontend indexes into it). It is computed per request from live pipeline health,
not from config, so a stream that drops mid-session flips it back within one
poll. The browser polls this endpoint every 15 s and uses it for feature
detection — never version sniffing.

Scope caveat, deliberate: the flag speaks for the local ingester only. A replica
whose own stream is down can still fan out events other replicas published to
Valkey while advertising nothing here, so the browser falls back conservatively.
See [WEBSOCKET.md](WEBSOCKET.md) "Capability detection and fallback".

`GET /api/v1/events` (history for Live scrollback) is implemented in M3 —
see "Implemented in M3" below.

### `GET /api/v1/matrix` and `GET /api/v1/topology` in M2

Both endpoints are unchanged and still the first-paint source. The same payloads
are now additionally pushed over the WebSocket (`matrix:{protocol}:pod`,
`topology`) by server-side timers every 15 s, so a Matrix page with a live socket
stops polling; a page without one keeps its M1 interval and says so with a
"Delayed data" badge. The Topology page still polls in M2 — the `topology` topic
is produced but has no browser consumer yet.

## Implemented in M3

Every route below requires database.mode != disabled (503 otherwise, the
same convention `handleTopology`/`handleMatrix` use for an unconfigured
upstream). See [SECURITY.md](SECURITY.md) §10–§11 for the auth/RBAC/CSRF
rules gating these routes and audit.go for what gets written to
`GET /api/v1/audit`.

### `GET /api/v1/events`

History for Live scrollback, backed by `topology_events` (DATA.md §5.2).
Newest first, opaque keyset cursor, `?type=` (repeatable, one of the five
known event types — WEBSOCKET.md "Payloads" — else `400`), `?scope=` (exact
match), `?from=`/`?to=` (RFC3339, `from` must precede `to`), `?limit=`
(clamped into `[1,500]`, default 100). Response body is the same
`LiveEvent` shape the `live` WebSocket topic uses, so the frontend reuses one
type for both:

```json
{"events": [{"id": "17-1753400000000000000", "seq": 17, "type": "check_observed", "severity": "error", "scope": "node-a→node-b", "timestamp": "2026-07-25T12:00:00Z", "summary": "tcp check node-a→node-b failed", "details": {"...": "..."}}], "nextCursor": "..."}
```

`nextCursor` is set only when the page came back exactly as full as
requested (a short page proves nothing older is left).

### `POST /api/v1/runs`

Starts a diagnostics run: fan-out over `sources`×`destinations` (full mesh
or one-sided when either is empty — resolved against live topology),
bounded to **400 pairs** (`checks.maxPairs`) after self-pair exclusion. A
malformed body or unknown `type` is `400`; a well-formed spec refused for
what it *would* produce (too many pairs, no pairs, no nodes) is `422`, never
`400` — mirroring the PromQL guards' own distinction. Answers `202
Accepted` with a `Location` header and the WebSocket topic name to
subscribe to:

```json
// request
{"sources": ["node-a"], "destinations": ["node-b", "node-c"], "type": "tcp", "plane": "pod", "timeoutNs": 5000000000}

// 202 response
{"id": "b8f9...", "status": "pending", "pairTotal": 2, "wsTopic": "run:b8f9..."}
```

**M4 added an external destination** via three optional fields —
`destinationKind`, `destinationTargetId`, `destinationAddress` — using the
same kind vocabulary as `check_definitions`, so an operator who has written a
check definition already knows the shape:

- `destinationKind` absent or `"node"` is the M3 contract, byte-identical:
  `destinations` holds node names. Sending either external field with this
  kind is `400`, not silently ignored.
- `"target"` resolves a saved `targets` row named by `destinationTargetId`
  (a UUID). A non-UUID is `400`; a UUID naming no row is `422`; targets
  unavailable (`database.mode=disabled`) is `503`.
- `"adhoc"` probes the operator-typed `destinationAddress` with no stored row.
- Both external kinds require `destinations` to be **empty**: one run probes
  either the mesh or one external destination, never a mix, and the mixed body
  is refused rather than half-honored.

Both external kinds reach the controller as `destinationKind=external`. The
audit log records `destinationKind` but **never** `destinationAddress` — the
address is the operator's data, not an audit field.

### `POST /api/v1/runs/{id}/cancel`

Stops a run in flight. `204 No Content`, no body — and that same `204` also
covers the two outcomes cancellation deliberately treats as non-errors:
cancelling a run that reached a terminal status a moment earlier, and
cancelling one this replica did not start. Neither is a `409`, because an
operator who clicked cancel on a run that just finished did nothing wrong, and
an endpoint answering differently depending on which replica took the request
would be reporting routing rather than run state.

An id naming no run is `404`; no runner (`database.mode=disabled`) is `503`.
Cancellation is **asynchronous** — the run's own goroutine writes the terminal
`cancelled` status once its in-flight pairs settle — so read the outcome from
`GET /api/v1/runs/{id}`, not from this response.

Gated on `runs:create`, not a permission of its own: starting fleet-wide probe
traffic and stopping it are the same operational class, and a role that can
start a 400-pair run must not need a second grant to stop it.

### `GET /api/v1/runs`

Run history, paged, newest first, `?type=`/`?status=` filters, opaque keyset
cursor, `?limit=` (clamped `[1,500]`, default 100):

```json
{"runs": [{"id": "b8f9...", "createdAt": "...", "startedAt": "...", "finishedAt": "...", "status": "succeeded", "type": "tcp", "plane": "pod", "initiatorKind": "user", "initiatorId": "...", "pairTotal": 2, "pairOk": 2, "pairFailed": 0}], "nextCursor": "..."}
```

### `GET /api/v1/runs/{id}`

One run's summary plus its per-pair results (spec snapshot, `404` for an
unknown id):

```json
{"id": "b8f9...", "status": "succeeded", "spec": {"...": "..."}, "results": [{"sourceNode": "node-a", "destinationNode": "node-b", "success": true, "durationNs": 1200000, "recordedAt": "..."}]}
```

Live progress for a run in flight arrives over its `run:{id}` WebSocket
topic (WEBSOCKET.md "Ephemeral `run:{id}` topics"); this endpoint is also
the documented REST-polling fallback when that socket is unavailable
(single-replica limitation — see the same section).

### `GET /api/v1/audit`

Newest-first, opaque keyset cursor, `?subjectKind=`/`?subjectId=` filters,
`?limit=` (clamped `[1,500]`, default 100). `detail` is whatever the
allow-list let through for that route — `{}` for almost everything
(SECURITY.md "The audit log's documented lossiness"):

```json
{"entries": [{"id": 1, "at": "...", "subjectKind": "user", "subjectId": "...", "action": "POST /api/v1/runs", "resource": "", "outcome": "allowed", "remoteAddr": "10.0.0.1:5432", "detail": {"type": "tcp", "plane": "pod"}}], "nextCursor": "..."}
```

### `/api/v1/rbac/*`

- `GET /api/v1/rbac/permissions` — the static `authz.AllPermissions` list;
  needs no database, always available.
- `GET`/`POST /api/v1/rbac/roles`, `DELETE /api/v1/rbac/roles/{name}` — custom
  roles only. `POST` is `422` for a built-in-colliding name or an unknown
  permission string; `DELETE` is `409` while any binding still references
  the role.
- `GET`/`POST /api/v1/rbac/bindings`, `DELETE /api/v1/rbac/bindings/{id}` —
  `POST` is `422` for `subjectKind` outside `user`/`group` (token bindings
  are schema-declared but unresolved — SECURITY.md §10.2), or for an unknown
  role name (built-in ∪ custom); `409` on a duplicate binding.

### `/api/v1/tokens`

- `GET /api/v1/tokens` — every token's metadata; **never** the hash or the
  secret (`store.Token` structurally has no such field).
- `POST /api/v1/tokens` — mints a token; the response is **the only place**
  the raw wire-form token (`kcm_...`) is ever returned, never stored or
  logged again:

  ```json
  {"id": "...", "name": "ci-pipeline", "token": "kcm_AbC...", "expiresAt": "2027-01-01T00:00:00Z"}
  ```

- `DELETE /api/v1/tokens/{id}` — revokes; `404` for an unknown/already-revoked id.

### `/api/v1/auth/*`

- `GET /api/v1/auth/me` — public route, subject-dependent answer: the
  resolved `{kind, id, displayName, groups, roles}` plus the caller's
  effective `permissions`; `401` if nothing resolved (never true for
  `auth.mode=anonymous`).
- `POST /api/v1/auth/login` — `auth.mode=local` only; `404` in every other
  mode (feature detection, not an error). Sets the session + CSRF cookies on
  success.
- `POST /api/v1/auth/logout` — mode-agnostic, idempotent, always `204`;
  deletes the session server-side (instant revocation) when one exists.
- `GET /api/v1/auth/oidc/start`, `GET /api/v1/auth/oidc/callback` —
  `auth.mode=oidc` only; `404` otherwise. `?returnTo=` must be a
  same-origin relative path (`400` otherwise).

## OpenAPI + codegen (landed M4)

Deferred out of M3 (Decision 12) and delivered in M4, but **not in the shape
the M3 note predicted**. What actually shipped:

- **Spec-first, hand-authored.** `docs/console-api.yaml` is written by hand
  and is the source of truth. It is not generated from the Go handlers —
  generating it from code would have made the spec a description of whatever
  the handlers currently do, which cannot catch a handler that does the wrong
  thing.
- **Types only, no client.** `make openapi` runs `openapi-typescript` over the
  spec into `web/src/lib/api-types.ts`, which is committed exactly like
  `api/proto/*.pb.go` and `internal/console/store/gen/`. CI regenerates and
  fails on a diff, so a spec edit without a regenerate cannot land. There is
  **no generated runtime client**: the fetch layer stays hand-written, because
  the value was in the types agreeing with the wire, not in replacing a
  fetch wrapper that already works.
- **A router-walking gate.** `internal/console/httpapi/openapi_test.go` walks
  the live chi router and joins it against the spec's `paths` in both
  directions, keyed on `METHOD /path` so a documented path that lost one of
  its verbs is drift too. A route added without a spec entry fails the test,
  and a spec entry naming no route fails it as well — which is the half that
  matters, since documentation drifts by describing endpoints that quietly
  went away. `/healthz`, `/readyz`, `/metrics` and `/ws` are deliberately
  outside the spec (probe/scrape endpoints, and a protocol documented in
  WEBSOCKET.md rather than a REST path), so only `/api/v1/*` is required to
  appear.

The generator is pinned by `web/package-lock.json` rather than by a
`go run tool@version` line, which is the same guarantee by another means. It
needs Node and an installed `web/` tree (`cd web && npm ci`).

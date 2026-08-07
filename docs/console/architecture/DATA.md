<!--
Status: draft
Owner: @EsDmitrii
Source: extracted from root DESIGN.md §5 in M0 (2026-07-14); §5.2/§5.3 updated
from the as-built M3 implementation (2026-08-06):
internal/console/store/migrations/, internal/console/store/events.go,
internal/console/authn/session.go.
This document is the source of truth for Data Architecture. Update it (and the ADRs) in the same PR as any deviation.
-->

# Data Architecture

## 5. Data architecture

Hard rule: **metrics live only in Prometheus.**

### 5.1 Prometheus (read-only)

Queried via `/api/v1/query`, `/query_range`, `/series`, `/labels` with
configurable base URL / auth / TLS (Thanos/Mimir/VM compatible). Consumers:
Explore, matrix history, object cards, Investigation, Time Machine, alert
rule preview. New external-check metrics are exported by agents through the
existing pipeline with `target` / `target_kind="external"` labels.

### 5.2 PostgreSQL (via CloudNativePG)

| Table                  | Purpose | Status |
| ---------------------- | ------- | ------ |
| `users`                | Local users (`auth.mode=local` only); OIDC users are virtual | **Landed M3** |
| `roles`, `role_bindings` | RBAC (§10.2); subjects: users, OIDC groups, tokens (`role_bindings.subject_kind` schema-declares `token` but nothing resolves it yet — SECURITY.md §10.2) | **Landed M3** |
| `api_tokens`           | Hashed PATs (SHA-256, Decision 11) with owner + expiry | **Landed M3** |
| `audit_log`            | Every mutation: who, what, when, from where, an allow-listed detail | **Landed M3** |
| `check_runs`           | Every diagnostics run: type/plane, spec snapshot, status, timings, initiator, pair counters | **Landed M3** |
| `check_results`        | One row per (run, source, destination): success, duration, error, the full `CheckResult` JSONB (incl. MTR hops) | **Landed M3** |
| `topology_events`      | Persisted controller events, all five types (see below) — Time Machine's future topology source | **Landed M3** |
| `targets`              | External targets: name, kind (host/url), address, labels | pending (M4) |
| `check_definitions`    | Saved specs: source selector, destination (node/target/adhoc), type, plane, params | pending (M4) |
| `check_schedules`      | one-shot / cron / interval / continuous bindings | pending (M4) |
| `mtr_path_snapshots`   | Normalized hop lists per (source, destination), content-hashed — powers path diff and "when did the route change?" | pending (M5) |
| `mtr_hop_enrichment`   | Cache: ip → {rdns, asn, geo, provider}, TTL'd (§7.5) | pending (M5) |
| `k8s_events`           | Filtered, retained copy of relevant K8s events (node/pod in scope) for post-hoc investigation (K8s only keeps ~1h) | pending (M6) |
| `alert_rules`          | Builder model (JSONB) + rendered PromQL + sync status | pending (M7) |
| `incidents`            | Investigation sessions: scope, ranges, pinned findings, notes, status | pending (M6) |
| `annotations`          | Notes pinned to time ranges, shown on charts | pending (M5) |
| `maintenance_windows`  | Scope + schedule; UI suppression + (optional) AM silences | pending (M6) |
| `webhooks`             | Outbound endpoints + event filters + secret | pending (M6) |
| `layouts`              | Saved topology layouts (per-user/global), pinned pairs | pending |
| `settings`             | Versioned Console settings (retention, defaults) | pending |

`topology_events` holds **all five** WebSocket event types (`topology_changed`,
`check_observed`, `mtr_triggered`, `mtr_completed`, `diagnostic_progress` —
WEBSOCKET.md "Payloads"), not only topology ones: `events.Ingester` writes
every event it receives to the same table `GET /api/v1/events` reads back
(the Live page's scrollback), and splitting a single already-ordered stream
across two tables by type would buy nothing — every consumer of "what
happened, in order" wants the whole stream, not a topology-only slice. Its
natural key is **`(event_seq, event_time)`**, the same pair the WebSocket
hub's `live` dedupe already uses (`LiveEvent.ID`, `"<seq>-<unixNano>"`,
`internal/console/events/live_event.go`) split into two typed columns: every
console replica ingests the same controller stream and writes the same row,
so `UNIQUE (event_seq, event_time)` plus `ON CONFLICT DO NOTHING` makes
N-replica ingestion idempotent instead of N-fold duplicated. Persistence
inherits exactly the hub's dedupe guarantee, no better, no worse.

Retention: `check_runs`/`check_results`/`topology_events`/`audit_log` are
pruned by a background job (defaults 90d, Helm-configurable via
`console.database.retentionDays`; 0 disables pruning). Partition
`check_results` by month if volume warrants it — not done in M3
(MILESTONES.md "Deferred out of M3").

Provisioning: chart takes **CNPG as optional dependency**; when enabled it
renders a `Cluster` CR (+ optional `ScheduledBackup`). External DSN
alternative. See §11.

### 5.3 Valkey (ephemeral only — flush must lose zero data)

- **Pub/sub** (M2, implemented): a single channel, `events:live`, fans
  controller domain events to every Console replica for WebSocket delivery.
  Channel names are `events:` + the WebSocket topic name, and the whole
  namespace is consumed by one `PSUBSCRIBE events:*` per replica, so N logical
  topics share one server-side subscription. Snapshot topics (`topology`,
  `matrix:*`) deliberately do **not** use pub/sub — each replica computes and
  broadcasts its own snapshots (see [WEBSOCKET.md](WEBSOCKET.md) "The
  deliberate asymmetry").
- **Live caches**: matrix/topology snapshots, `run:{id}:status`, short TTL.
  *Not implemented* — the pushers recompute on a 15 s timer and the
  latest snapshot lives in the Hub's per-topic replay ring instead.
- **PromQL response cache**: keyed (query, range, step), 15–60s TTL.
  *Not implemented.*
- **Sessions**: `sess:{id}` — instant revocation. **Landed in M3**, via a new
  seam, `cache.KV` (`internal/console/cache/kv.go`: `Get`/`Set`/`Delete`),
  *not* the pub/sub `Bus` §5.3 otherwise documents — a session store needs
  read-your-write key/value semantics a broadcast bus does not provide.
  `authn.SessionStore` (`internal/console/authn/session.go`) is the only
  consumer: `Create` stores a JSON-encoded `Session` under `sess:{id}` with a
  TTL derived from the session's own `ExpiresAt`, `Delete` gives instant
  revocation regardless of backend, and `Get`/`Refresh` re-check `ExpiresAt`
  independently of the KV entry's own TTL (belt and braces). `cache.KV` has
  both a `ValkeyKV` and an `InProcessKV` implementation, mirroring `Bus`'s own
  two backends and inheriting the same single-replica caveat when
  `console.valkey.mode=disabled`.
- **Locks/queues**: scheduler singleton ticks (`SET NX PX`), per-user
  diagnostics rate limits, background-job queue. Still *not implemented* — M4,
  with the scheduler.

So the Console's use of Valkey is two independent seams on the same client:
pub/sub (`Bus`: `Publish`/`Subscribe`, no liveness API by design) for
cross-replica event fan-out, and key/value (`KV`: `Get`/`Set`/`Delete`) for
sessions. Both are `rueidis`-backed (`internal/console/cache`) with an
in-process fallback for `console.valkey.mode=disabled`.

Helm: bundled single-replica Valkey (`console.valkey.mode=bundled`, an ephemeral
Deployment with **no PersistentVolumeClaim** — per ADR-002 a flush must lose zero
durable data, so losing the instance on a Pod restart is a liveness event, never
a data-loss one), external (`mode=external` + `address`), or disabled
(`mode=disabled`). If disabled the Console runs on `cache.InProcessBus`, which
has no cross-replica delivery and is therefore correct only for
`console.replicas=1`; the chart fails rendering when a resolved controller gRPC
address is combined with `mode=disabled` and more than one replica. A Valkey that
cannot be dialled at startup degrades to the same in-process bus with a warning
rather than blocking boot — and stays there until the console restarts.

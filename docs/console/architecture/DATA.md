<!--
Status: draft
Owner: @EsDmitrii
Source: extracted from root DESIGN.md §5 in M0 (2026-07-14); §5.2/§5.3 updated
from the as-built M3 implementation (2026-08-06):
internal/console/store/migrations/, internal/console/store/events.go,
internal/console/authn/session.go. §5.2's three M5 tables, the topology fold
and the retention sweeps written from the as-built M5 implementation
(2026-08-08): migrations/00005_mtr_timemachine.sql, store/prune.go,
store/events.go (TopologyAt).
This document is the source of truth for Data Architecture. Update it (and the ADRs) in the same PR as any deviation.
-->

# Data Architecture

## 5. Data architecture

Hard rule: **metrics live only in Prometheus.**

### 5.1 Prometheus (read-only)

Queried via `/api/v1/query`, `/query_range`, `/series`, `/labels` with
configurable base URL / auth / TLS (Thanos/Mimir/VM compatible). Consumers:
Explore, matrix history, object cards, Investigation, Time Machine, alert
rule preview. External-check metrics (M4) are exported by agents as a **new
metric family**, `kconmon_ng_external_*` (Decision 6) — not as `target` labels
bolted onto the peer pipeline. The peer families keep their exact label set
(`source_node`, `destination_node`, `source_zone`, `destination_zone`); the
external families carry `source_node`, `source_zone`, `target`, `target_kind`
instead, where `target` is the operator's NAME for the destination and
`target_kind` is the closed set `host|url`. Two families rather than one
widened family is what keeps every 1.5.0 dashboard and recording rule valid
and keeps a peer query from silently picking up external series. See
[metrics.md](../../metrics.md) "Agent — External".

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
| `targets`              | External targets: `name` (unique, 1–63 chars), `kind` (`host`/`url`), `address`, `labels` JSONB | **Landed M4** |
| `check_definitions`    | Saved specs: `source_selection` (`all`/`per-zone`/`one-per-zone`), `destination_kind` (`node`/`target`/`adhoc`) + `destination_target_id`/`destination_address`, `check_type`, `plane`, `params` JSONB, `enabled` | **Landed M4** |
| `check_schedules`      | `definition_id` + `kind` (`once`/`interval`/`continuous`), `interval_ns`, `run_at`, `enabled`, `last_fired_at`/`next_fire_at` | **Landed M4** |
| `mtr_path_snapshots`   | One row per DISTINCT route a pair has taken: `source_node`, `destination`, `path_hash`, `hop_count`, `hops` JSONB, `first_seen`, `last_seen`, `trace_count`, `run_id` (FK → `check_runs`, `ON DELETE SET NULL`); `UNIQUE (source_node, destination, path_hash)` | **Landed M5** |
| `mtr_hop_enrichment`   | TTL cache keyed by address: `ip` PK, `rdns`, `asn`, `provider`, `geo` JSONB, `resolved_at` | **Landed M5** |
| `annotations`          | Operator notes: `id`, `start_at`, `end_at` (NULL = instant mark), `scope` (`''` = global), `text`, `created_by`, `created_at` | **Landed M5** |
| `k8s_events`           | Filtered copy of K8s events for post-hoc investigation (K8s keeps ~1h): `uid`, `resource_ver`, `event_time`, `kind` (`Node`/`Pod`), `name`, `namespace` (`''` for node events), `reason`, `type` (`Normal`/`Warning`), `message`, `count`; `UNIQUE (uid, resource_ver)` | **Landed M6** |
| `alert_rules`          | Builder model (JSONB) + rendered PromQL + sync status | pending (M7) |
| `incidents`            | Investigation sessions: `id` UUID, `title`, `scope` (`''` = global), `from_at`, `to_at` (NULL = open-ended), `status` (`open`/`resolved`), `notes`, `pinned` JSONB array, `created_by`, `created_at`, `resolved_at` | **Landed M6** |
| `maintenance_windows`  | Declared change windows: `id` UUID, `scope`, `start_at`, `end_at`, `reason`, `created_by`, `created_at`; `CHECK (end_at > start_at)` | **Landed M6** |
| `webhooks`             | Outbound endpoints: `id` UUID, `name` (UNIQUE), `url`, `events` TEXT[], `secret_enc` BYTEA, `enabled`, `last_status`, `last_attempt`, `failures`, `created_at` | **Landed M6** |
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

**The three M5 tables, as built** (`migrations/00005_mtr_timemachine.sql`):

- `mtr_path_snapshots` is a **projection, never the authority**. The authority
  stays `check_results.result`; the Console's checks runner projects each MTR
  result into a normalized hop list at ingest time (Decision 1), and a
  projection that fails to be written loses history, not data. `path_hash` is a
  hex SHA-256 over the **ordered hop IP list and nothing else** (Decision 2) —
  not RTTs, which jitter on every trace and would make every trace a new path,
  and not hostnames, which are enrichment and would turn a PTR record change
  into a route change. `hops` carries the full payload of the **first** trace at
  that path rather than a running average: one concrete trace is an honest
  sample where an average across weeks is a number nothing ever measured.
  `destination` is a node NAME or a target NAME, never an address, for the same
  reason `targets.name` is. `run_id` is `ON DELETE SET NULL`, not `CASCADE` — a
  path outliving the run that first produced it is the entire point of keeping
  path history.
- `mtr_hop_enrichment` is a **TTL cache**, not a source of truth (Decision 4):
  every row is re-derivable from the resolvers that wrote it, so a sweep costs
  at most one lookup. `resolved_at` is the TTL anchor. There is no background
  refresher in M5, so an address nobody reads costs nothing and a stale one
  costs exactly one lookup.
- `annotations` — `end_at NULL` means an **instant mark**, not "still open";
  `scope = ''` is the global scope and is a real value, matched exactly. Neither
  `scope` nor `text` is ever exported as a Prometheus label.

**The four M6 tables, as built** (`migrations/00006_investigation.sql`):

- `k8s_events` is deduped on **`(uid, resource_ver)`**, not on `uid` alone: the
  apiserver bumps `resourceVersion` every time an event's `count` or
  `lastTimestamp` changes, so keying on the UID would either drop the update or
  need a read-modify-write. `ON CONFLICT DO NOTHING` on that pair makes a
  relist — which the reader performs every `resyncInterval` by design — cost one
  rejected INSERT and a `duplicate` counter increment, never a duplicate row.
  `kind` is a **closed** set (`Node`/`Pod`); `type` and `reason` are only
  length-bounded, because core/v1 may add values and rejecting a row for an
  unknown reason would silently lose the very event an operator is looking for.
  A row whose `event_time` is entirely zero is **rejected**, never re-stamped
  with `now()`: fabricating a timestamp puts a wrong point on the timeline.
- `incidents.to_at` NULL means an **open-ended range** — the opposite of
  `annotations.end_at`, where NULL means an instant. The two conventions live in
  the same schema because they answer different questions, and both list queries
  say so in their `coalesce`: incidents use `coalesce(to_at, 'infinity')`.
  `pinned` is a JSONB array of typed refs whose `kind` is the closed vocabulary
  `event | audit | annotation | snapshot | run | k8s` (`store/incidents.go`) —
  an unknown kind is rejected at write time rather than stored as a dangling
  reference nothing can resolve. `notes` is bounded at 16 KiB, `title` at 255.
  Updates are deliberately **narrow** (status, notes, pinned), which is what
  makes the API's `PATCH` exception honest.
- `maintenance_windows` has a real `CHECK (end_at > start_at)` — the only M6
  table with a database-level invariant, because a zero-length or inverted
  window is meaningless rather than merely unusual. `scope = ''` is the global
  scope, matched exactly, the same convention `annotations` uses.
- `webhooks.secret_enc` is **opaque BYTEA to the store**: the AES-GCM
  nonce‖ciphertext is produced and opened by `internal/console/webhooks`, and no
  query, index or constraint looks inside it. `name` is UNIQUE over a restricted
  charset (lowercase alphanumerics and hyphens, ≤64), `url` ≤2048, and `events`
  is a `TEXT[]` over the closed set
  `incident.created|incident.resolved|incident.reopened` with duplicates
  rejected. `last_status`/`last_attempt`/`failures` are the delivery ledger:
  one row per ENDPOINT, updated per delivery — there is no delivery-log table
  (MILESTONES.md "Deferred out of M6").

**Time Machine's topology fold** reads `topology_events` and nothing else
(Decision 6): `GET /api/v1/topology?at=` replays `topology_changed` rows in
`(event_time, id)` order up to the instant asked about. What that fold can
honestly reconstruct is bounded by what the events record, and today the
controller publishes `TopologyChanged` with a **reason only** — no
`node_name`, no `agent_id` — so a fold over events written by this release
counts every one of them as unfoldable and returns an empty node set. The
response says so in numbers (`eventsFolded`, `unfoldableEvents`) rather than
letting an empty array read as "the cluster was empty". `zone` and `podIP`
are never recorded by any event type, so both come back empty on every folded
entry even once attribution lands. See API.md and the M5 carry-forward in
MILESTONES.md.

Retention: `check_runs`/`check_results`/`topology_events`/`audit_log` are
pruned by a background job (defaults 90d, Helm-configurable via
`console.database.retentionDays`; 0 disables pruning), and **M5 added three
sweeps** — `mtr_path_snapshots` by `last_seen`, `mtr_hop_enrichment` by
`resolved_at`, `annotations` by `start_at`. Each ages out on the column that
means "still relevant", not on when the row was written: a route the pair
still takes is current however long ago it was first observed, and a mark ages
out with the data it annotates rather than with when it was typed. The three
are new **closed label values** on
`kconmon_ng_console_retention_deleted_total{table}` (docs/metrics.md).

**M6 added three more sweeps and deliberately not a fourth** — `k8s_events` by
`event_time` (a capture ages out with the window it describes, exactly as
`topology_events` does), `incidents` by `resolved_at`, `maintenance_windows` by
`end_at`. Pruning incidents on `resolved_at` means **an open incident is never
pruned**, and that falls out of SQL rather than out of a special case: an open
incident has a NULL `resolved_at`, and `NULL < cutoff` is NULL, never true, so
no cutoff can select one. An investigation nobody closed is unfinished work,
not stale data, and a retention sweep that deleted it would be closing it on
the operator's behalf. **`webhooks` is not a retention table at all** (pinned by
`TestWebhooksAreNotARetentionTable`): a webhook row is configuration, not
observation — it does not accumulate with time, and its only time column
records when the endpoint was set up rather than when it last mattered. Ageing
rows out of it would silently switch off notifications for a still-wanted
endpoint, and a retention policy is not a deconfiguration policy.
`check_results` has no sweep of its own — `ON DELETE CASCADE` on
`check_results.run_id` makes deleting the run row enough. Partition
`check_results` by month if volume warrants it — still not done
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
- **Rate limits** (M4, implemented): the fixed-window counters behind
  `console.rateLimit.runsPerMinute` and `console.rateLimit.loginPerMinute` are
  ordinary `cache.KV` entries, so the limit is cluster-wide with Valkey and
  per-replica with `console.valkey.mode=disabled` (N replicas admit up to N
  times the rate — weaker than configured, never stronger). Both **fail open**
  when the KV is unreadable (Decision 8: a Valkey outage must not become a
  login outage), counted by
  `kconmon_ng_console_rate_limit_failopen_total`.
- **Locks**: the scheduler singleton is **NOT** a Valkey `SET NX PX` lock. It
  is a **PostgreSQL advisory lock** (Decision 2), and deliberately so: the rows
  the loop reads (`check_schedules`) and the lock guarding them then live in
  the same store and the same transaction boundary, so a replica cannot hold a
  lock in one system while another system's view of the work moved on. It also
  means the loop has exactly one hard dependency (`database.mode=cnpg|external`)
  rather than two. The continuous external-check reconciler shares that same
  lock and the same tick gate. The advisory lock is released per tick, so a
  replica dying mid-tick costs one tick, not a wedged fleet.
- **Background-job queue**: still *not implemented*, and not scheduled to a
  milestone — the scheduler landing on an advisory lock removed the reason
  M4 was going to need one.

So the Console's use of Valkey is two independent seams on the same client:
pub/sub (`Bus`: `Publish`/`Subscribe`, no liveness API by design) for
cross-replica event fan-out, and key/value (`KV`: `Get`/`Set`/`Delete`) for
sessions and rate-limit windows. Both are `rueidis`-backed
(`internal/console/cache`) with an in-process fallback for
`console.valkey.mode=disabled`. Cross-replica **mutual exclusion** is not one
of those seams: it lives in PostgreSQL.

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

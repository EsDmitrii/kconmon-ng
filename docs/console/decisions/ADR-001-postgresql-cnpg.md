# ADR-001 — PostgreSQL via CloudNativePG

- Status: accepted
- Date: 2026-07-14
- Deciders: @EsDmitrii

## Context

Console needs durable storage for configuration and history that Prometheus
must not hold: RBAC, API tokens, external targets, check definitions/schedules,
run results (incl. MTR hop payloads), MTR path snapshots and hop enrichment,
persisted topology events (the basis of Time Machine), a retained copy of
relevant Kubernetes events, alert-rule builder models, incidents, annotations,
maintenance windows, webhooks, saved layouts, audit log, and settings
(DESIGN.md §5.2). Metrics stay in Prometheus (§1.1.4); this store is for
relational, mutable, queryable state — not time series.

Requirements: type-safe access without ORM magic, in-cluster operation, HA and
backups possible, and it must be optional so `console.enabled=false` (and even
the M1 read-only mode) impose no database. It must also not lock us to a hosted
service the chart cannot ship.

## Decision

We will use PostgreSQL as the relational store, accessed with `pgx` + `sqlc`
(generated repositories in `internal/console/store`, the only package touching
`pgx`) and migrations via `goose` (embedded, advisory-locked, run on start).

We will provision it with CloudNativePG (CNPG) as a **condition-gated Helm
dependency** that renders a `Cluster` CR (and optional `ScheduledBackup`) when
`console.database.mode=cnpg`. An external DSN (`database.mode=external`) and a
`disabled` mode (M1 read-only) are first-class alternatives. The CNPG operator
is assumed cluster-wide by default (`installOperator: false`).

## Consequences

### Positive

- Battle-tested relational semantics for audit, RBAC, incidents, and history.
- `sqlc` gives compile-time-checked queries; no ORM runtime surprises.
- CNPG delivers HA, backups, and PITR declaratively, in-cluster, air-gap friendly.
- Optionality keeps the lowest milestones DB-free.

### Negative / trade-offs

- A CRD chicken-and-egg: the CNPG CRDs must exist before the chart's `Cluster`
  CR applies. Documented in packaging (§11); operator install is opt-in.
- Another stateful system to operate when enabled.

### Follow-ups

- Partition `check_results` by month if volume warrants (§5.2). Not done as
  of M3 — see "Status" below.
- Retention job defaults (90d) are Helm-configurable.

## Status: implemented in M3 (2026-08-06)

Built as decided, with the following pins actually used:
`github.com/jackc/pgx/v5 v5.10.0`, `github.com/pressly/goose/v3 v3.27.3`
(both in `go.mod`), and `sqlc v1.31.1` pinned as a dev/CI tool only (not a
Go module dependency — `Makefile`'s `SQLC_VERSION`, invoked via
`go run github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION) generate` so a
locally-installed sqlc of a different version cannot silently drift the
generated code). `internal/console/store` is, as decided, the only package
importing `pgx`.

The CRD chicken-and-egg this ADR predicted is handled exactly as planned:
this chart does **not** install the CNPG operator or its CRDs
(`installOperator` is intentionally absent from `console.database.cnpg.*`),
and `console.database.mode=cnpg` without the operator already present fails
`helm install` outright with Kubernetes' own clear "no matches for kind
Cluster" error — not a partial, confusing render.

Landed tables (migrations 00001–00003): `topology_events`, `users`,
`roles`/`role_bindings`, `api_tokens`, `audit_log`, `check_runs`,
`check_results`. Every other table this ADR's Context lists (`targets`,
`check_definitions`, `check_schedules`, `mtr_path_snapshots`,
`mtr_hop_enrichment`, `k8s_events`, `alert_rules`, `incidents`,
`annotations`, `maintenance_windows`, `webhooks`, `layouts`, `settings`)
remains unbuilt — see DATA.md §5.2 for the per-table milestone pin and
`roadmap/MILESTONES.md`'s M3 deferral list.

`check_results` partitioning (the Follow-up above) is not done: the table is
pruned by the retention job (`console.database.retentionDays`, default 90d)
alone.

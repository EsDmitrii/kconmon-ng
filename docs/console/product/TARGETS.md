<!--
Status: draft
Owner: @EsDmitrii
Source: extracted from root DESIGN.md §7.2–7.3 in M0 (2026-07-14).
This document is the source of truth for Targets, Schedules & Diagnostics. Update it (and the ADRs) in the same PR as any deviation.
-->

# Targets, Schedules & Diagnostics

### 7.2 External targets & schedules

**Target** = `{name, kind: host|url, address, labels}`. Checks reference
nodes (as today), targets, or ad-hoc destinations.

- *One-shot* (**landed M4**): controller diagnostics v2.
- *Scheduled* (**landed M4, `once` and `interval` only**): the Console's
  schedule loop creates one-shot runs and persists results — "MTR to 8.8.8.8
  every 5m, keep the paths". **`cron` did not ship**: `check_schedules.kind`
  is plain `TEXT` with a comment rather than an enum or a `CHECK` precisely so
  adding it later is code and not a migration (Decision 9). The loop is
  **off by default** (`console.scheduler.enabled`).
- *Continuous* (**landed M4**): pushed to agents over the controller's
  external-check stream; agents probe targets in the normal checker loop and
  **export Prometheus metrics** — the `kconmon_ng_external_*` family, a new
  family rather than the peer one (metrics.md "Agent — External"). Agent
  selection per definition: `all | per-zone | one-per-zone (default)` —
  bounds cardinality and probe traffic.

**The continuous probe cadence is not yet operator-configurable.** Every
continuous check is pushed with a **30s interval and a 5s per-probe timeout**,
both compile-time constants in `internal/console/checks/reconciler.go`. There
is no config key, no Helm value and no API field. The reason is a data-model
gap, not a missing knob: `check_schedules` is the only row carrying a cadence
and `kind='continuous'` is deliberately forbidden from carrying one (a
continuous check is never *fired*), which leaves the agent-side probe interval
with nowhere to live. 30s matches the agents' own checker-loop cadence. A
per-schedule cadence field is carried forward out of M4 — see
[CONFIG.md](../architecture/CONFIG.md) for the shape it would take.

Cardinality guardrail (**landed M4**): `POST /api/v1/checks/projection`
computes `agents × protocols` for one definition and the UI warns on it before
enabling. The ceiling is **400 series per definition** — a compile-time
constant, reusing the diagnostics runner's own 400-pair bound so both guards
tell the same story — and the same number the write path enforces, so the
warning can never disagree with the enforcement. Not fleet-wide: it is the
number the form displays.

Continuous checks only actually run when `console.scheduler.enabled=true` —
the reconciler that pushes assignments to agents shares the scheduler's flag
and its PostgreSQL advisory lock. And the agent, not the Console, decides
whether a destination may be probed at all
([SECURITY.md](../architecture/SECURITY.md) §10.2.1).

### 7.3 Diagnostics runs

A *run* fans one spec into individual checks (bounded concurrency, per-user
rate limits from Valkey). Each `CheckResult` is persisted, progress streams
over WS, summary computed (ok/fail, worst RTT). MTR results upsert
`mtr_path_snapshots` (normalize → hash → dedupe), so scheduled MTRs feed
path history automatically. Every run has a permalink; actions: Re-run,
Save as definition, Attach to incident.

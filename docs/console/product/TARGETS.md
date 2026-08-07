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

- *One-shot*: controller diagnostics v2.
- *Scheduled*: Console cron creates one-shot runs, persists results —
  "MTR to 8.8.8.8 every 5m, keep the paths".
- *Continuous*: pushed to agents via a new task type; agents probe targets
  in the normal checker loop and **export Prometheus metrics** identical in
  semantics to pair checks. Agent selection per definition:
  `all | per-zone | one-per-zone (default)` — bounds cardinality and probe
  traffic.

Cardinality guardrail: the UI computes and warns on projected series count
(agents × targets × protocols) before enabling a definition.

### 7.3 Diagnostics runs

A *run* fans one spec into individual checks (bounded concurrency, per-user
rate limits from Valkey). Each `CheckResult` is persisted, progress streams
over WS, summary computed (ok/fail, worst RTT). MTR results upsert
`mtr_path_snapshots` (normalize → hash → dedupe), so scheduled MTRs feed
path history automatically. Every run has a permalink; actions: Re-run,
Save as definition, Attach to incident.

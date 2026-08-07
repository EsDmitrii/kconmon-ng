<!--
Status: current
Owner: @EsDmitrii
Source: extracted from root DESIGN.md §1.1, §6.1 in M0 (2026-07-14).
This document is the source of truth for UX Principles. Update it (and the ADRs) in the same PR as any deviation.
-->

# UX Principles

### 1.1 Product principles (binding)

1. **Workflow-first, not page-first.** The UI is organized around what an
   operator is doing (investigating, verifying a fix, watching a rollout),
   not around chart types. Pages exist, but the primary entry points are
   workflows: Investigate, Run a check, Watch live.
2. **No dead screens.** Every page answers "what is happening right now?"
   or "what changed recently?" — each object page carries a compact
   "Recent changes" block fed by the event stream.
3. **Everything is deep-linkable.** Time range, filters, selected objects,
   Time Machine state — all in the URL. A permalink pasted into an incident
   channel must reproduce the exact view.
4. **Metrics live only in Prometheus.** Console never writes time series.
5. **Console is optional.** With `console.enabled=false` kconmon-ng works
   exactly as today. Controller/agent changes are minimal and independently
   useful.
6. **Not a Grafana clone.** No general-purpose dashboard engine; a fixed
   set of opinionated, domain-specific views. Grafana dashboards in
   `dashboards/` remain supported and are not deprecated.

### 6.1 Design language

Dense, data-first, keyboard-friendly. Dark theme default, light available;
CSS variables + `prefers-color-scheme`, persisted per user. Consistent
green→amber→red health scale everywhere + colorblind-safe alternative.
Global time-range picker for historical views. Every chart/table: hover
detail, CSV/JSON export, URL-encoded state. A `docs/console/design/`
mini design system (tokens, spacing, chart palettes, component inventory)
is written in M0 and enforced in review.

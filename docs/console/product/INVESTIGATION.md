<!--
Status: draft
Owner: @EsDmitrii
Source: extracted from root DESIGN.md §7.6 in M0 (2026-07-14).
This document is the source of truth for Investigation Mode. Update it (and the ADRs) in the same PR as any deviation.
-->

# Investigation Mode

### 7.6 Investigation Mode (flagship)

Entry: `Investigate` page, or "Investigate" on any card/matrix cell, or the
palette. Input: **scope** (pair | node | target | zone pair | whole
cluster) + **time range** (with presets: "last hour", "around alert X").

Console assembles, in one view:

1. **Timeline** (center): merged, time-ordered events — alert fired/
   resolved (rules eval via Prometheus + optional Alertmanager API),
   metric threshold crossings (latency/loss/jitter derived from
   `query_range` with baseline deltas), MTR path changes, DNS resolution
   changes, topology events (agent restarts, node NotReady), **K8s events**
   for nodes/pods in scope (`kubectx`), config changes (audit), maintenance
   windows.
2. **Signal panels**: latency/loss charts for the scope with the timeline
   cursor synced; matrix delta (window vs baseline); MTR path diff if
   snapshots exist on both sides.
3. **Correlation (v1 = honest heuristics)**: bucket events into windows,
   rank by temporal proximity to the anomaly onset, surface "N seconds
   before loss started: hop #7 path change / node cordoned". No ML, no
   pretending — the ranking rules are documented in
   `docs/console/product/INVESTIGATION.md`.
4. **Actions rail**: Run MTR now, Run TCP/HTTP now, Compare vs 1h earlier /
   same window yesterday, Show all degradations in range, Add annotation,
   Create maintenance, Export.

An investigation can be **saved as an Incident** (`incidents`): pinned
findings, notes, status (open/resolved), shareable permalink. Incidents
appear on Overview and on related object cards. This is deliberately
lightweight incident tracking — not a ticketing system; a webhook can
notify external systems (§7.9).

<!--
Status: draft
Owner: @EsDmitrii
Source: extracted from root DESIGN.md §7.4 in M0 (2026-07-14).
This document is the source of truth for Topology. Update it (and the ADRs) in the same PR as any deviation.
-->

# Topology

### 7.4 Topology (React Flow)

Custom node types (zone group, node, external target) and edge rendering:
color = health of the selected protocol, width = RTT bucket, dashed =
maintenance. Interactions: drag, pin, search, filter (zone/protocol/state),
"highlight problem paths" toggle, optional probe-flow animation (off by
default; respects `prefers-reduced-motion`). Layouts are saveable
(`layouts` table): auto (elk/dagre) or manual per-user/global. Time slider
+ **compare mode** (two timestamps side-by-side, changed edges
highlighted) — this is the topology half of incident diffing. Falls back to
zone-level aggregation above ~60 nodes; node-level expands per zone.

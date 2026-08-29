# Topology

<!-- screenshot: console-topology.png pending post-redesign reshoot -->

An interactive zone/node map. It answers: **where do the problem paths run, and between which zones?**

## What this page shows

Nodes are boxes grouped into zone lanes; problem paths (TCP fail ≥ 1%) are drawn as edges, worst first, and hovering
an edge shows its failure ratio. Live, the node set refreshes every 15 seconds; with the
[Time Machine](time-machine.md) engaged, the map is **reconstructed from topology events** at the viewed instant — a
different mechanism from the live view, with its own honest edge cases (see below).

## Nodes and edges

- **Node colour** comes from the probe matrix, using the same tiers as [Matrix](matrix.md): *Healthy*,
  *Degraded · worst path 1–10%*, *Failing · ≥ 10% or not ready*. A not-ready node carries a "not ready" badge.
- **Edges** are only drawn for problem paths. The edge label names its vector — a plain percentage is a failure
  ratio, "{pct} loss" is packet loss. The caption counts them ("3 problem paths", or
  "showing {shown} worst of {total} problem paths" when capped; "no problem paths right now" otherwise).
- Selecting a node and pressing Enter (or clicking it) opens its [node page](pair-and-node-pages.md).
- Map controls: *Zoom in*, *Zoom out*, *Fit the whole map*. The map is read-only — nodes are not draggable.

## Layout and grouping

One lane per zone, headed "{zone} · {count} nodes"; big zones wrap into a grid rather than a single column. Nodes
whose zone label is absent gather in a lane named **no zone reported** — that is what is true (no zone was reported to
the console), and it is also the state you get on a cluster whose nodes lack the `topology.kubernetes.io/zone` label.

When the controller has no Kubernetes node view at all, the map is drawn from registered agents and the zones they
registered with, and a notice says so — node colour still works, readiness is unknown.

Historical reconstructions state their own bounds: an instant older than what the event log kept ("This
reconstruction is incomplete"), events that name no node ("Nothing to reconstruct at this time"), or simply no nodes
at that time. The live empty state — "No nodes reported by the controller yet" — points at the agent DaemonSet.

## Deep links

- Node → `/nodes/<name>` ([node page](pair-and-node-pages.md)), carrying `?at=` when engaged.

## Use it when

- You suspect a zonal problem — cross-zone edges clustering on one lane are the tell.
- You want a quick head-count per zone and readiness state without reading a table.
- During an incident you need "which paths are bad *right now*", ranked worst-first, on one screen.

Verified against `web/src/pages/topology.tsx`, `web/src/lib/i18n/dict/topology.ts`, `web/src/hooks/use-topology.ts`
(`GET /api/v1/topology`, optionally `?at=`).

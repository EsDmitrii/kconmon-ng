# kconmon-ng Console — documentation

kconmon-ng Console turns kconmon-ng from a metrics exporter with Grafana
dashboards into a cloud-native network observability platform for Kubernetes.
This directory is the source of truth for the Console; the root `DESIGN.md`
is now a short index that points here.

Documentation is written incrementally with the code it describes. A doc marked
`draft` is a scaffold whose deeper detail lands with its milestone.

## Status

| Doc | Topic | Status |
| --- | --- | --- |
| [architecture/OVERVIEW.md](architecture/OVERVIEW.md) | Component overview (§4) | current |
| [architecture/BACKEND.md](architecture/BACKEND.md) | Backend stack & module boundaries (§4.2–4.3) | current |
| [architecture/FRONTEND.md](architecture/FRONTEND.md) | Frontend stack & UX surface (§4.2, §6) | current |
| [architecture/DATA.md](architecture/DATA.md) | Data architecture: Prometheus/Postgres/Valkey (§5) | draft — §5.2/§5.3 are current for what M3 landed; the still-pending tables in §5.2 are unbuilt design |
| [architecture/API.md](architecture/API.md) | REST API surface (§8) | current |
| [architecture/WEBSOCKET.md](architecture/WEBSOCKET.md) | WebSocket protocol (§8) | current |
| [architecture/CONFIG.md](architecture/CONFIG.md) | Console config file + Helm mapping | current |
| [architecture/SECURITY.md](architecture/SECURITY.md) | AuthN/Z, security, observability (§10, §12) | current for §10/§11 (as built in M3); §12's alerting/webhook items are still design |
| [product/UX_PRINCIPLES.md](product/UX_PRINCIPLES.md) | Product principles & design language (§1.1, §6.1) | current |
| [product/PAGES.md](product/PAGES.md) | Navigation, object cards (§6.2–6.4), Live (§7.8), Diagnostics (§7.x) | draft — §7.8 Live, §6.4 object cards v1, and §7.x Diagnostics are current; §6.3 Time Machine and the Target card still await M4/M5 |
| [product/INVESTIGATION.md](product/INVESTIGATION.md) | Investigation Mode + correlation heuristics (§7.6) | draft |
| [product/TIME_MACHINE.md](product/TIME_MACHINE.md) | Time Machine global context (§6.3) | draft |
| [product/MTR_EXPLORER.md](product/MTR_EXPLORER.md) | MTR Explorer (§7.5) | draft |
| [product/TOPOLOGY.md](product/TOPOLOGY.md) | Topology view (§7.4) | draft |
| [product/ALERTING.md](product/ALERTING.md) | Alert rule management (§7.11) | draft |
| [product/TARGETS.md](product/TARGETS.md) | External targets, schedules, diagnostics runs (§7.2–7.3) | draft |
| [design/DESIGN_SYSTEM.md](design/DESIGN_SYSTEM.md) | Design system v0 (tokens, palettes, components) | current |
| [roadmap/MILESTONES.md](roadmap/MILESTONES.md) | Milestones M0–M7 (§13) | current |
| [decisions/](decisions/) | Architecture Decision Records | current |

**M1 shipped** (read-only observability, `database.mode=disabled`): Topology,
Matrix, Overview, Explore, and PromQL Console pages backed by the four
`/api/v1/{topology,matrix,promql/query,promql/query_range}` endpoints — no
database, no Valkey, no realtime push (that's M2). See
[architecture/API.md](architecture/API.md) and
[architecture/FRONTEND.md](architecture/FRONTEND.md) for what was actually
built vs. the fuller M2+ surface those docs still describe.

**M2 shipped** (realtime, still no database): controller
`EventStream.WatchEvents` behind `controller.events.enabled`, the Console event
ingester, Valkey pub/sub on one channel with an in-process fallback, the
multiplexed `GET /ws` (`live`, `topology`, `matrix:{tcp,udp,icmp}:pod`), the
**Live page**, and pushed matrix snapshots with M1 polling kept verbatim as the
automatic fallback. Chart 1.4.0. No PostgreSQL, no `topology_events`, no event
scrollback — those landed with M3, below. Protocol:
[architecture/WEBSOCKET.md](architecture/WEBSOCKET.md); configuration:
[architecture/CONFIG.md](architecture/CONFIG.md); the full deferral list is in
[roadmap/MILESTONES.md](roadmap/MILESTONES.md).

**M3 shipped** (persistence, auth, diagnostics): PostgreSQL via CloudNativePG
(`console.database.mode`, optional), full `auth.mode=anonymous|local|header|oidc`
with built-in and custom RBAC roles, an async best-effort audit log, and API
tokens; the Diagnostics runner with persisted run history/permalinks and a
live `run:{id}` WebSocket topic (REST-polling fallback on a non-runner
replica); `topology_events` persisting all five event types behind
`GET /api/v1/events`; Node and Pair object cards v1 with a "Recent changes"
rail. Chart 1.5.0. Security: [architecture/SECURITY.md](architecture/SECURITY.md);
config/secrets: [architecture/CONFIG.md](architecture/CONFIG.md); API:
[architecture/API.md](architecture/API.md); the full deferral list is in
[roadmap/MILESTONES.md](roadmap/MILESTONES.md).

## Decisions

- [ADR-001 — PostgreSQL via CloudNativePG](decisions/ADR-001-postgresql-cnpg.md)
- [ADR-002 — Valkey is ephemeral only](decisions/ADR-002-valkey-ephemeral-only.md)
- [ADR-003 — Multiplexed WebSocket protocol](decisions/ADR-003-websocket-protocol.md)
- [ADR-004 — CodeMirror 6 over Monaco](decisions/ADR-004-codemirror-over-monaco.md)
- [ADR-005 — No plugins, no multi-tenancy in v1](decisions/ADR-005-no-plugins-no-multitenancy-v1.md)
- ADR template: [decisions/ADR-template.md](decisions/ADR-template.md)

## Rules

1. One doc = one owner topic.
2. Every non-obvious architectural choice gets an ADR.
3. This README tracks each doc's status (draft / current / stale).
4. Any deviation from a doc updates the doc in the same PR.

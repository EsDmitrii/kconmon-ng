# DESIGN.md — kconmon-ng Console (index)

Status: **v2 — split into `docs/console/` in M0**
Owner: @EsDmitrii

The full specification now lives as thematic documents under
[`docs/console/`](docs/console/README.md). This file is a short index. Any
deviation from a doc updates that doc in the same PR.

## Where things live

- **Start here:** [`docs/console/README.md`](docs/console/README.md) — navigation + per-doc status.

### Architecture
- [Overview](docs/console/architecture/OVERVIEW.md) — components, data flow, rules (§4).
- [Backend](docs/console/architecture/BACKEND.md) — stack + module boundaries (§4.2–4.3).
- [Frontend](docs/console/architecture/FRONTEND.md) — stack + UX surface (§4.2, §6).
- [Data](docs/console/architecture/DATA.md) — Prometheus / PostgreSQL / Valkey roles (§5).
- [API](docs/console/architecture/API.md) — REST surface (§8).
- [WebSocket](docs/console/architecture/WEBSOCKET.md) — realtime protocol (§8).
- [Security](docs/console/architecture/SECURITY.md) — authN/Z, security, observability (§10, §12).

### Product
- [UX principles](docs/console/product/UX_PRINCIPLES.md) — principles + design language (§1.1, §6.1).
- [Pages](docs/console/product/PAGES.md) — navigation, object cards (§6.2–6.4).
- [Investigation](docs/console/product/INVESTIGATION.md) — flagship workflow + heuristics (§7.6).
- [Time Machine](docs/console/product/TIME_MACHINE.md) — global time context (§6.3).
- [MTR Explorer](docs/console/product/MTR_EXPLORER.md) — path history + diff (§7.5).
- [Topology](docs/console/product/TOPOLOGY.md) — interactive map (§7.4).
- [Alerting](docs/console/product/ALERTING.md) — PrometheusRule builder (§7.11).
- [Targets & schedules](docs/console/product/TARGETS.md) — external checks + runs (§7.2–7.3).

### Design, roadmap, decisions
- [Design system](docs/console/design/DESIGN_SYSTEM.md) — tokens, palettes, components.
- [Milestones](docs/console/roadmap/MILESTONES.md) — M0–M7 (§13).
- [Decisions (ADRs)](docs/console/decisions/) — ADR-001…005 + template.

## Positioning (unchanged)

kconmon-ng Console turns kconmon-ng from a metrics exporter with Grafana
dashboards into a cloud-native network observability platform for Kubernetes:
live connectivity, an event feed, Investigation Mode, Time Machine, an MTR
Explorer, browser-driven and scheduled/continuous checks, a PrometheusRule
alert builder, and a PromQL console — OIDC + RBAC first, and **optional**
(`console.enabled=false` leaves kconmon-ng exactly as it is today).

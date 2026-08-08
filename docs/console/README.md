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
| [architecture/DATA.md](architecture/DATA.md) | Data architecture: Prometheus/Postgres/Valkey (§5) | draft — §5.2/§5.3 are current through M7 (every table through `alert_rules`, the topology fold and all nine retention sweeps included); `layouts` and `settings` in §5.2 remain unbuilt design, pinned to no milestone |
| [architecture/API.md](architecture/API.md) | REST API surface (§8) | current |
| [architecture/WEBSOCKET.md](architecture/WEBSOCKET.md) | WebSocket protocol (§8) | current |
| [architecture/CONFIG.md](architecture/CONFIG.md) | Console config file + Helm mapping | current — through chart 1.9.0 |
| [architecture/SECURITY.md](architecture/SECURITY.md) | AuthN/Z, security, observability (§10, §12) | current — §10/§11 as built in M3, §12.1 (webhook signing, at-rest encryption, SSRF posture) as built in M6, §10.2's ws authorization and §10.3's namespaced alerting Role as built in M7. §12 now states plainly that nothing self-monitors out of the box |
| [product/UX_PRINCIPLES.md](product/UX_PRINCIPLES.md) | Product principles & design language (§1.1, §6.1) | current |
| [product/PAGES.md](product/PAGES.md) | Navigation, object cards (§6.2–6.4), MTR (§7.5), Live (§7.8), Explore A/B + annotations (§7.x), Diagnostics (§7.x), Alerting (§7.11), Settings (§7.12) | draft — current as of M7; every navigation-tree entry now exists, and §6.2 records where the Settings page is narrower than the M0 tree promised |
| [product/INVESTIGATION.md](product/INVESTIGATION.md) | Investigation Mode + correlation heuristics (§7.6) | current — as built in M6 plus M7's alert source; §7.6's own bullet list is the original design text, and the as-built note above it says which parts shipped |
| [product/TIME_MACHINE.md](product/TIME_MACHINE.md) | Time Machine global context (§6.3) | current — as built in M5, including the three named limitations |
| [product/MTR_EXPLORER.md](product/MTR_EXPLORER.md) | MTR Explorer (§7.5) | current — as built in M5 |
| [product/TOPOLOGY.md](product/TOPOLOGY.md) | Topology view (§7.4) | draft |
| [product/ALERTING.md](product/ALERTING.md) | Alert rule management (§7.11) | current — as built in M7, including the dropped `cert-expiry` template and the missing parser dependency |
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

**M4 shipped** (external targets, schedules, continuous checks): CRUD
`/api/v1/{targets,checks,schedules}` backed by PostgreSQL with a projection
guard (400-series limit), the advisory-locked console scheduler
(`console.scheduler.enabled`, off by default) firing `once`/`interval`
schedules plus a stuck-run reaper and run cancellation; continuous external
checks — the controller streams per-agent assignments, the console reconciles
them, and the agent enforces its own CIDR allowlist
(`config.checkers.external.enabled`, off by default; the agent, not the
Console, is authoritative); the `kconmon_ng_external_*` metric family; rate
limits on runs and login; a committed OpenAPI spec with a router-walking
drift gate; the Targets page and Target card. Chart 1.6.0. The full deferral
list is in [roadmap/MILESTONES.md](roadmap/MILESTONES.md).

**M5 shipped** (MTR Explorer + Time Machine): MTR path history — snapshots
content-hashed per route in `mtr_path_snapshots`, deduped at result-ingest,
behind `GET /api/v1/mtr/*` and the three-pane `/mtr` Explorer with
client-side path diff and a loss-overlay changes timeline; optional hop
enrichment (rDNS + MaxMind mmdb, `console.mtr.enrichment.*`, off by
default); the Time Machine `?at=` global context (topology folded from
`topology_events`, PromQL evaluated at `t`, mutations disabled while
engaged) and chart annotations. Chart 1.7.0. Deferrals — including the
controller's then-unattributed topology events, which M7 finally fixed — are named in
[roadmap/MILESTONES.md](roadmap/MILESTONES.md).

**M6 shipped** (Investigation Mode + Incidents): the `/investigate` page —
a merged timeline over eight sources (**nine since M7's alert row**),
cursor-synced signal panels and an
actions rail, all assembled **client-side** over the existing read APIs, with
correlation v1 as four documented arithmetic steps whose constants
[product/INVESTIGATION.md](product/INVESTIGATION.md) restates verbatim;
incidents saved from an investigation and reshared as
`/investigate?incident={id}`, where the **row**, not the URL, is the authority,
surfaced on Overview and on all three object cards; maintenance windows as
data and chart rendering (**still not suppression**, even after M7: there is no
Alertmanager client in this repository);
outbound webhooks on incident lifecycle, HMAC-signed with a bounded retry
ladder and per-endpoint secrets sealed under
`console.webhooks.encryptionKeySecret`; and `kubectx`, the console's first
apiserver client (`console.kubernetesContext.enabled`, off by default, zero new
Go dependencies), capturing cluster events filtered to this fleet. Overview's
"recent events" placeholder — carried since M2 — is finally a real panel.
Chart 1.8.0 adds a **console-only** ServiceAccount and ClusterRole rather than
widening the agent/controller grant. Deviations and deferrals — the dropped
30 s bucketing, the `PATCH` exception, the zone-pair scope lossiness, the
never-built DNS-resolution source — are named in
[roadmap/MILESTONES.md](roadmap/MILESTONES.md).

**M7 shipped** (Alerting + polish — the last planned milestone): alert rule
management, where the Console **manages** and Prometheus **evaluates** —
`/alerting` builds a rule from six typed templates or raw PromQL
([product/ALERTING.md](product/ALERTING.md)), and a reconciler
server-side-applies every enabled one into **one** `PrometheusRule` object,
recording drift and then fixing it; validation is render goldens plus a live
preview that runs the expression against your Prometheus, because the plan's
parser dependency was **not** taken; foreign rules listed read-only and adopted
by an explicit **copy** that never mutates the object it read;
`alert.fired`/`alert.resolved` webhooks off a polling edge detector that
baselines on boot and freezes rather than "resolving" the fleet when Prometheus
goes quiet; versioned configuration export/import, admin-only and dry-run
first; a Settings page and a `⌘K` command palette. `alerts:read` and
`alerts:manage` bring the permission set to **25**, with `alert-editor` finally
holding the permission it was named for. M7 also **closed the M5/M6 carry**:
the controller attributes every topology change, so Time Machine reconstructs a
real node set. Chart 1.9.0 adds `console.alerting.*` and a **namespaced** Role —
never a ClusterRole — over `prometheusrules`. The dropped `cert-expiry`
template, the missing parser dependency, the `alert-editor` decision, and the
**final** deferral ledger are in
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

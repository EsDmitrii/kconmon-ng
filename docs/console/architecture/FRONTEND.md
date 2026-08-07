<!--
Status: current
Owner: @EsDmitrii
Source: extracted from root DESIGN.md §4.2, §6 in M0 (2026-07-14).
This document is the source of truth for Frontend Architecture. Update it (and the ADRs) in the same PR as any deviation.
-->

# Frontend Architecture

## Technology choices (frontend rows of §4.2)

| Layer           | Choice                                        | Rationale |
| --------------- | --------------------------------------------- | --------- |
| Frontend        | React 19 + TypeScript + Vite                  | Current stable; no experimental APIs required |
| Routing/data    | TanStack Router + TanStack Query + Zustand    | Type-safe URLs (deep-linking is a core principle), server cache w/ WS invalidation |
| UI kit          | Tailwind CSS + shadcn/ui (Radix)              | Themeable via CSS variables, accessible primitives |
| Charts          | ECharts                                        | Heatmap + time series + scrubbing in one lib |
| Topology        | **React Flow**                                 | Interactive graph: drag, pin, saved layouts, custom nodes (§7.4) |
| Code editor     | **CodeMirror 6** + promql extension            | Purpose-built PromQL support; ~10x smaller than Monaco; Monaco rejected (bundle weight, no PromQL gain) |
| Motion          | CSS transitions; Framer Motion allowed only for the Live feed and palette | Motion is seasoning, not architecture |

## Data layer

M1 shipped five pages — Overview, Matrix, Topology, Explore, PromQL Console — on
top of the four REST endpoints in
[API.md](API.md#implemented-in-m1-internalconsolehttpapi), with polling as the
only transport. M2 added the Live page and a WebSocket client
(`web/src/lib/ws.ts`, `useWsTopic`, `useCapabilities`) **on top of** that, never
in place of it: REST is still the first paint everywhere, and every page falls
back to its polling interval when the socket or the `events` capability is
missing. Only `useMatrix` currently switches transport — and only while the
capability *and* the socket are both up. See
[WEBSOCKET.md](WEBSOCKET.md) for the protocol and the fallback rules.

The polling layer, unchanged since M1:

- **TanStack Query** owns server cache and polling. `useMatrix`/`useTopology`
  (`web/src/hooks/`) poll every **15s** (`MATRIX_POLL_MS` /
  `TOPOLOGY_POLL_MS`, both `refetchInterval`); Explore polls every **30s**
  (`EXPLORE_POLL_MS` in `web/src/pages/explore.tsx`) since its five curated
  `query_range` charts are heavier queries.
- **Types are hand-written**, not generated: `web/src/lib/types.ts` mirrors
  the Go JSON structs field-by-field (`Topology`, `Matrix`/`MatrixCell`,
  `PromResult`, `Problem`, and from M2 `LiveEvent`; `WsEnvelope`/`ClientMessage`
  live in `web/src/lib/ws.ts`), checked against the backend in code review —
  see API.md's "Deferred: OpenAPI + codegen" for why there's no codegen yet.
- **Tests mock `fetch` at the API-client seam**, never inside components:
  `web/src/lib/api.ts` is the only module that calls `fetch`, and tests
  `vi.stubGlobal("fetch", ...)` around it (per the repo's mock-strategy
  convention). `ApiError` wraps RFC 7807 problem bodies; a Prometheus-native
  error envelope (`{"status":"error",...}`) from the promql endpoints
  resolves normally rather than throwing, so callers branch on
  `data.status`.
- **`@xyflow/react` v12** (not `reactflow` — that's the pre-v12 legacy
  package name for the same project) renders the Topology page: nodes
  grouped into one `type: "group"` node per zone, health-colored via
  CSS class (`topo-node--ok|degraded|failing`) computed from the live
  Matrix's TCP fail ratio, edges drawn only for pairs at ≥1% fail ratio.
- **ECharts** (`echarts@^6.1.0`) is wrapped in a thin `EChart` component
  (`web/src/components/echart.tsx`) around `echarts.init`/`setOption` —
  no `echarts-for-react`. The version was bumped up from an originally
  planned `^5.6.0` during implementation to close a moderate XSS advisory
  present in versions before 6.1.0; the API surface this repo uses
  (`echarts.init`, `EChartsOption`) is unaffected by the major-version
  bump. Explore renders five curated charts this way; the Matrix heatmap
  uses ECharts directly for the same reason.
- **CodeMirror 6 + `@prometheus-io/codemirror-promql`** (ADR-004) powers the
  PromQL Console editor (`web/src/components/promql-editor.tsx`): syntax
  highlighting and autocomplete, table/chart/JSON result tabs, `Mod-Enter`
  (Ctrl on Windows/Linux, Cmd on macOS) to execute.
- **The PromQL Console's step is a suggestion, not a leash**: unlike Explore
  (which hides the control and always auto-sizes), the Console exposes an
  explicit step picker whose value is re-derived from the selected range only
  until the operator picks one by hand. After that first manual pick it is
  sticky — changing the range no longer overwrites it — because a dev tool that
  silently resets a deliberate choice is worse than one that keeps a suboptimal
  step.

## 6. Frontend UX

### 6.1 Design language

Dense, data-first, keyboard-friendly. Dark theme default, light available;
CSS variables + `prefers-color-scheme`, persisted per user. Consistent
green→amber→red health scale everywhere + colorblind-safe alternative.
Global time-range picker for historical views. Every chart/table: hover
detail, CSV/JSON export, URL-encoded state. A `docs/console/design/`
mini design system (tokens, spacing, chart palettes, component inventory)
is written in M0 and enforced in review.

### 6.2 Navigation

```
├── Overview             health summary, worst pairs, firing alerts, recent events
├── Live                 real-time event feed (§7.8)
├── Investigate          Investigation Mode entry (§7.6) + saved incidents
├── Matrix               live/historical N×N heatmap
├── Topology             React Flow map (§7.4)
├── MTR                  MTR Explorer (§7.5)
├── Diagnostics          run checks, run history
├── Targets & Schedules  external targets, definitions, schedules
├── Explore              curated metrics + A/B compare
├── Alerting             rule list + builder
├── Console              PromQL dev-tools
└── Settings             auth, RBAC, retention, maintenance, webhooks, export/import
```

Command palette (`⌘K`): jump to any node/target/pair, run a check, start an
investigation, create alert/maintenance/annotation, toggle Time Machine,
switch theme. Palette actions are the same registry the UI buttons use —
every action gets a palette entry for free.

### 6.3 Time Machine (global time context)

A top-bar control with two states: **Live** and **@ timestamp**. It is a
single piece of global state (`timemachine` store) that every data hook
resolves through:

- Prometheus reads become instant/range queries evaluated at/around `t`.
- Topology is reconstructed from `topology_events` up to `t`.
- Matrix renders the historical snapshot; Live feed becomes a scrollback
  around `t`; object cards show state-as-of-`t` with "Recent changes"
  relative to `t`.
- Mutating actions (run check, edit rule) are disabled with a clear banner
  ("You are viewing 15:34 yesterday — return to Live to act").
- The state is in the URL (`?at=`), so a Time Machine view is shareable.

Implementation note: this is why §4.1 persists `topology_events` — the
controller only knows *now*.

### 6.4 Object cards

Uniform "card" pages for **Target**, **Node/Agent**, and **Pair**. Layout:
header (identity, health %, status), tab strip, and a persistent right rail
**Recent changes** (latency shifts, path changes, loss onset, agent
upgrades, config edits — from the event stream / `topology_events`).

Target card tabs: Overview (health, latency/loss/jitter sparklines),
Checks & Schedules, MTR history, DNS history, HTTP history (phases, cert
expiry when HTTPS), Alerts, Incidents, Maintenance, Audit. Node/Agent card:
per-destination breakdown, agent version/uptime, K8s node info + recent
node events. Pair card: per-protocol series, last MTR, quick actions
("Run check", "Run MTR", "Investigate this pair").

Quick actions on every card; "Investigate" pre-fills scope + range.

<!--
Status: current
Owner: @EsDmitrii
Source: extracted from root DESIGN.md §4 in M0 (2026-07-14).
This document is the source of truth for Architecture Overview. Update it (and the ADRs) in the same PR as any deviation.
-->

# Architecture Overview

## Component overview

Console is a **new binary** (`cmd/console`), its own Deployment, the only
component talking to PostgreSQL/Valkey/Kubernetes API. Controller stays a
lean data-plane orchestrator.

```
                      ┌──────────────────────────────────┐
   Browser (SPA)      │           Console (new)          │
  ──HTTPS/WSS───────▶ │  REST + WebSocket API │ SPA      │
                      │  AuthN (OIDC/...) + RBAC + audit │
                      │  Scheduler (cron)  │ Jobs        │
                      │  Event ingester    │ PromQL proxy│
                      │  PrometheusRule manager          │
                      │  K8s context reader (events)     │
                      └──┬──────┬──────┬──────┬──────┬───┘
                         │      │      │      │      │
            ┌────────────┘      │      │      │      └─────────────┐
            ▼                   ▼      ▼      ▼                    ▼
   ┌─────────────────┐  ┌───────────┐ ┌────────┐ ┌──────────┐ ┌─────────┐
   │ Controller      │  │ Prometheus│ │ Valkey │ │ Postgres │ │ K8s API │
   │ (leader):       │  │ query API │ │ cache  │ │ (CNPG)   │ │ events, │
   │ topology,       │  │ read-only │ │ pubsub │ │ config,  │ │ nodes,  │
   │ diagnostics,    │  └───────────┘ │ locks  │ │ results, │ │ Prom-   │
   │ WatchEvents ────┼──▶ ingested    └────────┘ │ history, │ │ Rules   │
   └───────┬─────────┘                           │ audit    │ └─────────┘
           │ gRPC task/peer streams (existing + extended)
           ▼
        Agents (DaemonSet) ── probes ──▶ node peers + external targets
```

Rules:

- **Controller** gains exactly three things: `WatchEvents` stream (§9.1),
  diagnostics v2 (§9.2), external-check task type (§9.3). No DB access.
- **Console** owns persistence, auth, scheduling, rules, K8s context.
- Console → controller via in-cluster Service; non-leader returns 503 →
  retry (optionally headless Service for faster leader discovery).
- Agents never talk to Console.
- Console persists a copy of controller events it needs for history
  (`topology_events`, MTR snapshots) — this is what makes Time Machine and
  Investigation possible without turning the controller into a database.

## Technology choices

| Layer           | Choice                                        | Rationale |
| --------------- | --------------------------------------------- | --------- |
| Backend         | Go (same module), `net/http` + chi            | One language, reuse `internal/` types and proto |
| Realtime        | WebSocket only (M2); REST polling as the fallback | Live feed, matrix snapshots, run progress. SSE is **not** implemented — see [BACKEND.md](BACKEND.md) §4.2 and [WEBSOCKET.md](WEBSOCKET.md) |
| Frontend        | React 19 + TypeScript + Vite                  | Current stable; no experimental APIs required |
| Routing/data    | TanStack Router + TanStack Query + Zustand    | Type-safe URLs (deep-linking is a core principle), server cache w/ WS invalidation |
| UI kit          | Tailwind CSS + shadcn/ui (Radix)              | Themeable via CSS variables, accessible primitives |
| Charts          | ECharts                                        | Heatmap + time series + scrubbing in one lib |
| Topology        | **React Flow**                                 | Interactive graph: drag, pin, saved layouts, custom nodes (§7.4) |
| Code editor     | **CodeMirror 6** + promql extension            | Purpose-built PromQL support; ~10x smaller than Monaco; Monaco rejected (bundle weight, no PromQL gain) |
| Motion          | CSS transitions; Framer Motion allowed only for the Live feed and palette | Motion is seasoning, not architecture |
| DB              | `pgx` + `sqlc`, migrations via `goose` (embedded, advisory-locked) | Type-safe, no ORM magic. **Landed in M3** — `internal/console/store`, the only package touching `pgx` |
| Cache/pubsub    | Valkey via `rueidis`, in-process fallback      | Pub/sub since M2; a sibling `cache.KV` seam landed in M3 for sessions. `InProcessBus`/`InProcessKV` cover `valkey.mode=disabled` (ADR-002) |
| OIDC            | `coreos/go-oidc`, code flow + PKCE             | Battle-tested. **Landed in M3**, with `auth.mode=oidc` |
| K8s client      | client-go (reuse controller's patterns)        | **Landed in M6** — `internal/console/kubectx`, core/v1 Events list+watch only, on the client-go the controller already depends on (zero new deps). No nodes read (the controller's topology API answers that) and no PrometheusRule SSA yet (M7). Off by default: `kubernetesContext.enabled` |

Frontend is embedded via `go:embed` — single image, no nginx sidecar.
`Dockerfile.console` mirrors the existing multi-stage pattern
(node build → Go build → distroless).

## Backend module boundaries (plugin-readiness without plugins)

Internal packages with narrow interfaces; nothing imports across layers
except through them:

```
internal/console/
├── httpapi/        REST handlers (thin; no business logic)
├── ws/             WebSocket hub, topic registry, per-topic seq
├── events/         ingester (controller stream → bus), LiveEvent
├── push/           server-side snapshot timers (matrix, topology)
├── store/          sqlc-generated repos; the ONLY package touching pgx
├── cache/          Bus abstraction: ValkeyBus + InProcessBus
├── promql/         Prometheus client, query guard, response cache
├── checks/         run orchestration, fan-out, result normalization
├── scheduler/      cron ticks → checks; Valkey-locked
├── mtr/            path normalization, hashing, diffing, enrichment
├── alerting/       rule model, rendering, k8s sync (SSA), drift
├── investigate/    Investigation assembly heuristics
├── timemachine/    time-context resolution (live vs @t)
├── kubectx/        K8s events/nodes reader
├── authn/ authz/   modes, sessions, RBAC middleware, audit
└── domain/         shared types; no dependencies on the above
```

The event bus (`cache.Bus`) is the future plugin seam: v2 plugins would be
consumers/producers on this bus plus registered UI routes. Do **not** build
the registration machinery now — just keep the seam clean.

This tree is the target end state. [BACKEND.md](BACKEND.md) §4.3 carries the same
list annotated with which packages exist after M3 and which are still ahead.

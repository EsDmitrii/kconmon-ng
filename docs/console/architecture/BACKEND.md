<!--
Status: draft
Owner: @EsDmitrii
Source: extracted from root DESIGN.md §4.2–4.3 in M0 (2026-07-14); §4.2/§4.3
updated from the as-built M3 implementation (2026-08-06): go.mod, Makefile
(SQLC_VERSION), internal/console/{store,checks,authn,authz,cache}/.
This document is the source of truth for Backend Architecture. Update it (and the ADRs) in the same PR as any deviation.
-->

# Backend Architecture

### 4.2 Technology choices

| Layer           | Choice                                        | Rationale |
| --------------- | --------------------------------------------- | --------- |
| Backend         | Go (same module), `net/http` + chi            | One language, reuse `internal/` types and proto |
| Realtime        | WebSocket only (M2); REST polling as the fallback | Live feed, matrix snapshots, run progress. SSE is **not** implemented — where the socket or the `events` capability is unavailable the UI degrades to REST polling with a "Delayed data" badge, which covers the same failure mode with no second transport to maintain |
| Frontend        | React 19 + TypeScript + Vite                  | Current stable; no experimental APIs required |
| Routing/data    | TanStack Router + TanStack Query + Zustand    | Type-safe URLs (deep-linking is a core principle), server cache w/ WS invalidation |
| UI kit          | Tailwind CSS + shadcn/ui (Radix)              | Themeable via CSS variables, accessible primitives |
| Charts          | ECharts                                        | Heatmap + time series + scrubbing in one lib |
| Topology        | **React Flow**                                 | Interactive graph: drag, pin, saved layouts, custom nodes (§7.4) |
| Code editor     | **CodeMirror 6** + promql extension            | Purpose-built PromQL support; ~10x smaller than Monaco; Monaco rejected (bundle weight, no PromQL gain) |
| Motion          | CSS transitions; Framer Motion allowed only for the Live feed and palette | Motion is seasoning, not architecture. M2's Live feed uses plain CSS transitions; Framer Motion is not a dependency |
| DB              | `pgx` + `sqlc`, migrations via `goose` (embedded, advisory-locked) | Type-safe, no ORM magic. **Landed in M3** — `internal/console/store` (the only package touching `pgx`), pinned `github.com/jackc/pgx/v5 v5.10.0`, `github.com/pressly/goose/v3 v3.27.3` (both in `go.mod`), and `sqlc` `v1.31.1` as a pinned CI/dev tool (`Makefile`'s `SQLC_VERSION`, invoked via `go run .../sqlc@$(SQLC_VERSION)` — never a locally-installed sqlc of a different version) |
| Cache/pubsub    | Valkey via `rueidis`, in-process fallback      | Pub/sub (`cache.Bus`) since M2; a sibling key/value seam, `cache.KV` (`Get`/`Set`/`Delete`), **landed in M3** for sessions (DATA.md §5.3) — same `ValkeyKV`/`InProcessKV` split as `Bus`. `cache.InProcessBus`/`InProcessKV` keep a single-replica console working with Valkey disabled (ADR-002) |
| OIDC            | `coreos/go-oidc`, code flow + PKCE             | Battle-tested. **Landed in M3** — `github.com/coreos/go-oidc/v3 v3.20.0`, `internal/console/authn/oidc.go`; `auth.mode` now selects `anonymous\|local\|header\|oidc` (SECURITY.md §10.1) |
| K8s client      | client-go (reuse controller's patterns)        | Events/nodes read, PrometheusRule SSA. **Not yet** — M6+ (`kubectx` M6, alerting sync M7); the console still has no K8s client as of M3 |

Frontend is embedded via `go:embed` — single image, no nginx sidecar.
`Dockerfile.console` mirrors the existing multi-stage pattern
(node build → Go build → distroless).

### 4.3 Backend module boundaries (plugin-readiness without plugins)

Internal packages with narrow interfaces; nothing imports across layers
except through them:

```
internal/console/
├── httpapi/        REST handlers (thin; no business logic)             — M0/M1/M2
├── ws/             WebSocket hub, topic registry, per-topic seq        — M2
├── events/         ingester (controller stream → bus), LiveEvent       — M2
├── push/           server-side snapshot timers (matrix, topology)      — M2
├── cache/          Bus abstraction: ValkeyBus + InProcessBus           — M2
├── controllerclient/ controller HTTP client (topology, version)        — M1
├── matrix/         N×N matrix computation from Prometheus              — M1
├── promql/         Prometheus client, query guard                      — M1
├── metrics/        Console self-metrics (<prefix>_console_*)           — M0
├── config/         Console config file loader/validator                — M0
├── ui/             embedded SPA handler (go:embed)                     — M0
├── store/          sqlc-generated repos; the ONLY package touching pgx     — M3, shipped
├── checks/         run orchestration, fan-out, result normalization        — M3, shipped
├── authn/ authz/   modes, sessions, RBAC middleware, audit                 — M3, shipped
├── scheduler/      cron ticks → checks; Valkey-locked                      — M4
├── mtr/            path normalization, hashing, diffing, enrichment        — M5
├── timemachine/    time-context resolution (live vs @t)                    — M5
├── investigate/    Investigation assembly heuristics                       — M6
├── kubectx/        K8s events/nodes reader                                 — M6
├── alerting/       rule model, rendering, k8s sync (SSA), drift            — M7
└── domain/         shared types; no dependencies on the above
```

Everything marked M0–M3 exists on disk today; everything marked M4+ does not.
`promql/` shipped without the response cache its original line promised, and
`cache/` was narrowed in M2 to pub/sub only, then widened in M3 with a
sibling `KV` seam (sessions only) — the locks/queues and PromQL response
cache DATA.md §5.3 lists still have no consumer before M4.

The event bus is the future plugin seam: v2 plugins would be consumers/producers
on `cache.Bus` plus registered UI routes. Do **not** build the registration
machinery now — just keep the seam clean. The seam is currently two methods
(`Publish`, `Subscribe`) and is deliberately frozen: it has no liveness API,
which is why the `events` capability can only speak for the local ingester
(see [WEBSOCKET.md](WEBSOCKET.md) "Capability detection and fallback").

`cache.KV` (M3) is a **sibling** of `Bus`, not an extension of it: a
short-TTL key/value seam (`Get`/`Set`/`Delete`) for session storage
(`authn.SessionStore`, DATA.md §5.3), added specifically because widening
`Bus` itself to cover read-your-write lookups would have broken the frozen
seam's own reasoning above. Like `Bus`, it has a `ValkeyKV` and an
`InProcessKV` backend, and inherits the same `console.valkey.mode=disabled`
single-replica caveat.

## Embedded SPA build contract (M0)

The Go binary embeds `internal/console/ui/dist` at compile time. Git tracks
only two files there: a **placeholder `index.html`** and a `.gitignore`
whitelist — so `go build`/`go test`/CI compile without Node. Consequences:

- `make build-console` alone produces a binary serving the placeholder. A
  real UI requires `cd web && npm run build` first (Vite writes into the
  embed dir), or the `Dockerfile.console` image build, which always does it.
- `emptyOutDir` wipes the dir on every build; the `restore-dist-gitignore`
  plugin in `web/vite.config.ts` rewrites the `.gitignore` whitelist after
  each build. The placeholder `index.html` is restored manually (or by
  `git checkout -- internal/console/ui/dist/index.html` once committed)
  before committing, so the tracked state stays node-free.
- **Build order matters.** `cd web && npm run build` writes the real SPA into
  the embed dir; `go build ./cmd/console` must run **before** the tracked
  placeholder `index.html` is restored, or the binary silently embeds the
  placeholder and serves a blank page with no error anywhere. This has bitten
  this project twice. Restore the placeholder only after the binary exists.

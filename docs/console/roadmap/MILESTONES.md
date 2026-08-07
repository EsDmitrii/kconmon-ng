<!--
Status: current
Owner: @EsDmitrii
Source: extracted from DESIGN.md §13, kept current; M3 rewritten from the
as-built implementation (2026-08-06).
-->

# Milestones

## 13. Implementation plan (milestones — each shippable)

**M0 — Foundation.** `cmd/console`, embedded SPA scaffold (React 19, Vite,
Tailwind, dark/light, TanStack Router), Dockerfile.console, Helm
`console.*` (anonymous auth only), health/metrics, CI matrix extension,
**`docs/console/` scaffolded from this spec (§15) incl. design system v0
and ADR-001…005**.

**M1 — Read-only observability.** Topology (React Flow, live only), Matrix
(Prometheus polling), Overview, Explore v1, PromQL Console. Runs with
`database.mode=disabled` — lowest possible entry barrier.

**M2 — Realtime (delivered).** Controller `EventStream.WatchEvents` (leader-only)
+ capability flags (`controller.events.enabled`), the Console event ingester with
capability precheck and reconnect, Valkey pub/sub with a documented in-process
fallback (`console.valkey.mode=bundled|external|disabled`), the multiplexed `/ws`
protocol (`live`, `topology`, `matrix:{tcp,udp,icmp}:pod` — see
`architecture/WEBSOCKET.md`), the **Live page** (virtualized feed,
type/severity/scope filters, pause-resume with buffering, missed-event
accounting), and matrix goes push with the M1 polling path kept verbatim as the
automatic fallback. Chart 1.4.0 (M2 shipped in the 1.4.0 release; the version
originally planned for it was renumbered after `ea820d2`).

Deferred out of M2, deliberately and by name:

- **No PostgreSQL and no `topology_events` persistence.** M2 ships the realtime
  *path*, not durable history; no `internal/console/store`, no `pgx`/CNPG. That
  moves Time Machine scrollback, object-card "Recent changes" and
  `GET /api/v1/events` to M3.
- **WS topics `run:{id}` and `mtr` not wired** — no consumer before the M3
  diagnostics runner and the M5 MTR Explorer. Their events still appear in
  `live`.
- **The `topology` topic is produced but unconsumed.** The pusher runs; the
  Topology page was left on its M1 15 s polling. Wiring it is frontend-only.
- **Overview's "recent events" panel untouched** — still an honest placeholder.
- **No Framer Motion.** The Live feed uses plain CSS transitions; BACKEND.md
  permits the dependency but M2 does not take it.
- **No SSE fallback.** WebSocket plus REST polling covers the same failure mode;
  ADR-003's SSE line is not implemented.
- **`check_observed` is on-demand diagnostics only.** Continuous agent probes
  never reach the controller, so they cannot appear in the event stream — they
  reach the UI through Prometheus (matrix topics).
- **`topology_changed` carries only a reason.** The controller does not yet
  attribute a registry change to a node, so those rows read scope `cluster`.

**M3 — Persistence, auth, diagnostics (delivered).** PostgreSQL via CloudNativePG
(`console.database.mode=cnpg|external|disabled`, ADR-001), `pgx`+`sqlc`+`goose`
migrations (`internal/console/store`), `topology_events` persisting all five
WebSocket event types behind `GET /api/v1/events` (DATA.md §5.2); full
`auth.mode=anonymous|local|header|oidc` (SECURITY.md §10.1), built-in
compiled-in roles plus a custom-role admin API (`/api/v1/rbac/*`), async
best-effort audit logging (`GET /api/v1/audit`), and API tokens (PATs,
`/api/v1/tokens`, SHA-256-hashed, Decision 11); the Diagnostics runner
(`internal/console/checks`) with bounded fan-out, persisted run
history/permalinks (`POST`/`GET /api/v1/runs`, `GET /api/v1/runs/{id}`), and
a live `run:{id}` WebSocket topic with REST-polling fallback (Decision 14);
object cards v1 for Node and Pair (with the "Recent changes" rail) — see
`architecture/{DATA,SECURITY,API,WEBSOCKET}.md` and `product/PAGES.md` §6.4/§7.x.
Chart 1.5.0.

Deferred out of M3, deliberately and by name:

- **Time Machine and `?at=`** — historical topology/matrix reconstruction
  stays M5, even though `topology_events` (its data source) now exists.
- **The Target card** — object cards v1 shipped Node and Pair only; Target
  waits on the `targets` table (M4).
- **`targets`/`check_definitions`/`check_schedules`/`alert_rules`/`incidents`/
  `annotations`/`maintenance_windows`/`webhooks`/`layouts`/`settings`
  tables** — none exist yet; DATA.md §5.2 marks each with the milestone that
  adds it.
- **Run cancellation** — a run in flight cannot be stopped short of its own
  deadline or process shutdown; nothing external can cancel one in M3.
- **OpenAPI codegen** — moved from its original M3 target to M4 (Decision
  12): the M3 surface grew substantially but stayed hand-checked
  successfully; M4's targets/schedules CRUD is where the tooling investment
  starts to pay for itself.
- **Overview's "recent-events" panel** — still an honest placeholder,
  untouched since M2, even though `GET /api/v1/events` now exists to back
  it.
- **`check_results` partitioning** — ADR-001's "partition by month if volume
  warrants" follow-up is not done; the table is pruned by retention alone.

**M4 — Targets & external checks.** Targets CRUD, scheduler, diagnostics
v2, external-check agent task + new metrics/labels (update
`docs/metrics.md`), cardinality guardrails, agent-side CIDR enforcement.

**M5 — MTR Explorer + Time Machine.** Path snapshots/dedupe/diff, hop
enrichment (rdns/GeoIP, off by default), topology time slider + compare,
Explore A/B, **Time Machine** global context, annotations.

**M6 — Investigation + Incidents.** Investigation assembly (timeline,
heuristic correlation, actions rail), `kubectx` K8s events capture,
incidents (save/share), maintenance windows, webhooks.

**M7 — Alerting + polish.** PrometheusRule builder/sync/import, config
export/import, command palette completion, a11y pass, docs, demo
walkthrough extension (breaking-cni.md gains Console steps), README
screenshots.

DoD per milestone: unit tests, e2e in `e2e/` (extend the CI harness — `kind`
via `helm/kind-action@v1` in `.github/workflows/e2e.yaml`, **not** Minikube;
`hack/local-test.sh` runs the same install flow against Minikube for local
iteration, but it is a developer convenience script, not the CI harness;
Playwright UI smoke), helm-lint value sets, docs updated.

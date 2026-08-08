<!--
Status: current
Owner: @EsDmitrii
Source: extracted from DESIGN.md §13, kept current; M3 rewritten from the
as-built implementation (2026-08-06), M4 likewise (2026-08-07) — including
the two shipped shapes that differ from the plan and the correction of a DoD
line that named a Playwright harness this repo never had. M5 written from the
as-built implementation (2026-08-08), with four plan deviations and the
deferral list named rather than reconciled.
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

**M4 — Targets & external checks (delivered).** The `targets`,
`check_definitions` and `check_schedules` tables (DATA.md §5.2) with CRUD at
`/api/v1/targets|checks|schedules`, all gated on
`database.mode=cnpg|external` and `503` otherwise (Decision 13); the Console
**schedule loop** with a stuck-run reaper, a singleton on a **PostgreSQL
advisory lock** and off by default (`console.scheduler.enabled`); diagnostics
v2 — `POST /api/v1/runs` gained
`destinationKind`/`destinationTargetId`/`destinationAddress`, and
`POST /api/v1/runs/{id}/cancel` (204, asynchronous) closed M3's
run-cancellation gap; the **continuous external-check** path end to end
(Console reconciler → controller `WatchExternalChecks` stream → agent), the
agent's `config.checkers.external` allowlist with **agent-side CIDR
enforcement**, and the `kconmon_ng_external_*` metric family as a **new
family** rather than new labels on the peer pipeline (Decision 6); the
per-definition **cardinality guardrail** (`POST /api/v1/checks/projection`,
400 series, fail-open); request **rate limits** (`console.rateLimit.*`,
fail-open on a KV outage, Decision 8); the **Target card** with three real
tabs; and the hand-authored **OpenAPI spec** (`docs/console-api.yaml`) with
generated TS types and a router-walking test. See
`architecture/{DATA,SECURITY,API,CONFIG,WEBSOCKET}.md`, `product/TARGETS.md`
and `product/PAGES.md` §6.4. Chart 1.6.0.

Two shipped shapes differ from the plan and are recorded here rather than
quietly reconciled:

- **There is no `console.externalChecks.*` config block.** The plan called for
  `console.externalChecks.{enabled,reconcileInterval,maxProjectedSeries}`. The
  reconciler instead **shares `console.scheduler.{enabled,tickInterval}` and
  the scheduler's advisory lock**, so one flag and one lock govern the
  schedule loop, the reaper and the reconciler together. Two independent tick
  loops racing for the same fleet would have been two things to reason about
  for no gain, and the Console config is parsed with `KnownFields(true)` —
  emitting Helm values the binary never parses would have been a crashloop at
  worst and a documented lie at best. `maxProjectedSeries` is a **compile-time
  constant of 400** (httpapi Decision 12), reusing the diagnostics runner's own
  400-pair bound so the two cardinality guards tell the same story.
- **The continuous probe cadence is a pair of constants, not a field.** 30s
  interval / 5s timeout, in `internal/console/checks/reconciler.go`. The data
  model has nowhere to put a per-schedule value yet — see the deferral list.

Deferred out of M4, deliberately and by name:

- **`cron` schedules** — `check_schedules.kind` ships `once`, `interval` and
  `continuous`. The column is plain `TEXT` with a comment rather than an enum
  or a `CHECK` precisely so adding `cron` later is code and not a migration
  (Decision 9).
- **Continuous MTR and UDP** — continuous external checks are `tcp`, `icmp`,
  `dns` and `http` only. MTR is a burst of traceroute traffic a continuous
  loop would turn into a standing load, and UDP's loss measurement needs a
  peer that answers, which an arbitrary external address is not. One-shot
  runs of both are unaffected. The reconciler filters these **Console-side**
  and counts them
  (`kconmon_ng_console_external_specs_skipped_total{reason="check-type"}`)
  rather than letting the controller reject them: the controller answers
  `400` for one ineligible spec and applies **nothing**, so a single saved
  `mtr` definition would otherwise cost every agent in the fleet its entire
  assignment on every tick until someone noticed. Node destinations are
  filtered the same way (`reason="destination-kind"`) — that is the peer
  mesh, not an external check.
- **`mtr_path_snapshots`** — path history stays M5, so scheduled MTRs do not
  yet feed a path diff.
- **The Target card's other four tabs** — Alerts, Incidents, Maintenance and
  Audit-per-target are **absent, not empty**, because their tables
  (`alert_rules`, `incidents`, `maintenance_windows`) land in M5–M7.
- **A per-schedule continuous cadence field** — the natural home is
  `check_schedules.interval_ns` with the `continuous` prohibition relaxed to
  mean "the probe interval": one migration-free validation change plus a read
  in the reconciler. Carried forward.
- **`checkers.external.maxTargets` enforcement** — the value is validated at
  agent startup and defaulted to 100, but nothing checks the assignment the
  controller pushes against it. It is a declared intent, not a running
  control, and SECURITY.md §10.2.1 says so.
- **A server-side `?node=`/`?pair=`/`?target=` filter on `GET /api/v1/runs`**
  — the Target card's Runs tab scans recent runs client-side, the same honest
  unindexed scan the Node and Pair cards have done since M3, and says so in
  the UI.
- **A generated runtime API client** — codegen stops at types
  (`web/src/lib/api-types.ts`). The fetch layer stays hand-written: the value
  was in the types agreeing with the wire, not in replacing a wrapper that
  works.
- **Overview's "recent-events" panel** — still an honest placeholder,
  untouched since M2 and still deferred for the second milestone running.
- **`check_results` partitioning** — ADR-001's "partition by month if volume
  warrants" follow-up is still not done.

**M5 — MTR Explorer + Time Machine (delivered).** Three new tables
(`mtr_path_snapshots`, `mtr_hop_enrichment`, `annotations` — DATA.md §5.2) and
six new routes (`/api/v1/mtr/destinations|snapshots|snapshots/{id}`,
`GET`/`POST`/`DELETE /api/v1/annotations`), all gated on
`database.mode=cnpg|external` and `503` otherwise; **path history as a
projection** of the MTR results the Console already persists, deduped by a
SHA-256 over the ordered hop IPs (Decisions 1–2) with the route-changed
alerting primitive
`kconmon_ng_console_mtr_snapshots_total{result="new-path"}`; the **/mtr
Explorer** — three panes, client-side LCS path diff (Decision 3), a
path-changes timeline over the pair's loss series, a per-hop trend read from
snapshot history rather than Prometheus (Decision 13), and a Runner on the
existing `POST /api/v1/runs`; **hop enrichment** as a synchronous-on-read TTL
cache over independently-gated rDNS and MaxMind mmdb sources
(`console.mtr.enrichment.*`, OFF by default, Decisions 4–5) — the milestone's
**one** new Go dependency, `github.com/oschwald/maxminddb-golang/v2`, with zero
transitives; **Time Machine** as one global `?at=` context every read surface
resolves through (Decisions 8–9), with `GET /api/v1/topology?at=` folding
`topology_events` server-side (Decision 6) and the historical matrix rebuilt
from PromQL at `t` (Decision 7); **annotations** as chart markers, card
overlays and inline Live entries (Decision 10); **Explore A/B** compare with a
metric-B-or-time-shift exclusivity; and three new permissions — `mtr:read` and
`annotations:read` reaching `viewer` because path history is telemetry, not
configuration, while `annotations:write` stops at `operator` (Decision 11). See
`architecture/{DATA,API,CONFIG,SECURITY}.md` and
`product/{MTR_EXPLORER,TIME_MACHINE,PAGES}.md`. Chart 1.7.0.

Four shipped shapes differ from the plan and are recorded here rather than
quietly reconciled:

- **The hop table draws `#, Address, Hostname, RTT, Loss` — not
  `loss/avg/best/worst/jitter`.** The plan's column list was written against
  the design doc's aspiration; Decision 2's stored shape is one trace's hop
  payload (`{number, ip, hostname, rttNs, lossRatio}`), deliberately, because a
  running average across weeks is a number nothing ever measured. Four
  permanently dashed columns would have read as missing data rather than as a
  shape that does not exist. The across-time dimension the plan wanted is the
  per-hop trend chart.
- **The MTR routes are `mtr/destinations` + `mtr/snapshots`, not
  `mtr/paths` + `mtr/paths/diff`.** API.md's route sketch predicted a
  server-side diff endpoint; Decision 3 put the diff client-side, so the
  endpoint would have duplicated presentation logic for zero authority gain.
- **`GET /api/v1/matrix` never grew an `at` parameter.** The plan's own
  Decision 7 said the historical matrix comes from Prometheus, and the frontend
  reconstructs it in full from the promql proxy at `t`. The route stays
  live-only.
- **The mmdb mount is an opaque VolumeSource passthrough at a fixed path.**
  `console.mtr.enrichment.geoip.volume` is spliced verbatim into the console
  Pod's `volumes[]` and mounted read-only at `/geoip`; the chart deliberately
  does not model the `{configMap|secret|hostPath|persistentVolumeClaim}` union,
  and setting a geoip path without a volume **fails rendering** rather than
  booting a console with geoip silently off (CONFIG.md).

Deferred out of M5, deliberately and by name:

- **A per-schedule continuous cadence field** — still carried, for the second
  milestone running. The natural home remains `check_schedules.interval_ns`
  with the `continuous` prohibition relaxed to mean "the probe interval": one
  migration-free validation change plus a read in the reconciler
  (CONFIG.md "Continuous probe cadence is not operator-configurable yet").
- **Controller attribution of `topology_changed` events** — the controller
  publishes a *reason* and throws the agent snapshot away
  (`internal/controller/controller.go`), so `node_name`/`agent_id`/`zone` are
  empty on every event. Time Machine's topology fold is coded against the full
  event shape and is therefore structurally complete and **empty in practice**
  until the controller attributes. The API reports this in numbers
  (`eventsFolded`, `unfoldableEvents`) and the Topology page renders those
  numbers rather than an empty cluster. This is the single highest-value
  follow-up in the list: it is the difference between a working feature and a
  correct one nobody can use.
- **Router search-param adoption (`?at=` across in-app navigation)** —
  Decision 9 kept `?at=` on `window.location`, so a `<Link>` drops the param
  from the URL while the context keeps the value. The shareable-link guarantee
  holds for the URL you are on, not across in-app navigations.
- **An enrichment background refresher** — Decision 4: a cache row past its TTL
  re-resolves on the next read that wants it, and an address nobody looks at
  costs nothing. Resolution stays synchronous on the request that missed.
- **Click-to-annotate at a chart's time anchor** — the create affordance takes
  a typed timestamp; clicking the canvas does not set it. There is also no
  edit: M5 is create/list/delete, because a mark is not a document
  (Decision 10).
- **A server-advertised `metricsPrefix` capability** — the historical matrix
  writes `kconmon_ng_*` literally, exactly as `curated-metrics.ts` and the pair
  card already did before M5. A non-default `config.metricsPrefix` therefore
  yields an **empty** historical matrix rather than a mis-attributed one. One
  capability field would fix all three call sites at once; it was named rather
  than smuggled into this milestone.
- **`kubectl-kconmon` trace capture** — the CLI talks to the controller
  directly and never touches the Console, so its traces are **invisible to path
  history** (Decision 1). Routing the CLI through the Console is an M6+
  decision, not an oversight.
- **The `mtr` WebSocket topic** — still rejected with an error frame. The
  Explorer reads path history over REST and needs no push topic, and an allowed
  topic that never delivers would be worse than an honest error.
- **Uniform store instrumentation** — the M5 `mtr`/`annotations` queries are
  observability-instrumented, but M4's `targets`/`checks` queries still are
  not. A ledger line in M4 claimed otherwise; this corrects it.
- **Overview's "recent-events" panel** — still an honest placeholder,
  untouched since M2 and now deferred for the third milestone running.
- **`check_results` partitioning** — ADR-001's "partition by month if volume
  warrants" follow-up is still not done.

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
iteration, but it is a developer convenience script, not the CI harness),
helm-lint value sets, docs updated.

**On UI smoke testing, stated honestly.** Earlier revisions of this line
listed "Playwright UI smoke" as part of the DoD. **No Playwright harness has
ever existed in this repository** — not in `web/package.json`, not in `e2e/`
(which is Go), not in any workflow. The claim was aspirational text that read
as a description of something in place, which is the worst kind of
documentation error: it tells a reviewer a gate exists that would have caught
nothing.

What actually covers the UI today, through M5:

- **Component and page tests with Vitest + Testing Library** (jsdom),
  colocated as `web/src/pages/*.test.tsx` and run by `npm test`. Every page
  shipped since M1 has one — the M4 pages (`targets.tsx`, `target-card.tsx`)
  and the M5 surfaces (`mtr.tsx`, the three `mtr-*` components, `timemachine`,
  `annotations`, and the per-surface `*.timemachine.test.tsx` /
  `*.annotations.test.tsx` files) included.
- **Go e2e against a real `kind` cluster** (`e2e/console_test.go`), which
  exercises the served API and the degraded-mode paths but not the rendered
  DOM.
- **Manual browser smoke**, the M3 precedent: a human loads the pages against
  a local install before the milestone closes.

A real browser-driven smoke suite remains an **outstanding commitment**, not a
delivered capability. It should be named as one until a harness is committed
and running in CI.

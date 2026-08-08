<!--
Status: current
Owner: @EsDmitrii
Source: extracted from DESIGN.md §13, kept current; M3 rewritten from the
as-built implementation (2026-08-06), M4 likewise (2026-08-07) — including
the two shipped shapes that differ from the plan and the correction of a DoD
line that named a Playwright harness this repo never had. M5 written from the
as-built implementation (2026-08-08), with four plan deviations and the
deferral list named rather than reconciled. M6 written from the as-built
implementation (2026-08-08) — six plan deviations, including a narrower
`kubectx` RBAC grant than the plan specified and a console-only ServiceAccount
rather than a widened shared one. M7 written from the as-built implementation
(2026-08-08) — ten plan deviations, two long-running carries closed, and the
FINAL deferral ledger: M7 is the last planned milestone, so that list is what
this project has not built rather than what is queued next.
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
  history** (Decision 1). Routing the CLI through the Console is an M7+
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

**M6 — Investigation Mode + Incidents (delivered).** Four new tables
(`k8s_events`, `incidents`, `maintenance_windows`, `webhooks` — DATA.md §5.2)
and fifteen new routes across four families
(`/api/v1/incidents` with the API's one `PATCH`, `/api/v1/maintenance`,
`/api/v1/webhooks` + `/{id}/test`, `GET /api/v1/k8s-events`), all gated on
`database.mode=cnpg|external` and `503` otherwise; **Investigation Mode** as a
three-pane page whose timeline is assembled **client-side** over eight sources
(Decision 1), each one permission-gated with **zero requests** when denied and
one muted line per absent or bounded source; **correlation v1** as four
arithmetic steps an operator can reproduce by hand — edge-triggered threshold
crossings over a median RTT baseline, onset, a 300 s candidate window, and a
linear proximity decay against documented class weights — with the exported
constants restated verbatim in INVESTIGATION.md and linked from the panel
(Decision 2); **`kubectx`**, the console's first apiserver client, on the
client-go the controller already depends on — **zero new Go dependencies** —
list+watching core/v1 Events and failing **closed** on node events when no
topology vouches for the node (Decision 3); **incidents** as annotations-class
records with a permalink that rehydrates the page from the row rather than from
the URL (Decision 7), surfaced on Overview and on all three object cards;
**maintenance windows** as data and rendering, not suppression, because nothing
evaluates alerts yet (Decision 6); **outbound webhooks** fired on incident
lifecycle only (Decision 5), HMAC-SHA256 over the raw body, a 0s/30s/5m ±20%
ladder, with each endpoint's signing secret sealed under a config-supplied
AES-256-GCM key (Decision 4); and five new permissions — `incidents:read` and
`maintenance:read` reaching every role, their `:write` pair stopping at
`operator`, and `webhooks:manage` admin-ONLY on the `tokens:manage` precedent
(Decision 8). See `architecture/{DATA,API,CONFIG,SECURITY,BACKEND}.md` and
`product/{INVESTIGATION,PAGES}.md`. Chart 1.8.0.

Six shipped shapes differ from the plan and are recorded here rather than
quietly reconciled:

- **Correlation does not bucket events into 30 s windows.** Plan Decision 2(a)
  called for it; the implementation replaced it with **edge-triggered**
  detection on the raw series. Quantizing to 30 s blurs an onset the samples
  already resolve more finely than that, and the whole value of the panel is
  the "N seconds before" number beside each row. What bucketing was there to
  prevent — forty rows for one forty-sample degradation — edge triggering
  prevents better: one entry when the signal crosses, one `info` entry when it
  recovers, and none in between.
- **`PATCH /api/v1/incidents/{id}` breaks this repo's full-replace `PUT`
  convention, on purpose.** Every other mutable resource (M4's targets, checks
  and schedules; M6's own webhooks) takes a full-replace `PUT`. An incident
  evolves under collaboration — one operator pinning findings while another
  writes notes — so a full replace would let the last writer silently discard
  the other's work. The exception is one route, its patchable subset is exactly
  `status`/`notes`/`pinned`, and API.md documents it as an exception rather
  than as a second convention.
- **Zone-pair and cluster scopes are lossy when saved.** The store's scope
  vocabulary is the annotations one (`''` global, node, `src→dst`, target
  name) and has no zone member, so both `zone-pair` and `cluster`
  investigations save with an empty scope and reopen as global. The save
  popover **warns before the write**, not after — an operator finding out at
  save time can retitle the incident to carry what the scope cannot. A related
  edge: a bare-name scope needs `targets:read` to reopen *as a target*, and
  reopens as a node otherwise.
- **The matrix cell's Investigate affordance is a sibling of the cell, not
  inside its tooltip.** The tooltip is `pointer-events-none` (it has to be, or
  it would eat the hover it exists to serve) and a nested `<a>` inside the
  cell's own link is invalid HTML. Placing the affordance beside the cell is
  the only shape that is both clickable and valid.
- **The `kubectx` RBAC grant is narrower than the plan wrote it.** Decision 3
  and SECURITY.md §10.3 both said `events`/`nodes`/`pods` read; the reader
  calls `Events().List` and `.Watch` and nothing else, because the node set it
  filters against comes from the controller's topology API. The chart grants
  `events: list, watch` and no more. Least privilege beat the plan text.
- **A console-only ServiceAccount, rather than a widened shared one.** The
  chart's single ClusterRole is bound to the ServiceAccount the agent DaemonSet
  and the controller Deployment share, and the console set no
  `serviceAccountName` at all — it ran as the namespace `default` SA. Adding
  event read to the shared role would have granted it to every agent pod on
  every node **and still not reached the console**. Chart 1.8.0 adds a second
  SA + ClusterRole + binding, all three rendered only under
  `console.kubernetesContext.enabled`, so the default render is unchanged.

Deferred out of M6, deliberately and by name:

- **The `settings` table** — SECURITY.md §12 promised webhook secrets
  "encrypted at rest (app-level, `settings`-keyed)". That table does not exist
  and is not pinned to a milestone, and inventing a versioned settings store to
  hold one key was scope creep. The key is `console.webhooks.encryptionKey`
  (Decision 4) instead. Rotating it does not re-seal existing rows — an admin
  must `PUT` each endpoint with a fresh secret — which is the cost of the
  shortcut, stated rather than discovered.
- **A webhooks UI.** Endpoints are API-only in M6: there is no Settings page
  and no webhooks page, so declaring one is a `POST` with `webhooks:manage`.
  The navigation tree in PAGES.md §6.2 still lists a Settings page; it is
  design intent.
- **Alertmanager silences from maintenance windows** — the spec's "(optional)
  AM silences". There is no Alertmanager client anywhere in this repository,
  and a window cannot silence what nothing evaluates. It lands with alerting.
- **Alert-fired webhooks** — dispatch fires on incident lifecycle only
  (`incident.created|resolved|reopened`), the one event class M6 itself
  introduces. The `events` column is a `TEXT[]` over a closed set, so widening
  it in M7 is code plus a vocabulary entry, not a migration. **That prediction
  held exactly**: M7 added `alert.fired`/`alert.resolved` with one const-list
  edit and no migration.
- **A delivery-log table** — one row per delivery is unbounded growth for
  marginal value. The outcome lives on the endpoint row (`last_status`,
  `last_attempt`, `failures`), the ledger is the console log, and `webhooks` is
  deliberately **not** a retention table (DATA.md §5.2).
- **The DNS-resolution-change timeline source** — §7.6's bullet list has named
  it since M0 and **nothing has ever recorded it**: no table, no API, no agent
  check that stores a resolution result over time. It was not cut in M6; it was
  never built, and INVESTIGATION.md now says so rather than leaving a bullet
  that reads as shipped.
- **The timeline's "alert fired/resolved" row** — ships **empty and says so**
  (Decision 12): *"Alert state arrives with alerting (M7) — that is a missing
  engine, not a quiet fleet."* Carried as a visible, permanent note rather than
  an absence. **Closed in M7** — and only half of it: fired rows landed,
  resolved rows never will (INVESTIGATION.md).
- **A live WebSocket topic for incidents** — considered as a piggyback on the
  existing hub and **not built**. Incidents are polled: the Overview card and
  the cards' Related-incidents block are TanStack queries, and an incident open
  for three days does not become more legible at minute resolution. Nothing
  about incidents is live in M6.
- **A maintenance bar on the Node card** — Pair, Target and Explore have one;
  Node does not. Not a decision about node scope, just a gap.
- **Controller attribution of `topology_changed` events** — carried from M5,
  untouched by M6, and named here as the single highest-value follow-up in this
  file. **M7 closed it** — see the M7 section below. It took two milestones of
  carrying because the fix was not "add a field": a single event cannot name
  three evicted nodes, so the emission shape had to become one event per
  subject before the field had anywhere to go.
- **A per-schedule continuous cadence field** — carried for the third
  milestone running.
- **`check_results` partitioning** — still not done.

**M7 — Alerting + polish (delivered).** One new table (`alert_rules`,
migration `00007`), ten new routes, two new permissions (**25 total**), two new
webhook events, two new pages, and chart **1.9.0**. The console **manages**
Prometheus alert rules; Prometheus **evaluates** them, and nothing in the
console ever decides that an alert fired.

What shipped:

- **The builder and the renderer.** Seven template kinds plus `raw`, rendered
  by a pure, total function into deterministic PromQL. Every constant is named
  in `internal/console/alerting/render.go` and pinned by byte-exact goldens;
  ALERTING.md restates them and says the code wins.
- **The reconciler.** Every enabled rule renders into **one** `PrometheusRule`
  object, server-side-applied every 60s (jittered ±20%) and on every write.
  Drift is **recorded, then fixed** in the same pass. Failure is a per-rule
  status with a closed cause class, never a crash.
- **Foreign rules and adoption.** `GET /alert-rules/foreign` lists what the
  console did not write; `POST /alert-rules/import` **copies** one into builder
  rows and **never mutates the foreign object** — so until an operator removes
  one copy, the same alerts exist twice. Said plainly in the API, in the UI and
  in ALERTING.md.
- **Alert state and alert webhooks.** `GET /api/v1/alerts` projects the firing
  set; `alert.fired`/`alert.resolved` deliver on a polled edge detector with
  baseline-on-boot, freeze-on-failure and no leader election.
- **Config export/import.** Versioned bundle v1, admin-only, dry-run first.
- **The command palette** (`⌘K`), the **Settings** page, the **Alerting** page,
  firing alerts on Overview, an alert row on the Investigate timeline.

Deviations from the plan, each on evidence:

- **No Prometheus parser dependency.** Decision 2 called for validating with
  `prometheus/prometheus` as a library. It was not taken. Validation is **render
  goldens plus a live preview that runs the expression against the real
  Prometheus** — which is a stronger answer than a parser gives, because the
  thing that will evaluate the rule is the thing that vets it. ALERTING.md §3.
- **`cert-expiry` was DROPPED**, with grep evidence: no certificate-expiry
  metric family exists anywhere in this codebase, so the template would have
  rendered an alert that can never fire. Pinned by `ValidKind` and the
  golden-coverage test. The migration's `CHECK` still lists the value, so a
  legacy row lists and opens honestly; the builder just cannot create one.
- **`agent-missing` is a controller-side count comparison** and
  **`external-target-down` is a fail RATE**, both because the per-node /
  success-series shapes the plan assumed do not exist in the exporter.
- **`alert-editor` was granted `alerts:manage`** — a coordinator decision over
  the uniform "statement writes stop at operator/admin" groove. The builtin had
  sat as a placeholder since M3 waiting for exactly this permission; a role
  named `alert-editor` that cannot edit an alert rule breaks its promise on
  first click, and renaming it would break existing `role_bindings` rows and
  `auth.anonymous.role` references. Pinned so narrowing it back is a conscious
  diff.
- **`GET /alert-rules/{id}/state` was not built.** Decision 6 sketched it;
  `/alerts?managedOnly=true` covers it set-wise, and the decision was amended
  rather than shipping a second read of the same upstream call.
- **`GET /api/v1/alerts` answers `200` where `/matrix` answers `503`** on a
  missing Prometheus. Deliberate, and the one intentional inconsistency in the
  API's degradation rules — API.md and ALERTING.md §7.1 both state why.
- **Adoption skips an unparseable `for`** rather than importing it as `0`. A
  misread `5m` silently becoming fire-instantly is a 3am page nobody asked for.
- **No leader election on the alert watcher.** N replicas deliver N copies of
  every edge; a lock would trade duplicates for **missed** edges when the holder
  dies. The dedupe key is documented for receivers.
- **The palette does not jump to an arbitrary node, target or pair.** The
  roadmap called the palette "completion"; it was built from scratch, and
  object jumping needs a live object search, not a static registry. PAGES.md
  §6.2 says so rather than implying it.
- **`console.webhooks.alertPollInterval` was unreachable from Helm** until
  chart 1.9.0 — the binary had the key and the default worked, but no value
  rendered it. Closed here.

Two carries closed, both long-running:

- **Controller attribution of `topology_changed`** — carried from M5 through
  M6 as "the single highest-value follow-up in this file", **closed in M7**. All
  four emission sites attribute, one event per subject, carrying `nodeName`,
  `agentId` and `zone`. Deregistration read the placement *before* the map
  delete; eviction had already built the list and thrown it away. Pre-M7 rows
  still fold to an honest empty and shrink with retention.
- **WebSocket per-topic authorization** — M3 follow-up #10, carried to M7. `/ws`
  is now the API's only `anyOf` route (`events:read` OR `runs:read`), with a
  per-connection topic authorizer.

Also in M7: `values.schema.json` closed at 44 chart-owned levels (which
immediately exposed four keys the templates had always used and the schema
never declared), two new chart-test profiles for the auth modes, and a **real
nil-pointer class fixed** in the CNPG block — a `null` override of
`console.database.cnpg` or any of its sub-blocks crashed rendering.

### The final deferral ledger

M7 is the last planned milestone, so this list is not "deferred to M8" — it is
**what this project has not built**, stated once, in one place.

- **OIDC provisioning items** — group-to-role auto-provisioning and
  just-in-time user creation beyond the virtual-user model. Carried since M3.
- **Alertmanager silences from maintenance windows.** There is still no
  Alertmanager client anywhere in this repository. M6 deferred this on "a
  window cannot silence what nothing evaluates"; M7 built the evaluator's
  *input* and still did not build the client. A maintenance window renders on
  charts and explains a degradation; **it suppresses nothing**.
- **The `settings` table** — declined twice now, by M6 (for one webhook key)
  and by M7 (for export/import, which reads the collections that exist). It is
  **not pinned to any milestone**. The Settings page's About section renders
  only what `GET /api/v1/config` already serves, and where the API serves
  nothing — retention numbers — the page says so and names
  `console.retention.*` rather than inventing a value.
- **The `layouts` table** — saved topology layouts and pinned pairs. Named in
  DATA.md §5.2 since M0, never pinned to a milestone, never built.
- **`check_results` partitioning** — carried through M4, M5, M6 and M7. The
  table has the growth profile that wants it and `ON DELETE CASCADE` plus the
  retention sweep have been sufficient at the documented scale target.
- **A browser-driven smoke suite.** This is a **commitment**, not a gap in
  scope — see the honest note below. Vitest + jsdom covers components and
  pages, Go e2e covers the served API against a real `kind` cluster, and a
  human still loads the pages before a milestone closes.
- **The DNS-resolution-change timeline source.** Named in §7.6 since M0 and
  never built, because nothing in the fleet records a resolution result over
  time: no table, no API, no agent check. It is design intent in that bullet
  list, and INVESTIGATION.md says so.
- **A live WebSocket topic for incidents** — and now for alerts too. Both are
  polled. An incident open for three days does not become more legible at
  minute resolution, and the firing set is a read the Overview card refetches.
- **A maintenance bar on the Node card.** Pair, Target and Explore have one;
  Node still does not (`web/src/pages/node-card.tsx` has no maintenance
  affordance). Carried from M6 and not picked up by M7's polish pass. Not a
  decision about node scope — still just a gap.
- **A per-schedule continuous cadence field** — carried for the fourth
  milestone running.
- **A delivery-log table for webhooks.** The outcome stays on the endpoint row.
- **`GET /alert-rules/{id}/state`** — see the deviations above; covered
  set-wise by `/alerts?managedOnly=true`.
- **A server-advertised `metricsPrefix` capability.** M7 fixed the prefix
  problem **server-side** — the renderer is built from `config.metricsPrefix`
  and a hardcoded-default renderer is impossible to construct — but the
  frontend still writes `kconmon_ng_*` literally in `matrix-promql.ts`,
  `curated-metrics.ts` and the pair card, and the Grafana dashboards in
  `dashboards/` do the same. On a custom prefix those surfaces come back
  **empty rather than wrong**, which is the visible failure, not the silent
  one. Named in the M5 carry-forwards and still open.
- **Alert rule scoping on the Investigate timeline.** `/api/v1/alerts` takes no
  scope filter, and filtering client-side on labels would silently hide rules
  grouped differently from the way a scope is expressed. The source is
  deliberately unscoped; flagged for the QA phase rather than guessed at.

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

What actually covers the UI today, through M7:

- **Component and page tests with Vitest + Testing Library** (jsdom),
  colocated as `web/src/pages/*.test.tsx` and run by `npm test`. Every page
  shipped since M1 has one — the M4 pages (`targets.tsx`, `target-card.tsx`)
  and the M5 surfaces (`mtr.tsx`, the three `mtr-*` components, `timemachine`,
  `annotations`, and the per-surface `*.timemachine.test.tsx` /
  `*.annotations.test.tsx` files) included, plus the M6 investigation surfaces
  and the M7 pages (`alerting.tsx`, `settings.tsx`, `commands.ts`).
- **Go e2e against a real `kind` cluster** (`e2e/console_test.go`), which
  exercises the served API and the degraded-mode paths but not the rendered
  DOM. The Prometheus Operator is **not** installed in the `kind` harness, so
  any alerting coverage there has to apply the `PrometheusRule` CRD manifest
  itself.
- **Manual browser smoke**, the M3 precedent: a human loads the pages against
  a local install before the milestone closes.

A real browser-driven smoke suite remains an **outstanding commitment**, not a
delivered capability. It should be named as one until a harness is committed
and running in CI.

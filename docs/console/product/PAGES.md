<!--
Status: draft
Owner: @EsDmitrii
Source: extracted from root DESIGN.md §6.2–6.4 in M0 (2026-07-14); §6.4 and
§7.x Diagnostics updated/added from the as-built M3 implementation
(2026-08-06): web/src/pages/{diagnostics,run-detail,node-card,pair-card}.tsx,
web/src/hooks/use-run.ts.
This document is the source of truth for Pages & Navigation. Update it (and the ADRs) in the same PR as any deviation.
-->

# Pages & Navigation

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

**Node and Pair cards shipped v1 in M3** (`web/src/pages/node-card.tsx`,
`pair-card.tsx`); **the Target card shipped in M4**
(`web/src/pages/target-card.tsx`), once `targets` existed to have a card at
all. What each actually ships, versus the fuller design above, per this doc's
own "any deviation updates the doc" rule:

- **The shared right rail is real**, `RecentChanges` (`web/src/components/recent-changes.tsx`),
  scope-pinned per card (a bare node name for Node, `` `${src}→${dst}` `` —
  literal U+2192, matching `internal/console/events/live_event.go`'s
  `pairScope` — for Pair) and backed by `GET /api/v1/events?scope=`. It
  degrades honestly when `database.mode=disabled`: a one-line note, no
  request made, live events keep arriving.
- **Node card**: header health%/tier reuses the Topology page's own
  worst-outbound-fail-ratio semantics, so a node's badge never disagrees
  with what Topology already shows. Overview = agent identity + a
  per-destination breakdown table (protocol-selectable). Diagnostics =
  "runs touching this node".
- **Pair card**: header shows both directions' fail ratios side by side
  (fixed to TCP for the header stats). Overview = per-protocol RTT p95
  series via the PromQL proxy (all three protocols at once, not yet
  selectable). Diagnostics = the last run for this pair, plus a "Run check"
  action gated on `runs:create` that posts and navigates to the new run's
  permalink.
- **Diagnostics tabs are an honest, unindexed scan, not a server-side
  filter.** `GET /api/v1/runs` carries no source/destination filter and its
  list rows carry no node fields at all — only `GET /api/v1/runs/{id}`
  does. Both cards' Diagnostics tabs fetch the most recent 20 runs' details
  client-side and scan them, and say so in the UI copy ("an older run may
  exist but is not shown here") rather than silently claiming completeness.
  A server-side `?node=`/`?pair=` filter on `GET /api/v1/runs` is a
  plausible follow-up, not yet built.
- **Quick actions** are narrower than the full design: "Investigate this
  pair" and MTR actions wait on Investigation (M6) and MTR (M5)
  respectively; only "Run check" exists today.
- **Target card ships THREE real tabs and no placeholders** (M4 Plan
  Decision 17):
  - **Checks & Schedules** — the definitions probing this target and their
    schedules, gated on `checks:read`.
  - **History** — the target's `kconmon_ng_external_*` series through the
    PromQL proxy: probe latency p95 per source node. This is the only tab
    that needs `console.prometheus.url`; it degrades to an explanatory note
    when Prometheus is unconfigured, and the other two tabs do not depend
    on it.
  - **Runs** — diagnostics runs that touched this target, by the same honest
    client-side scan of the most recent runs the Node and Pair cards use, and
    labelled as such.

  The other four designed tabs — **Alerts, Incidents, Maintenance and
  Audit-per-target** — are **absent, not empty**: their backing tables
  (`alert_rules`, `incidents`, `maintenance_windows`, and a per-target audit
  view — DATA.md §5.2) land in M5–M7. An absent tab is honest; an empty one
  promises something that does not exist.

  The whole card requires `console.database.mode=cnpg|external`. With the
  database disabled it renders a one-line explanation rather than an error:
  targets are configuration, and configuration gets no in-memory fallback.

### 7.x Diagnostics

Delivered in M3 (`web/src/pages/diagnostics.tsx`, `run-detail.tsx`). Two
halves: a run-creation form plus history list at `/diagnostics`, and a
per-run permalink page at `/diagnostics/runs/{id}`.

- **Form**: source/destination node pickers (default "all nodes", sourced
  from `useTopology`), a check-type `Segmented` control
  (`CHECK_TYPES` — the same set the controller's diagnostics API accepts),
  plane fixed to `pod` (the only plane that exists, API.md). Submitting
  `POST`s `checks.Spec` and navigates straight to the new run's permalink
  (`goTo(/diagnostics/runs/{id})`) — the run detail page is the only place
  progress is ever shown, not a modal or inline expansion on the form
  itself.
- **Bounds, mirrored client-side from the server's own guard**: `MAX_PAIRS =
  400`, matching `checks.maxPairs` (`internal/console/checks/checks.go`)
  exactly. Two counts are computed and shown, not one: `estimatePairCount`
  (the deduplicated, self-pair-excluded count a submitter actually cares
  about) and `estimateRawPairCount` (`len(sources) * len(destinations)`,
  mirroring `checks.Plan`'s own **first** gate, which runs before dedup or
  self-pair exclusion). The two can disagree — 20 sources and 21
  destinations sharing 20 names have a raw product of 420 but a
  self-excluded count of exactly 400 — and the raw count is what actually
  gates the submit button, because gating on the friendlier deduplicated
  number alone would let a selection through that the server still 422s
  with `ErrTooManyPairs`. The client-side check is a UX preview only; the
  server remains the sole real enforcement point.
- **Permission gating**: creating a run requires `runs:create`
  (`useAuth().can("runs:create")`); a caller without it sees an explanatory
  card in place of the form but still sees run history below it — reading
  history only needs `runs:read`.
- **History list**: `GET /api/v1/runs`, paginated behind the same opaque
  keyset cursor convention as Live/Events/Audit ("Load older", disabled once
  `nextCursor` is empty).
- **Permalink page** (`/diagnostics/runs/{id}`, `runIdFromPath` reads
  straight off `window.location.pathname`, the same convention `pages/login.tsx`
  uses for `?returnTo=`): works identically whether reached by navigating
  from the form or as a cold load of a bookmarked/shared link — nothing but
  the REST payload is available on a cold load, ever.
- **Socket+poll hybrid** (`useRun`, `web/src/hooks/use-run.ts`): `GET
  /api/v1/runs/{id}` polled every 5s (`RUN_POLL_MS`) until the run reaches a
  terminal status, **plus** the run's `run:{id}` WebSocket topic
  (WEBSOCKET.md "Ephemeral `run:{id}` topics") for live per-pair progress
  when available. REST wins on overlap for any pair it has actually
  recorded — a persisted result is authoritative over a frame this tab
  merely observed in transit — and REST alone is what drives the page to
  completion when the socket contributes nothing at all: a direct load of
  the permalink, a replica this tab is not streaming the run from (the
  single-replica limitation, same WEBSOCKET.md section), or `realtime`
  being off entirely. The socket subscription only opens once the first
  REST response has landed and the run is not yet terminal, and closes for
  good once the topic's `closed` control frame arrives or the run turns
  terminal via polling.

### 7.8 Live

Delivered in M2 (`web/src/pages/live.tsx`). The Live page is the event feed for
the realtime pipeline: it subscribes to the `live` WebSocket topic
([WEBSOCKET.md](../architecture/WEBSOCKET.md)) and renders newest-first.

- **Virtualized list** (`@tanstack/react-virtual`): only visible rows are in the
  DOM, so a busy feed stays responsive (SECURITY.md §12 "Live feed
  virtualized"). Rows are a fixed 44 px, so the virtualizer never measures.
- **Filters**: event type, severity, and a free-text substring match on `scope`.
  Applied client-side over the in-memory ring — they never change the
  subscription, so toggling one is instant and loses nothing. The type/severity
  unions are ours, not a promise from the wire: an event whose type the UI does
  not know still renders, labelled with its raw string, and is only hidden when a
  type filter that does not match it is active. The guard is on the *picker* —
  an unrecognised value selected in the `<select>` falls back to "all" rather
  than filtering the feed down to nothing forever.
- **Pause / resume**: pausing freezes the rendered list while the subscription
  stays up and arrivals queue; the resume button shows the buffered count, so
  "how much did I miss" is answered before you click. Whatever arrived in the
  frame of the click is drained into the feed first, not filed as
  arrived-during-pause.
- **Bounded ring**: 2000 events, oldest dropped, and the cap is stated in the UI
  copy — a feed that silently forgets is worse than one that says it forgets.
- **Ordering**: `(timestamp, seq)` descending, timestamp leading. Seq alone would
  be wrong across a controller restart, which takes the counter back to 1 and
  would file the newest event at the very bottom where the cap then eats it.
  Arrivals are merged once per animation frame, not once per event.
- **Missed-events notice**: counts holes in the controller's own event numbering
  *plus* events this tab discarded itself when arrivals outran it (a hidden tab
  gets no frames). The two numbers are separated in the tooltip. See
  WEBSOCKET.md "What the envelope sequence cannot tell you" for why the envelope
  seq cannot serve as this signal.
- **Scroll anchoring**: rows prepend, so the row at the top of the viewport is
  re-anchored by identity after every merge. Anchoring by row count silently
  stops working once the ring is full, which is exactly when it matters.
- **Realtime badge**: the shared `RealtimeBadge` renders **Live** or **Delayed
  data** — those two states and nothing else — from the same signal the Matrix
  page uses. Until `GET /api/v1/version` has answered, the page renders a
  separate neutral **Connecting…** badge in its place rather than letting an
  unanswered poll read as "delayed".
- **A11y**: the feed is `role="log"` with `aria-live` pinned **off** — a stream
  that announces every arriving row talks over the operator. The counts line and
  the missed-events notice carry their own `role="status"`.
- **Motion**: plain CSS transitions. Framer Motion is permitted for this page by
  [BACKEND.md](../architecture/BACKEND.md) §4.2 but is not a dependency.

The page subscribes unconditionally, even when this replica advertises no
`events` capability: another replica's events still arrive over the Valkey bus.
When the capability is absent it says so in a status card rather than looking
broken — the feed is not broken, it is unfed.

Three honest limits in M2, one of them since closed. **Scrollback landed in
M3**: the feed now loads its initial history from `GET /api/v1/events`
(backed by the durable `topology_events` table, DATA.md §5.2), the same
opaque keyset-cursor "Load older" convention as Diagnostics' run history and
`GET /api/v1/audit`; the live stream still fills in from there forward. The
other two limits remain as of M3: the feed's `check_observed` entries are
on-demand diagnostic completions, not continuous background probes — those
never reach the controller (see [WEBSOCKET.md](../architecture/WEBSOCKET.md)
"Payloads"). And `topology_changed` rows always read scope `cluster`, because
the controller does not yet attribute a registry change to a node. Time
Machine scrollback around `t` (§6.3) is a different feature — a global `?at=`
time context, not per-page history — and still waits for M5 even though its
data source (`topology_events`) now exists.

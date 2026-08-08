<!--
Status: draft
Owner: @EsDmitrii
Source: extracted from root DESIGN.md §6.2–6.4 in M0 (2026-07-14); §6.4 and
§7.x Diagnostics updated/added from the as-built M3 implementation
(2026-08-06): web/src/pages/{diagnostics,run-detail,node-card,pair-card}.tsx,
web/src/hooks/use-run.ts. §6.3, §7.5 and §7.x Annotations written from the
as-built M5 implementation (2026-08-08): web/src/lib/{timemachine.tsx,
annotations.ts}, web/src/components/{timemachine-bar,annotations}.tsx,
web/src/pages/{mtr,topology,live,explore}.tsx. §7.11, §7.12, the §6.2 palette
block and the §6.3/§6.1 as-built corrections written from the as-built M7
implementation (2026-08-08): web/src/pages/{alerting,settings,overview}.tsx,
web/src/lib/commands.ts, internal/console/events/live_event.go.
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
├── Alerting             rule list + builder + foreign rules (§7.11)
├── Console              PromQL dev-tools
└── Settings             webhooks, export/import, read-only deployment info (§7.12)
```

**The Settings line above is narrower than the M0 draft promised**, and the
difference is recorded rather than quietly corrected: `auth`, `RBAC`,
`retention` and `maintenance` are **not** on the page. RBAC has an API
(`/api/v1/rbac/*`) and no UI; retention is `console.database.retentionDays`, a
Helm value with no runtime setter; maintenance windows are created where they
are used, on Explore's global-scope bar. See §7.12.

**Command palette (`⌘K` / `Ctrl-K`) — delivered in M7.** Hand-rolled
(`web/src/lib/commands.ts` for the registry and the scoring,
`web/src/components/command-palette.tsx` for the overlay), no palette or fuzzy
library. What it holds:

- **Navigation** — one entry per nav item, built from `web/src/nav.ts` itself,
  so a label or a description can never drift away from the sidebar. The query
  matches the description too, not only the label.
- **Actions**, each a deep link to the page that already owns the affordance,
  not a form inside the overlay: run a diagnostic check (`/diagnostics`), start
  an investigation (`/investigate`), create an alert rule (`/alerting`),
  declare a maintenance window and add an annotation (both `/explore`, where
  the global-scope bars live).
- **View** — switch theme, and the Time Machine pair: *Return to Live* while
  engaged, and while Live an entry that OPENS the Time Machine picker. The
  palette deliberately does not engage at an instant of its own choosing —
  picking "an hour ago" for you would be inventing the answer to the only
  question that matters.

Gating follows the console-wide split: permissions **hide** an entry
(`alerts:manage`, `maintenance:write`, `annotations:write`, `runs:create`), the
Time Machine **disables** the create actions in place with a "Live only" tag.
Ranking is word-boundary-prefix > substring > keyword, every word of a
multi-word query must match, ties broken by title.

**Not as originally drafted, and worth saying plainly:** this is a registry the
palette OWNS. The sidebar links, the page buttons and the annotation /
maintenance bars were not migrated onto it, so an action does *not* get a
palette entry "for free" — each one is listed here by hand. Jumping to an
arbitrary node/target/pair is also absent: that needs a live object search, not
a static registry, and it is not in M7. What the palette does share is
navigation, which is generated from `nav.ts`.

### 6.3 Time Machine (global time context)

**Delivered in M5.** A top-bar control with two states, **Live** and
**@ timestamp**, held in one React context (`web/src/lib/timemachine.tsx`)
mounted in the AppShell, that every read surface resolves through. The full
contract — strict RFC 3339 parsing, seconds truncation, `pushState`/`popstate`,
the two hooks, and the three named limitations — is
[TIME_MACHINE.md](TIME_MACHINE.md). What matters at page level:

- Prometheus reads become instant/range queries evaluated at/around `t`.
- Topology is reconstructed from `topology_events` up to `t`, server-side.
- Matrix is rebuilt from PromQL at `t` (`GET /api/v1/matrix` stayed live-only);
  the Live feed becomes a scrollback **ending** at `t`; object cards show
  state-as-of-`t` with "Recent changes" bounded to `≤ t`; Explore anchors both
  its A and B legs at `t`.
- Mutating actions are disabled — through one hook, `useWritesDisabled()` —
  with the amber banner as the **single** explanation, no per-button tooltips.
  Permission **hides** an affordance and Time Machine **disables** it: two
  signals that compose rather than collapse into one greyed-out button meaning
  either.
- The state is in the URL (`?at=`), so a Time Machine view is shareable — for
  the URL you are **on**. A `<Link>` to another page drops the param from the
  URL while the context keeps the value, because Decision 9 deliberately did
  not adopt router search params.

**The Topology page's historical view reconstructs a real cluster now — M7
closed the carry.** Through M6 the controller published `topology_changed` with
a reason and no node or agent identity, so a fold over its events reconstructed
nothing, and this section said so. M7 attributes every change at all four
emission sites — register, zone update, deregister and stale eviction — as
**one event per affected agent** carrying `nodeName`, `agentId` and `zone`. A
single event cannot name three evicted nodes, which is exactly why the old fold
never worked.

What the page still says out loud, and should:

- **Events written by an earlier controller are unfoldable.** They carry a
  reason and nobody's name, they cannot be repaired retroactively, and they age
  out with retention. The page reads the server's own
  `eventsFolded`/`unfoldableEvents` counters and reports both, so a thin
  historical view is legible as "the controller was not recording this yet"
  rather than as "the cluster was empty". That counter shrinks on its own.
- The copy blames the **age of the events**, not the running controller. The
  pre-M7 wording would have sent an operator hunting a bug that is already
  fixed, and a pin asserts the old phrasing is gone.
- The fold's zone rule: a **stated** zone wins, an **omitted** one leaves the
  known zone alone (pre-M7 rows have no zone key at all, and overwriting with
  `""` would blank lanes at random), and a removal clears it so a rejoin starts
  fresh.
- A separate card still appears when the fold hit its 100 000-row guard
  (`truncated`), because a partial fold is a wrong fold.
- `podIP` is still never recorded by any event type and comes back empty on
  every folded entry.

Implementation note: this is why §4.1 persists `topology_events` — the
controller only knows *now*.

### 6.1 Overview — the placeholders, and which one is left

The Overview shipped in M1 with three panels drawn as honest placeholders
rather than fabricated rows. M6 resolved two of them; **M7 resolved the last
one and `LaterMilestone` is now deleted from the page.**

- **Recent events** now reads `GET /api/v1/events`, the API that had existed
  since M3. The deferral chain ends here: the panel was carried as "still an
  honest placeholder" through the M2, M3, M4 and M5 deferral lists, every time
  because the milestone had something with a harder dependency to build, and
  never because the data was missing. Four milestones is exactly how long a
  cheap panel survives when nothing forces it — worth recording as a process
  observation, not just a changelog line.
- **Open incidents** is new in M6: the newest five still-open incidents, each
  row a permalink (`/investigate?incident={id}`). There is no incident *page* —
  the permalink hydrates Investigate from the saved row.
- **Firing alerts is real in M7.** It reads `GET /api/v1/alerts` with **no**
  `managedOnly` filter — this is the fleet's morning view, and an alert somebody
  else wrote is still firing at you. Unmanaged rows carry a badge and never
  link anywhere the console cannot go. Sorted by severity, ties broken
  **oldest-first**, so the alert nobody has dealt with does not drift off the
  bottom. Capped at 8 with the remainder stated rather than silently dropped.
  Four honest states, and they are four because "nothing is firing" and "nobody
  is watching" are different sentences: Prometheus unconfigured (`200`,
  `promConfigured: false`), Prometheus failing (the `502` detail, verbatim),
  nothing firing, and rows. **Pending alerts are excluded** — the same line the
  webhook contract draws, because inside a rule's `for` window nothing has
  fired.
  Each row links into `/investigate` **only when its labels support a scope**,
  through a pure `scopeFromAlertLabels`: a destination with no source is
  deliberately *not* treated as a node scope, because it would scope the
  investigation to the wrong end of the pair.

All three panels are **fully gated**: no permission means no request and a muted
one-line note (`incidents:read`, `events:read`, `alerts:read`), and a
database-less console says so instead of erroring.

### 7.6 Investigate — the entry contract

**Delivered in M6.** Two ways in, and they are deliberately different shapes:

| Entry | URL | Authority |
| --- | --- | --- |
| a card action, a matrix cell, or the page's own form | `/investigate?kind=&scope=&from=&to=` | the **URL** |
| an incident permalink | `/investigate?incident={id}` | the **incident row** |

The scope form is `kind` (`pair`, `node`, `target`, `zone-pair`, `cluster`)
plus a `scope` string — a bare name, or `src→dst` joined with **U+2192**, the
same arrow `internal/console/events/live_event.go`'s `pairScope` uses. That is
not cosmetic: a pair investigation filters `GET /api/v1/events` on exact scope
equality, so a hyphen instead of the arrow opens an investigation of a node
that does not exist, with an empty timeline reading as a quiet fleet. One
writer, `buildInvestigateURL`, exists so four call sites cannot spell it four
ways, and it composes the parser's own inverse so the round trip is a
property-tested invariant rather than a convention. `scope` is **omitted** for
the cluster kind (there is nothing to name) and `from`/`to` are RFC 3339.

**An incident permalink carries the incident id and nothing else.** Scope and
range come from the row, because a link that also spelled them could disagree
with the incident it names after a single edit. Saving an investigation
therefore *rewrites* the URL to `?incident=`, dropping the four scope
parameters. Re-scoping while in incident mode **leaves incident mode**, on
purpose: the alternative is a URL that names an incident while showing
something else.

Two documented lossy edges of that contract:

- A bare-name scope needs `targets:read` to be reopened **as a target** — with
  only node visibility the same string reopens as a node.
- `zone-pair` and `cluster` both save as an empty scope string, because the
  store's scope vocabulary has no zone member. The save popover **warns before
  the write**, not after.

### 7.5 MTR

Delivered in M5 at `/mtr`, replacing the stub — three panes, a diff view, a
Runner segment, per-hop enrichment and a per-hop trend chart. It has its own
source-of-truth document: [MTR_EXPLORER.md](MTR_EXPLORER.md).

### 7.x Explore — A/B compare

Delivered in M5 as a **dedicated Compare panel above the curated grid**, not as
a second control on all five curated cards: five copies of the same two
dropdowns would have made the page about comparing rather than about looking.

Two **mutually exclusive** modes, with the exclusivity stated in the copy
rather than discovered by trying: B is either a second curated metric, or the
same metric A shifted against its own past (1h / 24h / 7d). "B is UDP loss AND
B is yesterday" has no single meaning, so it is not offered.

Shift math is pinned: `end_B = end_A − shift`, same window length. The fetched
leg's timestamps are then re-offset **forward** by the shift for drawing, so
the two lines overlay instead of B sitting a day to the left. B is drawn in a
muted dashed palette and captioned when the two metrics' units disagree. An
unshifted B shares the curated card's own request — the shift only enters the
query key above 0 — so the default state adds no traffic. Both legs anchor
through Time Machine.

### 7.x Annotations

Delivered in M5 as an overlay on existing surfaces rather than a page of its
own — an annotation is a mark on something, not a thing to browse.

- **Charts** (Explore, object cards) draw annotations in the visible range as
  echarts `markLine` (instant) / `markArea` (range), from pure geometry helpers
  in `web/src/lib/annotations.ts` that are unit-tested independently of the
  chart.
- **Live** shows global annotations inline at their timestamp, read-only.
- **Scope is never absent from a surface's fetch**, and `?scope=` present-but-
  empty means *global only* — `""` is a real scope value, so the code tests
  `scope !== undefined` rather than truthiness. Explore reads global only;
  cards read the object's scope and global as two legs.
- **Create** is an affordance beside the chart, hidden without
  `annotations:write` and disabled under Time Machine. **Delete** is a list row
  in a popover rather than a canvas tooltip — keyboard-reachable and testable,
  where a canvas hit-target is neither.
- Two things the design implies that did **not** ship: clicking a chart does
  not set the annotation's time (it is typed), and there is no edit — M5 is
  create/list/delete, because a mark is not a document (Decision 10).

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
- **Quick actions**, as of M6: **"Investigate this" shipped** on all three
  cards (`InvestigateLink`, `web/src/components/investigate-entry.tsx`) and on
  every matrix cell, and each card also grew a **Related incidents** block
  (`RelatedIncidents`, gated on `incidents:read`, scanning the newest 50 and
  matching on scope). The card's MTR actions were **not** added in M5 or M6 —
  the MTR Explorer has its own Runner (MTR_EXPLORER.md §7.5) and a card-level
  shortcut into it was not built.
- **Maintenance bars landed on Pair, Target and Explore — not on Node.**
  `MaintenanceBar` (`web/src/components/maintenance.tsx`) renders the declared
  windows for the surface's scope plus a create affordance, and
  `maintenanceOverlaySeries` draws them as `markArea` on the same charts
  annotations already mark, differentiated by a dashed border and a fainter
  fill rather than by colour alone (asserted from both sides, since one shared
  colour token makes colour-only differentiation impossible to verify). The
  fetch is **zero-request without `maintenance:read`** — a stricter gate than
  the annotations twin, and an M6 constraint rather than an accident. The Node
  card was left out and it is a real gap, not a decision about node scope:
  MILESTONES.md carries it in the M6 deferral list.
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
  Audit-per-target** — are **absent, not empty**: an absent tab is honest; an
  empty one promises something that does not exist. M6 did not turn any of them
  into a tab, but it did surface two of them **in the card body**: a Related
  incidents block and a maintenance bar, both scoped to the target's name. That
  is deliberate — an incident and a window are context you want beside the
  charts, not a place you navigate to. **Alerts** still waits on `alert_rules`
  (M7), and a per-target audit view was never built.

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
other two limits remain as of M5: the feed's `check_observed` entries are
on-demand diagnostic completions, not continuous background probes — those
never reach the controller (see [WEBSOCKET.md](../architecture/WEBSOCKET.md)
"Payloads"). The second limit **closed in M7**: `topology_changed` rows carry
`nodeName`, `agentId` and `zone` from all four emission sites, one event per
affected agent, and the feed's scope is now that node name — `cluster` is what
is left when the row names nobody, which is what a pre-M7 row does (§6.3). Old
rows keep their old shape and age out with retention.

**Time Machine scrollback landed in M5.** Engaging `?at=` turns this feed into
a scrollback *ending* at `t`, by passing `to=t` to the same
`GET /api/v1/events` the M3 scrollback already used — the events API's time
filtering was already there end to end, so this cost no server change at all.
Global annotations render inline at their timestamps in the same feed.

### 7.11 Alerting

**Delivered in M7.** Rule list, builder, foreign rules, import report.
[ALERTING.md](ALERTING.md) is the source of truth for what a rule *is*; this is
what the page does with it.

**Floor: `alerts:read`.** Without it the page renders a `PermissionCard` and
issues **zero** requests — not an empty table behind a spinner. Above the
floor, `alerts:manage` is the write split: `viewer` gets a read-only page where
even the per-row enabled checkbox is downgraded to a `Badge`, because a
checkbox that refuses to move is worse than a label that never claimed to.

**The builder mirrors `render.go`'s `kindSchemas` field for field**, and the
mirroring is cited in the code so a template change has one obvious second
place to visit. Two consequences worth stating:

- **`cert-expiry` is absent from the create form** but a legacy row **lists
  fine** and opens the builder with a named honest note. The lookup is total —
  an unknown kind does not throw.
- **One text box for the `for` duration**, accepting Prometheus' own spelling.
  Parse and format mirror `render.go`, round-trip pinned, and it refuses a bare
  number and an ascending composite (`30s5m`) rather than guessing.

**Preview is debounced 300ms and keyed on kind + params + labels**, so typing a
name costs no query. It has **four** honest states and never prints "0 series"
for an expression that was not evaluated — that is asserted negatively, because
"0 series" for an unrun query is the single most misleading thing this page
could say.

**Errors are routed, not dumped.** A `422` lands on the field it names
(nested scope fields resolve by their last path segment). A `409` from sync
lifts to a section banner. The sync button's `202` says **"requested"** — never
"synced", "succeeded" or "applied", asserted by name.

The **enabled toggle sends the whole rule**, because omitting `enabled` on a
`PUT` would enable it. Pinned.

**With `console.alerting.enabled=false` the page still fully CRUDs.** Only the
foreign section carries the `409` banner. Preparing rules before switching the
reconciler on is a normal way to roll this out.

The **import report** renders `created`, `skipped` and `notes` verbatim, each
with an explicit "none" rather than a blank, and states the consequence in
plain words: **the adopted alerts now exist twice** until one copy is removed.

### 7.12 Settings

**Delivered in M7**, and narrower than §6.2's original line — see the note
there. Three sections:

- **Webhooks** — full CRUD. The secret is a conditionally-added key: on the
  keep-path `"secret"` is not merely empty, it is **absent from the body**
  (asserted on `Object.keys`). Creating with a blank secret is refused
  client-side — the **one** rule mirrored from the server; everything else is
  the server's `422` shown verbatim. "Test" is honest about being asynchronous:
  *"Test queued; the outcome lands on this row"*, then a refetch. `lastStatus`
  is rendered verbatim and an unknown status is never toned as OK.
- **Export / import** — export is a **blob download**, never a navigation (the
  object URL is revoked, asserted). Import parses the file, runs an
  **automatic dry-run**, and shows a per-collection table with errors and
  warnings verbatim before anything is applied. Apply stays enabled on an
  all-zero report *and* on a report with errors — a no-op and a partial are both
  valid operator choices; it is disabled only with no bundle loaded. Export is
  deliberately **enabled under Time Machine**: it is a read, and it takes no
  `?at=`.
- **About** — renders **only what `GET /api/v1/config` actually serves**. A
  finding worth recording: the config endpoint serves **no retention numbers**,
  so the page says so and names `console.retention.*` instead of inventing a
  value. `auth.role` is surfaced only in anonymous mode.

**Gating:** `webhooks:manage` and `settings:write` are both admin-only in the
builtins, so `admin` sees both sections, `operator` and `viewer` see neither —
and when both are hidden the page says that in one line rather than rendering an
empty shell. The page waits for the subject to resolve before deciding, because
`can()` fails closed while in flight and a flash of "you have no access" is a
lie with a 200ms lifetime.

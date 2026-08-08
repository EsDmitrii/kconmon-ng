<!--
Status: current
Owner: @EsDmitrii
Source: extracted from root DESIGN.md §6.3 in M0 (2026-07-14); rewritten from
the as-built M5 implementation (2026-08-08): web/src/lib/timemachine.tsx,
web/src/components/timemachine-bar.tsx, web/src/lib/matrix-promql.ts,
web/src/hooks/{use-topology,use-matrix}.ts,
web/src/pages/{topology,matrix,live,explore,node-card,pair-card,target-card,mtr}.tsx,
internal/console/httpapi/data.go, internal/console/store/events.go.
This document is the source of truth for Time Machine. Update it (and the ADRs) in the same PR as any deviation.
-->

# Time Machine

### 6.3 Time Machine (global time context)

**Delivered in M5.** A top-bar control with two states, **Live** and
**@ timestamp**, backed by one React context (`web/src/lib/timemachine.tsx`)
mounted in the AppShell, that every read surface resolves through.

`?at=` is the **single URL carrier** (Decision 9), RFC 3339, read and written
through `window.location` + `window.history` — the same convention
`pages/login.tsx` uses for `?returnTo=` and `run-detail.tsx` for its path id.
No router search-param framework was adopted.

#### The contract, precisely

- **Strict RFC 3339, not `new Date()`.** The parser is a regex requiring a full
  instant with an offset. A bare `new Date()` happily accepts `"2026"` and
  `"2026-08-07"` and various browser-specific shapes, and the console would
  then disagree with the Go server (`time.RFC3339`) about what a shared link
  means. Anything that does not match degrades to **Live** with a
  `console.warn`, never an error page.
- **Seconds-truncated everywhere.** State, URL and wire instant are the same
  value, so a link never resolves to a slightly different moment than the tab
  that produced it.
- **A future `at` is clamped client-side** with a warning. The server answers
  `400` for one, and a server `400` must never be the user experience for
  moving a picker.
- **`pushState`, not `replaceState`**, so Back and Forward walk the history of
  what you looked at; a `popstate` listener re-reads the param, including
  degrading honestly when the popped URL carries a broken one.
- **Engaging and returning are named intents**, `engage(at)` /
  `returnToLive()`, not a nullable setter — the read side and the write side of
  this state want different things, and a single `setAt(null | Date)` made
  every call site restate which one it meant.
- **Two hooks, deliberately.** `useTimeMachine()` **throws** without a
  provider: the bar is a control, and a control that cannot engage anything is
  a bug worth crashing on. `useTimeContext()` is its read-side twin and
  resolves to Live without one, because fifteen data hooks must not crash in a
  provider-less test.

#### The bar

Live renders **only the toggle** — nothing else, because a time control that
is not doing anything should not occupy the header. Engaged renders the amber
banner verbatim from this spec ("You are viewing … — return to Live to act")
carrying its own escape hatch, plus a `datetime-local` adjust with draft state
so a half-typed timestamp never refetches the console.

The banner is `role="status"` (polite), **not** `role="alert"`: engaging Time
Machine is a thing the reader just did, and it should not interrupt a screen
reader mid-sentence. (The anonymous-access banner elsewhere in the app *is*
`role="alert"` — it reports a console-wide posture nobody chose in that
moment.)

#### Per surface, as built

| Surface | Engaged behaviour |
| --- | --- |
| Topology | `GET /api/v1/topology?at=` — a server-side fold over `topology_events` (Decision 6). No polling, no WebSocket. |
| Matrix | **Full PromQL reconstruction at `t`** through the promql proxy's existing `time` field (`lib/matrix-promql.ts`). `GET /api/v1/matrix` stays live-only and grew no `at`. Both live paths go quiet: the socket subscription is not opened at all, since a pushed frame is by definition *now*. |
| Live | `to=t` turns the feed into a scrollback **ending** at `t`. `GET /api/v1/events` already filtered on `to`, so this needed zero Go changes. |
| Object cards (Node/Pair/Target) | PromQL reads move to `t`; the "Recent changes" rail bounds to `≤ t`. |
| Explore | `end = t` instead of now, for both A and B legs. |
| MTR Explorer | Path history is already a time series of stored rows and needs no anchoring; the page's one **mutation** — the Runner — is disabled instead. |
| Every mutation | Disabled through one hook, `useWritesDisabled()`. |

**Write-blocking is a frontend affordance, not a server mode** (Decision 8).
Every mutation is already authz-gated server-side; the banner and the disabled
controls prevent *confusion*, not abuse. There is no server state and no new
middleware. Permission **hides** an affordance; Time Machine **disables** it —
two different signals that compose rather than collapse into one greyed-out
button meaning either.

#### Three limitations worth knowing before you rely on them

- **The topology fold is structurally ready and, with the controller shipped in
  this release, empty.** The controller publishes `TopologyChanged` with a
  *reason* only — `node_name` and `agent_id` are left empty
  (`internal/controller/controller.go`) — so every event folded today is
  unfoldable and the reconstructed node set comes back empty. This is **not**
  the UI failing to render, and the page says so with the server's own numbers
  ("found *N* events at or before this instant and could fold *M* of them"),
  naming the limitation as a property of what the controller records rather
  than of the instant you picked. The fold is coded against the full event
  shape, so history becomes real the day the controller attributes its events.
  Carried forward by name in MILESTONES.md.
- **`?at=` does not survive in-app navigation.** TanStack Router owns
  navigation, and a `<Link>` to another page carries no `at` in its href — the
  context keeps the value and every surface stays engaged, but the URL loses
  the param on such a navigation. Nothing breaks (the next engage or
  return-to-live rewrites the URL), and the shareable-link guarantee therefore
  holds for **the URL you are on**, not across in-app navigations. Propagating
  `at` through `<Link>` means teaching the router about search params, which
  Decision 9 deliberately did not adopt.
- **The historical matrix hardcodes the metric prefix.** `lib/matrix-promql.ts`
  writes `kconmon_ng_*` literally, exactly as `curated-metrics.ts` and the pair
  card already did before M5. On a console configured with a non-default
  `config.metricsPrefix` the historical matrix comes back **empty** rather than
  mis-attributed — the failure is visible, not silently wrong. A
  server-advertised `metricsPrefix` capability would fix all three call sites
  at once; it is named in the M5 carry-forwards, not smuggled in here.

Implementation note: this is why §4.1 persists `topology_events` — the
controller only knows *now*.

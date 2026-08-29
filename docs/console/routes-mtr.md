# Routes · MTR

<!-- screenshot: console-routes-mtr.png pending post-redesign reshoot -->

The path explorer. It answers: **which routes do packets actually take between these two points, and when did the
route change?**

## What this page shows

Two views, switched at the top: **Explorer** and **Runner**.

The Explorer is three panes:

1. **Destinations** — every destination the fleet's traces have reached, each card counting distinct paths and total
   traces ("{paths} · {traces}"). Expanding a destination lists the source nodes that traced it ("from {node}").
2. **Path history** — pick a source under a destination and this pane lists that pair's distinct routes over time,
   with per-route change summaries ("hop {hop}: {from} → {to}", "hop {hop} added: …", "{count} hops changed") and a
   *Load older* pager. The end of retention is stated, not implied: "nothing older is retained".
3. **Trace detail / Path diff** — one route's hops, or, after ticking two routes and pressing **Compare (2/2)**, a
   side-by-side diff ("a third pick replaces the earlier of the two").

Path history is a database projection, so the page needs the database and the `mtr:read` permission — which every
built-in role holds, viewer included.

Optionally, stored hop addresses can be enriched with reverse DNS and GeoIP — off by default
(`console.mtr.enrichment` in the [Helm values](../reference/helm-values.md)).

## Reading a trace

A trace is up to `checks.mtr.maxHops` hops (default 30). The hop table shows per-hop addresses and loss/latency
statistics; a route is identified by a short path hash, and two routes with the same hash are the same path.

## Reactive vs manual traces

- **Reactive** — agents fire a traceroute automatically when a probe to a peer fails; `checks.mtr.cooldown` (default
  `60s`) is the minimum interval between traces for the same (source, destination) pair. These arrive in
  [Events](events.md) as *MTR triggered* / *MTR completed* and land in this page's history.
- **Manual** — the **Runner** tab: "the same `POST /api/v1/runs` the Run checks page uses, with the check type fixed
  to mtr." Controls: **Duration** (*Instant* = one trace per pair, or an interval run with the cadence spelled out
  before you press Run), **Trace interval** (*Auto* or a preset), **Destination** kind (*Nodes* / *Target* /
  *Ad-hoc*), **Sources** and **Destinations** node pickers, and a live "~{count} pairs" estimate. Starting a run needs
  `runs:create`.

## Path history

Distinct routes, not raw traces: a pair with hundreds of traces may honestly show six paths. Every path change is
also a timeline row in [Incidents](incidents.md) (kind *path change*) — route flaps correlate with loss there.

Note for [Time Machine](time-machine.md) users, from the page itself: the destination and path-history reads take no
time parameter, so the Explorer is **live** even while the rest of the console is engaged — the banner says so rather
than implying a cut that never happened.

## Deep links

- *Run an MTR from the Run checks page* — the empty state's own link to [Run checks](run-checks.md).
- Run detail pages link recorded routes back here ("Open in MTR Explorer").

## Use it when

- Loss appeared and you want to know whether the *route* changed before blaming the endpoints.
- You need to prove a flapping path: run an interval MTR — "the one thing a single instant trace cannot see."
- You want the network's history for a pair before and after a maintenance window.

Verified against `web/src/pages/mtr.tsx`, `web/src/lib/i18n/dict/mtr.ts`, `web/src/components/mtr-hop-table.tsx`
(API: `GET /api/v1/mtr/destinations`, `/api/v1/mtr/snapshots`, `POST /api/v1/runs`), and
`charts/kconmon-ng/values.yaml` for `checks.mtr.*` and `console.mtr.enrichment.*`.

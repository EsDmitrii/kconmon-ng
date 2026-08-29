# Pair and node pages

Object detail pages — not in the sidebar, reached by clicking the object anywhere in the console. Three of a kind:
the **pair page**, the **node page**, and the **target card** (documented with
[Scheduled checks](scheduled-checks.md)); the run permalink is covered under [Run checks](run-checks.md#run-plans).
All of them honour the [Time Machine](time-machine.md).

## Pair page

`/pairs/<source>/<destination>` — one directed pair's connectivity.

- A tier badge using the console-wide vocabulary — **Healthy** / **Degraded** / **Failing** / **No data** — the same
  thresholds as the [Matrix](matrix.md) legend.
- **RTT p95 by protocol** — the pair's last hour (or the hour ending at the viewed instant), charted from Prometheus.
- **Last run for this pair** and a **Run check** button (needs `runs:create`) that starts a run without leaving the
  page. The run scan is honest about its bound: `GET /api/v1/runs` has no source/destination filter yet, so the page
  scans the most recent runs client-side and says an older matching run may exist.
- Leg badges read *no data* / *no fail data* exactly like matrix cells — silence is never rendered as a zero.
- A pair whose endpoints the fleet does not report gets a named 404: "This fleet has no node called “{name}”", with a
  *Back to Matrix* link.

![Node page](../img/node-detail.png)

## Node page

`/nodes/<name>` — one node, both directions.

- Header: zone, tier badge, and "{percent}% healthy" with its coverage ("{scored} of {total} pairs scored").
- **Agent identity** — Zone, Agent ID, Pod IP, Ready. Readiness comes from the Kubernetes node informer; a registered
  agent alone leaves it an em dash, and the page says why.
- **Per-destination breakdown** — a paged table of this node's outgoing pairs: Destination, Fail ratio, Packet loss
  (while measured), RTT p95. Each destination links onward.
- **Runs touching this node** — the same client-side scan as the pair page, with the same disclosed limit.
- **Annotations** — notes pinned to this node over the last 24 hours, plus fleet-wide ones.
- **Open incidents** naming this object, and a **Recent changes** rail (event history for this scope), each with an
  *Investigate* entry into [Incidents](incidents.md).

## Getting there from other pages

- [Matrix](matrix.md): row/column header → node page; any cell → Incidents for that pair (the pair name on
  [Overview](overview.md)'s worst-pairs table is the direct pair-page link).
- [Topology](topology.md): click a node (or press Enter on it).
- [Overview](overview.md): pair names in **Worst pairs**.
- [Scheduled checks](scheduled-checks.md): a target row opens its target card, `/targets/<id>`.
- Anywhere an *Investigate* affordance appears, the incident timeline's rows link back to the objects they name.

## Use it when

- The matrix shows one bad row — open the node page and read all of its destinations at once.
- You are watching one pair recover after a fix: pair page, RTT chart, Run check, all in place.
- You want the node's identity facts (zone, pod IP, agent id, readiness) without `kubectl`.

Verified against `web/src/pages/pair-card.tsx`, `web/src/pages/node-card.tsx`, `web/src/pages/target-card.tsx`,
`web/src/lib/i18n/dict/cards.ts`, `web/src/routes.tsx` (route paths).

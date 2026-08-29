# Overview

The landing page. It answers one question: **is the fleet healthy right now, and if not, which pairs are the problem?**

![Overview](../img/console-overview.png)

## What this page shows

Everything on this page is qualified as **TCP · pod plane** — one protocol, one network plane. The header leads with a
health statement in words, computed from the pair matrix: "All 42 pairs healthy", "3 pairs failing", or the scoped
variant "All 40 scored pairs healthy" when some measured pairs carry no failure-ratio samples.

While live, the page recomputes from Prometheus every 15 seconds. With the [Time Machine](time-machine.md) engaged,
nothing refreshes — the page states cluster health at the instant you are viewing.

## Cluster health summary

Three stat tiles:

| Tile | Meaning |
| --- | --- |
| **Nodes ready** | Ready nodes out of the Kubernetes node inventory. When the controller has no k8s node view, the tile counts registered agents (or matrix rows) instead and says so — readiness is then unknown. |
| **Failing pairs** | Pairs with a failure ratio **≥ 10%** ("Fail ≥ 10%"). |
| **Degraded pairs** | Pairs with a failure ratio between **1% and 10%** ("Fail 1–10%"). |

On a cluster with no probe data yet the pair tiles show an em dash rather than a `0` — the console never turns
"nothing measured" into a measured zero.

First run only: while no pair is measured, a **Setup progress** card replaces the empty state and tracks
*Agents registered* → *Prometheus scraped* → *First probe round*, each unmet step with a one-line fix.

## Worst pairs

The **Worst pairs** table ranks scored pairs by failure ratio, worst first. Columns: **Pair**, **Fail %**, **p95 RTT**,
**Status** (*Failing* / *Degraded*). The pair name links to that pair's [pair page](pair-and-node-pages.md), and each
row carries an *investigate* link that opens [Incidents](incidents.md) pre-scoped to the pair.

When only some measured pairs have a failure ratio, the table says so:
"{scored} of {total} pairs have a failure ratio; the rest have no failure samples." An empty list distinguishes three
cases — no probe data in Prometheus yet, pairs reporting latency but no failure-ratio samples, and the healthy case
("Every scored pair is under a 1% failure ratio").

## Firing alerts and open incidents

Three summary panels under the table:

- **Firing alerts** — the firing set of rules *this console manages* (`GET /api/v1/alerts?managedOnly=true`), with an
  *open Alerting* link and a per-row *investigate* link. A quiet list is a fact about this console's rules only; other
  rules' firing state lives in Alertmanager or Grafana. Needs `alerts:read` and a configured Prometheus. This panel is
  live-only: Prometheus keeps no firing history, so it shows nothing under the Time Machine.
- **Open incidents** — newest open incidents from `GET /api/v1/incidents`, each linking to its
  [incident permalink](incidents.md#working-an-incident). Needs `incidents:read` and the database.
- **Recent events** — the newest fleet events from `GET /api/v1/events`, with an *open Live* link to the full
  [Events](events.md) feed. Needs `events:read`.

## Deep links

- Pair name → `/pairs/<source>/<destination>` ([pair page](pair-and-node-pages.md))
- *investigate* (worst pair or firing alert row) → [Incidents](incidents.md) scoped to that pair or alert scope
- *open Alerting* → [Alerting](alerting.md); a firing alert row links to its rule via `/alerting?rule=<id>`
- *open Live* → [Events](events.md)

All links carry the current `?at=` instant when the Time Machine is engaged.

## Use it when

- You open the console cold and want a verdict before detail.
- An alert fired and you need the worst pairs ranked, with a one-click path into an investigation.
- You have just installed kconmon-ng and want to see the setup progress card confirm agents → scrape → first round.

Data sources verified against `web/src/pages/overview.tsx`: matrix via `GET /api/v1/matrix` (live) or the PromQL
equivalent at the viewed instant, topology via `GET /api/v1/topology`, plus the three panel endpoints above.

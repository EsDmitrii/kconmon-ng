# Matrix

The N×N node connectivity heatmap. It answers: **which source→destination pairs are failing, for which protocol?**

![Matrix](../img/console-matrix.png)

## What this page shows

One cell per directed pair, source rows × destination columns (the corner header reads `src \ dst`). Live, the grid is
recomputed from Prometheus every 15 seconds; with the [Time Machine](time-machine.md) engaged it is evaluated straight
from Prometheus at the viewed instant.

Controls:

- **Protocol** switch — **TCP**, **UDP**, **ICMP**. The choice is carried in the URL (`?protocol=`), so a matrix view
  is shareable.
- **Zoom** cluster — *Zoom in*, *Zoom out*, *Fit to view*, with the current level shown as a percentage. As the page
  itself states: "Ctrl and the wheel zoom the grid; the wheel alone scrolls it."
- When all node names share a prefix, the grid drops it and says so ("Node names drop the shared prefix …").

## Reading the heatmap

Each cell shows the pair's failure percentage and its p95 RTT (UDP/ICMP cells also carry packet loss, "loss {ratio}").
Hovering opens a tooltip with **Failure ratio**, **RTT p95** and, where measured, **Packet loss**; a pair with no
failure samples reads "no samples" rather than 0.

Legend, colour = worst of fail % and packet loss:

| Colour | Meaning |
| --- | --- |
| Green | **Healthy · fail < 1%** |
| Amber | **Degraded · 1–10%** |
| Red | **Failing · ≥ 10%** |
| Grey | **No data** — nothing probed this pair |

One honesty rule worth knowing: a cell whose failure counter emitted no samples still shows its p95 and stays green
"on the absence of a bad signal, not on a measured zero" — the legend note says exactly that, and the cell's second
line reads *no fail data*.

## Cell states

- **Diagonal** — "{node}: self"; a node never probes itself.
- **Measured cell** — click to open [Incidents](incidents.md) scoped to that pair ("Investigate {src} → {dst}").
- **Unmeasured cell** — "No probe data in Prometheus for this pair."
- **Row/column header** — click to open that node's [node page](pair-and-node-pages.md) ("Open the card for {node}").

## Deep links

- Cell → Incidents pre-scoped to the pair; header → node page. Both carry `?at=` when engaged.
- `?protocol=udp` etc. selects the protocol on arrival.

## Use it when

- One node's row (or column) lights up — a source-side vs destination-side problem is visible at a glance.
- You want the blast radius of a CNI or network change: watch the grid during the change
  (the [demo](../demo/breaking-cni.md) does exactly this).
- You need to jump from "this pair is red" to an investigation in one click.

Verified against `web/src/pages/matrix.tsx`, `web/src/lib/i18n/dict/matrix.ts`, `web/src/hooks/use-matrix.ts`
(`GET /api/v1/matrix` live; PromQL instant queries at the viewed instant via `web/src/lib/matrix-promql.ts`).

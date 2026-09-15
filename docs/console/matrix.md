# Matrix

The N×N connectivity heatmap: one cell per directed pair, source rows × destination columns, the corner header reading `src \ dst`. When one node's row or column lights up, you can tell a source-side problem from a destination-side one at a glance.

<figure markdown>
![Matrix in the full-bleed tool layout, TCP selected, plane: pod chip, Live, eleven rows during a staged break: the rows and columns of worker2, worker5, worker6 and worker7 red at 100.0%, the external agent edge-host-01 as the first row and column, and a tooltip on worker3 → worker6 reading Failure ratio 100.0%; legend and zoom controls in frame](../img/console-matrix-failing.png){ loading=lazy }
<figcaption>TCP matrix with four nodes cut off: worker2, worker5, worker6 and worker7 are red at 100.0% as rows and as columns, every other cell is green with its p95 RTT, and the hovered cell worker3 → worker6 opens its tooltip. The external agent <code>edge-host-01</code> is an ordinary row and column here, green except toward the four broken nodes; the legend and zoom controls frame the grid.</figcaption>
</figure>

## Reading the heatmap

Each cell prints the pair's failure percentage and its p95 RTT; UDP and ICMP cells add a second line with packet loss ("loss {ratio}"). Hovering opens a tooltip with **Failure ratio**, **RTT p95** and, where measured, **Packet loss**.

Colour is the worst of failure ratio and packet loss:

| Colour | Meaning |
| --- | --- |
| Green | **Healthy · fail < 1%** |
| Amber | **Degraded · 1–10%** |
| Red | **Failing · ≥ 10%** |
| Grey | **No data** — nothing probed this pair |

This page is the canonical home of the console's no-data rule, which every other surface links back to: **silence is never rendered as a zero.** A pair nothing probed is grey and reads "no data". A pair whose failure counter emitted no samples while its RTT did is a different fact: the cell keeps its p95, its second line reads *no fail data*, and it stays green on the absence of a bad signal rather than on a measured zero. A tooltip on such a cell says "no samples" where the ratio would go. The same two readings appear on the [pair and node pages](pair-and-node-pages.md) and feed the measured/scored split on [Overview](overview.md#the-health-statement).

<figure markdown>
![UDP matrix, Live, same break: worker2, worker5, worker6 and worker7 red at 100.0% as rows and columns, every other cell green with a p95 RTT of 0.5 to 1.4 ms on the second line, the edge-host-01 row and column green except toward the four broken nodes](../img/console-matrix-udp-loss.png){ loading=lazy }
<figcaption>UDP view of the same break: the same four nodes red at 100.0%, every other cell green with its p95 RTT on the second line (0.5 to 1.4 ms on UDP), and the <code>edge-host-01</code> row and column measured like every other node.</figcaption>
</figure>

## Controls

- **Protocol** switch: **TCP**, **UDP**, **ICMP**. The choice travels in the URL (`?protocol=`), so a matrix view is shareable as it stands, and `?protocol=udp` in a pasted link selects UDP on arrival.
- **Zoom** cluster: *Zoom in*, *Zoom out*, *Fit to view*, with the current level shown as a percentage. ++ctrl++ plus the mouse wheel zooms the grid; the wheel alone scrolls it. Zoom walks fixed steps from 40% to 150% rather than a continuous scale, so a size you liked is a size you can get back to. The 40% floor is deliberate: below it a cell is smaller than the smallest legible figure, and shrinking further would trade a grid you cannot fit for a grid you cannot read. At the floor the container pans instead.
- When all node names share a prefix, the grid drops it and says so ("Node names drop the shared prefix …").

## Cell states

- **Diagonal**: "{node}: self"; a node never probes itself.
- **Measured cell**: click to open [Incidents](incidents.md) scoped to that pair ("Investigate {src} → {dst}").
- **Unmeasured cell**: "No probe data in Prometheus for this pair."
- **Not probed**: a dashed hollow box, "not probed by the topology plan", for a pair a sparse plan (`topology.mode: sparse`, since 2.3.0) deliberately leaves out. The legend row "Not probed · excluded by the topology plan" appears only while a plan is in force.
- **Row/column header**: click to open that node's [node page](pair-and-node-pages.md).

Both kinds of link carry `?at=` while the Time Machine is engaged.

### Silence with a known cause

Since 2.4.0 the grid tells two more kinds of silence apart from plain no-data. Neither is a new colour: the console never turns an absence into a verdict, so these are hints about *why* a cell is empty, decided from the topology snapshot and applied only to cells that carry no measurement (a measured cell always shows its measurement).

- **Unscraped external agent.** An [external agent](../external-agents.md) whose name is never a cell's *source* while the grid has cells at all is a bare host Prometheus is not scraping: the cluster probes it (its column fills in) but nobody reads its own probes (its row is empty). Those cells keep the no-data fill, the em dash and the same screen-reader reading, because that is what they are; only the tooltip changes, to "No series from {src}: Prometheus is not scraping this external agent's metrics port. Add a scrape job — see External agents docs." A note above the grid, beside the shared-prefix note, lists the unscraped agents by name and carries the link to [Scraping external agents](../external-agents.md#scraping-external-agents). Both disappear the moment the source has a single measured cell.
- **A plane the source does not run.** An agent advertises the probe planes it runs (`plane:tcp`, `plane:udp`, `plane:icmp`, and so on) at registration. When the grid's protocol is one the source left out, its cells render like *not probed* (a dashed hollow box), the tooltip reads "{node} does not run {protocol} probes", and a legend row "Not run · the source does not run this protocol's probes" appears while at least one such cell is on the grid. The rule is fail-open: an agent that advertised no plane at all (every agent older than 2.4.0) is read as running every plane, never as unsupported, so a rolling upgrade cannot turn grey cells into calming dashed ones. It is decided per source only; the destination's planes are not consulted.
- **Precedence** when a cell has no data: excluded by the plan, then unsupported, then unscraped. The plan is the operator's own statement and outranks what an agent advertised.
- **External headers.** The row and column header of an external agent adds "external agent" as the second line of its tooltip, and its screen-reader label reads "Open the card for {node}, external agent".

## Where the grid comes from

The header also shows a **plane: pod** chip. It states which network plane these probes travel: the pod network, the only plane this release ships (the [Plane field on Scheduled checks](scheduled-checks.md#the-definition-form) is the same fact from the configuration side; the API answers 400 for any other value).

Live, the grid arrives two ways. On a replica receiving the controller event stream, whole matrices are pushed over the WebSocket as snapshot frames on the topic `matrix:<protocol>:pod`; every frame carries the complete grid, so a reconnect needs no replay. Without the stream, the page polls `GET /api/v1/matrix` every 15 seconds; either way each update replaces the grid wholesale, and the server computes it from Prometheus. With the [Time Machine](time-machine.md) engaged, the page skips that endpoint and evaluates the same PromQL directly at the viewed instant.

## When series are missing

The cardinality valve that arrived in 2.3.0 (`agent.metrics.detail` in `charts/kconmon-ng/values.yaml`) is a scrape-time relabeling, and this grid reads Prometheus, so the valve decides what a cell can show:

- `counters-only` drops the four per-pair histograms (TCP connect, TCP total, UDP RTT, ICMP RTT). Every cell's p95 line goes dark while the failure percentage and loss survive, since counters and gauges stay. A grid that is coloured normally but shows no RTT anywhere is this mode, not a bug.
- `zone-only` drops every series naming a destination node. The whole per-pair mesh blanks: every cell grey, "no probe data". The zone-level families the mode keeps are not drawn here.

A dark p95 column usually gets diagnosed on this page first, which is why the note lives here; [Overview](overview.md#when-series-are-missing) and [Metrics](metrics.md) describe the same modes from their side.

<!-- verified against: web/src/pages/matrix.tsx (Silence type + precedence comment L56-64, GridCellImpl why=measured?none
     L353-355, unsupported/excluded dashed rendering L389-399, unscraped tooltip L431-435, silenceFor + the
     useMemo sets L612-652), web/src/lib/agents.ts (isExternalAgent, agentPlanes null=unknown, unscrapedExternalNodes),
     web/src/lib/i18n/dict/matrix.ts (plane chip, legend.notProbed L98, header.external/header.node.external L114-115,
     tooltip.unscraped L120, docs.scrapeExternal L125, note.unscraped.* L128-131, tooltip/cell/legend.unsupported
     L136-140), web/src/lib/matrix-zoom.ts (ZOOM_STEPS 0.4..1.5, MIN_ZOOM floor comment), web/src/hooks/use-matrix.ts
     (matrixTopic, MATRIX_POLL_MS=15s, push-over-cache), web/src/lib/ws.ts L52-56 (isSnapshotTopic),
     internal/console/httpapi/data.go handleMatrix (plane=pod only; matrix.Compute against Prometheus),
     web/src/lib/matrix-promql.ts (engaged path), internal/agent/agent.go agentCapabilities (plane:* + fail-open rule),
     charts/kconmon-ng/values.yaml agent.metrics.detail + docs/metrics.md (the four per-pair histograms),
     RELEASE_NOTES.md v2.3.0 (the carried-over cardinality valve entry). -->

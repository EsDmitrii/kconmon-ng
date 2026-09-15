# Topology

An interactive zone and node map. Nodes are boxes grouped into zone lanes; problem paths are drawn as edges between them, worst first. During a zonal incident the tell is visual: cross-zone edges clustering on one lane.

<figure markdown>
![Topology map, Live, during a staged break: a No zone reported lane with worker10, an External lane with edge-host-01 badged external and failing, four zone lanes (zone-a with 3 nodes, zone-b 3, zone-c 2, zone-d 2) with every node badged failing, red problem edges converging on worker2, worker5, worker6 and worker7, and the caption showing 10 worst of 68 problem paths](../img/console-topology-problem-paths.png){ loading=lazy }
<figcaption>Problem paths across zones: the map caps its edges and says so ("showing 10 worst of 68 problem paths"), the red edges run into worker2, worker5, worker6 and worker7, and every node with an agent wears a <em>failing</em> badge. The external agent <code>edge-host-01</code> has its own <em>External</em> lane and an <em>external</em> badge beside the cluster nodes; worker10 sits alone in <em>No zone reported</em>, without a badge.</figcaption>
</figure>

## Nodes

Node colour comes from the probe matrix, using the same tiers as the [Matrix](matrix.md) legend: *Healthy*, *Degraded · worst path 1–10%*, *Failing · ≥ 10% or not ready*. A not-ready node carries a "not ready" badge. Selecting a node and pressing ++enter++ (or clicking it) opens its [node page](pair-and-node-pages.md), carrying `?at=` when the Time Machine is engaged.

Each zone is one lane, headed "{zone} · {count} nodes". A big zone wraps into a grid rather than a single column: the column count grows roughly as the square root of the node count and is capped at four, so a zone stays a shape a pane can hold instead of a tall strip the auto-fit has to shrink into illegibility. Nodes whose zone label is absent gather in a lane named **no zone reported** — which is exactly what is true, and also the state you get on a cluster whose nodes lack the `topology.kubernetes.io/zone` label.

Map controls: *Zoom in*, *Zoom out*, *Fit the whole map*. The map is read-only; nodes are not draggable.

### External agents on the map

An agent registered from outside the cluster (a bare host through the [gateway](../external-agents.md)) has no Kubernetes node, so since 2.4.0 the map merges it in from the agent list: it sits in the lane of the zone it registered with, takes its colour from the matrix like any other node, and wears a neutral **external** badge, an identity marker and never a health tier. Readiness is unknown by construction, since there is no node object to be ready, and the box says so to a screen reader: "{node}, {zone}, {health}, external agent, readiness unknown". The map's provenance stays the Kubernetes node view, so a bare host never triggers the "map built from registered agents" notice described below. Under the Time Machine the badge comes from the labels recorded on topology events and snapshots, which a controller older than 2.4.0 did not record: history from before that upgrade shows the host as a plain node.

## Edges

Edges are drawn only for problem paths (TCP fail ≥ 1%), and there is a budget: at most **10** edges, worst first. The caption counts what the budget hid ("showing {shown} worst of {total} problem paths"), or simply "3 problem paths", or "no problem paths right now". Each edge label names its vector: a plain percentage is a failure ratio, "{pct} loss" is packet loss, and hovering an edge shows its ratio.

One edge per ordered pair, keeping the worst reading. If the matrix somehow carries two cells for the same A→B, drawing both would double-count one path in the caption and collide two drawn edges under one identity, so the map arbitrates worst-of — the same rule every other severity read in the console applies.

## Degraded and stale states

All the map's non-happy paths, in one place:

- **"This map is no longer refreshing"**: the last refresh did not come back. What is on screen is the node set that loaded before it, and the console keeps retrying on its own. Distinct from "Topology is unavailable" on purpose: claiming *nothing* over a map that is visibly on screen would be wrong, what the page has is something older.
- **"Your browser reports no connection"**: the request never left the browser. It goes out by itself once the connection is back.
- **No Kubernetes node view at all**: the map is drawn from registered agents and the zones they registered with, a notice says so, node colour still works, and readiness is unknown.
- **Live empty state**: "No nodes reported by the controller yet" points at the agent DaemonSet.
- **Historical bounds**: see below.

## The map under the Time Machine

Live, the node set refreshes every 15 seconds. Engaged, the map switches mechanism entirely: it is **reconstructed from stored topology events**, folded up to the viewed instant.

Events only record what changed, so the fold starts from a snapshot of the whole topology that the console stores when it connects to the controller's event stream and every hour after that, then replays the events since. Instants from before a console's first snapshot, which includes everything recorded before 2.4.0, get the events alone: the map then shows only the nodes whose agents registered, moved or left inside the retained window, and misses the ones that sat still.

Why events rather than Prometheus? Prometheus can answer "what were the series at 03:12", but node identity, readiness transitions and agent registrations are not series: they are facts the controller reported as they happened. The event log is the only record with them in order, so the fold replays it. The trade is stated by the API itself: historical topology needs the database (`GET /api/v1/topology?at=` answers 503 without one, naming `console.database.mode`), and an instant older than what retention kept answers 422, telling you to pick a later time or raise `console.database.retentionDays`.

The reconstruction states its own bounds in place: an instant past the kept window ("This reconstruction is incomplete"), events that name no node ("Nothing to reconstruct at this time"), or simply no nodes at that time.

<figure markdown>
![Topology with the Time Machine engaged at 9/15/2026, 11:53:14: the banner and the amber control show the instant, the header says the map is reconstructed from topology events, a No zone reported lane with worker10, an External lane with edge-host-01 badged external and failing, four zone lanes (zone-a with 3 nodes, zone-b 3, zone-c 2, zone-d 2) with every node badged failing, red problem edges, and showing 10 worst of 68 problem paths](../img/console-topology-reconstruction.png){ loading=lazy }
<figcaption>A reconstruction: the header states the instant and that the map was rebuilt from topology events. At 11:53:14 it holds the same twelve node boxes as the live map above, the External lane with <code>edge-host-01</code> and the No zone reported lane with worker10 among them, and counts 68 problem paths, the ten worst drawn.</figcaption>
</figure>

<!-- verified against: web/src/pages/topology.tsx (EDGE_CAP=10 L68, worst-of dedupe, ZONE_MAX_COLS=4 + zoneColumns L52-58,
     mapNodes merging bare hosts with source kept "nodes" L134-184, node.aria.external L255-260, neutral Badge L387),
     web/src/lib/agents.ts (externalByNode), web/src/lib/i18n/dict/topology.ts (stale.*, offline.*, help.body, lane strings,
     node.external + node.aria.external L125-131), web/src/hooks/use-topology.ts (GET /api/v1/topology, ?at=),
     internal/console/httpapi/data.go L25-31 (topologyHistoryUnavailableDetail 503, topologyRetentionDetail 422),
     internal/console/store/events.go (fold keeps the last stated labels, starts from the newest topology_baseline),
     internal/console/events/baseline.go (baseline on connect, baselineInterval = 1h), docs/console-api.yaml TopologyAgent.labels
     (absent on history recorded before 2.4.0), internal/console/httpapi/topology_at_test.go. -->

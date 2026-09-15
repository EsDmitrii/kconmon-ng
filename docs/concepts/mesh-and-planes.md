# Mesh and planes

## The probe mesh

Every agent probes every other agent, and every **ordered** pair is measured
separately: `node-1 → node-2` and `node-2 → node-1` are two different series,
because asymmetric failure is common: a firewall rule, a policy, a broken
return path each affect one direction. N nodes make N×(N−1) directed pairs.

Probes travel pod-IP to pod-IP, and each protocol has a fixed rendezvous:

| Probe | Dials | Port (default) |
| --- | --- | --- |
| TCP | the peer agent's HTTP port | the peer's own `config.httpPort`, 8080 |
| UDP | the peer agent's gRPC/probe port | the peer's own `config.grpcPort`, 9090 |
| ICMP | the peer's pod IP | — |

"The peer's own" is literal since 2.4.0: every agent reports its listener
ports at registration and the controller hands them out with the peer list,
so a peer on non-default ports is dialled where it actually listens. An agent
that reports no ports (older than 2.4.0) is dialled on the prober's own
configured values, which is why a fleet keeps one port set until every member
is on 2.4.0 ([External agents](../external-agents.md#ports)).

Keep this table at hand when you write firewall rules, or when you break them
on purpose as [the demo](../demo/breaking-cni.md) does: blocking the wrong
port silently matches zero packets.

## Control plane vs data plane

The two never mix:

- **Control plane** is the agent ↔ controller gRPC: registration, heartbeats, the
  peer list pushed on every change, and dispatch of on-demand checks. Losing
  it degrades *coordination* only: agents keep probing the last known peer
  list while the controller is away.
- **Data plane** is agent ↔ agent probes and each agent's `/metrics`. Probe
  results never transit the controller; Prometheus scrapes them straight from
  the agents. The controller cannot become a throughput bottleneck for
  measurements, and a controller outage costs you peer-list freshness, not
  data.

```mermaid
flowchart LR
    C["Controller"]
    A["Agent on node A"]
    B["Agent on node B"]
    P["Prometheus"]

    C -. "control plane:<br/>peer list, heartbeats,<br/>on-demand checks" .-> A
    C -.-> B
    A == "data plane: probes<br/>(pod IP → pod IP)" ==> B
    B ==> A
    P == "data plane: scrape<br/>/metrics :9091" ==> A
    P ==> B
```

### The pod plane, and the traffic it measures

Diagnostic checks carry a lowercase `plane` field, and today `pod` is the
only value that exists: probes are sent from agent pod to agent pod, so what
gets measured is the path pod traffic actually takes through the CNI
datapath. That is the layer a CNI bug, a conntrack overflow or a fabric
problem lives in, and it is why the tool probes pod IPs rather than node
addresses. A workload on `hostNetwork` talks over node addresses instead, a
path these probes do not exercise. No separate `host` plane shipped with the
[external agents](../external-agents.md) work: a bare-host agent advertises
a host IP because it has no pod network at all, and its probes are still
recorded under `plane=pod`. Read the field as "the addresses the agents
advertise", not as a statement about which datapath carried the packets.

### Host networking

Since 2.4.0 the chart can move the whole agent DaemonSet onto node addresses
with `agent.hostNetwork: true`. Each agent then runs in its node's network
namespace, advertises the node IP, and listens on TCP `httpPort`, UDP
`grpcPort` and TCP `metricsPort` of the node itself. The option exists for
one reason: an [external agent](../external-agents.md#when-the-pod-network-does-not-route)
on a network that cannot route pod CIDRs can reach node IPs, so this is how
external↔cluster cells turn green without BGP or a VPN carrying the pod
network.

It changes the subject of measurement, and that is the cost to weigh before
flipping it. Every in-cluster pair then probes node IP to node IP over the
underlay; the overlay, conntrack and NetworkPolicy enforcement that the pod
plane exercises are out of the path, so a CNI bug that breaks pod traffic
can sit behind a fully green matrix. Diagnostics and the Console keep
reporting `plane=pod`, unchanged, and a 2.4.0 agent image marks itself
`kconmon-ng.io/host-network=true` in its registration so the API can tell a
node address from a pod address. The prerequisites (a `privileged` namespace,
free ports on every node, the `ping_group_range` sysctl from the node OS,
`networkPolicy.nodeCidrs` when policies are on) are listed with the option
in [External agents](../external-agents.md#when-the-pod-network-does-not-route).

## Zones and failure domains

At registration the controller reads each agent's node label named by
`config.failureDomainLabel` (default `topology.kubernetes.io/zone`) and hands
the zone back, so every peer metric carries `source_zone` and
`destination_zone` with no per-agent configuration. This needs
`controller.leaderElection: true`, because the node informer runs only on the
leader. An explicit `agent.zone` value always wins, and a node with no zone
label simply has empty zone labels (the Console's Topology page will say so
out loud).

Zones are also a measurement plane of their own: every peer probe is recorded
a second time under only `(source_zone, destination_zone)`, a family that
grows as Z² instead of N². Two bundled alert rules, `ZoneChecksFailing` and
`ZoneLossHigh`, read only that family, and the `agent.metrics.detail:
zone-only` scrape mode keeps only it. See the
[zone aggregates](../metrics.md#agent-zone-aggregates) reference.

Two design choices in the zone alerts answer questions people rightly ask.
`ZoneLossHigh` computes loss from the zone packet *counters* (sent minus
received over sent) rather than averaging the per-pair loss-ratio gauges,
because an average would weight an idle pair the same as a busy one and
report a number no packet ever experienced. And its default threshold is 0.1
where the per-pair `UDPLossHigh` uses 0.5, because the zone aggregate dilutes
any single link by the pair count: sustained loss at 10% of a whole zone pair
means the fabric is sick, not one node.

<figure markdown="span">
  ![Zone Heatmap Grafana dashboard during a staged break, 11:54 to 12:09 UTC: from-to tables with external and zone-a to zone-d as rows and columns for UDP packet loss, p95 UDP RTT, p95 ICMP RTT, TCP failure ratio, UDP failure ratio and MTR traces triggered over 1h; zone-c at 100.0% as a row and a column on loss and both failure ratios and NaN on both RTT tables](../img/zone-heatmap.png){ loading=lazy }
  <figcaption>The bundled Zone Heatmap Grafana dashboard reading the <code>kconmon_ng_zone_*</code> family on the kind stand during a staged break, last 15 minutes: zone-c is 100.0% as a source and as a destination on UDP loss and both failure ratios and NaN on both RTT tables, the zone-a and zone-b rows sit between 33.3% and 66.7% outside the zone-c column, and the external agent's zone has its own row and column like the four cluster zones.</figcaption>
</figure>

## Full mesh and its limits

Per-pair, per-protocol measurement is the point of the tool, and it is also
the bill. Where the ~70 comes from: the four per-pair histograms (TCP
connect, TCP total, UDP RTT, ICMP RTT) cost 16 series each on the shared
13-bucket scale (13 buckets plus `+Inf`, `_sum` and `_count`), for 64, plus
three loss/jitter gauges and three result counters. Roughly 70 active series
per directed pair, growing quadratically: about 690k series at 100 nodes.
**50–100 nodes is the production-proven envelope.**

!!! warning "Version skew: the zone family comes from the agent image"
    Everything zone-flavoured reads the `kconmon_ng_zone_*` family, and that
    family is exported by the **agent**, from v2.3.0 on. A default install is
    fine, since the chart pins its own appVersion; the trap is a fleet that
    pins an older agent image behind a newer chart. There the two zone alerts
    are silently inert (their expressions match no series), the Zone Heatmap
    renders empty, and flipping `agent.metrics.detail: zone-only` drops the
    per-pair series with nothing replacing them: Prometheus goes dark on the
    mesh while the console keeps working. Upgrade the agent image first, flip
    the valve second. Details in the
    [v2.3.0 release notes](../reference/release-notes.md).

The levers that exist today, in one line each:

- `agent.metrics.detail: counters-only` drops the per-pair histograms
  (~70 → ~10 series per pair) while every pair alert keeps firing.
- `agent.metrics.detail: zone-only` drops per-pair series entirely and keeps
  the Z² zone plane, subject to the version-skew warning above.
- Disabling a checker removes its families.

One prerequisite on the first two: the valve renders as `metricRelabelings`
on the agent ServiceMonitor and, since 2.4.0, on the external-agent
ScrapeConfig too, so it needs `serviceMonitor.enabled` or
`scrapeConfig.externalAgents.enabled`, and the chart refuses the valve
without either. Running plain Prometheus instead, copy the equivalent
`metric_relabel_configs` from
[Levers that exist today](../metrics.md#levers-that-exist-today).

The full arithmetic, what each checker costs and what each mode keeps, is in
[Scaling and cardinality](../metrics.md#scaling-and-cardinality). A sparse
mesh (`topology.mode: sparse`, since v2.3.0) probes a structured subset of
pairs instead of all of them and is the lever for fleets past the full-mesh
envelope; even with it, do not plan a 1000-node full mesh.

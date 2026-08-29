# Mesh and planes

## The probe mesh

Every agent probes every other agent, and every **ordered** pair is measured
separately: `node-1 → node-2` and `node-2 → node-1` are two different series,
because asymmetric failure is common — a firewall rule, a policy, a broken
return path each affect one direction. N nodes make N×(N−1) directed pairs.

Probes travel pod-IP to pod-IP, and each protocol has a fixed rendezvous:

| Probe | Dials | Port (default) |
| --- | --- | --- |
| TCP | the peer agent's HTTP port | `config.httpPort`, 8080 |
| UDP | the peer agent's gRPC/probe port | `config.grpcPort`, 9090 |
| ICMP | the peer's pod IP | — |

Worth knowing when you write firewall rules — or break them on purpose, as
[the demo](../demo/breaking-cni.md) does: blocking the wrong port silently
matches zero packets.

## Control plane vs data plane

The two never mix:

- **Control plane** — agent ↔ controller gRPC: registration, heartbeats, the
  peer list pushed on every change, and dispatch of on-demand checks. Losing
  it degrades *coordination* only: agents keep probing the last known peer
  list while the controller is away.
- **Data plane** — agent ↔ agent probes and each agent's `/metrics`. Probe
  results never transit the controller; Prometheus scrapes them straight from
  the agents. The controller cannot become a throughput bottleneck for
  measurements, and a controller outage costs you peer-list freshness, not
  data.

(There is also a lowercase "plane" field on diagnostic checks — the traffic
plane a probe runs on. Today only the `pod` plane is meaningful; a host plane
is reserved for the [external agents](../external-agents.md) roadmap.)

## Zones and failure domains

At registration the controller reads each agent's node label named by
`config.failureDomainLabel` (default `topology.kubernetes.io/zone`) and hands
the zone back, so every peer metric carries `source_zone` and
`destination_zone` with no per-agent configuration. This needs
`controller.leaderElection: true` — the node informer runs only on the
leader. An explicit `agent.zone` value always wins, and a node with no zone
label simply has empty zone labels (the Console's Topology page will say so
out loud).

Zones are also a measurement plane of their own: every peer probe is recorded
a second time under only `(source_zone, destination_zone)`, a family that
grows as Z² instead of N². Two bundled alert rules — `ZoneChecksFailing` and
`ZoneLossHigh` — read only that family, and the `agent.metrics.detail:
zone-only` scrape mode keeps only it. See the
[zone aggregates](../metrics.md#agent-zone-aggregates) reference.

## Full mesh and its limits

Per-pair, per-protocol measurement is the point of the tool, and it is also
the bill: each directed pair keeps roughly 70 active series, so pairs grow
quadratically — about 690k series at 100 nodes. **50–100 nodes is the
production-proven envelope.** The levers that exist today, in one line each:

- `agent.metrics.detail: counters-only` drops the per-pair histograms
  (~70 → ~10 series per pair) while every pair alert keeps firing.
- `agent.metrics.detail: zone-only` drops per-pair series entirely and keeps
  the Z² zone plane.
- Disabling a checker removes its families.

The honest arithmetic — what each checker costs, what each mode keeps — is in
[Scaling and cardinality](../metrics.md#scaling-and-cardinality). A sparse
mesh (probing a structured subset of pairs instead of all of them) is on the
roadmap for larger fleets; until it lands, do not plan a 1000-node full mesh.

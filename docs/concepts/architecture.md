# Architecture

Three components, one direction of trust: agents measure, the controller
coordinates, the console reads. Results never transit the controller — they go
straight from each agent's `/metrics` into your Prometheus.

## Components: agent, controller, console

```
                          +-------------------------------------------+
                          |          Controller (Deployment)          |
                          |  agent registry (heartbeat eviction)      |
                          |  node watcher (zone labels)               |
                          |  topology API + gRPC event stream         |
                          |  leader election (active/standby)         |
                          +---------------------+---------------------+
                                                |
                       gRPC stream: peer list, pushed on every change
                                                |
              +-------------------+  +-------------------+  +-------------------+
              | Agent (node-1)    |  | Agent (node-2)    |  | Agent (node-3)    |
              | TCP UDP ICMP      |  | TCP UDP ICMP      |  | TCP UDP ICMP      |
              | DNS HTTP          |  | DNS HTTP          |  | DNS HTTP          |
              | MTR on failure    |  | MTR on failure    |  | MTR on failure    |
              | /metrics :8080    |  | /metrics :8080    |  | /metrics :8080    |
              +-------------------+  +-------------------+  +-------------------+
                ^                                                             ^
                +------------------ probes between all pairs -----------------+
```

**The agent** is a DaemonSet: one pod per node, running the enabled checkers
(TCP, UDP, ICMP, DNS, HTTP, external) against every peer and firing a reactive
MTR trace when a TCP, UDP or ICMP probe fails. It exports everything as
Prometheus metrics and needs no Kubernetes API access at all — it never links
client-go.

**The controller** is a Deployment: it keeps the agent registry (heartbeats,
TTL eviction), watches nodes to resolve each agent's zone label, serves the
topology API and an on-demand [diagnostics endpoint](../api.md), and streams
domain events to the console when `controller.events.enabled` is on. It is
coordination only — no probe result ever flows through it.

**The console** is an optional web UI Deployment, off by default. It reads
your Prometheus and the controller's APIs; with a PostgreSQL DSN it adds
history, incidents, auth and managed alert rules. See
[Enable the console](../getting-started/enable-the-console.md).

## How a probe becomes a metric

1. A checker fires on its interval (5s for TCP/UDP/ICMP/DNS by default)
   against every peer on the current list.
2. The result lands in the agent's local Prometheus registry: latency
   histograms, loss/jitter gauges, result counters — labelled with
   `source_node`, `destination_node`, `source_zone`, `destination_zone`.
3. Prometheus scrapes each agent (and the controller). The chart offers a
   `ServiceMonitor`, or you write [one scrape
   job](../getting-started/install-15-min.md#no-prometheus-operator); either
   way there is a dedicated `metrics` port (9091) separate from the API port.
4. Everything downstream — dashboards, the Console's Matrix and Metrics
   pages, the alert rules — is a Prometheus query over those series.

Details that matter at 3am: a failed TCP/UDP/ICMP probe triggers MTR for that
pair under a per-pair cooldown, so a broken link cannot flood the cluster with
traces. When topology changes, stale per-pair gauges are reset instead of
leaving ghost readings for a node that no longer exists. An agent deregisters
on shutdown, so a rolling restart of kconmon-ng does not write false loss into
its own metrics. Config hot-reloads on change and is parsed with unknown keys
rejected — a typo fails fast instead of being silently ignored.

## Peer discovery over gRPC

Agents register with the controller over gRPC (`config.grpcPort`, 9090) and
subscribe to a peer-list stream — no polling, no per-agent configuration. The
controller pushes the full list on every change: an agent joining, leaving,
deregistering on shutdown, or being evicted after missing heartbeats for
`config.controllerAgentTtl` (30s by default, minimum 10s). At registration the
controller reads the node's `config.failureDomainLabel`
(`topology.kubernetes.io/zone` by default) and hands the zone back, so every
metric carries zones without anyone writing them down — see
[Mesh and planes](mesh-and-planes.md).

The channel is plaintext and unauthenticated **by design**, safe only inside
the cluster boundary: the port is never exposed, and the optional
NetworkPolicy pins it further. Do not expose it to run agents outside the
cluster — [External agents](../external-agents.md) explains what that would
hand over and what is planned instead.

## High availability

- **Controller**: set `controller.replicaCount: 2` with
  `controller.leaderElection: true` (the default). Only the leader is active —
  it alone holds the registry, watches nodes and serves topology; standbys
  wait on the lease. A PodDisruptionBudget is rendered automatically at more
  than one replica.
- **Agents survive the controller being away.** Registration retries in the
  background while the agent's health endpoints are already up, and during a
  controller restart, upgrade or outage the probes keep running against the
  last known peer list — an incident at the coordinator does not blind the
  fleet at exactly the wrong moment. The peer list resumes updating on
  re-registration.
- **The monitor watches itself.** Two bundled alert rules —
  `KconmonAgentsMissing` and `KconmonControllerDown` — page you when agents or
  the controller leader go missing, so a kconmon-ng that goes quiet does not
  read as a healthy network. Both need leader election on.
- **Console**: stateless; run `console.replicas: 2` once
  `redis.existingSecret` points at a shared bus (sessions, rate limits and
  realtime fan-out live there).

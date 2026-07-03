# Goldpinger vs kconmon-ng: two takes on Kubernetes node connectivity monitoring

*Draft for dev.to. TODO: swap in real screenshots, add author bio/links before publishing.*

If you run a Kubernetes cluster with more than a handful of nodes, you've probably reached for [Goldpinger](https://github.com/bloomberg/goldpinger) at some point — or you should have. It's a mature, widely deployed tool that answers a simple and important question: *can every node reach every other node?* It ships as a DaemonSet, polls its peers over HTTP, and draws the result as a live graph in its own web UI. Low setup cost, immediate value.

This post is not "Goldpinger is bad, use this instead." It's about a different layer of the same problem — what to do when the answer isn't a clean yes/no, and you need to know *which* protocol, *which* pair of nodes, and *which* network hop is degraded.

That's the gap [kconmon-ng](https://github.com/EsDmitrii/kconmon-ng) (Apache 2.0, currently v1.2.0) is built for.

## Same problem, different depth

Goldpinger's model: every agent pings every other agent over HTTP, results feed a graph in the built-in UI. Great for "is the mesh connected."

kconmon-ng's model: an **agent DaemonSet** and a **controller Deployment**, connected over gRPC. The controller keeps an agent registry (with heartbeat-based eviction), watches Kubernetes node objects for zone labels, and pushes peer lists to agents over a gRPC stream — full sync on connect, incremental updates on topology change (agents don't poll). Each agent runs five independent checkers against every peer: **TCP, UDP, ICMP, DNS, HTTP** — each exporting its own latency/loss/jitter metrics, not just a single up/down signal. When a TCP, UDP, or ICMP probe fails, the agent automatically fires an MTR trace for that (source, destination) pair (rate-limited by a per-pair cooldown) and exports per-hop RTT and loss as metrics.

## Feature comparison

This table reflects what's actually in kconmon-ng v1.2.0 and Goldpinger's documented feature set — not aspirational claims.

| Capability | Goldpinger | kconmon-ng |
|---|---|---|
| Node-to-node reachability | ✅ (HTTP peer ping; optional external TCP/HTTP/DNS targets) | ✅ (TCP, UDP, ICMP) |
| Built-in web UI / connectivity graph | ✅ | ❌ (Grafana dashboards instead) |
| Protocol breadth (per-protocol checks) | ✅ (HTTP peer ping + UDP probe + optional TCP/HTTP/DNS target checks), but not per-peer ICMP | ✅ (TCP/UDP/ICMP/DNS/HTTP as separate per-peer checks) |
| Per-hop traceroute on failure | ❌ | ✅ (reactive MTR, per-hop RTT/loss metrics) |
| Packet loss / hop-count / RTT | ✅ (UDP probe: loss %, hop count, RTT, dup/out-of-order) | ✅ (UDP jitter + loss ratio, ICMP loss ratio) |
| Zone/topology-aware labels | Partial | ✅ (`source_zone`/`destination_zone`, auto-discovered from node labels since v1.2.0) |
| Controller HA / leader election | N/A (fully mesh, no controller) | ✅ (`controller.leaderElection`, active/standby) |
| Prometheus metrics | ✅ | ✅ |
| Pre-built Grafana dashboards | Community-provided | ✅ (3 dashboards ship in-repo) |
| Self-monitoring (alerts if the monitor itself degrades) | ❌ | ✅ (`KconmonAgentsMissing`, `KconmonControllerDown`) |
| Setup complexity | Low (single DaemonSet) | Low-medium (Helm chart, agent + controller) |

## When Goldpinger is enough

If your question is "did last night's CNI rollout partition the mesh, yes or no" — Goldpinger answers that in minutes, with a UI you can point a non-Prometheus-fluent teammate at. It's simpler to operate, has one moving part, and its web graph is genuinely useful for a quick visual sanity check. Don't rip it out to install kconmon-ng if all you need is connectivity/no-connectivity.

## When you need kconmon-ng

The pattern that motivates kconmon-ng: intermittent, protocol-specific degradation. A CNI or kernel upgrade doesn't usually kill connectivity outright — it degrades *some* traffic on *some* node pairs. UDP jitter creeps up. DNS resolution starts timing out for some resolvers but not others. HTTP health checks flap. A binary reachability check won't surface any of that; you need per-protocol latency/loss time series and, when something does fail, an automatic hop-by-hop trace instead of someone SSHing in to run `mtr` by hand.

## Install

```bash
helm install kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng \
  --version 1.2.0 \
  --set serviceMonitor.enabled=true \
  --set prometheusRule.enabled=true
```

Verify:

```bash
kubectl get pods -l app.kubernetes.io/name=kconmon-ng -o wide
# Expect: 1 controller pod + one agent pod per node, all Running, RESTARTS 0
```

The agent DaemonSet needs the `NET_RAW` capability for ICMP and MTR (the chart sets this by default); the controller's ServiceAccount needs `get/list/watch` on `nodes` (also handled by the chart when `serviceAccount.create: true`).

```
![Overview dashboard screenshot: Connectivity Matrix + per-protocol success rate panels]
```

## PromQL you'll actually use

Cluster-wide UDP packet loss above the alert threshold, by node pair:

```promql
kconmon_ng_udp_packet_loss_ratio > 0.5
```

TCP failure rate, useful to eyeball trend before it crosses the alert threshold:

```promql
rate(kconmon_ng_tcp_results_total{result="fail"}[5m])
```

p95 TCP connect duration, per source/destination pair:

```promql
histogram_quantile(0.95, sum(rate(kconmon_ng_tcp_connect_duration_seconds_bucket[5m])) by (le, source_node, destination_node))
```

Per-hop RTT from the last MTR trace for a specific pair (swap in real node names):

```promql
kconmon_ng_mtr_hop_rtt_seconds{source_node="node-5", destination_node="node-17"}
```

Are all expected agents registered:

```promql
kconmon_ng_controller_registered_agents < kconmon_ng_controller_expected_agents
```

These map directly to the default `PrometheusRule` alerts shipped in the chart (`UDPLossHigh`, `TCPChecksFailing`, `DNSChecksFailing`, `KconmonAgentsMissing`, `KconmonControllerDown`), so you're not writing PromQL from scratch — the chart's `prometheusRule.enabled: true` gives you a working starting point.

```
![Zone Heatmap dashboard screenshot: cross-zone latency and loss panels]
```

## Notable v1.2.0 additions

Two changes worth knowing about if you're evaluating now:

- **Zone auto-discovery** — agents no longer need a manually configured zone. The controller resolves each agent's zone from its node's failure-domain label (`failureDomainLabel`, default `topology.kubernetes.io/zone`) at registration time and pushes it to the agent; `agent.zone` still overrides if explicitly set. This makes the Zone Heatmap dashboard useful out of the box on multi-zone clusters without per-agent config.
- **Strict config validation** — the config is now decoded with unknown-field rejection and per-checker validation (positive intervals/timeouts, valid HTTP target URLs, non-empty DNS host lists). A bad config now fails startup loudly, or is rejected on hot-reload with the previous config staying active, instead of silently doing nothing.

## Honest limitations

- No built-in web UI or connectivity graph — you're building on Grafana + Prometheus, not clicking through a standalone app.
- MTU detection, root-cause hints, and a kubectl plugin are on the roadmap, not shipped in v1.2.0 — don't evaluate against features that don't exist yet.
- This operates at the node-to-node probe level, not application traffic — it's not a substitute for service mesh telemetry or eBPF-based tracing if that's what you need.

## Bottom line

Goldpinger and kconmon-ng aren't really competing for the same job. Goldpinger tells you fast whether the mesh is connected, with minimal setup and a UI anyone can read. kconmon-ng trades that simplicity for protocol depth: five independent checkers, per-hop failure diagnostics, zone-aware metrics, and self-monitoring — useful when "is it up" isn't the question anymore and "which hop, which protocol, which pair" is.

- Repo: <https://github.com/EsDmitrii/kconmon-ng>
- Chart: `oci://ghcr.io/esdmitrii/charts/kconmon-ng`

# kconmon-ng

Kubernetes Node Connectivity Monitor — Next Generation. kconmon-ng continuously
measures pod-to-pod and node-to-node network health across a cluster. An **agent
DaemonSet** runs a probe on every node and a **controller Deployment** hands each
agent its peer list over gRPC. Agents run TCP, UDP, ICMP, DNS and HTTP checkers
against their peers and trigger a reactive MTR trace on failures, exposing all
results as Prometheus metrics.

## Prerequisites

- Kubernetes 1.19+
- Helm 3.8+ (OCI registry support)
- Optional: Prometheus Operator, if you want the `ServiceMonitor` and
  `PrometheusRule` resources (`serviceMonitor.enabled` / `prometheusRule.enabled`)
- The agent Pods request the `NET_RAW` capability (for ICMP / raw sockets used by
  the ICMP checker and MTR)

## Installing

The chart is published as an OCI artifact on GHCR.

```bash
helm install kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng --version 1.3.2
```

With custom values:

```bash
helm install kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng \
  --version 1.3.2 -f values.yaml
```

### Upgrading

```bash
helm upgrade kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng \
  --version 1.3.2 -f values.yaml
```

### Uninstalling

```bash
helm uninstall kconmon-ng
```

## Values

The table below lists the most relevant parameters. See
[`values.yaml`](values.yaml) for the complete set.

| Key | Default | Description |
| --- | --- | --- |
| `controller.replicaCount` | `1` | Number of controller replicas |
| `controller.leaderElection` | `true` | Enable leader election between controller replicas |
| `controller.resources` | requests `50m`/`64Mi`, limits `200m`/`128Mi` | Controller resource requests/limits |
| `agent.tolerations` | `[{operator: Exists}]` | Agent DaemonSet tolerations (default: run on all nodes) |
| `agent.resources` | requests `50m`/`64Mi`, limits `200m`/`128Mi` | Agent resource requests/limits |
| `config.metricsPrefix` | `kconmon_ng` | Prefix for all exported Prometheus metrics |
| `config.checkers.tcp.enabled` | `true` | Enable TCP checker (interval `5s`, timeout `1s`) |
| `config.checkers.udp.enabled` | `true` | Enable UDP checker (interval `5s`, timeout `250ms`, `packets: 5`) |
| `config.checkers.icmp.enabled` | `true` | Enable ICMP checker (interval `5s`, timeout `1s`) |
| `config.checkers.dns.enabled` | `true` | Enable DNS checker (interval `5s`, timeout `5s`) |
| `config.checkers.http.enabled` | `false` | Enable HTTP checker (interval `30s`, timeout `5s`) |
| `serviceMonitor.enabled` | `false` | Create a Prometheus Operator `ServiceMonitor` |
| `prometheusRule.enabled` | `false` | Create a Prometheus Operator `PrometheusRule` with the built-in alerts |
| `networkPolicy.enabled` | `false` | Create a `NetworkPolicy` (set `networkPolicy.prometheusNamespace` to allow scraping) |
| `pdb.enabled` | `true` | Create a `PodDisruptionBudget` (`pdb.minAvailable: 1`) |

## Metrics & Alerts

All metrics are prefixed with `config.metricsPrefix` (default `kconmon_ng`).
Selected key metrics:

- `kconmon_ng_tcp_results_total` — total TCP probe results (labelled by `result`)
- `kconmon_ng_udp_packet_loss_ratio` — UDP packet loss ratio (0.0–1.0)
- `kconmon_ng_icmp_packet_loss_ratio` — ICMP packet loss ratio (0.0–1.0)
- `kconmon_ng_dns_results_total` — total DNS resolution results (labelled by `result`)
- `kconmon_ng_controller_registered_agents` — agents currently registered with the controller
- `kconmon_ng_controller_expected_agents` — schedulable nodes expected to run an agent

When `prometheusRule.enabled` is `true`, the chart ships built-in alerting rules,
including:

- `UDPLossHigh` — sustained high UDP packet loss
- `TCPChecksFailing` — TCP connectivity checks failing
- `KconmonAgentsMissing` — fewer agents registered than schedulable nodes
- `KconmonControllerDown` — no active controller leader

## Links

- GitHub repository: <https://github.com/EsDmitrii/kconmon-ng>
- Grafana dashboards: [`dashboards/`](https://github.com/EsDmitrii/kconmon-ng/tree/main/dashboards)
  (`overview.json`, `node-detail.json`, `zone-heatmap.json`)

# kconmon-ng

Kubernetes Node Connectivity Monitor — Next Generation. kconmon-ng makes
inter-node connectivity a measured fact instead of a guess. An **agent
DaemonSet** probes from every node and a **controller Deployment** hands each
agent its peer list over gRPC. Agents run TCP, UDP, ICMP, DNS and HTTP checkers,
fire a reactive MTR trace when a probe fails, and export latency, jitter, loss
and per-hop results as Prometheus metrics — per ordered node pair, per protocol.

An optional [Console](#console-optional) web UI ships in the same chart, off by
default. The project README has the full tour:
<https://github.com/EsDmitrii/kconmon-ng#readme>.

## Prerequisites

- Kubernetes 1.31+ (CI tests against 1.36)
- Helm 4 (Helm ≥3.14 also works; the chart ships as an OCI artifact)
- Optional: Prometheus Operator, if you want the `ServiceMonitor` and
  `PrometheusRule` resources (`serviceMonitor.enabled` / `prometheusRule.enabled`)
- The agent Pods request the `NET_RAW` capability (for ICMP / raw sockets used by
  the ICMP checker and MTR)
- The ICMP checker opens an unprivileged ICMP "ping" socket, which the kernel
  gates on `net.ipv4.ping_group_range`. Some container runtimes leave this at the
  closed default (`1 0`), so the chart sets the (safe) sysctl via
  `agent.podSecurityContext`. Set `agent.podSecurityContext: {}` to opt out.

## Installing

The chart is published as an OCI artifact on GHCR.

```bash
helm install kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng --version 1.9.0
```

With the Prometheus Operator objects, which is what most installs want:

```bash
helm upgrade --install kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng \
  --version 1.9.0 \
  --set serviceMonitor.enabled=true \
  --set prometheusRule.enabled=true
```

With custom values:

```bash
helm install kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng \
  --version 1.9.0 -f values.yaml
```

### Upgrading

```bash
helm upgrade kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng \
  --version 1.9.0 -f values.yaml
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
| `agent.securityContext` | `{capabilities: {add: [NET_RAW]}}` | Agent container securityContext (NET_RAW for ICMP/MTR raw sockets) |
| `agent.podSecurityContext` | `{sysctls: [{name: net.ipv4.ping_group_range, value: "0 2147483647"}]}` | Agent Pod securityContext; opens `ping_group_range` so the ICMP checker can open ping sockets. Set to `{}` to opt out |
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

## Console (optional)

An optional web UI Deployment, off by default (`console.enabled: false`).
Read-only pages (topology, matrix, PromQL) work with no extra setup; setting
`console.database.mode` (PostgreSQL, via CloudNativePG or an external DSN)
and `console.auth.mode` (`anonymous | local | header | oidc`) adds durable
event/run history, authentication, RBAC and an on-demand diagnostics runner.
Every knob is documented inline in this chart's `values.yaml`, and the HTTP
API is specified in
[`docs/console-api.yaml`](https://github.com/EsDmitrii/kconmon-ng/blob/main/docs/console-api.yaml).

| Key | Default | Description |
| --- | --- | --- |
| `console.enabled` | `false` | Deploy the Console |
| `console.replicas` | `2` | Console replica count (stateless; realtime fan-out needs `console.valkey.mode` set for `replicas > 1`) |
| `console.auth.mode` | `anonymous` | `anonymous \| local \| header \| oidc` |
| `console.database.mode` | `disabled` | `disabled \| cnpg \| external` — PostgreSQL persistence |
| `console.kubernetesContext.enabled` | `false` | Capture core/v1 Events into the Investigate timeline; renders a console-only ServiceAccount and a `ClusterRole` for events |
| `console.alerting.enabled` | `false` | Manage Prometheus alert rules from the Console; renders a **namespaced** `Role` for `monitoring.coreos.com/prometheusrules` and applies one `PrometheusRule` object (`console.alerting.bundleName`). Needs a database and the Prometheus Operator CRD |
| `console.webhooks.encryptionKeySecret.name` | `""` | Secret holding the AES-256-GCM key that encrypts webhook signing secrets at rest; empty leaves webhook create/test answering 503 |

## Metrics & Alerts

All metrics are prefixed with `config.metricsPrefix` (default `kconmon_ng`).
Selected key metrics:

- `kconmon_ng_tcp_results_total` — total TCP probe results (labelled by `result`)
- `kconmon_ng_udp_packet_loss_ratio` — UDP packet loss ratio (0.0–1.0)
- `kconmon_ng_icmp_packet_loss_ratio` — ICMP packet loss ratio (0.0–1.0)
- `kconmon_ng_dns_results_total` — total DNS resolution results (labelled by `result`)
- `kconmon_ng_controller_registered_agents` — agents currently registered with the controller
- `kconmon_ng_controller_expected_agents` — schedulable nodes expected to run an agent

With `prometheusRule.enabled=true` the chart renders six built-in rules. Every
one of them annotates with the labels its own series carry, so a notification
names the failing pair, direction and measured value instead of repeating one
generic sentence per firing series:

| Rule | Fires when | Annotation identifies |
| --- | --- | --- |
| `UDPLossHigh` | `kconmon_ng_udp_packet_loss_ratio > 0.5` for 5m | source → destination node, both zones, loss % |
| `TCPChecksFailing` | TCP **failure ratio** > 5% for 5m | source → destination node, both zones, failed % |
| `DNSChecksFailing` | DNS **failure ratio** > 5% for 5m | source node + zone, queried `host`, `resolver`, failed % |
| `ExternalChecksFailing` | External **failure ratio** > 10% for 5m | source node + zone, `target`, `target_kind`, failed % |
| `KconmonAgentsMissing` | `expected_agents - registered_agents > 0` for 10m | controller `instance` and how many agents are missing |
| `KconmonControllerDown` | `absent(kconmon_ng_controller_leader == 1)` for 5m | nothing to identify; `absent()` has no series labels |

The three `*ChecksFailing` rules compare a **failure ratio** —
`rate(fail) / rate(all results)` for the same pair — rather than the older
`rate(fail) > 0`. A raw rate stays positive for the whole window after one
flaky probe, so it reported "a probe failed recently" instead of "this link is
unhealthy". The ratio keeps a single failure inside a healthy stream below the
threshold while a genuinely broken link crosses it immediately. Thresholds are
5% in-cluster (TCP, DNS) and 10% for external targets, which cross networks the
cluster operator does not run. Grouping stays per pair — that granularity is the
point, and it is what lets the annotation name a specific link.

A pair that has never failed has no `{result="fail"}` series, so the division
produces no sample for it and the rule stays silent rather than materialising a
zero for every pair in the mesh.

The last two rules watch kconmon-ng itself, so a monitor that goes quiet pages
you instead of looking healthy. Both need `controller.leaderElection=true`: the
node informer and the leader metric only run on the leader.

Alert expressions are the only field the chart rewrites: `config.metricsPrefix`
is applied to `expr` via `replace "kconmon_ng" $prefix`, so metric names in
`prometheusRule.rules` must stay written with the literal `kconmon_ng` prefix.
Annotations are passed through untouched, which is what lets
`{{ $labels.source_node }}` and `{{ $value | humanizePercentage }}` reach
Prometheus intact — and also why annotations avoid naming metrics, since those
would not follow a custom prefix.

`prometheusRule.enabled` and `console.alerting.enabled` are two different things
and neither implies the other. The former renders a static `PrometheusRule` from
the `prometheusRule.rules` values — the self-monitoring set above, edited in Git.
The latter lets operators build rules in the Console UI, stores them in
PostgreSQL and reconciles them into a *separate*, console-owned `PrometheusRule`
object. Run both, either, or neither.

## Links

- GitHub repository: <https://github.com/EsDmitrii/kconmon-ng>
- Grafana dashboards: [`dashboards/`](https://github.com/EsDmitrii/kconmon-ng/tree/main/dashboards)
  (`overview.json`, `node-detail.json`, `zone-heatmap.json`)

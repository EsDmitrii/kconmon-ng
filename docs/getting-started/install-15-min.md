# Install in 15 minutes

From `helm install` to per-pair, per-protocol metrics in Prometheus. Nothing on
this page needs the Console — that is the [next page](enable-the-console.md).

## Prerequisites

- **Kubernetes 1.31+** (CI runs against 1.36).
- **Helm 4** — the chart ships as an OCI artifact; Helm ≥3.14 also works.
- **Prometheus** somewhere to scrape the metrics. The Prometheus Operator is
  optional: with it you get the bundled `ServiceMonitor` and alert rules; a
  [plain `scrape_config`](#no-prometheus-operator) works too.

The agent needs **no added capabilities**: ICMP and MTR ride the unprivileged
ICMP socket that the `net.ipv4.ping_group_range` sysctl opens, and the chart
sets that sysctl — a kubelet *safe* one — along with RBAC for the controller's
node watch. Every component drops `ALL` capabilities and passes restricted
Pod Security Standards unchanged.

!!! tip "No cluster at hand?"
    `make local-up` in a repo clone brings up Minikube with Prometheus, Grafana
    and kconmon-ng in one command, and
    [the demo](../demo/breaking-cni.md) then breaks the network on purpose so
    you can watch the tool catch it.

## Install the chart

The chart is published as an OCI artifact on GHCR. With the Prometheus
Operator objects, which is what most installs want:

```bash
helm upgrade --install kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng \
  --set serviceMonitor.enabled=true \
  --set prometheusRule.enabled=true
```

Without the operator, drop both `--set` flags. The examples track the latest
published chart; pass `--version` if you need a pinned, reproducible install.

Expect one controller pod plus one agent per node — the agent DaemonSet
tolerates every taint by default, so control-plane nodes get one too:

```bash
kubectl get pods -l app.kubernetes.io/name=kconmon-ng -o wide
```

## Verify agents are probing

Metrics start flowing within seconds of the agents registering. To look at
them raw, port-forward any agent and read its `/metrics`:

```bash
AGENT=$(kubectl get pods -l app.kubernetes.io/component=agent -o jsonpath='{.items[0].metadata.name}')
kubectl port-forward "$AGENT" 8080 &
curl -s http://localhost:8080/metrics | grep '^kconmon_ng' | head
```

You should see per-pair series naming real node pairs —
`kconmon_ng_udp_packet_loss_ratio{source_node="…",destination_node="…"}` and
friends. If the list is empty, check that more than one agent is running: a
one-node cluster has no peers to probe.

## Look at the metrics

Three questions, three queries, against your Prometheus:

```promql
# Is any pair losing UDP packets? (all pairs should read 0)
kconmon_ng_udp_packet_loss_ratio

# Is any TCP probe failing anywhere?
sum(rate(kconmon_ng_tcp_results_total{result="fail"}[5m]))

# Is the fleet complete? (both numbers should match)
kconmon_ng_controller_registered_agents
kconmon_ng_controller_expected_agents
```

Every exported family, its labels and the nine bundled alert rules are in the
[metrics and alerting reference](../metrics.md).

### Grafana dashboards

Three dashboards ship with the project — cluster overview, zone heatmap,
per-node detail. If your Grafana runs the dashboard sidecar, let the chart
install them as ConfigMaps:

```bash
helm upgrade kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng \
  --reuse-values --set dashboards.enabled=true
```

Otherwise import the JSON files from the repo's
[`dashboards/`](https://github.com/EsDmitrii/kconmon-ng/tree/main/dashboards)
directory through the Grafana UI.

### No Prometheus Operator?

Skip `serviceMonitor.enabled` and add one scrape job instead. The agent and
the controller both expose a dedicated `metrics` port (`config.metricsPort`,
`9091` by default) on their Services — separate from the unauthenticated API
port on purpose — so a single endpoints-role job covers the whole fleet:

```yaml
scrape_configs:
  - job_name: kconmon-ng
    kubernetes_sd_configs:
      - role: endpoints
        namespaces:
          names: [default] # the namespace kconmon-ng is installed into
    relabel_configs:
      - source_labels: [__meta_kubernetes_service_label_app_kubernetes_io_name]
        regex: kconmon-ng
        action: keep
      - source_labels: [__meta_kubernetes_endpoint_port_name]
        regex: metrics
        action: keep
      - source_labels: [__meta_kubernetes_pod_node_name]
        target_label: node
```

The bundled alert rules are plain PromQL: if `PrometheusRule` is not an option
either, lift them from the
[default alerting rules](../metrics.md#default-alerting-rules) into your own
`rule_files`.

## Next steps

- **[Enable the console](enable-the-console.md)** — the web UI is one flag on
  the same release.
- **[Catch a breakage](catch-a-breakage.md)** — break a node pair on purpose
  and watch the tool isolate it.
- **[Helm values](../reference/helm-values.md)** — every knob, with the full
  commented `values.yaml`.
- **[Scaling and cardinality](../metrics.md#scaling-and-cardinality)** — read
  this before installing on a large cluster: pairs grow as N×(N−1).

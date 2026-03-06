# kconmon-ng

[![Release](https://github.com/EsDmitrii/kconmon-ng/actions/workflows/release.yaml/badge.svg)](https://github.com/EsDmitrii/kconmon-ng/actions/workflows/release.yaml)
[![Artifact Hub](https://img.shields.io/endpoint?url=https://artifacthub.io/badge/repository/kconmon-ng)](https://artifacthub.io/packages/search?repo=kconmon-ng)
[![Go](https://img.shields.io/badge/go-1.25-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![Go Report Card](https://goreportcard.com/badge/github.com/EsDmitrii/kconmon-ng)](https://goreportcard.com/report/github.com/EsDmitrii/kconmon-ng)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue)](LICENSE)

**Kubernetes Node Connectivity Monitor — Next Generation**

kconmon-ng continuously probes network connectivity between every pair of Kubernetes nodes using TCP, UDP, ICMP, DNS, and HTTP checks. All results are exported as Prometheus metrics with `source_node`, `destination_node`, `source_zone`, and `destination_zone` labels, enabling per-node and cross-zone visibility in Grafana. When a probe fails, MTR traceroute is triggered automatically to capture the failure path.

## Table of Contents

- [How it works](#how-it-works)
- [Features](#features)
- [Requirements](#requirements)
- [Quick Start](#quick-start)
- [Configuration](#configuration)
- [Helm Chart](#helm-chart)
- [Metrics](#metrics)
- [Alerting](#alerting)
- [Grafana Dashboards](#grafana-dashboards)
- [API Endpoints](#api-endpoints)
- [Development](#development)
- [Contributing](#contributing)
- [License](#license)

## How it works

```bash
                          +-------------------------------------------+
                          |          Controller (Deployment)          |
                          |                                           |
                          |  - Agent registry (heartbeat eviction)    |
                          |  - NodeWatcher (k8s node zone labels)     |
                          |  - Topology API / gRPC streaming server   |
                          |  - Leader election (active/standby HA)    |
                          +---------------------+---------------------+
                                                |
                      gRPC stream (peer list sync + updates on change)
                                                |
                        +----------------------+----------------------+
                        |                      |                      |
              +---------+---------+  +---------+---------+  +---------+---------+
              | Agent (node-1)    |  | Agent (node-2)    |  | Agent (node-3)    |
              | DaemonSet         |  | DaemonSet         |  | DaemonSet         |
              |                   |  |                   |  |                   |
              | TCP / UDP / ICMP  |  | TCP / UDP / ICMP  |  | TCP / UDP / ICMP  |
              | DNS / HTTP        |  | DNS / HTTP        |  | DNS / HTTP        |
              | MTR on failure    |  | MTR on failure    |  | MTR on failure    |
              | /metrics :8080    |  | /metrics :8080    |  | /metrics :8080    |
              +-------------------+  +-------------------+  +-------------------+
                ^                                                             ^
                +------------------- probes between all pairs ---------------+
```

Each agent registers with the controller over gRPC and receives a live-updated list of peers. The scheduler runs each enabled checker concurrently against every peer. Agents filter themselves out of the peer list to prevent self-probing. When a TCP, UDP, or ICMP probe fails, MTR is triggered once per (source, destination) pair per cooldown window, and its hop-by-hop metrics are exported. On peer topology changes, stale gauge values (loss ratios, jitter, MTR hops) are reset automatically to avoid ghost metrics.

## Features

| Feature                   | Details                                                                                                                      |
| ------------------------- | ---------------------------------------------------------------------------------------------------------------------------- |
| **TCP probing**           | Connect + HTTP readiness handshake; measures connect time and total RTT                                                      |
| **UDP probing**           | Configurable packet burst; measures mean RTT, jitter, and packet loss ratio                                                  |
| **ICMP ping**             | Raw Echo Request/Reply; IPv4 and IPv6; measures RTT and loss ratio                                                           |
| **DNS checks**            | Resolves configurable hostnames via system resolver or explicit upstream DNS servers; one metric series per (host, resolver) |
| **HTTP checks**           | External URL probes with phased timing: DNS, TCP connect, TLS handshake, TTFB, total; optional response body pattern match   |
| **MTR on failure**        | Automatic traceroute when TCP/UDP/ICMP fails; per-hop RTT and loss exported as metrics; configurable cooldown per pair       |
| **gRPC streaming**        | Controller pushes full peer list on connect and incremental updates on change — agents never poll                            |
| **Self-probe prevention** | Agents filter themselves from the peer list by agent ID, node name, and pod IP                                               |
| **Controller HA**         | Leader election via `leaderElection: true`; standby replicas stay ready but passive                                          |
| **Node topology**         | Controller watches Kubernetes node objects; zone info enriches topology API and can be used in dashboards                    |
| **Config hot-reload**     | Config file changes are applied without restart via fsnotify                                                                 |
| **Prometheus metrics**    | All checks export metrics with `source_node`, `destination_node`, `source_zone`, `destination_zone` labels                   |
| **OpenTelemetry**         | Optional OTLP trace export for probe runs                                                                                    |
| **Helm chart**            | ServiceMonitor, PrometheusRule, NetworkPolicy, PDB, RBAC — all toggleable                                                    |
| **Grafana dashboards**    | Three pre-built dashboards: overview, per-node detail, cross-zone heatmap                                                    |

## Requirements

| Component           | Minimum version                           |
| ------------------- | ----------------------------------------- |
| Kubernetes          | 1.25+                                     |
| Helm                | 3.14+                                     |
| Prometheus Operator | any (for ServiceMonitor / PrometheusRule) |
| Go (build only)     | 1.25+                                     |

The agent DaemonSet requires the `NET_RAW` Linux capability for ICMP and MTR. The controller ServiceAccount requires `get`, `list`, `watch` on `nodes` (provided by the chart's ClusterRole when `serviceAccount.create: true`).

## Quick Start

### Helm (recommended)

```bash
# Install from OCI registry
helm install kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng \
  --version 1.0.0 \
  --set serviceMonitor.enabled=true \
  --set prometheusRule.enabled=true
```

Verify pods are running:

```bash
kubectl get pods -l app.kubernetes.io/name=kconmon-ng -o wide
# Expected: 1 controller pod + one agent pod per node, all Running, RESTARTS 0
```

Check that agents are exporting metrics:

```bash
AGENT=$(kubectl get pods -l app.kubernetes.io/component=agent -o jsonpath='{.items[0].metadata.name}')
kubectl port-forward "$AGENT" 8080 &
curl -s http://localhost:8080/metrics | grep '^kconmon_ng' | head -20
```

### From source

```bash
git clone https://github.com/EsDmitrii/kconmon-ng.git
cd kconmon-ng
make build          # produces bin/kconmon-ng-agent and bin/kconmon-ng-controller
make docker-build   # builds Docker images
```

For a full local environment with Minikube, Prometheus, and Grafana:

```bash
make local-up    # create cluster, build images, deploy everything, run smoke tests
make local-urls  # print Grafana / Prometheus / kconmon-ng URLs
make local-down  # tear down
```

See [hack/README.md](hack/README.md) for the step-by-step guide.

## Configuration

Configuration is loaded from a YAML file (default `/etc/kconmon-ng/config.yaml`, override with `KCONMON_NG_CONFIG`) and can be selectively overridden via environment variables. The config file is watched for changes and reloaded at runtime — no restart required.

### Full config reference

```yaml
metricsPrefix: kconmon_ng # prefix for all Prometheus metric names
httpPort: 8080 # HTTP port: /metrics, /healthz, /readyz, /api/v1/...
grpcPort: 9090 # gRPC port: agent-controller communication
logLevel: info # debug | info | warn | error
logFormat: json # json | text
failureDomainLabel: topology.kubernetes.io/zone # node label used as zone

# Agent-only: gRPC address of the controller
controllerAddress: "" # e.g. kconmon-ng-controller:9090

controller:
  leaderElection: true # enable leader election for HA (requires k8s RBAC)
  agentTtl: 30s # evict agents that miss heartbeats for this duration

checkers:
  tcp:
    enabled: true
    interval: 5s
    timeout: 1s

  udp:
    enabled: true
    interval: 5s
    timeout: 250ms
    packets: 5 # packets per probe burst (min 1)

  icmp:
    enabled: true
    interval: 5s
    timeout: 1s # requires NET_RAW capability

  dns:
    enabled: true
    interval: 5s
    hosts:
      - kubernetes.default.svc.cluster.local
    resolvers: [] # empty = system resolver; add IPs for explicit upstream DNS

  http:
    enabled: false
    interval: 30s
    timeout: 5s
    targets:
      - url: https://example.com/healthz
        method: GET # default GET
        expectStatus: 200 # 0 = any 2xx/3xx
        bodyPattern: "" # optional Go regexp matched against response body

  mtr:
    cooldown: 60s # minimum interval between traces for the same (src, dst) pair
    maxHops: 30 # max TTL / hop count (1–64)

observability:
  otel:
    enabled: false
    endpoint: "" # OTLP gRPC endpoint, e.g. otel-collector:4317
```

### Environment variable overrides

| Variable                          | Config field             |
| --------------------------------- | ------------------------ |
| `KCONMON_NG_CONFIG`               | path to config file      |
| `KCONMON_NG_MODE`                 | `mode`                   |
| `KCONMON_NG_METRICS_PREFIX`       | `metricsPrefix`          |
| `KCONMON_NG_LOG_LEVEL`            | `logLevel`               |
| `KCONMON_NG_LOG_FORMAT`           | `logFormat`              |
| `KCONMON_NG_CONTROLLER_ADDRESS`   | `controllerAddress`      |
| `KCONMON_NG_FAILURE_DOMAIN_LABEL` | `failureDomainLabel`     |
| `KCONMON_NG_NODE_NAME`            | injected by Downward API |
| `KCONMON_NG_POD_NAME`             | injected by Downward API |
| `KCONMON_NG_POD_IP`               | injected by Downward API |
| `KCONMON_NG_ZONE`                 | injected by Downward API |

## Helm Chart

### Installation

```bash
# From OCI registry
helm install kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng

# From local chart
helm install kconmon-ng ./charts/kconmon-ng -f my-values.yaml
```

### Key values

```yaml
controller:
  replicaCount: 2 # run 2 replicas; only the leader is active (leaderElection: true)
  leaderElection: true

agent:
  tolerations:
    - operator: Exists # schedule on ALL nodes, including control-plane and tainted nodes
  securityContext:
    capabilities:
      add: [NET_RAW] # required for ICMP and MTR

config:
  checkers:
    http:
      enabled: true
      targets:
        - url: https://kubernetes.default.svc.cluster.local/healthz
          method: GET
          expectStatus: 200

serviceMonitor:
  enabled: true # scrape agents and controller via Prometheus Operator
  interval: 15s

prometheusRule:
  enabled: true # deploy default alerting rules

networkPolicy:
  enabled: true # restrict ingress/egress to required paths only
  prometheusNamespace: monitoring

pdb:
  enabled: true # prevent controller eviction during node drain
  minAvailable: 1

serviceAccount:
  create: true # creates ClusterRole with nodes get/list/watch
```

See [charts/kconmon-ng/values.yaml](charts/kconmon-ng/values.yaml) for the complete reference.

## Metrics

All metric names use the configurable prefix (default `kconmon_ng`). The common label set for peer metrics is `source_node`, `destination_node`, `source_zone`, `destination_zone`.

### Agent — TCP

| Metric                                    | Type      | Labels          | Description                        |
| ----------------------------------------- | --------- | --------------- | ---------------------------------- |
| `kconmon_ng_tcp_connect_duration_seconds` | histogram | peer            | TCP connect phase duration         |
| `kconmon_ng_tcp_total_duration_seconds`   | histogram | peer            | Total TCP probe RTT                |
| `kconmon_ng_tcp_results_total`            | counter   | peer + `result` | Probe outcomes: `success` / `fail` |

### Agent — UDP

| Metric                             | Type      | Labels          | Description                  |
| ---------------------------------- | --------- | --------------- | ---------------------------- |
| `kconmon_ng_udp_rtt_seconds`       | histogram | peer            | Mean UDP round-trip time     |
| `kconmon_ng_udp_jitter_seconds`    | gauge     | peer            | Inter-packet delay variation |
| `kconmon_ng_udp_packet_loss_ratio` | gauge     | peer            | Packet loss ratio (0.0–1.0)  |
| `kconmon_ng_udp_results_total`     | counter   | peer + `result` | Probe outcomes               |

### Agent — ICMP

| Metric                              | Type      | Labels          | Description                 |
| ----------------------------------- | --------- | --------------- | --------------------------- |
| `kconmon_ng_icmp_rtt_seconds`       | histogram | peer            | ICMP round-trip time        |
| `kconmon_ng_icmp_packet_loss_ratio` | gauge     | peer            | Packet loss ratio (0.0–1.0) |
| `kconmon_ng_icmp_results_total`     | counter   | peer + `result` | Probe outcomes              |

### Agent — DNS

| Metric                            | Type      | Labels                                           | Description                              |
| --------------------------------- | --------- | ------------------------------------------------ | ---------------------------------------- |
| `kconmon_ng_dns_duration_seconds` | histogram | `host`, `resolver`, `source_node`, `source_zone` | Resolution duration per (host, resolver) |
| `kconmon_ng_dns_results_total`    | counter   | same + `result`                                  | Resolution outcomes                      |

### Agent — HTTP

| Metric                                     | Type      | Labels                                                                 | Description            |
| ------------------------------------------ | --------- | ---------------------------------------------------------------------- | ---------------------- |
| `kconmon_ng_http_dns_duration_seconds`     | histogram | `url`, `source_node`, `source_zone`                                    | DNS phase              |
| `kconmon_ng_http_connect_duration_seconds` | histogram | same                                                                   | TCP connect phase      |
| `kconmon_ng_http_tls_duration_seconds`     | histogram | same                                                                   | TLS handshake phase    |
| `kconmon_ng_http_ttfb_seconds`             | histogram | same                                                                   | Time to first byte     |
| `kconmon_ng_http_total_duration_seconds`   | histogram | same                                                                   | Total request duration |
| `kconmon_ng_http_results_total`            | counter   | `url`, `method`, `status_code`, `source_node`, `source_zone`, `result` | Request outcomes       |

### Agent — MTR

| Metric                           | Type    | Labels                                                    | Description                    |
| -------------------------------- | ------- | --------------------------------------------------------- | ------------------------------ |
| `kconmon_ng_mtr_triggered_total` | counter | peer                                                      | Number of MTR traces triggered |
| `kconmon_ng_mtr_hops`            | gauge   | peer                                                      | Hop count in the last trace    |
| `kconmon_ng_mtr_hop_rtt_seconds` | gauge   | `source_node`, `destination_node`, `hop_number`, `hop_ip` | Per-hop RTT                    |

### Controller

| Metric                                     | Type    | Description                               |
| ------------------------------------------ | ------- | ----------------------------------------- |
| `kconmon_ng_controller_registered_agents`  | gauge   | Currently registered agents               |
| `kconmon_ng_controller_grpc_connections`   | gauge   | Active gRPC streaming connections         |
| `kconmon_ng_controller_peer_updates_total` | counter | Peer list updates broadcast to agents     |
| `kconmon_ng_controller_leader`             | gauge   | `1` if this instance is the active leader |

## Alerting

Default rules are deployed when `prometheusRule.enabled: true`. The metric prefix in `expr` is substituted automatically from `config.metricsPrefix`.

```yaml
- alert: UDPLossHigh
  expr: kconmon_ng_udp_packet_loss_ratio > 0.5
  for: 5m
  labels:
    severity: warning
  annotations:
    summary: High UDP packet loss detected between nodes

- alert: TCPChecksFailing
  expr: rate(kconmon_ng_tcp_results_total{result="fail"}[5m]) > 0
  for: 5m
  labels:
    severity: warning
  annotations:
    summary: TCP connectivity checks are failing

- alert: DNSChecksFailing
  expr: rate(kconmon_ng_dns_results_total{result="fail"}[5m]) > 0
  for: 5m
  labels:
    severity: warning
  annotations:
    summary: DNS resolution checks are failing
```

Additional rules can be appended under `prometheusRule.rules` in Helm values.

## Grafana Dashboards

Three dashboards are included in `dashboards/`. Import them via the Grafana UI or the API:

```bash
GRAFANA_URL=http://localhost:3000
for f in dashboards/*.json; do
  curl -s -X POST "$GRAFANA_URL/api/dashboards/db" \
    -H "Content-Type: application/json" \
    -u admin:admin \
    -d "{\"dashboard\": $(cat "$f"), \"overwrite\": true}" | python3 -m json.tool
done
```

| Dashboard               | UID                       | Contents                                                                    |
| ----------------------- | ------------------------- | --------------------------------------------------------------------------- |
| **kconmon-ng Overview** | `kconmon-ng-overview`     | Cluster-wide success rates, latencies, controller status, registered agents |
| **Node Detail**         | `kconmon-ng-node-detail`  | Per-node TCP/UDP/ICMP/DNS/HTTP breakdown by destination                     |
| **Zone Heatmap**        | `kconmon-ng-zone-heatmap` | Cross-zone latency and loss heatmap for TCP, UDP, and ICMP                  |

## API Endpoints

### Agent

| Endpoint          | Method | Description                                           |
| ----------------- | ------ | ----------------------------------------------------- |
| `/healthz`        | GET    | Liveness probe — always `200 ok`                      |
| `/readyz`         | GET    | Readiness probe — `503` until peer watch is confirmed |
| `/metrics`        | GET    | Prometheus metrics                                    |
| `/api/v1/version` | GET    | `{"version":"…","commit":"…"}`                        |

### Controller

| Endpoint           | Method | Description                                                             |
| ------------------ | ------ | ----------------------------------------------------------------------- |
| `/healthz`         | GET    | Liveness probe — always `200 ok`                                        |
| `/readyz`          | GET    | Readiness probe — `503` until gRPC server is bound                      |
| `/metrics`         | GET    | Prometheus metrics                                                      |
| `/api/v1/topology` | GET    | JSON snapshot of all registered agents and cluster nodes with zone info |
| `/api/v1/version`  | GET    | `{"version":"…","commit":"…"}`                                          |

Example topology response:

```json
{
  "nodes": [
    { "name": "node-1", "zone": "us-east-1a", "ready": true },
    { "name": "node-2", "zone": "us-east-1b", "ready": true }
  ],
  "agents": [
    {
      "id": "node-1-kconmon-ng-agent-xxxxx",
      "nodeName": "node-1",
      "podIP": "10.0.0.1",
      "zone": "us-east-1a"
    },
    {
      "id": "node-2-kconmon-ng-agent-yyyyy",
      "nodeName": "node-2",
      "podIP": "10.0.0.2",
      "zone": "us-east-1b"
    }
  ],
  "timestamp": "2025-01-01T00:00:00Z"
}
```

## Development

```bash
make build        # build agent and controller binaries → bin/
make test         # run unit tests
make test-race    # run tests with race detector
make test-cover   # run tests with coverage → coverage.html
make test-fuzz    # run fuzz tests (30s)
make lint         # golangci-lint
make fmt          # gofmt + goimports
make proto        # regenerate protobuf (requires buf)
make helm-lint    # lint Helm chart against all CI value sets
make docker-build # build Docker images
```

For local end-to-end testing with Minikube, Prometheus, and Grafana see [hack/README.md](hack/README.md).

### CI

The CI pipeline (`.github/workflows/ci.yaml`) runs on every push and pull request to `main`:

- **Lint** — golangci-lint
- **Test** — `go test -race -covermode=atomic ./...`
- **Build** — `CGO_ENABLED=0` cross-compile of both binaries
- **Helm Lint** — chart linted against default, full, and minimal value sets

E2E tests (`.github/workflows/e2e.yaml`) run automatically after a successful Release workflow (i.e. on every `v*` tag).

Releases are published when a `v*` tag is pushed — Docker images and the Helm chart are pushed to GHCR.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for the full guide: prerequisites, building, testing, submitting PRs, and the release process.

## License

Apache License 2.0. See [LICENSE](LICENSE).

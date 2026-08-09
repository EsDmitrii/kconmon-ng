# kconmon-ng

[![Release](https://github.com/EsDmitrii/kconmon-ng/actions/workflows/release.yaml/badge.svg)](https://github.com/EsDmitrii/kconmon-ng/actions/workflows/release.yaml)
[![Artifact Hub](https://img.shields.io/endpoint?url=https://artifacthub.io/badge/repository/kconmon-ng)](https://artifacthub.io/packages/search?repo=kconmon-ng)
[![Go](https://img.shields.io/badge/go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue)](LICENSE)

**Someone says "the network is fine." Prove it.**

kconmon-ng makes inter-node connectivity a measured fact. An agent on every node
probes every other node over TCP, UDP and ICMP every five seconds, resolves DNS
and checks HTTP endpoints from each node, and exports latency, jitter and packet
loss for every ordered node pair, per protocol. A failed TCP, UDP or ICMP probe
fires an MTR trace to that peer, so the bad hop is already on record before
anyone goes looking for it.

The failures that cost you the evening are the partial ones. UDP dropping between
two nodes while TCP on the same pair stays clean. DNS timing out from one node
only. Jitter creeping up across a zone boundary. Each protocol is measured
separately for each pair, so a partial failure reads as exactly that — this pair,
this protocol, this hop — instead of disappearing into an aggregate that is still
green.

Everything downstream is built on those measurements and nothing else: an N×N
matrix, a topology map, MTR path history, incident timelines, alert rules that
Prometheus evaluates, and a `?at=` on the URL that rewinds every page to the
minute it broke.

<p align="center">
  <img src="docs/img/console-overview.png" alt="Console Overview: cluster health summary, worst node pairs, firing alerts and open incidents" width="100%">
</p>

<table>
  <tr>
    <td width="50%"><img src="docs/img/console-matrix.png" alt="Console Matrix: N×N heatmap of node-to-node loss and latency, one cell per ordered pair — a broken node reads as a red column"></td>
    <td width="50%"><img src="docs/img/console-investigate.png" alt="Console Investigate: merged timeline around one scope and window, with ranked candidate causes"></td>
  </tr>
  <tr>
    <td width="50%"><img src="docs/img/console-alerting.png" alt="Console Alerting: managed rules with their live sync status against the cluster"></td>
    <td width="50%"><img src="docs/img/console-timemachine.png" alt="Console Time Machine: the same matrix resolved at a past instant instead of now"></td>
  </tr>
</table>

## What it does

### The measurement core

One controller Deployment and one agent per node. Agents register with the
controller over gRPC and get a live-updated peer list — no polling, no per-agent
configuration. The controller reads each node's zone label at registration, so
every metric carries `source_zone`/`destination_zone` without anyone writing them
down.

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

Each enabled checker runs concurrently against every peer:

| Checker | What it measures |
|---|---|
| TCP | connect time and total RTT per peer |
| UDP | mean RTT, jitter and packet loss over a configurable packet burst |
| ICMP | echo RTT and loss (IPv4/IPv6) |
| DNS | resolution time per (hostname, resolver), system or explicit upstreams |
| HTTP | phased timing for configured URLs: DNS, connect, TLS, TTFB, total |
| MTR | reactive traceroute on failure, per-hop RTT and loss |

TCP, UDP, ICMP and DNS run out of the box on a 5s interval; HTTP is opt-in
because it needs URLs only you know.

Details that show up at 3am: a failed TCP, UDP or ICMP probe triggers MTR for
that pair under a per-pair cooldown, so a broken link cannot flood the cluster
with traces. When topology changes, stale per-pair gauges are reset instead of
leaving ghost readings for a node that no longer exists. An agent deregisters on
shutdown, so a rolling restart of kconmon-ng does not write false loss into its
own metrics. Config hot-reloads on change and is parsed with unknown keys
rejected — a typo fails fast instead of being silently ignored.

Six alert rules ship with the chart (`prometheusRule.enabled=true`): high UDP
loss, failing TCP, DNS and external checks, plus two that watch the monitor
itself — `KconmonAgentsMissing` and `KconmonControllerDown`. A kconmon-ng that
goes quiet pages you instead of looking healthy. Every metric, label and rule is
in [docs/metrics.md](docs/metrics.md).

### The Console

An optional web UI, **off by default**, deployed by the same chart. It reads the
same Prometheus and the same controller you already run — no second data path,
no agent changes. Read-only pages work with nothing but a Prometheus URL;
history, auth, incidents and alert rules need PostgreSQL.

- **Matrix, Topology, Overview and Explore** — the fleet as an N×N heatmap, as a
  map, as a health summary and as curated charts with A/B compare. With
  `controller.events.enabled` the matrix and topology are pushed over a
  WebSocket; without it they poll and say so.
- **Time Machine** — put `?at=` on any URL and every read surface resolves
  through that instant instead of now: topology folded from stored events, PromQL
  evaluated at `t`. Mutations are disabled while it is engaged, so you cannot
  change the fleet from inside the past.
- **Investigate and incidents** — one page that merges nine timeline sources
  around a scope and a window (topology events, K8s events, audit writes, MTR
  path changes, diagnostic runs, maintenance windows, annotations, threshold
  crossings, firing alerts) and ranks candidate causes by documented arithmetic:
  class weight times a linear decay over the five minutes before onset. No ML,
  and the constants are plain exported values in the scoring source the panel
  links to. Save one as an incident; the permalink rehydrates the exact scope
  and window.
- **Alerting** — build a rule from six typed templates or raw PromQL, watch the
  preview run that expression against your Prometheus right now, then let the
  console reconcile every enabled rule into **one** `PrometheusRule` object.
  Prometheus evaluates; the console only manages, and it records drift before
  fixing it. Rules other tools wrote are listed read-only and adopted by an
  explicit copy that never mutates the object it read.
- **MTR Explorer** — every traced path is content-hashed and deduped at ingest,
  so path history is a list of *changes*, not a wall of identical traces. Three
  panes, client-side diff, optional rDNS and GeoIP hop enrichment (off by
  default — it is the only part that talks to anything outside the cluster).
- **Diagnostics, external targets and schedules** — probe a pair on demand
  mid-incident, with run history and permalinks; define external targets and let
  agents check them continuously, with the CIDR allowlist enforced by the agent
  rather than the console.
- **Live feed and command palette** — a virtualized event stream with
  type/severity/scope filters, pause-and-buffer, and missed-event accounting;
  `⌘K` over navigation, actions and Time Machine.
- **Auth, RBAC, audit and webhooks** — `anonymous | local | header | oidc`,
  25 permissions across four built-in roles (`viewer`, `operator`,
  `alert-editor`, `admin`) plus custom ones, an audit log, API tokens, and
  outbound webhooks on incident and alert transitions, HMAC-signed with
  per-endpoint secrets encrypted at rest. Configuration exports as a versioned
  bundle and imports dry-run first.

### Grafana and PromQL

Everything the Console shows comes from Prometheus, so nothing stops you staying
there. Three dashboards ship in [`dashboards/`](dashboards/) — cluster overview,
zone heatmap, per-node detail — and the metric names are stable and documented,
so your own panels and recording rules are a `grep` away in
[docs/metrics.md](docs/metrics.md).

<p align="center">
  <img src="docs/img/overview.png" alt="Grafana Overview dashboard: fleet status tiles, per-protocol failure ratios and the top-10 worst pairs table" width="100%">
</p>

<table>
  <tr>
    <td width="50%"><img src="docs/img/zone-heatmap.png" alt="Grafana Zone Heatmap dashboard: zone-to-zone loss, RTT and MTR trigger matrices"></td>
    <td width="50%"><img src="docs/img/node-detail.png" alt="Grafana Node Detail dashboard: one node's outbound and inbound health against every peer"></td>
  </tr>
</table>

### From your terminal

`kubectl-kconmon` talks to the controller's HTTP API over a client-go
port-forward, so you can list topology and fire a one-shot check without a
browser. A failed check exits `2`, distinct from `1` for CLI or API errors, so it
composes in pipelines; `-o json` prints the raw result.

```
$ kubectl kconmon topology
NODE     ZONE         READY   AGENT                           AGENT IP
node-1   us-east-1a   yes     node-1-kconmon-ng-agent-aaaaa   10.0.0.1
node-2   us-east-1b   yes     node-2-kconmon-ng-agent-bbbbb   10.0.0.2
node-3   us-east-1c   no      -                               -

$ kubectl kconmon check node-1 node-2 --type udp
OK udp node-1 -> node-2 (us-east-1a -> us-east-1b)  duration=1.1ms
  sent=5 recv=5 loss=0% rtt=1.1ms jitter=240µs
```

Install it via krew from a release manifest, until it lands in the krew index:

```bash
kubectl krew install --manifest-url \
  https://github.com/EsDmitrii/kconmon-ng/releases/download/v1.4.0/kconmon.yaml
```

## Quickstart

Kubernetes 1.31+ (CI runs against 1.36), Helm 4 (the chart ships as an OCI
artifact; Helm ≥3.14 also works), and the
Prometheus Operator if you want the bundled `ServiceMonitor` and alert rules. The
agent needs `NET_RAW` for ICMP and MTR; the chart sets that up, along with the
sysctl the ICMP checker needs and RBAC for the controller's node watch.

```bash
helm upgrade --install kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng \
  --version 1.10.0 \
  --set serviceMonitor.enabled=true \
  --set prometheusRule.enabled=true
```

Expect one controller pod plus one agent per node:

```bash
kubectl get pods -l app.kubernetes.io/name=kconmon-ng -o wide
```

Metrics start flowing within seconds. To look at them raw:

```bash
AGENT=$(kubectl get pods -l app.kubernetes.io/component=agent -o jsonpath='{.items[0].metadata.name}')
kubectl port-forward "$AGENT" 8080 &
curl -s http://localhost:8080/metrics | grep '^kconmon_ng' | head
```

Then import the dashboards from [`dashboards/`](dashboards/) into Grafana — via
the UI, or:

```bash
for f in dashboards/*.json; do
  curl -s -X POST "http://localhost:3000/api/dashboards/db" \
    -H "Content-Type: application/json" -u admin:admin \
    -d "{\"dashboard\": $(cat "$f"), \"overwrite\": true}"
done
```

### Turn on the Console

It is a flag on the same release. Nothing else in the chart changes:

```bash
helm upgrade --install kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng \
  --version 1.10.0 \
  --set console.enabled=true \
  --set console.prometheus.url=http://prometheus-operated.monitoring:9090

kubectl port-forward svc/kconmon-ng-console 8081:8080
```

That gets you the read-only pages as an anonymous viewer on
<http://localhost:8081>. The rest is opt-in, one flag at a time: history, auth,
incidents and alert rules need `console.database.mode=cnpg` or `external`;
realtime push needs `controller.events.enabled=true`; alerting needs
`console.alerting.enabled=true` and the Prometheus Operator's `PrometheusRule`
CRD. Every knob is documented inline in
[charts/kconmon-ng/values.yaml](charts/kconmon-ng/values.yaml) and mapped in
the [chart README](charts/kconmon-ng/README.md).

### See it catch a failure

[docs/demo/breaking-cni.md](docs/demo/breaking-cni.md) is a reproducible
walkthrough on a disposable Minikube cluster. Blackhole UDP between two nodes,
watch exactly one cell of the matrix go red while TCP and ICMP on the same pair
stay green, watch MTR fire and the bundled alert go pending. Then break TCP, ICMP
and HTTP in three other places at once and see each failure isolated to its own
pair and protocol. The last part runs the same break through the Console:
correlate it on `/investigate`, save it as an incident, and declare an alert rule
that fires a webhook when the pair loses packets again.

## Reference

- [Configuration](docs/configuration.md) — config file, environment variables,
  Helm values, zone auto-discovery.
- [Metrics and alerting](docs/metrics.md) — every exported metric with types and
  labels, the default rules, self-monitoring.
- [HTTP API](docs/api.md) — health endpoints, topology API, on-demand
  diagnostics.
- [charts/kconmon-ng/values.yaml](charts/kconmon-ng/values.yaml) — every Helm
  value, documented inline.
- [docs/console-api.yaml](docs/console-api.yaml) — the Console's HTTP API as a
  hand-authored OpenAPI 3.1 spec; the TypeScript client types are generated
  from it and CI fails on drift.

## Development

```bash
make build      # agent + controller binaries → bin/
make test       # unit tests; make test-race adds the race detector
make lint       # golangci-lint; make helm-lint runs the chart against CI value sets
make local-up   # Minikube + Prometheus + Grafana + kconmon-ng, one command
```

CI runs lint, race tests, cross-compile and helm-lint on every PR; a `v*` tag
publishes images and the chart to GHCR and runs e2e. Start with
[CONTRIBUTING.md](CONTRIBUTING.md) and [hack/README.md](hack/README.md).

## License

Apache License 2.0. See [LICENSE](LICENSE).

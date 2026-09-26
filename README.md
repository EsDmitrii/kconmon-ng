# kconmon-ng

[![Release](https://github.com/EsDmitrii/kconmon-ng/actions/workflows/release.yaml/badge.svg)](https://github.com/EsDmitrii/kconmon-ng/actions/workflows/release.yaml)
[![Artifact Hub](https://img.shields.io/endpoint?url=https://artifacthub.io/badge/repository/kconmon-ng)](https://artifacthub.io/packages/search?repo=kconmon-ng)
[![Docs](https://img.shields.io/badge/docs-esdmitrii.github.io%2Fkconmon--ng-4051b5?logo=materialformkdocs&logoColor=white)](https://esdmitrii.github.io/kconmon-ng/)
[![Go](https://img.shields.io/badge/go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue)](LICENSE)

![The console matrix of a 10-node cluster: on TCP all 90 pairs are green; on PMTU the pairs between zone-b and zone-c, every path into worker2 except worker3's and three single pairs turn red as black holes at 1400 and 1280 bytes and worker3's row, its path into worker2 included, turns amber on a reduced 1400-byte path, 38 of 90 pairs in all, while TCP stays green; last, the worker6 to worker2 pair card shows 0.0% TCP failures next to a 1400 of 1500 byte black hole one way and full size the other](docs/img/pmtu-black-hole.gif)

**Someone says "the network is fine." Prove it.**

kconmon-ng makes inter-node connectivity a measured fact. An agent on every
node probes every other node over TCP, UDP and ICMP every five seconds,
resolves DNS and checks HTTP endpoints from each node, and exports latency,
jitter and packet loss for every ordered node pair, per protocol; a failed
probe fires an MTR trace, so the bad hop is on record before anyone looks.
Each protocol is measured separately for each pair, so the partial failures
that cost you the evening (UDP dropping on one pair while TCP stays clean)
read as exactly that instead of vanishing into a green aggregate. Everything
downstream is built on those measurements: an N×N matrix, a topology map, MTR
path history, incident timelines, alert rules that Prometheus evaluates, and
a `?at=` on the URL that rewinds every page to the minute it broke.

<p align="center">
  <img src="docs/img/console-overview.png" alt="Console Overview on a six-node kind cluster during a staged outage: 10 pairs failing (TCP) in the header, 6/6 nodes ready, 10 failing and 0 degraded pairs, the Worst pairs table with five worker4 pairs at 100.0%, the console rules NodeTcpUnreachable (critical) and PairPmtuBlackHole (warning) firing, and three open incidents: PMTU black hole to worker5, Zone b network degradation and worker4 TCP outage" width="100%"><br>
  <sub>Overview mid-incident on a six-node kind cluster: worker4 has lost TCP to and from every peer, so 10 of 30 pairs fail and the worst-pairs table ranks five of them at 100.0%. Two console rules fire, NodeTcpUnreachable for worker4 and PairPmtuBlackHole for a path MTU black hole from worker3 to worker5, and three incidents are open, one per problem plus a global one for zone b.</sub>
</p>

<table>
  <tr>
    <td width="50%"><img src="docs/img/console-matrix.png" alt="Console Matrix, TCP, live, zoomed to 150% during a staged break: the worker4 row and column red at 100.0%, 10 of 30 cells, the other 20 green at 0.0%"><br><sub>Matrix, TCP, live, mid-break, zoomed to 150% so the grid fills the page: worker4 is red at 100% both as a row and as a column, 10 of 30 cells. The 20 pairs among control-plane, worker, worker2, worker3 and worker5 stay green with a p95 RTT of 1.8 to 2.2 ms.</sub></td>
    <td width="50%"><img src="docs/img/console-incidents-timeline.png" alt="Console Incidents scoped to the pair worker3 → worker5 over one hour: the action row, a timeline of 338 entries led by tcp diagnostic timeouts and audit rows, and a Signals panel with the fail ratio going from 0.0% to 100.0% and a packet-loss chart flat at 0% until about 06:17 and at 100% after"><br><sub>Incidents, scoped to worker3 → worker5 over 1h: 338 timeline entries, the newest the pair's TCP diagnostic timeouts between audit rows, and the Signals panel showing the fail ratio going from 0.0% to 100.0%. Its packet-loss chart sits at 0% until about 06:17 and holds 100% after.</sub></td>
  </tr>
  <tr>
    <td width="50%"><img src="docs/img/console-alerting-rules.png" alt="Console Alerting: two managed rules, acc-agent-missing and acc-pair-udp-loss, both synced and enabled, the chart's own PrometheusRule kconmon-ng with 1 group of 13 rules offered for import, and one maintenance window below"><br><sub>Alerting: two console-managed rules, both synced against the cluster and enabled, the chart's own PrometheusRule offered for import as a foreign rule (1 group, 13 rules, Helm), and one declared maintenance window at the bottom.</sub></td>
    <td width="50%"><img src="docs/img/console-timemachine.png" alt="Console Time Machine: the Matrix resolved at 9/26/2026 08:37:30 instead of now, zoomed to 150%, all 30 cells green"><br><sub>Time Machine: the same Matrix resolved at 9/26/2026 08:37:30, about a minute and a half before that break. The amber banner marks the viewed instant; all 30 pairs are green, worker4 included, with a p95 RTT of 2.2 to 3.9 ms.</sub></td>
  </tr>
  <tr>
    <td width="50%"><img src="docs/img/install-15-min-grafana-overview.png" alt="Bundled Grafana Overview dashboard on a healthy stand, 13:44 to 13:59 UTC: key indicators on top in two rows, 6 agents registered, 6 reporting, 0 missing, LEADER OK, 30 monitored pairs, 0 pairs with failures, 0 black-hole pairs and 0 reduced-path pairs over 15 minutes; below them the worst-pair failure ratio bars for TCP, UDP, ICMP, DNS and UDP packet loss at 0.00% and the pairs-by-MTR-traces bars all at 0"><br><sub>Bundled Grafana Overview on a healthy stand: the key indicators sit on top (agents, leader, pairs, failures, and path MTU black holes and reduced paths), then the worst-pair and MTR bars, with the charts and tables below. Everything reads 0 failing across 30 pairs.</sub></td>
    <td width="50%"><img src="docs/img/zone-heatmap.png" alt="Bundled Grafana Zone Heatmap during a staged break, 06:11 to 06:26 UTC: from-to tables for UDP packet loss, p95 UDP and ICMP RTT, TCP and UDP failure ratios and MTR traces triggered over 1h, with zones a, b and c as rows and columns; zone c between 68.7% and 70.1% as a row and a column on loss and both failure ratios, a to a about 46%, a to b and b to a about 23%, b to b 0.0%, the c to c cells empty, and every RTT cell green"><br><sub>Bundled Zone Heatmap during a staged break, with worker2 in zone a and worker5, zone c's only node, cut off: zone-to-zone UDP loss, p95 UDP and ICMP RTT, and TCP and UDP failure ratios over the last 15 minutes, plus MTR traces triggered over the last hour. Zone c reads about 69% to 70% as a source and as a destination on loss and both failure ratios, zone a to itself about 46%, zones a and b to each other about 23%, zone b to itself 0.0%, and the RTT tables stay green because only answered probes have a round trip.</sub></td>
  </tr>
  <tr>
    <td colspan="2" align="center"><img src="docs/img/grafana-node-detail.png" alt="Bundled Grafana Node Detail dashboard for kc-accept-worker3, 05:40 to 05:55 UTC: 5 peers probed outbound, worst outbound and inbound failure ratio 0.00%, worst outbound and inbound UDP loss 0.00%, 0 MTR traces from this node, and outbound charts by peer for failure ratio, p95 UDP RTT, UDP packet loss and ICMP packet loss" width="50%"><br><sub>Bundled Node Detail dashboard for kc-accept-worker3: 5 peers probed outbound, every worst-case failure and UDP loss tile at 0.00%, no MTR traces from this node, and per-peer outbound charts with p95 UDP RTT flat at about 480 µs.</sub></td>
  </tr>
</table>

## Documentation

Everything lives at **<https://esdmitrii.github.io/kconmon-ng/>**; start with:

- [Install in 15 minutes](https://esdmitrii.github.io/kconmon-ng/getting-started/install-15-min/)
- [Enable the console](https://esdmitrii.github.io/kconmon-ng/getting-started/enable-the-console/)
- [Console guide](https://esdmitrii.github.io/kconmon-ng/console/overview/)
- [Helm values](https://esdmitrii.github.io/kconmon-ng/reference/helm-values/)
- [Metrics and alerting](https://esdmitrii.github.io/kconmon-ng/metrics/)
- [External agents](https://esdmitrii.github.io/kconmon-ng/external-agents/)
- [FAQ](https://esdmitrii.github.io/kconmon-ng/faq/)
- [Release notes](https://esdmitrii.github.io/kconmon-ng/reference/release-notes/)

## What it does

- **Measures every pair on every protocol.** One controller Deployment, one
  agent per node; agents register over gRPC and get a live-pushed peer list,
  and every metric carries `source_zone`/`destination_zone` read from the node
  labels. TCP, UDP, ICMP and DNS run on a 5s interval; HTTP is opt-in.
  [Architecture](https://esdmitrii.github.io/kconmon-ng/concepts/architecture/)
- **Traces the failure while it is happening.** A failed TCP, UDP or ICMP probe
  triggers MTR for that pair under a per-pair cooldown; paths are content-hashed
  at ingest, so path history is a list of changes, not identical traces.
  [Routes (MTR)](https://esdmitrii.github.io/kconmon-ng/console/routes-mtr/)
- **Finds MTU black holes.** A once-a-minute path MTU probe sends full-size
  datagrams with DF set and bisects on loss, so a pair where handshakes and
  pings pass but large packets vanish goes red with the size that still
  crosses, instead of staying green on every other plane.
  [Catch an MTU black hole](https://esdmitrii.github.io/kconmon-ng/scenarios/mtu-black-hole/)
- **Speaks Prometheus.** Stable metric names, three Grafana dashboards in
  [`dashboards/`](dashboards/), fourteen alert rules with the chart (UDP loss,
  failing TCP, an MTU black hole per pair and per zone pair, a node
  unreachable or isolated, DNS and external checks, a pair gone silent, two
  zone rules,
  `KconmonAgentsMissing` and `KconmonControllerDown`, so a monitor that goes
  quiet pages you, plus an opt-in `KconmonExternalAgentDown` for bare-host
  agents), each tunable via `prometheusRule.<alertName>.*`; the
  console's own families share the prefix, documented by their HELP strings.
  [Metrics and alerting](https://esdmitrii.github.io/kconmon-ng/metrics/)
- **Adds an optional Console, off by default.** N×N heatmap, topology map,
  curated charts; an Investigate page merging nine timeline sources, causes
  ranked by documented arithmetic; alert rules from typed templates or raw
  PromQL, reconciled into one `PrometheusRule`; diagnostics, external targets,
  schedules; four auth modes, 26 permissions across four built-in roles, an
  audit log, HMAC-signed webhooks. Read-only pages need only a Prometheus
  URL; the rest needs PostgreSQL.
  [Console guide](https://esdmitrii.github.io/kconmon-ng/console/overview/)
- **Rewinds.** `?at=` on any URL resolves every read surface through that
  instant, with mutations disabled while it is engaged.
  [Time Machine](https://esdmitrii.github.io/kconmon-ng/console/time-machine/)
- **Takes vantage points outside the cluster.** Since v2.3.0 the same agent
  installs on a bare host from deb or rpm and joins through a TLS gateway with
  a bearer token and optional client-certificate pinning.
  [External agents](https://esdmitrii.github.io/kconmon-ng/external-agents/)

## Quick start

Kubernetes 1.31+, Helm 4 (or Helm 3 from 3.14; the chart is an OCI artifact)
and a Prometheus to scrape. The Operator is optional; with it you get the
bundled `ServiceMonitor` and alert rules:

```bash
helm upgrade --install kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng \
  --set serviceMonitor.enabled=true \
  --set prometheusRule.enabled=true
```

```bash
# one controller pod plus one agent per node
kubectl get pods -l app.kubernetes.io/name=kconmon-ng -o wide
```

Metrics flow within seconds; then follow
[Install in 15 minutes](https://esdmitrii.github.io/kconmon-ng/getting-started/install-15-min/)
for the first queries, the dashboards, the scrape job without the operator and
the `make local-up` Minikube demo. The agent needs no added capabilities and
passes restricted PSS unchanged ([why](https://esdmitrii.github.io/kconmon-ng/faq/#what-privileges-do-the-agents-need)).

## No Prometheus Operator?

Moved to the site: [one endpoints-role scrape job](https://esdmitrii.github.io/kconmon-ng/getting-started/install-15-min/#no-prometheus-operator) covers the fleet, and the [alert rules](https://esdmitrii.github.io/kconmon-ng/metrics/#default-alerting-rules) are plain PromQL.

## Turn on the Console

Moved to the site: [Enable the console](https://esdmitrii.github.io/kconmon-ng/getting-started/enable-the-console/), one flag on the same release, then one capability at a time.

## From your terminal

`kubectl-kconmon` reaches the controller's HTTP API over a client-go
port-forward. A failed check exits `2`, distinct from `1` for CLI or API
errors, so it composes in pipelines; `-o json` prints the raw result.

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

Install it from the krew index with `kubectl krew install kconmon`, or pin a
release with `kubectl krew install --manifest-url https://github.com/EsDmitrii/kconmon-ng/releases/latest/download/kconmon.yaml`.

## Scope and limits

- **External agents are supported since v2.3.0**: TLS gateway on the
  controller, bearer token, optional client-certificate pinning, deb and rpm
  packages. 2.4.0 adds per-agent ports (an external host no longer mirrors the
  fleet's port pair), a Prometheus HTTP SD endpoint so external hosts are
  scraped without hand-written targets, `agent.hostNetwork` for probing from
  the node's own address, and console awareness of external members.
- **What stays out**: Windows hosts
  ([why](https://esdmitrii.github.io/kconmon-ng/faq/#does-the-agent-run-on-windows)),
  two agents on one IP, a CSR flow for agent certificates; the full list is
  [What v1 does not do](https://esdmitrii.github.io/kconmon-ng/external-agents/#what-v1-does-not-do).
- **Scale envelope**: 50–100 nodes on a full mesh is production-proven; each
  directed pair keeps 78 active series and pairs grow as N×(N−1), about 770k
  series at 100 nodes. `topology.mode: sparse` (since v2.3.0) trims probed
  pairs to a ring plus cross-zone chords. Arithmetic and levers:
  [Scaling and cardinality](https://esdmitrii.github.io/kconmon-ng/metrics/#scaling-and-cardinality).

## How it compares

kconmon-ng inherits its name and shape from
[kconmon](https://github.com/Stono/kconmon) by Karl Stoney (Node.js, archived
in June 2026) and is a ground-up Go implementation of the same idea, not a
fork, extended with ICMP, reactive MTR tracing and the Console. Reach for
goldpinger when an HTTP-level pod graph is enough, for kubenurse when the
question is the path through ingress and API server, and for kconmon-ng when
you need per-protocol pair evidence with the bad hop already traced; the
feature table is in the
[FAQ](https://esdmitrii.github.io/kconmon-ng/faq/#how-does-this-relate-to-the-original-kconmon).

## Links

- Documentation: <https://esdmitrii.github.io/kconmon-ng/>
- Chart: [Artifact Hub](https://artifacthub.io/packages/helm/kconmon-ng/kconmon-ng),
  `oci://ghcr.io/esdmitrii/charts/kconmon-ng`; images
  `ghcr.io/esdmitrii/kconmon-ng-{agent,controller,console}` on
  [GHCR](https://github.com/EsDmitrii?tab=packages&repo_name=kconmon-ng)
- [Releases](https://github.com/EsDmitrii/kconmon-ng/releases) (deb/rpm host
  packages, krew manifest), [`dashboards/`](dashboards/),
  [CONTRIBUTING.md](CONTRIBUTING.md), [SECURITY.md](SECURITY.md)

## Development

```bash
make build      # agent, controller and console binaries → bin/
make test       # unit tests; make test-race adds the race detector
make lint       # golangci-lint; make helm-lint runs the chart against CI value sets
make local-up   # Minikube + Prometheus + Grafana + kconmon-ng, one command
```

CI runs lint (the e2e suite included), race tests, cross-compile, helm-lint
and a build of every image on every PR; a `v*` tag builds and signs the
images, runs e2e on them, and only then publishes the chart, the release and,
on the newest stable tag, `:latest`. Start with
[CONTRIBUTING.md](CONTRIBUTING.md) and [hack/README.md](hack/README.md).

## Community

[Governance](GOVERNANCE.md), [maintainers](MAINTAINERS.md), [roadmap](ROADMAP.md),
[code of conduct](CODE_OF_CONDUCT.md) and [adopters](ADOPTERS.md). Running
kconmon-ng somewhere? A pull request adding a line to ADOPTERS.md helps more
than a star.

## License

Apache License 2.0. See [LICENSE](LICENSE).

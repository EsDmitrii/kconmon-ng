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
- The agent Pods request NO capabilities. ICMP and MTR run on the unprivileged
  ICMP socket; `NET_RAW` used to be requested and never reached the effective set
  of a non-root container with `allowPrivilegeEscalation: false`, so it bought
  nothing and cost the `restricted` PSS profile
- The ICMP checker opens an unprivileged ICMP "ping" socket, which the kernel
  gates on `net.ipv4.ping_group_range`. Some container runtimes leave this at the
  closed default (`1 0`), so the chart sets the (safe) sysctl via
  `agent.podSecurityContext`. Set `agent.pingGroupRange: false` to drop just that
  sysctl — `agent.podSecurityContext: null` also works but takes
  `runAsNonRoot`, `runAsUser: 65532` and `seccompProfile: RuntimeDefault` with
  it, which is exactly the set a namespace enforcing restricted PSS requires, so
  the DaemonSet is then rejected at admission.

## Installing

The chart is published as an OCI artifact on GHCR.

```bash
helm install kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng
```

With the Prometheus Operator objects, which is what most installs want:

```bash
helm upgrade --install kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng \
  --set serviceMonitor.enabled=true \
  --set prometheusRule.enabled=true
```

With custom values:

```bash
helm install kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng \
  -f values.yaml
```

### Multi-node kind/minikube needs a real local provisioner

The default `standard` StorageClass on minikube (`minikube-hostpath`) binds
immediately and creates the backing directory only on the control plane, with no
node affinity — a database Pod scheduled on a worker then dies in `initdb` with
`Permission denied`. Install a `WaitForFirstConsumer` provisioner (`minikube
addons enable storage-provisioner-rancher`, or local-path on kind) and point
your database chart's `storageClass` at it.

### Upgrading

```bash
helm upgrade kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng \
  -f values.yaml
```

### Uninstalling

```bash
helm uninstall kconmon-ng
```

## Values

`values.yaml` is laid out in nine sections, and the first thing worth knowing is
what is NOT under `console:` — the stack it runs on is configured on its own:

| Block | What it is |
| --- | --- |
| `database:` | PostgreSQL: the Secret holding its `postgres://` DSN, plus pool/retention settings |
| `redis:` | The session/rate-limit/pub-sub server: the Secret holding its `redis://` DSN |
| `console:` | The Console workload itself — auth, features, ingress, resources |
| `agent:` / `controller:` | The two always-on workloads |

So "bring your own PostgreSQL" is `database.existingSecret`, not a Console
setting, and any Redis-compatible server is `redis.existingSecret` — the Console
never had an opinion about either, and the chart installs neither.

The table below lists the most relevant parameters. See
[`values.yaml`](values.yaml) for the complete set.

| Key | Default | Description |
| --- | --- | --- |
| `controller.replicaCount` | `1` | Number of controller replicas |
| `controller.leaderElection` | `true` | Enable leader election between controller replicas |
| `controller.resources` | requests `50m`/`64Mi`, limits `200m`/`128Mi` | Controller resource requests/limits |
| `controller.externalGateway.enabled` | `false` | TLS + bootstrap-token gRPC gateway for agents OUTSIDE the cluster, with its own NodePort/LoadBalancer Service exposing the gateway port ALONE (the plaintext in-cluster gRPC port authenticates by network position and never leaves the cluster). Requires `tls.secretName` and `bootstrapToken.secretName`; with `networkPolicy.enabled` also `networkPolicy.externalAgentCidrs`. Rotating the referenced Secrets needs a controller restart |
| `controller.externalGateway.tls.clientCaKey` | `""` | Key in the TLS Secret holding the CA that signs agent CLIENT certs; setting it pins each cert's CN/URI SAN to the agent's node name. Empty is token-only mode: any token holder can impersonate any agent |
| `agent.tolerations` | `[{operator: Exists}]` | Agent DaemonSet tolerations (default: run on all nodes) |
| `agent.resources` | requests `50m`/`64Mi`, limits `200m`/`128Mi` | Agent resource requests/limits |
| `agent.securityContext` | `{allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: [ALL]}}` | Agent container securityContext; add `NET_RAW` yourself only if you also make it effective |
| `agent.podSecurityContext` | `{runAsNonRoot: true, runAsUser: 65532, seccompProfile: {type: RuntimeDefault}, sysctls: [{name: net.ipv4.ping_group_range, value: "0 2147483647"}]}` | Agent Pod securityContext. `null` deletes the WHOLE sub-tree, restricted-PSS keys included; to drop only the sysctl use `agent.pingGroupRange: false` |
| `agent.pingGroupRange` | `true` | Render the `net.ipv4.ping_group_range` sysctl. Set `false` on a runtime that already opens it, or where the sysctl is not allowed, without losing the restricted-PSS keys |
| `agent.metrics.detail` | `full` | Scrape-time cardinality valve on the agent ServiceMonitor: `full \| counters-only \| zone-only` (~70 / ~10 / ~0 series per directed pair). Needs `serviceMonitor.enabled`; `zone-only` needs agents that export the zone metric family. Series math in `docs/metrics.md`, "Scaling and cardinality" |
| `topology.mode` | `full` | Probe topology plan: `full` probes every peer from every agent; `sparse` trims it to a ring over sorted node names (`topology.sparse.ringDegree`) plus cross-zone chords (`topology.sparse.zoneChords`), with `topology.sparse.autoThreshold` as a fleet-size floor below which the mesh stays full. Needs controller and agent images newer than appVersion 2.2.0 |
| `config.metricsPrefix` | `kconmon_ng` | Prefix for all exported Prometheus metrics |
| `config.checkers.tcp.enabled` | `true` | Enable TCP checker (interval `5s`, timeout `1s`) |
| `config.checkers.udp.enabled` | `true` | Enable UDP checker (interval `5s`, timeout `250ms`, `packets: 5`) |
| `config.checkers.icmp.enabled` | `true` | Enable ICMP checker (interval `5s`, timeout `1s`) |
| `config.checkers.dns.enabled` | `true` | Enable DNS checker (interval `5s`, timeout `5s`) |
| `config.checkers.http.enabled` | `false` | Enable HTTP checker (interval `30s`, timeout `5s`) |
| `serviceMonitor.enabled` | `false` | Create a Prometheus Operator `ServiceMonitor` |
| `prometheusRule.enabled` | `false` | Create a Prometheus Operator `PrometheusRule` with the nine built-in alerts ([Alerting rules](#alerting-rules)) |
| `prometheusRule.<alertName>` | all enabled | Per-rule `enabled` / `threshold` / `for` / `severity` |
| `prometheusRule.additionalRules` | `[]` | Extra rules appended to the group verbatim |
| `networkPolicy.enabled` | `false` | Create a `NetworkPolicy` (set `networkPolicy.prometheusNamespace` to allow scraping) |
| `controller.pdb.enabled` | `true` | PodDisruptionBudget for the controller — rendered ONLY at `controller.replicaCount > 1` |

## Console (optional)

An optional web UI Deployment, off by default (`console.enabled: false`).
Read-only pages (topology, matrix, PromQL) work with no extra setup; setting
`database.existingSecret` (any PostgreSQL, by DSN)
and `console.auth.mode` (`anonymous | local | header | oidc`) adds durable
event/run history, authentication, RBAC and an on-demand diagnostics runner.
Every knob is documented inline in this chart's `values.yaml`, and the HTTP
API is specified in
[`docs/console-api.yaml`](https://github.com/EsDmitrii/kconmon-ng/blob/main/docs/console-api.yaml).

| Key | Default | Description |
| --- | --- | --- |
| `console.enabled` | `false` | Deploy the Console |
| `console.replicas` | `1` | Console replica count. More than 1 REQUIRES `redis.existingSecret`: sessions, the rate-limit counters and the realtime fan-out live there, and the chart refuses the combination rather than silently multiplying every rate limit by the replica count |
| `console.auth.mode` | `anonymous` | `anonymous \| local \| header \| oidc` |
| `console.auth.groupRoles` | `{}` | Group the identity provider asserts → role this console grants. The union with API-made bindings; a group absent from the map grants nothing. What makes an oidc/header install usable from a cold database |
| `console.auth.session.ttl` | `12h` | Absolute session lifetime, counted from login and never extended |
| `console.auth.session.idleTimeout` | `1h` | Session is refused and purged after this much inactivity; slides forward on every request, never past `ttl`. `0` disables it |
| `database.existingSecret` | `""` | Secret holding a `postgres://` DSN; empty means an in-memory console |
| `redis.existingSecret` | `""` | Secret holding a `redis://` DSN; empty means the in-process bus (`console.replicas: 1`) |
| `console.kubernetesContext.enabled` | `false` | Capture core/v1 Events into the Investigate timeline; renders a console-only ServiceAccount and a `ClusterRole` for events |
| `console.alerting.enabled` | `false` | Manage Prometheus alert rules from the Console; renders a **namespaced** `Role` for `monitoring.coreos.com/prometheusrules` and applies one `PrometheusRule` object (`console.alerting.bundleName`). Needs a database and the Prometheus Operator CRD |
| `console.webhooks.existingSecret` | `""` | Secret holding the AES-256-GCM key that encrypts webhook signing secrets at rest; empty leaves webhook create/test answering 503 |
| `<consumer>.secret.create` | `false` | Let the chart render the Secret instead of referencing one ([Chart-managed Secrets](#chart-managed-secrets)) |

## Metrics

All metrics are prefixed with `config.metricsPrefix` (default `kconmon_ng`).
Selected key metrics:

- `kconmon_ng_tcp_results_total` — total TCP probe results (labelled by `result`)
- `kconmon_ng_udp_packet_loss_ratio` — UDP packet loss ratio (0.0–1.0)
- `kconmon_ng_icmp_packet_loss_ratio` — ICMP packet loss ratio (0.0–1.0)
- `kconmon_ng_zone_{udp,icmp}_packets_{sent,received}_total` — the zone plane's loss counters; the whole `kconmon_ng_zone_*` family is in `docs/metrics.md`
- `kconmon_ng_dns_results_total` — total DNS resolution results (labelled by `result`)
- `kconmon_ng_controller_registered_agents` — agents currently registered with the controller
- `kconmon_ng_controller_expected_agents` — schedulable nodes expected to run an agent

## Alerting rules

`prometheusRule.enabled=true` renders one `PrometheusRule` with nine built-in
alerts. The rules themselves live in the chart
([`templates/_rules.tpl`](templates/_rules.tpl)), not in `values.yaml`: rule
text, rate windows and label groupings are chart code, and `values.yaml` carries
only what an operator actually tunes.

| Rule | Fires when | Values key | Tunables |
| --- | --- | --- | --- |
| `UDPLossHigh` | `<prefix>_udp_packet_loss_ratio > 0.5` for 5m | `prometheusRule.udpLossHigh` | `threshold` `0.5`, `for` `5m`, `severity` `warning` |
| `TCPChecksFailing` | TCP **failure ratio** > 5% for 5m | `prometheusRule.tcpChecksFailing` | `threshold` `0.05`, `for` `5m`, `severity` `warning` |
| `PairWentSilent` | a pair probed within the last hour reports **nothing** for ~15m | `prometheusRule.pairWentSilent` | `for` `10m`, `severity` `warning` |
| `DNSChecksFailing` | DNS **failure ratio** > 5% for 5m | `prometheusRule.dnsChecksFailing` | `threshold` `0.05`, `for` `5m`, `severity` `warning` |
| `ExternalChecksFailing` | External **failure ratio** > 10% for 5m | `prometheusRule.externalChecksFailing` | `threshold` `0.1`, `for` `5m`, `severity` `warning` |
| `ZoneChecksFailing` | zone-pair **failure ratio** across TCP+UDP+ICMP > 5% for 5m | `prometheusRule.zoneChecksFailing` | `threshold` `0.05`, `for` `5m`, `severity` `warning` |
| `ZoneLossHigh` | zone-pair packet loss (from sent/received counters) > 10% for 5m | `prometheusRule.zoneLossHigh` | `threshold` `0.1`, `for` `5m`, `severity` `warning` |
| `KconmonAgentsMissing` | `expected_agents - registered_agents > 0` for 10m | `prometheusRule.kconmonAgentsMissing` | `for` `10m`, `severity` `warning` |
| `KconmonControllerDown` | `absent(<prefix>_controller_leader == 1)` for 5m | `prometheusRule.kconmonControllerDown` | `for` `5m`, `severity` `critical` |

The two `Zone*` rules read the zone-level metric family
(`<prefix>_zone_*`), which only agents new enough to export it serve — on an
older fleet they are silently inert (their expressions match no series) and
start working when the agent image is upgraded. They aggregate at the source,
so they keep firing under every `agent.metrics.detail` scrape mode, including
`zone-only`.

Every rule takes `enabled` (all `true` by default) alongside the tunables above.
Setting one to `false` removes exactly that rule and nothing else. A `threshold`
is a ratio in `0.0-1.0` and is interpolated into the alert's own annotation text,
so a retuned rule still describes itself correctly.

A `threshold` may be written as a number (`0.25`) or as a string (`"0.25"`) and
renders identically either way — `helm --set` types only integers, booleans and
null, so `--set prometheusRule.udpLossHigh.threshold=0.25` arrives as a string
and must keep working:

```bash
helm upgrade --install kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng \
  --set prometheusRule.enabled=true \
  --set prometheusRule.udpLossHigh.threshold=0.25
```

```yaml
prometheusRule:
  enabled: true
  udpLossHigh:
    threshold: 0.25      # page earlier on UDP loss
    severity: critical
  externalChecksFailing:
    enabled: false       # not running external checks
  additionalRules:
    - alert: MyOwnRule
      expr: up{job="kconmon-ng"} == 0
      for: 1m
      labels: {severity: page}
      annotations: {summary: custom}
```

`prometheusRule.additionalRules` is appended to the group verbatim — nothing in
it is rewritten, so write metric names with whatever `config.metricsPrefix` you
actually run.

### Annotations name the pair

Every rule annotates with the labels its own series carry, so a notification
names the failing pair (or zone pair), direction and measured value instead of
repeating one generic sentence per firing series:

| Rule | Annotation identifies |
| --- | --- |
| `UDPLossHigh` | source → destination node, both zones, loss % |
| `TCPChecksFailing` | source → destination node, both zones, failed % |
| `PairWentSilent` | source → destination node |
| `DNSChecksFailing` | source node + zone, queried `host`, `resolver`, failed % |
| `ExternalChecksFailing` | source node + zone, `target`, `target_kind`, failed % |
| `ZoneChecksFailing` | source → destination zone, failed %; `investigateUrl` deep link |
| `ZoneLossHigh` | source → destination zone, loss %; `investigateUrl` deep link |
| `KconmonAgentsMissing` | controller `instance` and how many agents are missing |
| `KconmonControllerDown` | nothing to identify; `absent()` has no series labels |

The series already carry the peer label set (`source_node`,
`destination_node`, `source_zone`, `destination_zone`). An annotation that omits
them turns N distinct broken links into N byte-identical notifications, and the
operator cannot tell which link, which direction, or how bad. `$labels` costs
nothing and answers all three. DNS and external checks are not pair-scoped —
their series carry `host`/`resolver` and `target`/`target_kind` instead of a
destination — which is why those two annotations name a resolver or a target
rather than a peer.

The two zone rules also annotate `investigateUrl`, a **console-relative** deep
link (`/investigate?kind=zone-pair&scope=<source>-><destination>`) into the
Investigate page scoped to the firing zone pair. Relative because the chart
cannot know the console's external URL — ingress is optional — so a
notification template prepends its own console origin; the console normalises
the typeable `->` into its canonical pair arrow.

### Why a ratio, not a rate

The three `*ChecksFailing` rules compare a **failure ratio** —
`rate(fail) / rate(all results)` for the same pair — rather than
`rate(fail) > 0`. A raw rate stays positive for the whole window after one flaky
probe, so it reported "a probe failed recently" instead of "this link is
unhealthy". The ratio keeps a single failure inside a healthy stream below the
threshold while a genuinely broken link crosses it immediately. Thresholds are
5% in-cluster (TCP, DNS) and 10% for external targets, which cross networks the
cluster operator does not run and where a stricter bound would page on somebody
else's packet loss.

The denominator deliberately selects the metric family with **no** `result`
selector, so a third `result` value added later widens the denominator instead
of silently skewing the ratio.

A pair that has never failed has no `{result="fail"}` series, so the division
produces no sample for it and the rule stays silent rather than materialising a
zero for every pair in the mesh.

### Why `PairWentSilent` uses `unless`, not `== 0`

That same property is the blind spot. A pair that stops reporting altogether is
`0/0` — no numerator, no denominator, no sample — so every ratio rule goes quiet
on the one failure that matters most, and quiet reads as healthy.
`PairWentSilent` compares two windows instead of two counters: pairs being
probed in the hour before last (`[1h] offset 5m`) `unless` pairs still being
probed now (`[5m]`).

`unless` rather than `rate(...[5m]) == 0` because a scrape target that
disappears takes its series with it, and a rate window holding fewer than two
samples returns *no series* rather than a zero — the `== 0` form catches only a
counter that froze while the agent kept reporting, and misses the common case of
the agent itself being gone. `A unless B` is a difference of label sets, so it
covers both at once.

The grouping is deliberately just `(source_node, destination_node)` and not the
four peer labels the other rules group by: `unless` matches on the whole label
set, so a node that changes zone between the two windows would read as one pair
disappearing and a different one appearing, and the rule would fire on a relabel
instead of on an outage.

**The ~15m timing, and why a rollout does not page anyone:**

- `rate` over `[5m]` absorbs an agent restart by itself. A pod away for well
  under five minutes still leaves two samples in the window, so the right-hand
  side keeps the pair and the difference stays empty.
- `for: 10m` means the silence has to outlast a DaemonSet rollout, a drain or a
  reschedule; anything that completes inside ~15m total is never notified. The
  annotation's "15m" is that sum — retuning `for` moves the real threshold while
  the sentence keeps saying 15m.
- `offset 5m` keeps the "was reporting" window clear of the same five minutes
  the left side is judging, so a pair can never prove its own liveness with the
  very samples that are missing.
- The 1h lookback is also the alert's lifetime: once the silence is an hour old
  the offset window empties, the pair leaves the left-hand side and the alert
  resolves. The rule reports a *transition* — a link that WAS probed and is not
  any more — while a node gone for good belongs to `KconmonAgentsMissing`. A
  cluster scaled down on purpose gets one bounded warning, not a permanent one.
- `severity: warning`, not critical, for the same reason: the expression cannot
  tell a broken agent from a node that left the cluster, and only one of those
  deserves a page.

Both halves read `<prefix>_tcp_results_total`, the probe every default install
runs against every peer, and an agent reports all of its checkers or none of
them. If you disable the TCP checker, repoint both halves at the `udp` or `icmp`
results family — the shape is identical.

**The `probe_intended` join (2.3.0):** under `topology.mode: sparse` most pairs
are deliberately never probed, and a pair the plan *drops* would satisfy the
two-window comparison for the whole hour its results take to age out. So the
rule fires only for pairs present in `<prefix>_probe_intended` — the plan the
source agent exports, 1 per assigned pair, pruned on every plan change. The
fallback joins per source rather than globally: a `source_node` with no
`probe_intended` series at all gets the plain two-window comparison, which
covers a pre-2.3.0 agent image (fires exactly as before, mixed fleets included)
*and* an agent that died outright — a dead agent's `probe_intended` goes stale
with it, so its pairs land in the fallback and the alert keeps catching the
case it was written for.

### Self-monitoring

The last two rules watch kconmon-ng itself, so a monitor that goes quiet pages
you instead of looking healthy. Both need `controller.leaderElection=true`: the
node informer and the leader metric only run on the leader.
`KconmonAgentsMissing` is written as a subtraction rather than
`registered < expected` so that `$value` is the *number of missing agents*; with
`<` the alert value would be the registered count, the one number an operator
can already see everywhere.

### Metric prefix

Built-in rule expressions print `config.metricsPrefix` directly, so a custom
prefix renders correctly with no rewriting. Annotations deliberately do not name
metrics, since prose is not rewritten either. The Grafana dashboards in
`dashboards/` are rewritten AT RENDER TIME: `dashboards.enabled` substitutes
`kconmon_ng_` for `<config.metricsPrefix>_` in every panel, so the shipped JSON
must keep the literal `kconmon_ng_` prefix and must NOT be hand-edited — this
paragraph used to say the opposite and sent operators off to edit that JSON by
hand, and a fork of it would then trip `make dashboards-check`.

### `prometheusRule.rules` is removed

The pre-1.12 shape — a full list of rule objects — **replaced every built-in
rule** and silently discarded the per-rule knobs beside it. 2.0.0 removes it, and
a values file that still sets it fails the render with this paragraph's advice.

To migrate: drop the entries that are just the built-ins, move genuinely custom
rules to `additionalRules` (rewriting their metric names to your real prefix),
and express threshold/severity changes through the per-rule knobs.

### Two different `PrometheusRule` objects

`prometheusRule.enabled` and `console.alerting.enabled` are two different things
and neither implies the other. The former renders the static object described
here, from the chart, edited in Git. The latter lets operators build rules in
the Console UI, stores them in PostgreSQL and reconciles them into a *separate*,
console-owned `PrometheusRule` object. Run both, either, or neither.

## The stack around it: bring your own

The chart installs the **monitor** — agent, controller, console — and nothing
else. PostgreSQL, a Redis-compatible bus and Prometheus are infrastructure this
chart *consumes*: a cluster has one of each, shared by everything in it, and a
product chart is the wrong owner for them. There are no subcharts and no
`helm dependency` step.

Each is one connection string:

| What | How you configure it | Empty means |
| --- | --- | --- |
| PostgreSQL | `database.existingSecret` → a Secret holding a `postgres://` DSN | in-memory console: no history, no incidents, no local/oidc auth |
| Redis-compatible bus | `redis.existingSecret` → a Secret holding a `redis://` DSN | in-process bus, and `console.replicas` must be 1 |
| Prometheus | `console.prometheus.url` | the matrix, Explore and PromQL pages answer 503 |

So any provider works: CloudNativePG, Percona, Zalando, RDS, Cloud SQL, a plain
StatefulSet; Valkey, Redis, a sentinel-fronted pair, ElastiCache. The DSN carries
the host, the credentials, the database number and TLS (`rediss://`,
`postgres://…?sslmode=verify-full`), so the chart never has to model any of it.

### The stack this chart is tested against

Nothing here is required — it is what CI and the maintainer's own cluster run,
written down so "it works on our stack" is a checkable claim rather than a
promise:

**PostgreSQL — [CloudNativePG](https://cloudnative-pg.io/).** Install the
operator once per cluster, then a `Cluster` per application:

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: kconmon-db
spec:
  instances: 2
  storage:
    size: 10Gi
```

CNPG publishes the DSN itself, in a Secret named `<cluster>-app` under the key
`uri` — point the chart straight at it:

```yaml
database:
  existingSecret: kconmon-db-app
  existingSecretKey: uri
```

**Bus — [valkey-helm](https://github.com/valkey-io/valkey-helm)** (the official
Valkey chart). A single instance is plenty; nothing on this bus is durable, so
persistence can stay off:

```yaml
# values for the valkey chart, installed as its own release
fullnameOverride: kconmon-valkey
replica:
  enabled: false
auth:
  enabled: true
  usersExistingSecret: kconmon-redis-credentials
  aclUsers:
    default:
      permissions: "~* &* +@all"
      passwordKey: password
metrics:
  enabled: true
  serviceMonitor:
    enabled: true
```

Then give the console a DSN pointing at it:

```bash
kubectl create secret generic kconmon-redis-dsn \
  --from-literal=console-redis-dsn="redis://default:$PASSWORD@kconmon-valkey:6379"
```

```yaml
redis:
  existingSecret: kconmon-redis-dsn
```

**Prometheus — [kube-prometheus-stack](https://github.com/prometheus-community/helm-charts).**
`serviceMonitor.enabled` and `prometheusRule.enabled` render objects for its
operator; `console.prometheus.url` points the data pages at the server.

## Batteries: what the chart sets up for you

| Thing you need | How you get it | Manual work left |
| --- | --- | --- |
| Grafana dashboards | `dashboards.enabled=true` renders them as ConfigMaps with the `grafana_dashboard` sidecar label | none, if you run the Grafana sidecar |
| GeoLite2 databases | `console.mtr.enrichment.geoip.mode=auto` runs the official `maxmind/geoipupdate` sidecar | supply MaxMind credentials once |
| PostgreSQL | yours, by DSN (`database.existingSecret`) | run one — CNPG, Percona, RDS… |
| Redis-compatible bus | yours, by DSN (`redis.existingSecret`) | run one — Valkey, Redis, ElastiCache… |
| Secrets | `existingSecret`, or `secret.create` with an injector | none |
| Pod hardening | restricted-PSS defaults for every component, agent included (runAsNonRoot, drop ALL, seccomp RuntimeDefault, read-only root) | none — `net.ipv4.ping_group_range` is a kubelet safe sysctl. If you re-add `NET_RAW` to `agent.securityContext`, the agent needs a `privileged` namespace or a PSS exemption — `baseline` refuses an added capability too, and the DaemonSet is then rejected at admission with nothing in the release marked unhealthy |
| Prometheus / Grafana themselves | **not installed** — external infrastructure | you run them |

### GeoLite2 databases keep themselves current

Hop enrichment needs MaxMind's GeoLite2 ASN and City databases. With
`mode: auto` — which you must set; enrichment ships with `mode: ""` — the chart runs MaxMind's own
`geoipupdate` image as a sidecar on the Console Pod. It downloads both editions
into an `emptyDir` mounted at `/geoip` and re-downloads every
`updateIntervalHours`. No files to stage, no versions to carry by hand.

```yaml
console:
  mtr:
    enrichment:
      enabled: true
      geoip:
        mode: auto
        updateIntervalHours: 24
        reloadInterval: 1h
        secret:
          create: true
          accountId: ${vault:secret/data/example/maxmind#accountId}
          licenseKey: ${vault:secret/data/example/maxmind#licenseKey}
```

Credentials come from a free GeoLite2 account and use the same Secret pattern as
everything else: `existingSecret` (keys `console-maxmind-account-id` /
`console-maxmind-license-key`, both overridable) or the managed `secret:` block
with `accountId` / `licenseKey`, placeholders included.

**A refreshed database is picked up without a restart.** The console's
enrichment reader re-stats the two files every `reloadInterval` and reopens
whichever one changed, swapping it in under a lock so no in-flight lookup ever
reads a closed mmap. A half-written download simply fails to open and is retried
on the next tick, and the previous database keeps serving until then. Set
`reloadInterval: 0` to switch that off, in which case pickup is restart-based.

For an **airgapped** cluster, `mode: volume` keeps the previous behaviour: you
supply an opaque VolumeSource in `geoip.volume` and it is mounted read-only at
`/geoip`. `mode: disabled` turns the geoip sources off entirely.

Paths default to `/geoip/<edition>.mmdb` from `geoip.editions`; set `asnPath` /
`cityPath` only if your files are named differently.

## UX debt: every manual step, fixed or justified

| Point | Verdict |
| --- | --- |
| Grafana dashboards were repo JSON nothing installed — while alert annotations told operators to open them | **fixed** — `dashboards.enabled` |
| GeoLite2 files had to be staged by hand, forever | **fixed** — `geoipupdate` sidecar + hot reload |
| No `NOTES.txt`: nothing told a fresh operator what was still unconfigured | **fixed** — post-install checklist naming only what *this* release left undecided |
| CNPG operator assumed pre-installed | **by design** — the chart installs no infrastructure; the README documents the stack it is tested against |
| Valkey had no managed option beyond a hand-rolled Deployment | **by design** — bring any Redis-compatible server by DSN; the README documents the tested one |
| Secrets all had to pre-exist | **fixed** — `secret.create` with semantic fields |
| Secret key defaults collided in a shared Secret | **fixed** — component-scoped key names |
| `console.prometheus.url` empty ⇒ silent 503 pages | **partly fixed** — `NOTES.txt` calls it out by name at install time. Not auto-detected: a cluster can hold several Prometheis and guessing wrong points the Console at someone else's data |
| Webhook encryption key must be supplied | **justified manual** — auto-generating with `randAlphaNum` + `lookup` looks convenient and is a data-loss trap: `lookup` returns empty during `helm template`, `--dry-run` and any upgrade run without cluster access, so the key silently rotates and every stored webhook secret becomes undecryptable. Keyless is already a supported, documented state |
| Prometheus Operator / Grafana not installed | **justified manual** — a cluster has one monitoring stack, shared by everything; a product chart is the wrong owner |
| CNPG operator + database in one release | **not this chart's problem any more** — install your database however you install infrastructure, then hand the chart its DSN |
| `dashboards/` duplicated into the chart (Helm cannot read outside the chart dir) | **guarded** — `make dashboards-check` and a CI step fail on drift |

## Chart-managed Secrets

Every sensitive value the chart consumes normally lives in a Secret **you**
create and name through `existingSecret`. Each of those consumers also accepts a
sibling `secret:` block that makes the chart render the Secret itself — which is
what lets a secrets injector (the Vault mutating webhook, External Secrets,
SOPS) rewrite placeholder values at admission time.

```yaml
console:
  auth:
    oidc:
      existingSecretKey: console-oidc-client-secret   # the key read AND written
      secret:
        create: true
        name: ""                                      # empty = <release>-console-oidc
        annotations:
          vault.security.banzaicloud.io/vault-addr: "https://vault.example.com:8200"
          vault.security.banzaicloud.io/vault-role: "kconmon-ng-console"
          vault.security.banzaicloud.io/vault-path: "kubernetes"
        labels: {}
        clientSecret: ${vault:secret/data/example/oidc#clientSecret}
```

The block asks for the credential **by name** — `clientSecret`, `dsn`,
`password`, `encryptionKey` — rather than a free-form `stringData` map. That
follows how mature charts shape the same choice: `bitnami/postgresql` exposes
`auth.password` + `auth.existingSecret` + `auth.secretKeys.*`, `bitnami/valkey`
exposes `auth.password` + `auth.existingSecret` +
`auth.existingSecretPasswordKey`, and `grafana` exposes `adminPassword` +
`admin.existingSecret` + `admin.passwordKey`. None of them accept an arbitrary
map, and neither does this chart: a chart that writes keys it never reads cannot
validate anything, and anything the chart does not consume belongs in a Secret
of your own that you point `existingSecret` at.

Field values are rendered verbatim: Helm's delimiters are `{{ }}`, so a
`${vault:...}` placeholder passes through byte-for-byte and reaches the injector
intact. Annotations and labels land on the generated Secret, which is how the
injector is addressed. See
[`ci/console-managed-secrets-values.yaml`](ci/console-managed-secrets-values.yaml)
for a full profile.

### The consumers, their key, and their field

| Consumer | Existing Secret | Key (`existingSecretKey` default) | Create block field |
| --- | --- | --- | --- |
| PostgreSQL DSN | `database.existingSecret` | `console-database-dsn` | `database.secret.dsn` |
| Redis-compatible DSN | `redis.existingSecret` | `console-redis-dsn` | `redis.secret.dsn` |
| Local bootstrap admin | `console.auth.local.existingSecret` | `console-local-admin-password` | `console.auth.local.secret.password` |
| OIDC client secret | `console.auth.oidc.existingSecret` | `console-oidc-client-secret` | `console.auth.oidc.secret.clientSecret` |
| Webhook encryption key | `console.webhooks.existingSecret` | `console-webhooks-encryption-key` | `console.webhooks.secret.encryptionKey` |
| MaxMind credentials | `console.mtr.enrichment.geoip.existingSecret` | `console-maxmind-account-id` / `console-maxmind-license-key` (`…geoip.accountIdKey` / `…licenseKeyKey`) | `console.mtr.enrichment.geoip.secret.{accountId,licenseKey}` |

The key defaults are **component-scoped on purpose**, so one shared Secret can
carry the whole stack's credentials without two components fighting over a
generic `password` or `dsn`. Override `existingSecretKey` to anything you like;
whatever you set is both the key the console reads and the key the create path
writes.

A database that publishes its own DSN Secret needs no create path at all: point
`existingSecret`/`existingSecretKey` at it (CNPG's is `<cluster>-app`, key
`uri`).

### Rules

- **`existingSecret` XOR `secret.create`.** Setting both fails the render with a
  message naming the values path. Setting neither, where the consumer requires
  one, keeps the exact error it raised before.
- **A required field left empty with `create: true` fails the render** naming
  the field. The chart writes exactly the keys it reads, and nothing else.
- **`secret.name` overrides the generated name**; leave it empty for the
  fullname-derived default.
- The generated Secret is a normal chart resource: `helm uninstall` removes it,
  and its values live in your release. Prefer the create path for *placeholders*
  an injector resolves, not for literal credentials.

## Upgrading to 2.0.0

**No old key is silently honoured.** Every key listed below fails the render with
a message naming its replacement — see
[`templates/_migrations.tpl`](templates/_migrations.tpl). That is the point: a
rename that keeps quietly accepting the old name leaves two names for one setting
in every template, and an operator whose values file still carries the old one
never learns it moved. A rename is only safe when the old name fails.

### The chart installs no infrastructure any more

PostgreSQL and the Redis-compatible bus were subcharts and modes; they are now
two DSNs. Run whatever you already run — CNPG, Percona, RDS, Valkey, a sentinel
quorum, ElastiCache — and hand the chart a Secret holding the URL. See
[The stack around it: bring your own](#the-stack-around-it-bring-your-own) for
the stack this is tested against.

| Removed | Replacement |
| --- | --- |
| `console.database.*` | top-level `database.*` |
| `console.valkey.*` | top-level `redis.*` |
| `database.mode`, `database.cnpg.*` | `database.existingSecret` → a `postgres://` DSN |
| `redis.mode`, `redis.address`, the separate password Secret | `redis.existingSecret` → a `redis://` DSN |
| the `valkey` and `cnpg-operator` subcharts | install them yourself |
| `console.networkPolicy.valkeyEgress` | `console.networkPolicy.redisEgress` |
| `console.networkPolicy.valkeyIngressFrom` | write it where your bus is installed |
| `networkPolicy.cnpgOperatorNamespace`, `…PodLabels` | write it where your database is installed |

### Renamed or removed elsewhere

| Removed | Replacement |
| --- | --- |
| `console.webhooks.encryptionKeySecret.{name,key}` | `console.webhooks.existingSecret` / `existingSecretKey` |
| `console.controller.grpcAddr` | `console.controller.grpcAddress` |
| `pdb.*` | `controller.pdb.*` — it only ever rendered the controller's budget |
| `prometheusRule.rules` | per-rule knobs + `prometheusRule.additionalRules` |

### Breaking: `existingSecretKey` defaults changed

If you rely on a **default** key name inside an existing Secret, set it
explicitly to keep the old value — or rename the key in your Secret. This one
cannot fail loudly: the chart cannot tell a value you set deliberately from the
default it shipped, so a silent coalesce would guess. It is a major bump instead.

| Values key | Old default | New default |
| --- | --- | --- |
| `database.existingSecretKey` | `dsn` | `console-database-dsn` |
| `console.auth.local.existingSecretKey` | `password` | `console-local-admin-password` |
| `console.auth.oidc.existingSecretKey` | `clientSecret` | `console-oidc-client-secret` |
| `console.webhooks.existingSecretKey` | `encryptionKey` | `console-webhooks-encryption-key` |

```yaml
# keep 1.x behaviour verbatim
database:
  existingSecretKey: dsn
console:
  auth:
    local: {existingSecretKey: password}
    oidc: {existingSecretKey: clientSecret}
  webhooks: {existingSecretKey: encryptionKey}
```

**Deliberately not renamed.** `controller.replicaCount` and `console.replicas`
still differ: both are idiomatic Helm, and a key with a non-empty chart default
cannot be coalesced without guessing which of the two the user meant — the cost
is a permanent shim on a hot path for a cosmetic gain. `timeoutMs` keeps its
unit suffix because it is an integer of milliseconds, not a duration string like
every `timeout:` next to it. `url` versus `address` tracks a real difference (a
URL versus `host:port`). The `grpcAddr` key inside the *rendered console config
file* is the application's own schema, not a chart value, and is unchanged.

### GeoIP gained an automated path, opt-in

`console.mtr.enrichment.geoip.mode` is new and defaults to **empty**, which
means: `volume` when a volume or an explicit path is supplied, and `disabled`
otherwise. An upgrade therefore changes nothing on its own — no `geoipupdate`
sidecar appears, no MaxMind egress is needed, and hop enrichment that was off
stays off. Ask for the sidecar explicitly with `mode: auto` plus MaxMind
credentials. If you were mounting your own mmdb files with `geoip.volume`, you
can set `mode: volume` to say so out loud; leaving it empty keeps exactly the
old behaviour:

```yaml
console:
  mtr:
    enrichment:
      geoip:
        mode: volume        # was implicit before 2.0.0
        volume: {persistentVolumeClaim: {claimName: geolite2}}
```

Rendering fails fast either way — `mode: volume` with no volume, or `mode: auto`
with no MaxMind credentials — rather than starting a console with geoip silently
off.

### values.yaml was regrouped

The file is now nine numbered sections: naming, shared config, database, redis,
agent, controller, console, observability, kubernetes. The two infrastructure
blocks sit at the top level ahead of the components that consume them, rather
than nested under `console:` — you configure a database whether or not the
console is on, and `console.database.*` said otherwise. YAML key order does not
affect rendering, and CI proves it: every profile renders byte-identically
across the move.

## Chart internals worth knowing

- **Null overrides delete defaults.** Naming a block with nothing under it
  (`secret:`, `resources:`) merges a null over the chart default and removes the
  sub-tree.
- **Secrets are referenced by name by default.** The chart never GENERATES or
  reads credential material, and `existingSecret` is the recommended path. The
  `secret.create` blocks are the exception: they template a Secret from the
  values you supply, which is meant for an injector's placeholders
  (`${vault:...}`) rather than for literals — a literal in `values.yaml` is a
  credential in your release history. The DSN, bootstrap password, OIDC client
  secret and webhook encryption key all ride one projected volume mounted at
  `/etc/kconmon-ng-console-secrets` — a sibling of the config mount, because a
  mountpoint cannot be created inside an already-mounted read-only volume. All
  four are read once at boot, so rotating one is an operator-initiated restart.
- **The console gets a Kubernetes identity only when it needs one.**
  `console.kubernetesContext.enabled` (event reader) or
  `console.alerting.enabled` (rule reconciler) render a console-only
  ServiceAccount, `POD_NAMESPACE`, the apiserver egress rule and the matching
  grant — a cluster-scoped `ClusterRole` for events, a *namespaced* `Role` for
  `prometheusrules` whose verbs are exactly the calls the client makes
  (`get`, `list`, `create`, `patch`, `delete`): server-side apply needs `patch`
  plus `create`, and `delete` is what removes the bundle when the last rule is
  disabled. There is no `update` (apply never falls back to read-modify-write)
  and no `watch` (the reconciler polls). The agent/controller grant is never
  widened.
- **The console's own ingress rule is open by default, and that is a choice.**
  Nothing inside the release dials the console, so the only legitimate caller is
  whatever fronts the UI — an ingress controller, a `NodePort`, a
  `LoadBalancer` — whose namespace, labels or source CIDR the chart cannot know.

  One caller IS inside the release: with `serviceMonitor.enabled`, Prometheus
  scrapes the console's own `/metrics`. That gets its own ingress rule, and the
  rule renders **only when `networkPolicy.prometheusNamespace` is set** —
  `/metrics` has a listener of its own on `config.metricsPort`, and the rule
  opens that port only: a rule on the API port would admit everything else in
  the scraper's namespace to the whole API, quietly undoing the narrowing
  `ingressFrom` exists to express. So if you narrow `ingressFrom`,
  set `networkPolicy.prometheusNamespace` alongside it, or the console's own
  metrics go dark (`up{job="…-console"} = 0`) while everything else keeps
  working. Restricting by default would mean a silent,
  total UI outage on upgrade for every existing install, so the default stays
  open and `console.networkPolicy.ingressFrom` narrows it:

  ```yaml
  console:
    networkPolicy:
      ingressFrom:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: ingress-nginx
  ```

  Set it. `kubectl port-forward` does **not** traverse NetworkPolicy — the
  kubelet attaches to the pod's network namespace directly — so restricting this
  cannot lock you out of the UI for debugging. Note the console's real
  authentication boundary is `console.auth.mode`, which is `anonymous` unless
  you change it; this rule is defence in depth, not the lock.
- **Optional config blocks are emitted only when enabled.** Both config files
  are parsed with unknown fields rejected, so an unconditional key would
  crashloop an older image while a Deployment or DaemonSet rolls.
- **NetworkPolicy v1 cannot say "ICMP".** `ports[].protocol` accepts only
  `TCP`, `UDP` and `SCTP`; anything else is rejected by the API server. An ICMP
  allowance can therefore only be written as a peer rule with **no `ports`
  section**, which allows every protocol to (or from) that peer. Agent-to-agent
  ICMP and MTR get such a rule on **both** halves — egress *and* ingress, since
  a policy that opens only one direction drops the probes just as dead — and
  both stay narrow because the peer is a `podSelector`, not a CIDR.
- **Every probe rule is a matched pair.** The TCP checker dials the *peer's*
  `config.httpPort` (and then `GET /readyz` on it), the UDP probe server listens
  on `config.grpcPort`; both appear in one egress rule and one identical ingress
  rule. Note that `config.httpPort` ingress from the console is its own rule:
  the Prometheus scrape rule only covers console-to-controller traffic while
  `networkPolicy.prometheusNamespace` is empty, which is exactly the kind of
  accident that works in CI and fails on a real cluster.
- **The external checker has no default egress rule, on purpose.** Its targets
  are operator-chosen and take `tcp`/`udp`/`icmp`/`mtr` on any port, so the only
  default the chart could render is a ports-less rule to `0.0.0.0/0` — which is
  allow-everything egress and not something to hand out silently.
  `config.checkers.external.enabled` together with `networkPolicy.enabled` and
  an empty `networkPolicy.externalEgress` therefore **fails the render** with a
  message naming the key. Set it to the rules your targets need, scoped to the
  same range as `config.checkers.external.allowedCidrs`:

  ```yaml
  networkPolicy:
    externalEgress:            # a list of whole egress rules, spliced in verbatim
      - to:
          - ipBlock:
              cidr: 10.0.0.0/8 # no ports: key, so ICMP and MTR pass too
  ```

  Ask for the old permissive shape explicitly with
  `[{to: [{ipBlock: {cidr: 0.0.0.0/0}}]}]` if that is really what you want.
- **Your database's own policies are yours.** The chart renders egress from the
  *console* to whatever `database.existingSecret` points at, and nothing else —
  it installs no database and writes no policy for one. If your PostgreSQL is a
  CNPG Cluster, the operator still needs its own rule to reach the instance
  manager on `8000` (without one the Cluster hangs on `Instance Status
  Extraction Error`), and that rule belongs with the Cluster, in whatever
  installs it. Same for a Percona cluster, a sentinel quorum, or anything else
  you run: this chart's business ends at the DSN.
- **The apiserver rules assume your CNI matches pre-DNAT.** Both
  `networkPolicy.kubeAPIEgress` and `console.networkPolicy.kubeAPIEgress`
  default to TCP `443`/`6443` to `0.0.0.0/0`, which matches the `kubernetes`
  *Service* ClusterIP. Policy engines that evaluate the post-DNAT **endpoint**
  (Calico among them) see the real backend port instead, and that is often
  neither — minikube's is `8443`. Check with `kubectl get endpointslices -n
  default -l kubernetes.io/service-name=kubernetes` and, if it differs, put the
  real address and port in the knob. The symptom is a clean `dial tcp
  10.96.0.1:443: i/o timeout` from the controller's Lease renewal or the
  console's alert-rule sync.
- **NetworkPolicy is only the cluster-side gate.** For any destination outside
  the cluster (an external Valkey or PostgreSQL, an OIDC IdP, a control plane),
  the destination's own firewall — iptables/nftables, a cloud security group —
  must be opened separately. Egress allowed here and still refused on the wire
  almost always means that layer was missed.
- **Nothing in the bus is durable.** Sessions, rate-limit counters and pub/sub only: losing the
  instance on a Pod restart is a liveness event, never a data-loss one — which is why the subchart's
  persistence can stay off.

## Links

- GitHub repository: <https://github.com/EsDmitrii/kconmon-ng>
- Grafana dashboards: [`dashboards/`](https://github.com/EsDmitrii/kconmon-ng/tree/main/dashboards)
  (`overview.json`, `node-detail.json`, `zone-heatmap.json`)

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
| `prometheusRule.enabled` | `false` | Create a Prometheus Operator `PrometheusRule` with the seven built-in alerts ([Alerting rules](#alerting-rules)) |
| `prometheusRule.<alertName>` | all enabled | Per-rule `enabled` / `threshold` / `for` / `severity` |
| `prometheusRule.additionalRules` | `[]` | Extra rules appended to the group verbatim |
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
| `<consumer>.secret.create` | `false` | Let the chart render the Secret instead of referencing one ([Chart-managed Secrets](#chart-managed-secrets)) |

## Metrics

All metrics are prefixed with `config.metricsPrefix` (default `kconmon_ng`).
Selected key metrics:

- `kconmon_ng_tcp_results_total` — total TCP probe results (labelled by `result`)
- `kconmon_ng_udp_packet_loss_ratio` — UDP packet loss ratio (0.0–1.0)
- `kconmon_ng_icmp_packet_loss_ratio` — ICMP packet loss ratio (0.0–1.0)
- `kconmon_ng_dns_results_total` — total DNS resolution results (labelled by `result`)
- `kconmon_ng_controller_registered_agents` — agents currently registered with the controller
- `kconmon_ng_controller_expected_agents` — schedulable nodes expected to run an agent

## Alerting rules

`prometheusRule.enabled=true` renders one `PrometheusRule` with seven built-in
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
| `KconmonAgentsMissing` | `expected_agents - registered_agents > 0` for 10m | `prometheusRule.kconmonAgentsMissing` | `for` `10m`, `severity` `warning` |
| `KconmonControllerDown` | `absent(<prefix>_controller_leader == 1)` for 5m | `prometheusRule.kconmonControllerDown` | `for` `5m`, `severity` `critical` |

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
names the failing pair, direction and measured value instead of repeating one
generic sentence per firing series:

| Rule | Annotation identifies |
| --- | --- |
| `UDPLossHigh` | source → destination node, both zones, loss % |
| `TCPChecksFailing` | source → destination node, both zones, failed % |
| `PairWentSilent` | source → destination node |
| `DNSChecksFailing` | source node + zone, queried `host`, `resolver`, failed % |
| `ExternalChecksFailing` | source node + zone, `target`, `target_kind`, failed % |
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
`dashboards/` are imported as plain JSON with `kconmon_ng_` written out
literally and nothing rewrites them, so a non-default prefix means editing that
JSON by hand.

### Migrating from `prometheusRule.rules`

The pre-1.12 shape — a full list of rule objects in `prometheusRule.rules` — is
deprecated but still works. A non-empty list **replaces every built-in rule**
and keeps the old behaviour, including the `replace "kconmon_ng" <prefix>`
rewrite of each `expr`, so metric names in it must stay written with the literal
`kconmon_ng` prefix. `additionalRules` is appended either way.

To migrate: drop the entries that are just the built-ins, move genuinely custom
rules to `additionalRules` (rewriting their metric names to your real prefix),
and express threshold/severity changes through the per-rule knobs.

### Two different `PrometheusRule` objects

`prometheusRule.enabled` and `console.alerting.enabled` are two different things
and neither implies the other. The former renders the static object described
here, from the chart, edited in Git. The latter lets operators build rules in
the Console UI, stores them in PostgreSQL and reconciles them into a *separate*,
console-owned `PrometheusRule` object. Run both, either, or neither.

## Optional dependencies

The chart can install the pieces of **its own stack** — the PostgreSQL operator
and the Valkey the Console uses for cross-replica fan-out. Both are **off by
default**, so an install that does not ask for them renders exactly as before.

**The monitoring stack stays external.** Prometheus Operator and Grafana are
infrastructure this chart *consumes*, never installs: `serviceMonitor` and
`prometheusRule` produce objects for an operator you already run, and the
dashboards under `dashboards/` are JSON you import. A cluster has one monitoring
stack shared by everything in it, and a product chart is the wrong owner for it.

| Subchart | Values key | Version | Default |
| --- | --- | --- | --- |
| [`cloudnative-pg`](https://github.com/cloudnative-pg/charts) | `cnpg-operator` | `0.29.0` | off |
| [`valkey`](https://github.com/valkey-io/valkey-helm) (official) | `valkey` | `0.11.0` | off |

Versions are pinned in `Chart.lock`, which is committed; the fetched `charts/*.tgz`
are not. A fresh clone needs `helm dependency build charts/kconmon-ng` before
`helm lint`/`template`/`package` (the `Makefile` targets and CI do this).

### Helm puts subchart values at the top level

This trips everyone once: a subchart is configured under a **top-level key named
after the dependency alias**, not nested under the feature it belongs to. So the
operator is `cnpg-operator:` at the root of your values file — *not*
`console.database.cnpg.operator`. Everything under that key goes to the subchart
verbatim, so the whole upstream values surface is available:

```yaml
cnpg-operator:
  enabled: true          # read by this chart's dependency condition
  replicaCount: 1        # everything else is upstream cloudnative-pg values
  monitoring:
    podMonitorEnabled: true

valkey:
  enabled: true
  architecture: standalone

console:
  valkey:
    mode: dependency     # points the console at the subchart's primary Service
  database:
    mode: cnpg
```

`console.database.cnpg.*` still describes the **Cluster CR** this chart renders;
`cnpg-operator.*` describes the **operator** that reconciles it. Two different
things, deliberately two different keys.

### CNPG: operator and database in one release — the honest answer

**Use two steps.** Install the operator first, let it become ready, then turn the
database on:

```bash
# 1. operator only
helm upgrade --install kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng \
  --set cnpg-operator.enabled=true --wait

# 2. now the Cluster CR
helm upgrade --install kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng \
  --set cnpg-operator.enabled=true \
  --set console.enabled=true --set console.database.mode=cnpg
```

A single-shot install of both is **not supported**, for two independent reasons,
and neither is a bug this chart can fix:

1. The `cloudnative-pg` chart ships its CRDs in `templates/crds/` (gated by
   `crds.create`), not in Helm's special top-level `crds/` directory. Only the
   latter is guaranteed to be applied before Helm resolves the REST mappings for
   the rest of the release, so a `Cluster` CR in the same release can fail with
   `no matches for kind "Cluster"`.
2. Even with the CRDs in place, CNPG registers validating and mutating webhooks
   pointing at the operator Service. Creating a `Cluster` before the operator
   Pod is ready fails on a webhook dial error. Nothing about resource ordering
   fixes an async readiness race.

If the operator is already in the cluster — the common case — leave
`cnpg-operator.enabled=false` and set `console.database.mode=cnpg`: that is the
pre-existing behavior and a single install works.

### Valkey: four modes

| `console.valkey.mode` | What runs | Use when |
| --- | --- | --- |
| `disabled` (default) | in-process bus | `console.replicas: 1` |
| `bundled` | one ephemeral `valkey/valkey` Deployment this chart renders | you want a small cross-replica bus and nothing else |
| `dependency` | the official `valkey` subchart (`valkey.enabled=true`) | you want replication, ACL auth, TLS, metrics |
| `external` | nothing; you supply `address` | Valkey already exists |

**Authentication.** `dependency` and `external` both carry a password:
`console.valkey.existingSecret` / `existingSecretKey` (or the managed `secret:`
block) mounts it and the console AUTHs with it. For the subchart, point *both*
sides at one Secret — `valkey.auth.existingSecret` +
`valkey.auth.existingSecretPasswordKey` and `console.valkey.existingSecret` —
or set `valkey.auth.enabled=false` for an unauthenticated in-cluster bus. The
chart refuses the combination that cannot work (subchart auth on, console
without a password) rather than letting every publish fail with `NOAUTH`.

`bundled` stays the default path and is unchanged — it was **not** migrated onto
the subchart. Two reasons. It is a 12-line ephemeral Deployment pinned to the
official upstream `valkey/valkey:9-alpine` image (BSD), which is the right size
for a pub/sub bus that ADR-002 requires to lose zero durable data on a flush;
replacing it would also change the rendered objects for every existing user.

The `dependency` path uses the **official** chart from the Valkey project
(`valkey-io/valkey-helm`, published at
`oci://ghcr.io/valkey-io/valkey-helm/valkey`). It runs the upstream
`docker.io/valkey/valkey` image tagged from its own appVersion, ships
restricted-PSS container defaults (`runAsNonRoot`, `readOnlyRootFilesystem`,
`drop: [ALL]`, seccomp `RuntimeDefault`) and carries a `values.schema.json`.
Bitnami's chart was **not** used: since the 2025 move to Bitnami Secure Images
its free images are the legacy generation with `image.tag: latest` as the chart
default, and a moving tag is not something to depend on.

Its auth is **ACL-based and off by default**, which suits an in-cluster bus
reachable only through the console's NetworkPolicy. To turn it on, point both
sides at one Secret — the subchart keys passwords by *username*, so the ACL
`default` user means key `default`:

```yaml
valkey:
  enabled: true
  auth:
    enabled: true
    usersExistingSecret: valkey-acl
    aclUsers:
      default:
        permissions: "~* &* +@all"
console:
  valkey:
    mode: dependency
    existingSecret: valkey-acl
    existingSecretKey: default     # the ACL username
```

Configuring the subchart's auth while leaving the console without a password
fails the render rather than letting every publish die with `NOAUTH`.

## Batteries: what the chart sets up for you

| Thing you need | How you get it | Manual work left |
| --- | --- | --- |
| Grafana dashboards | `dashboards.enabled=true` renders them as ConfigMaps with the `grafana_dashboard` sidecar label | none, if you run the Grafana sidecar |
| GeoLite2 databases | `console.mtr.enrichment.geoip.mode=auto` runs the official `maxmind/geoipupdate` sidecar | supply MaxMind credentials once |
| PostgreSQL operator | `cnpg-operator.enabled=true` (subchart) | two-step install, see above |
| Valkey | `console.valkey.mode=bundled` (default) or the `valkey` subchart | none |
| Secrets | `existingSecret`, or `secret.create` with an injector | none |
| Pod hardening | restricted-PSS defaults (runAsNonRoot, drop ALL, seccomp RuntimeDefault, read-only root) | the **agent** needs `NET_RAW` for ICMP/MTR, so it needs a `baseline` namespace or a PSS exemption |
| Prometheus / Grafana themselves | **not installed** — external infrastructure | you run them |

### GeoLite2 databases keep themselves current

Hop enrichment needs MaxMind's GeoLite2 ASN and City databases. With
`mode: auto` — the default when enrichment is on — the chart runs MaxMind's own
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
| CNPG operator assumed pre-installed | **fixed** — optional subchart |
| Valkey had no managed option beyond the bundled Deployment | **fixed** — `valkey` subchart as a fourth mode |
| Secrets all had to pre-exist | **fixed** — `secret.create` with semantic fields |
| Secret key defaults collided in a shared Secret | **fixed** — component-scoped key names |
| `console.prometheus.url` empty ⇒ silent 503 pages | **partly fixed** — `NOTES.txt` calls it out by name at install time. Not auto-detected: a cluster can hold several Prometheis and guessing wrong points the Console at someone else's data |
| Webhook encryption key must be supplied | **justified manual** — auto-generating with `randAlphaNum` + `lookup` looks convenient and is a data-loss trap: `lookup` returns empty during `helm template`, `--dry-run` and any upgrade run without cluster access, so the key silently rotates and every stored webhook secret becomes undecryptable. Keyless is already a supported, documented state |
| Prometheus Operator / Grafana not installed | **justified manual** — a cluster has one monitoring stack, shared by everything; a product chart is the wrong owner |
| CNPG operator + database in one release | **justified manual** — two independent causes documented above; the chart does not pretend otherwise |
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
| PostgreSQL DSN (`mode: external`) | `console.database.existingSecret` | `console-database-dsn` | `console.database.secret.dsn` |
| Local bootstrap admin | `console.auth.local.existingSecret` | `console-local-admin-password` | `console.auth.local.secret.password` |
| OIDC client secret | `console.auth.oidc.existingSecret` | `console-oidc-client-secret` | `console.auth.oidc.secret.clientSecret` |
| Webhook encryption key | `console.webhooks.existingSecret` | `console-webhooks-encryption-key` | `console.webhooks.secret.encryptionKey` |
| CNPG backup credentials | `console.database.cnpg.backup.existingSecret` | `ACCESS_KEY_ID` + `ACCESS_SECRET_KEY` (fixed by CNPG) | `…backup.secret.accessKeyId` / `.secretAccessKey` |

The key defaults are **component-scoped on purpose**, so one shared Secret can
carry the whole stack's credentials without two components fighting over a
generic `password` or `dsn`. Override `existingSecretKey` to anything you like;
whatever you set is both the key the console reads and the key the create path
writes. CNPG's two backup keys are the exception — the operator fixes those
names, so the chart cannot rename them.

`console.database.mode: cnpg` is not in this table: CNPG generates the DSN
Secret itself (`<cluster>-app`, key `uri`) and the chart only reads it.

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

One breaking change, plus renames that keep working.

### Breaking: `existingSecretKey` defaults changed

If you rely on a **default** key name inside an existing Secret, set it
explicitly to keep the old value — or rename the key in your Secret.

| Values key | Old default | New default |
| --- | --- | --- |
| `console.database.existingSecretKey` | `dsn` | `console-database-dsn` |
| `console.auth.local.existingSecretKey` | `password` | `console-local-admin-password` |
| `console.auth.oidc.existingSecretKey` | `clientSecret` | `console-oidc-client-secret` |
| `console.webhooks.existingSecretKey` | `encryptionKey` | `console-webhooks-encryption-key` |

```yaml
# keep 1.x behaviour verbatim
console:
  database: {existingSecretKey: dsn}
  auth:
    local: {existingSecretKey: password}
    oidc: {existingSecretKey: clientSecret}
  webhooks: {existingSecretKey: encryptionKey}
```

A default cannot be migrated automatically: the chart cannot tell a value you
set deliberately from the default it shipped, so a silent coalesce would guess.
It is a major bump instead.

### Renamed, with the old key still honoured

Naming is unified on `existingSecret` / `existingSecretKey` everywhere. The
deprecated key keeps working **when it is the only one set**; setting both fails
the render naming the conflict.

| Deprecated | Use instead |
| --- | --- |
| `console.database.cnpg.backup.credentialsSecret` | `console.database.cnpg.backup.existingSecret` |
| `console.webhooks.encryptionKeySecret.name` | `console.webhooks.existingSecret` |
| `console.webhooks.encryptionKeySecret.key` | `console.webhooks.existingSecretKey` |
| `console.controller.grpcAddr` | `console.controller.grpcAddress` |
| `prometheusRule.rules` | per-rule knobs + `prometheusRule.additionalRules` |

[`ci/deprecated-keys-values.yaml`](ci/deprecated-keys-values.yaml) renders
byte-identically to its new-name twin, and CI keeps it that way.

**Deliberately not renamed.** `controller.replicaCount` and `console.replicas`
still differ: both are idiomatic Helm, and a key with a non-empty chart default
cannot be coalesced without guessing which of the two the user meant — the cost
is a permanent shim on a hot path for a cosmetic gain. `timeoutMs` keeps its
unit suffix because it is an integer of milliseconds, not a duration string like
every `timeout:` next to it. `url` versus `address` tracks a real difference (a
URL versus `host:port`). The `grpcAddr` key inside the *rendered console config
file* is the application's own schema, not a chart value, and is unchanged.

### GeoIP now defaults to the automated path

`console.mtr.enrichment.geoip.mode` is new and defaults to `auto`, which runs
the `geoipupdate` sidecar. If you were mounting your own mmdb files with
`geoip.volume`, set `mode: volume` to keep exactly the old behaviour:

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

The file is now eight numbered sections — naming, shared config, agent,
controller, console (core, database, auth, valkey, webhooks, alerting, extras,
plumbing), observability, kubernetes, dependencies. YAML key order does not
affect rendering, and CI proves it: every profile renders byte-identically
across the move.

## Chart internals worth knowing

- **CNPG ordering.** `console.database.mode=cnpg` renders a CNPG `Cluster` CR
  but does *not* install the operator; `helm install` fails with "no matches for
  kind Cluster" until the operator's CRDs exist.
- **Null overrides delete defaults.** Naming a block with nothing under it
  (`cnpg:`, `storage:`) merges a null over the chart default and removes the
  sub-tree. The chart fails with an actionable message for the two blocks it
  cannot guess (`cnpg.instances`, `cnpg.storage.size`).
- **Secrets are referenced by name only.** The chart never templates, generates
  or reads credential material; the DSN, bootstrap password, OIDC client secret
  and webhook encryption key all ride one projected volume mounted at
  `/etc/kconmon-ng-console-secrets` — a sibling of the config mount, because a
  mountpoint cannot be created inside an already-mounted read-only volume. All
  four are read once at boot, so rotating one is an operator-initiated restart.
- **The console gets a Kubernetes identity only when it needs one.**
  `console.kubernetesContext.enabled` (event reader) or
  `console.alerting.enabled` (rule reconciler) render a console-only
  ServiceAccount, `POD_NAMESPACE`, the apiserver egress rule and the matching
  grant — a cluster-scoped `ClusterRole` for events, a *namespaced* `Role` for
  `prometheusrules` with no `delete` verb. The agent/controller grant is never
  widened.
- **Optional config blocks are emitted only when enabled.** Both config files
  are parsed with unknown fields rejected, so an unconditional key would
  crashloop an older image while a Deployment or DaemonSet rolls.
- **NetworkPolicy is only the cluster-side gate.** For any destination outside
  the cluster (an external Valkey or PostgreSQL, an OIDC IdP, a control plane),
  the destination's own firewall — iptables/nftables, a cloud security group —
  must be opened separately. Egress allowed here and still refused on the wire
  almost always means that layer was missed.
- **The bundled Valkey is deliberately ephemeral.** No PVC, no writable data
  dir: per ADR-002 a flush must lose zero durable data, so losing the instance
  on a Pod restart is a liveness event, never a data-loss one.

## Links

- GitHub repository: <https://github.com/EsDmitrii/kconmon-ng>
- Grafana dashboards: [`dashboards/`](https://github.com/EsDmitrii/kconmon-ng/tree/main/dashboards)
  (`overview.json`, `node-detail.json`, `zone-heatmap.json`)

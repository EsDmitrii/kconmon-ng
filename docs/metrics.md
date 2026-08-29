# Metrics and alerting reference

All metric names use the configurable prefix (default `kconmon_ng`). The common
label set for peer metrics — "peer" below — is `source_node`,
`destination_node`, `source_zone`, `destination_zone`.

External checks (M4) use a **different** label set — "external" below:
`source_node`, `source_zone`, `target`, `target_kind`, `check_type`. There is no
`destination_node` or `destination_zone`, because the destination is not a
peer: `target` is the operator's NAME for it (never an address),
`target_kind` is the closed set `host|url` and `check_type` is the probe's own
type (`icmp|tcp|udp|dns|http`). `check_type` is what keeps two checks on one
target apart: everything that is not http collapses to `target_kind="host"`, so
without it an icmp and a tcp check on the same target shared one series and
averaged each other's failures away. **The peer label set is unchanged
by M4** — no external family reuses it and no peer family gained a `target`
label, so an existing dashboard or recording rule keyed on peer labels behaves
exactly as it did in 1.5.0.

## Agent — TCP

| Metric                                    | Type      | Labels          | Description                        |
| ----------------------------------------- | --------- | --------------- | ---------------------------------- |
| `kconmon_ng_tcp_connect_duration_seconds` | histogram | peer            | TCP connect phase duration         |
| `kconmon_ng_tcp_total_duration_seconds`   | histogram | peer            | Total TCP probe RTT                |
| `kconmon_ng_tcp_results_total`            | counter   | peer + `result` | Probe outcomes: `success` / `fail` |

## Agent — UDP

| Metric                             | Type      | Labels          | Description                  |
| ---------------------------------- | --------- | --------------- | ---------------------------- |
| `kconmon_ng_udp_rtt_seconds`       | histogram | peer            | Mean UDP round-trip time     |
| `kconmon_ng_udp_jitter_seconds`    | gauge     | peer            | Inter-packet delay variation |
| `kconmon_ng_udp_packet_loss_ratio` | gauge     | peer            | Packet loss ratio (0.0–1.0)  |
| `kconmon_ng_udp_results_total`     | counter   | peer + `result` | Probe outcomes               |

## Agent — ICMP

| Metric                              | Type      | Labels          | Description                 |
| ----------------------------------- | --------- | --------------- | --------------------------- |
| `kconmon_ng_icmp_rtt_seconds`       | histogram | peer            | ICMP round-trip time        |
| `kconmon_ng_icmp_packet_loss_ratio` | gauge     | peer            | Packet loss ratio (0.0–1.0) |
| `kconmon_ng_icmp_results_total`     | counter   | peer + `result` | Probe outcomes              |

## Agent — DNS

| Metric                            | Type      | Labels                                           | Description                              |
| --------------------------------- | --------- | ------------------------------------------------ | ---------------------------------------- |
| `kconmon_ng_dns_duration_seconds` | histogram | `host`, `resolver`, `source_node`, `source_zone` | Resolution duration per (host, resolver) |
| `kconmon_ng_dns_results_total`    | counter   | same + `result`                                  | Resolution outcomes                      |

## Agent — HTTP

| Metric                                     | Type      | Labels                                                                 | Description            |
| ------------------------------------------ | --------- | ---------------------------------------------------------------------- | ---------------------- |
| `kconmon_ng_http_dns_duration_seconds`     | histogram | `url`, `source_node`, `source_zone`                                    | DNS phase              |
| `kconmon_ng_http_connect_duration_seconds` | histogram | same                                                                   | TCP connect phase      |
| `kconmon_ng_http_tls_duration_seconds`     | histogram | same                                                                   | TLS handshake phase    |
| `kconmon_ng_http_ttfb_seconds`             | histogram | same                                                                   | Time to first byte     |
| `kconmon_ng_http_total_duration_seconds`   | histogram | same                                                                   | Total request duration |
| `kconmon_ng_http_results_total`            | counter   | `url`, `method`, `status_code`, `source_node`, `source_zone`, `result` | Request outcomes       |

## Agent — MTR

| Metric                           | Type    | Labels                                                    | Description                    |
| -------------------------------- | ------- | --------------------------------------------------------- | ------------------------------ |
| `kconmon_ng_mtr_triggered_total` | counter | peer                                                      | Number of MTR traces triggered |
| `kconmon_ng_mtr_hops`            | gauge   | peer                                                      | Hop count in the last trace    |
| `kconmon_ng_mtr_hop_rtt_seconds` | gauge   | `source_node`, `destination_node`, `hop_number`, `hop_ip` | Per-hop RTT                    |

## Agent — External

Every probe whose destination is not a peer agent: the Console's external
targets (continuous) and one-shot external diagnostics. Gated on
`config.checkers.external.enabled`, which is **off by default**.

Every vec below stays EMPTY until an external probe reports, and an empty vec
collects nothing — an agent with the feature off exposes a `/metrics` that is
byte-identical to the one it exposed before this family existed. That is also
why the `ExternalChecksFailing` rule is inert rather than firing on an install
that never enabled the feature.

| Metric                                  | Type      | Labels              | Description                                             |
| --------------------------------------- | --------- | ------------------- | ------------------------------------------------------- |
| `kconmon_ng_external_duration_seconds`  | histogram | external            | Probe duration                                          |
| `kconmon_ng_external_rtt_seconds`       | histogram | external            | Round-trip time                                         |
| `kconmon_ng_external_packet_loss_ratio` | gauge     | external            | Packet loss ratio (0.0–1.0)                             |
| `kconmon_ng_external_results_total`     | counter   | external + `result` | Results that reached the network                        |
| `kconmon_ng_external_http_status_code`  | gauge     | external            | Last HTTP status code from an external `http` check     |
| `kconmon_ng_external_denied_total`      | counter   | external + `reason` | Probes refused by the allowlist: `cidr`/`resolve`/`disabled` |
| `kconmon_ng_external_specs_rejected_total` | counter | `source_node`, `check_type` | Assignment entries this agent could not parse (a definition no agent can run) |

`external_denied_total` is the one to alert on when a probe never happens:
a refused probe increments it and **not** `external_results_total`, so a
denied destination is a visible zero on the results counter rather than a
failure. `reason=cidr` means the resolved address fell outside `allowedCidrs`
or inside `deniedCidrs`; `resolve` means the name did not resolve; `disabled`
means a spec arrived while `checkers.external.enabled` was false.

## Controller

| Metric                                     | Type    | Description                                |
| ------------------------------------------ | ------- | ------------------------------------------ |
| `kconmon_ng_controller_registered_agents`  | gauge   | Currently registered agents                |
| `kconmon_ng_controller_expected_agents`    | gauge   | Schedulable nodes expected to run an agent |
| `kconmon_ng_controller_grpc_connections`   | gauge   | Active gRPC streaming connections          |
| `kconmon_ng_controller_peer_updates_total` | counter | Peer list updates broadcast to agents      |
| `kconmon_ng_controller_leader`             | gauge   | `1` if this instance is the active leader  |
| `kconmon_ng_controller_diagnostics_total` | Counter | On-demand diagnostics dispatched, by `type` and `result`. |
| `kconmon_ng_controller_peer_updates_total` | Counter | Peer-list pushes sent to agents. |
| `kconmon_ng_controller_event_subscribers` | Gauge | Open event-stream subscribers. |
| `kconmon_ng_controller_external_subscribers` | gauge | Active agent `WatchExternalChecks` subscriptions on this replica |
| `kconmon_ng_controller_external_assignments` | gauge | Agents with a non-empty continuous external-check assignment |

Both external gauges are deliberately unlabelled. `external_assignments`
counts **agents, never specs**: a per-agent series would grow with the cluster
for no operational gain.

## Console

The Console exposes its own families under the same prefix, namespaced
`_console_`. M4 additions:

| Metric                                             | Type    | Labels     | Description                                                        |
| -------------------------------------------------- | ------- | ---------- | ------------------------------------------------------------------ |
| `kconmon_ng_console_rate_limited_total`            | counter | `limit`    | Requests refused with 429, by limit (`runs`, `login`, `promql`)    |
| `kconmon_ng_console_rate_limit_failopen_total`     | counter | `limit`    | Requests admitted because the KV backend was unreadable (fail-open) |
| `kconmon_ng_console_projection_guard_failopen_total` | counter | —        | Definition writes admitted because the topology was unreadable      |
| `kconmon_ng_console_scheduler_ticks_total`         | counter | `result`   | Schedule loop ticks: `ok`, `not-leader`, `error`                    |
| `kconmon_ng_console_scheduler_fired_total`         | counter | `kind`     | Runs started by the loop, by schedule kind (`once`, `interval`)     |
| `kconmon_ng_console_scheduler_skipped_total`       | counter | `reason`   | Due schedules not fired: `overrun`, `disabled`                      |
| `kconmon_ng_console_runs_reaped_total`             | counter | —          | Runs force-finished as cancelled by the stuck-run reaper            |
| `kconmon_ng_console_external_series_projected`     | gauge   | —          | Prometheus series the assigned continuous external checks project   |
| `kconmon_ng_console_external_reconciles_total`     | counter | `result`   | Reconcile ticks: `pushed`, `unchanged`, `not-leader`, `error`       |
| `kconmon_ng_console_external_specs_skipped_total`  | counter | `reason`   | Definitions left out of the assignment: `check-type`, `destination-kind` |

`scheduler_ticks_total{result="not-leader"}` is the **normal** case on every
replica but one — the loop is a singleton on a PostgreSQL advisory lock — so
alerting on it is alerting on correct behaviour. The two `failopen` counters
are the honest cost of Decision 8 (a Valkey outage must not become a login
outage; a controller outage must not become a config-write outage): every
admission they count is a control that did not run.

M5 additions:

| Metric                                          | Type    | Labels             | Description                                                          |
| ----------------------------------------------- | ------- | ------------------ | -------------------------------------------------------------------- |
| `kconmon_ng_console_mtr_snapshots_total`        | counter | `result`           | MTR traces projected into path history: `new-path`, `repeat`, `error` |
| `kconmon_ng_console_enrichment_cache_total`     | counter | `result`           | Hop addresses the TTL cache was asked about: `hit`, `miss`            |
| `kconmon_ng_console_enrichment_lookups_total`   | counter | `source`, `result` | Source lookups run for cache misses: `rdns\|asn\|city` × `ok\|miss\|error` |

`mtr_snapshots_total{result="new-path"}` is **the route-changed alerting
primitive** — it fires when a pair takes a route it has never taken before,
which is otherwise something an operator has to notice by diffing two traces
by hand. `repeat` is the steady state (a stable route re-confirmed) and is what
makes `new-path` meaningful: without it a silent projector and a stable network
look identical. `error` counts projections that never landed; a projection
failure deliberately never fails the pair (the `check_results` row is the
authority, the snapshot is a projection), so this counter is the **only** place
it is visible.

Enrichment is **two** counters rather than one, because a cache hit and a
source lookup are not the same event and cannot share a label set honestly: one
cached row answers rdns, asn and city at once, so folding hits into a
`{source,result}` counter would mean attributing a hit to a source that never
ran. `enrichment_cache_total` increments **once per requested IP**, which makes
`hit/(hit+miss)` the cache hit ratio — the number that says whether
`mtr.enrichment.ttl` is doing its job. `enrichment_lookups_total` increments
once per source that **actually ran** for a missed IP: `ok` = the source
returned data, `miss` = it ran and knew nothing about the address (no PTR
record, or the address is not in the mmdb — an ordinary answer, not a failure),
`error` = the lookup itself failed. A source switched off in config, or one
whose file failed to open at boot, is **never counted at all**: a series pinned
at zero would read as "working and finding nothing".

`kconmon_ng_console_retention_deleted_total{table}` gained **three closed label
values** in M5 — `mtr_path_snapshots`, `mtr_hop_enrichment`, `annotations` —
alongside the existing `topology_events`, `audit_log`, `check_runs` and
`check_results`.

M6 additions:

| Metric                                          | Type    | Labels    | Description                                                                 |
| ----------------------------------------------- | ------- | --------- | --------------------------------------------------------------------------- |
| `kconmon_ng_console_k8s_events_total`           | counter | `result`  | Kubernetes events the reader decided about: `stored`, `duplicate`, `filtered`, `error` |
| `kconmon_ng_console_webhook_deliveries_total`   | counter | `result`  | Webhook deliveries reaching a terminal decision: `ok`, `failed`, `filtered`  |

`k8s_events_total{result="duplicate"}` is the **normal** outcome of a relist,
not a failure: `kubernetesContext.resyncInterval` forces a periodic list, and
every already-stored row it returns costs one rejected INSERT and one increment
here. Alerting on it is alerting on the resync doing its job. `filtered` is the
**fail-closed** drop — a node event with no topology to vouch for the node —
and is the counter to watch if the timeline looks quiet: a `filtered` rate that
tracks the total usually means `controller.url` is unset, not that the cluster
is calm. Events for kinds the reader does not handle are skipped **uncounted**,
deliberately, so `filtered` stays readable as the one thing it means.

`webhook_deliveries_total` counts **one per DELIVERY, never per HTTP attempt**:
a delivery that succeeds on the third rung of the 0s/30s/5m ladder is one `ok`,
not two `failed` and an `ok`. That is what makes `failed/(ok+failed)` an
endpoint-health ratio rather than a retry-count artefact. `filtered` is the
steady state of an endpoint that does not subscribe to the event — the
equivalent of `repeat` above, and what makes `failed` meaningful — and a
**disabled** endpoint is not counted at all, since a switched-off endpoint that
kept incrementing a series would read as a working one.

`kconmon_ng_console_retention_deleted_total{table}` gained **three more closed
label values** in M6 — `k8s_events`, `incidents`, `maintenance_windows` —
bringing the set to ten. The tenth is `check_results`, the highest-volume table
the pruner sweeps: leaving it out of this list made a sweep that was falling
behind on the biggest table look idle in any view built from these docs. There is deliberately **no `webhooks` value**: webhook
rows are configuration, not observation, and are never swept.

**No M6 metric carries a node name, a pod name, a namespace, an event reason or
message, a webhook name, a webhook URL, an endpoint secret, or an incident's
title, scope or notes.** Both counters are `{result}`-only, and that is the
whole label set. The temptation is real in both places — a
`{node}`/`{reason}` breakdown of cluster events, a `{webhook}` breakdown of
deliveries — and both were rejected: the first is an unbounded cardinality bomb
fed by whatever the cluster decides to emit, and the second puts an
operator-typed name (and, one refactor later, a URL) into a series that lands in
long-term storage. Per-endpoint outcome lives on the `webhooks` row, which is
where a bounded per-endpoint fact belongs.

**No M5 metric carries an IP, a hostname, an ASN, an organization, a country, a
path hash, a node name, a destination or an annotation's text.** Enrichment is
where that rule is easiest to break, since every one of those values is sitting
right there in the resolved row. Per-hop RTT already has a metric with `hop_ip`
in its label set (`kconmon_ng_mtr_hop_rtt_seconds`, exported by the **agent**);
the Console deliberately did not add a second one, which is why the per-hop
trend chart in the MTR Explorer reads snapshot history rather than Prometheus
(M5 Decision 13).

## Default alerting rules

Deployed when `prometheusRule.enabled: true`. The rules live in the chart
(`charts/kconmon-ng/templates/_rules.tpl`), not in Helm values: each one has an
`enabled` toggle plus its tunable numbers under
`prometheusRule.<alertName>.{enabled,threshold,for,severity}`, and extra rules
are appended verbatim under `prometheusRule.additionalRules`. Metric names in
`expr` are printed from `config.metricsPrefix` directly. The chart README's
"Alerting rules" section documents every knob and the reasoning behind each
rule.

The Grafana dashboards in `dashboards/` get the SAME substitution: the chart
rewrites `kconmon_ng_` to `<config.metricsPrefix>_` in every panel as it renders
them (`templates/observability/dashboards.yaml`), so the shipped JSON keeps the literal
`kconmon_ng_` prefix and needs no hand-editing for a custom prefix.

**There are two different `PrometheusRule` objects once the Console is
running, and neither implies the other.** This one is static: rendered by the
chart, edited in Git, gated on `prometheusRule.enabled`. The other is written by
the Console's alerting reconciler (`console.alerting.enabled`) from rules
operators build in the UI and stored in PostgreSQL. Run both, either, or
neither. The Console's renderer takes the configured prefix as a constructor
argument rather than defaulting to `kconmon_ng`, so rules it emits follow
`config.metricsPrefix` too.

The DASHBOARDS follow it too, and are stated above: the chart rewrites
`kconmon_ng_` to `<config.metricsPrefix>_` as it renders each JSON into its
ConfigMap, so nothing under `dashboards/` needs hand-editing. Every surface the
chart owns therefore tracks the prefix; the only files that keep the literal
`kconmon_ng_` are the sources in the repo, which is what makes the rewrite
possible.

```yaml
- alert: UDPLossHigh
  expr: kconmon_ng_udp_packet_loss_ratio > 0.5
  for: 5m
  labels:
    severity: warning
  annotations:
    summary: High UDP packet loss detected between nodes

- alert: TCPChecksFailing
  expr: >-
    sum by (source_node, destination_node, source_zone, destination_zone)
    (rate(kconmon_ng_tcp_results_total{result="fail"}[5m]))
    /
    sum by (source_node, destination_node, source_zone, destination_zone)
    (rate(kconmon_ng_tcp_results_total[5m])) > 0.05
  for: 5m
  labels:
    severity: warning
  annotations:
    summary: More than 5% of TCP probes on a pair are failing

- alert: PairWentSilent
  expr: >-
    sum by (source_node, destination_node)
    (rate(kconmon_ng_tcp_results_total[1h] offset 5m)) > 0
    unless
    sum by (source_node, destination_node)
    (rate(kconmon_ng_tcp_results_total[5m])) > 0
  for: 10m
  labels:
    severity: warning
  annotations:
    summary: No probe results at all from a pair that was reporting an hour ago

- alert: DNSChecksFailing
  expr: >-
    sum by (source_node, source_zone, host, resolver)
    (rate(kconmon_ng_dns_results_total{result="fail"}[5m]))
    /
    sum by (source_node, source_zone, host, resolver)
    (rate(kconmon_ng_dns_results_total[5m])) > 0.05
  for: 5m
  labels:
    severity: warning
  annotations:
    summary: More than 5% of DNS resolutions for a host are failing

- alert: ExternalChecksFailing
  expr: >-
    sum by (source_node, source_zone, target, target_kind, check_type)
    (rate(kconmon_ng_external_results_total{result="fail"}[5m]))
    /
    sum by (source_node, source_zone, target, target_kind, check_type)
    (rate(kconmon_ng_external_results_total[5m])) > 0.1
  for: 5m
  labels:
    severity: warning
  annotations:
    summary: More than 10% of external checks for a target are failing

- alert: KconmonAgentsMissing
  # Standbys hold no agents by design, so only the lease holder's counts are evidence.
  expr: >-
    (kconmon_ng_controller_expected_agents
    - kconmon_ng_controller_registered_agents > 0)
    and (kconmon_ng_controller_leader == 1)
  for: 10m
  labels:
    severity: warning
  annotations:
    summary: Fewer kconmon-ng agents registered than schedulable nodes

- alert: KconmonControllerDown
  expr: absent(kconmon_ng_controller_leader == 1)
  for: 5m
  labels:
    severity: critical
  annotations:
    summary: No active kconmon-ng controller leader
```

Seven rules. `expr`/`for`/`severity` above are what the chart renders at its
default knob values; the `annotations` are abridged — what ships carries a
templated `summary` and `description` naming the pair, the zones and the
measured value.

**`PairWentSilent` is the only one that fires on an absence, and it exists
because the ratio rules cannot.** A rule like `TCPChecksFailing` divides a
pair's failing probes by that same pair's total, so a link that stops reporting
altogether has neither a numerator nor a denominator: the division produces no
sample at all, the rule stays quiet, and the worst failure — a link nobody is
measuring any more — reads exactly like a link that never fails. It is written
as `unless` rather than `rate(...) == 0` because a series that is no longer
scraped does not go to zero, it ceases to exist, and `A unless B` is a
difference of label sets: everything probed an hour ago, minus everything still
being probed. The grouping is deliberately just `(source_node,
destination_node)` — matching on the four peer labels would read a zone
relabel as one pair disappearing and another appearing, and fire on a rename.
The 1h lookback is also the alert's lifetime: once the silence is an hour old
the offset window empties, the pair leaves the left-hand side and the alert
resolves, so a cluster scaled down on purpose gets one bounded warning while a
node that is gone for good belongs to `KconmonAgentsMissing`. The full
reasoning, including why a rollout does not page anyone, is in the chart
README's "Alerting rules" section.

## Scaling and cardinality

Per-pair, per-protocol measurement is the point of the tool, and it is also the
bill. This is the honest arithmetic, so nobody discovers it from a Prometheus
that stopped fitting in memory.

### What one pair costs

Every directed pair keeps these peer-labelled families
(`internal/metrics/prometheus.go`; each histogram uses the same 13-bucket
scale):

| Families | Kind | Series per directed pair |
| --- | --- | --- |
| `tcp_connect_duration_seconds`, `tcp_total_duration_seconds`, `udp_rtt_seconds`, `icmp_rtt_seconds` | 4 histograms | 64 — each is 13 buckets + `+Inf` + `_sum` + `_count` = 16 |
| `udp_jitter_seconds`, `udp_packet_loss_ratio`, `icmp_packet_loss_ratio` | 3 gauges | 3 |
| `tcp_results_total`, `udp_results_total`, `icmp_results_total` | 3 counters | 3, split further by `result` |

Call it **~70 active series per directed pair** with the default checkers on.
Pairs are ordered — node A probes B *and* B probes A — so N nodes make N×(N−1)
directed pairs:

| Nodes | Directed pairs | Active series at ~70/pair |
| --- | --- | --- |
| 10 | 90 | ~6.3k |
| 50 | 2,450 | ~170k |
| 100 | 9,900 | ~690k |

The MTR families (`mtr_triggered_total`, `mtr_hops`, `mtr_hop_rtt_seconds`)
appear for a pair only after a failed probe triggered a trace. DNS and HTTP
scale differently — hosts × resolvers × nodes and URLs × nodes, linear in N —
and are negligible next to the mesh.

### The proven envelope

**50–100 nodes is the production-proven envelope.** At 100 nodes, budget ~0.7M
active series for kconmon-ng alone and size Prometheus accordingly. Above that
the quadratic growth is unforgiving — 300 nodes is ~6.3M series — and nothing
at that scale has been validated: reducing per-pair cost and probing a sparse
mesh instead of the full N×N are roadmap work, not a config flag today. Do not
plan a 1000-node deployment on these defaults.

### Levers that exist today

- **Disable checkers you do not need** (`config.checkers.<type>.enabled`).
  Each protocol takes its whole per-pair family with it: TCP off saves ~33
  series/pair (it owns two of the four histograms), UDP off ~19, ICMP off ~18.
- **Drop histogram buckets you will never query.** The four histograms are 64
  of the ~70; if a family is only ever used through `_sum`/`_count` or the
  gauges, dropping its `_bucket` series costs you quantiles on that family and
  nothing else. Plain Prometheus:

  ```yaml
  metric_relabel_configs:
    - source_labels: [__name__]
      regex: kconmon_ng_(tcp_connect|tcp_total)_duration_seconds_bucket
      action: drop
  ```

  The chart's `ServiceMonitor` does not expose `metricRelabelings` yet, so with
  the Prometheus Operator this currently means bringing your own
  `ServiceMonitor` in place of `serviceMonitor.enabled`.
- **A longer scrape interval** (`serviceMonitor.interval`) cuts sample ingest
  and query cost, **not** series count — head cardinality stays the same.
- **Shorter retention or downsampling** on the backend bounds history cost;
  it does nothing for active series.

## Self-monitoring

kconmon-ng monitors itself so that degradation of the monitor raises an alert
instead of a silent gap. The controller derives
`kconmon_ng_controller_expected_agents` from its node informer — the number of
schedulable nodes (`spec.unschedulable == false`), each of which should run an
agent. Two default rules cover the failure modes:

- `KconmonAgentsMissing` (warning) fires when registered agents stay below the
  expected count for 10m — agents failing to register or crash-looping.
- `KconmonControllerDown` (critical) fires when no controller reports itself
  leader for 5m — the control plane is down and no other alert would be
  evaluated.

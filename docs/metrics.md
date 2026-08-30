# Metrics and alerting reference

All metric names use the configurable prefix (default `kconmon_ng`). The common
label set for peer metrics — "peer" below — is `source_node`,
`destination_node`, `source_zone`, `destination_zone`.

External checks use a **different** label set — "external" below:
`source_node`, `source_zone`, `target`, `target_kind`, `check_type`. There is
no `destination_node` or `destination_zone`, because the destination is not a
peer: `target` is the operator's NAME for it (never an address), `target_kind`
is the closed set `host|url` and `check_type` is the probe's own type
(`icmp|tcp|udp|dns|http`). `check_type` is what keeps two checks on one target
apart. Everything that is not http collapses to `target_kind="host"`, so
without it an icmp and a tcp check on the same target shared one series and
averaged each other's failures away. **The peer label set was not changed by
the external family**: no external metric reuses it and no peer family gained
a `target` label, so a dashboard or recording rule keyed on peer labels
behaves exactly as it did before v1.6.0.

Every histogram on this page uses the same 13-bucket scale, in seconds:

```
0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0
```

plus the implicit `+Inf`, `_sum` and `_count` — 16 series per histogram.

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

Every metric vector below stays empty until an external probe reports. A
"vec" in Prometheus client terms is a metric family whose series appear only
when a label combination is first written. An empty vec collects nothing, so
an agent with the feature off exposes a `/metrics` that is byte-identical to
the one it exposed before this family existed. That is also why the
`ExternalChecksFailing` rule stays inert instead of firing on an install that
never enabled the feature.

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
denied destination is a visible zero on the results counter, not a
failure rate. `reason=cidr` means the resolved address fell outside
`allowedCidrs` or inside `deniedCidrs`; `resolve` means the name did not
resolve; `disabled` means a spec arrived while `checkers.external.enabled`
was false.

## Agent — Zone aggregates

The zone plane: every peer probe is recorded a second time under only
`source_zone` and `destination_zone` — "zone" below. Where the per-pair
families grow as N×(N−1) directed pairs, this family grows as Z² directed
zone pairs and is what the `agent.metrics.detail: zone-only` scrape mode
keeps (see [Scaling and cardinality](#scaling-and-cardinality)). Each agent
exports its own zone view, so queries aggregate with
`sum by (source_zone, destination_zone)` exactly as they would across nodes.

!!! warning "Chart 2.2.0 still pins agents that do not export this family"
    The zone family comes from the **agent binary**, and agents at appVersion
    2.0.3, the version chart 2.2.0 pins, do not export it. Until the fleet
    runs a newer agent image, the two zone alerts match no series and stay
    silent, the Zone Heatmap dashboard renders empty, and flipping
    `agent.metrics.detail: zone-only` drops the per-pair series with nothing
    replacing them: Prometheus goes dark on the mesh while the console keeps
    working. Upgrade the agent image first, flip the valve second.

| Metric                                        | Type      | Labels          | Description                        |
| --------------------------------------------- | --------- | --------------- | ---------------------------------- |
| `kconmon_ng_zone_tcp_connect_seconds`         | histogram | zone            | TCP connect phase duration         |
| `kconmon_ng_zone_tcp_total_seconds`           | histogram | zone            | Total TCP probe RTT                |
| `kconmon_ng_zone_udp_rtt_seconds`             | histogram | zone            | UDP round-trip time                |
| `kconmon_ng_zone_icmp_rtt_seconds`            | histogram | zone            | ICMP round-trip time               |
| `kconmon_ng_zone_tcp_results_total`           | counter   | zone + `result` | Probe outcomes: `success` / `fail` |
| `kconmon_ng_zone_udp_results_total`           | counter   | zone + `result` | Probe outcomes                     |
| `kconmon_ng_zone_icmp_results_total`          | counter   | zone + `result` | Probe outcomes                     |
| `kconmon_ng_zone_udp_packets_sent_total`      | counter   | zone            | UDP probe packets sent             |
| `kconmon_ng_zone_udp_packets_received_total`  | counter   | zone            | UDP probe packets received back    |
| `kconmon_ng_zone_icmp_packets_sent_total`     | counter   | zone            | ICMP probe packets sent            |
| `kconmon_ng_zone_icmp_packets_received_total` | counter   | zone            | ICMP probe packets received back   |

The histograms use the 13-bucket scale from the top of this page.

Loss is counters here, on purpose: there is no zone loss-ratio gauge.
Averaging the per-pair `*_packet_loss_ratio` gauges into a zone would weight
an idle pair the same as a busy one and report a number no packet ever
experienced. The zone loss ratio worth trusting is packet-weighted:

```promql
(  sum by (source_zone, destination_zone) (rate(kconmon_ng_zone_udp_packets_sent_total[5m]))
 - sum by (source_zone, destination_zone) (rate(kconmon_ng_zone_udp_packets_received_total[5m])))
/  sum by (source_zone, destination_zone) (rate(kconmon_ng_zone_udp_packets_sent_total[5m]))
```

MTR has no zone family, also on purpose: a traceroute is evidence about one
concrete path, and folding hop counts across a zone would describe no path at
all.

## Controller

| Metric                                     | Type    | Labels             | Description                                |
| ------------------------------------------ | ------- | ------------------ | ------------------------------------------ |
| `kconmon_ng_controller_registered_agents`  | gauge   | —                  | Currently registered agents                |
| `kconmon_ng_controller_expected_agents`    | gauge   | —                  | Schedulable nodes expected to run an agent |
| `kconmon_ng_controller_grpc_connections`   | gauge   | —                  | Active gRPC streaming connections          |
| `kconmon_ng_controller_peer_updates_total` | counter | —                  | Peer-list updates broadcast to agents      |
| `kconmon_ng_controller_leader`             | gauge   | —                  | `1` if this instance is the active leader  |
| `kconmon_ng_controller_diagnostics_total`  | counter | `type`, `result`   | On-demand diagnostics dispatched. `type` is the check type (`tcp`/`udp`/`icmp`/`dns`/`http`/`mtr`); `result` is `ok`, `not_found`, `unsupported`, `timeout`, `error` or `undelivered` |
| `kconmon_ng_controller_event_subscribers`  | gauge   | —                  | Open Console `WatchEvents` subscriptions on this replica |
| `kconmon_ng_controller_external_subscribers` | gauge | —                  | Active agent `WatchExternalChecks` subscriptions on this replica |
| `kconmon_ng_controller_external_assignments` | gauge | —                  | Agents with a non-empty continuous external-check assignment |

Both external gauges are unlabelled by design. `external_assignments` counts
**agents, never specs**: a per-agent series would grow with the cluster for no
operational gain.

## Console

The Console exposes its own families under the same prefix, namespaced
`_console_`. What follows is the full current registry, grouped by what each
family watches.

### HTTP, realtime and the ingester

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `kconmon_ng_console_build_info` | gauge | `version`, `commit` | Build info; the value is always 1 |
| `kconmon_ng_console_http_requests_total` | counter | `method`, `path`, `status` | Console HTTP requests |
| `kconmon_ng_console_http_request_duration_seconds` | histogram | `method`, `path` | Request duration |
| `kconmon_ng_console_events_received_total` | counter | `type` | Controller domain events received by this replica's ingester |
| `kconmon_ng_console_events_deduped_total` | counter | — | Live events dropped by the WebSocket hub as duplicates another replica already ingested |
| `kconmon_ng_console_ingester_connected` | gauge | — | 1 while this replica holds an established `WatchEvents` stream to the controller |
| `kconmon_ng_console_ingester_reconnects_total` | counter | `reason` | Reconnect attempts: `dial`, `stream`, `capability` |
| `kconmon_ng_console_ws_clients` | gauge | — | Currently connected WebSocket clients on this replica |
| `kconmon_ng_console_ws_messages_sent_total` | counter | `topic` | Envelopes handed to a client's send buffer |
| `kconmon_ng_console_ws_dropped_clients_total` | counter | — | Clients closed because their send buffer overflowed |
| `kconmon_ng_console_push_snapshots_total` | counter | `topic`, `result` | Server-side snapshot pushes: `ok`, `error` |
| `kconmon_ng_console_ws_topics` | gauge | — | Ephemeral `run:{id}` WebSocket topics currently registered |

### Store and retention

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `kconmon_ng_console_store_queries_total` | counter | `query`, `result` | Database queries by generated query name: `ok`, `conflict`, `error` |
| `kconmon_ng_console_store_query_duration_seconds` | histogram | `query` | Query duration |
| `kconmon_ng_console_store_pool_conns` | gauge | `state` | Connection pool size: `acquired`, `idle`, `total` |
| `kconmon_ng_console_events_persisted_total` | counter | `result` | Controller events written to `topology_events`: `ok`, `conflict`, `error` |
| `kconmon_ng_console_retention_deleted_total` | counter | `table` | Rows deleted by the retention pruner, per swept table |

`retention_deleted_total{table}` deserves a proper introduction, since it is
the only visibility into the pruner. The `table` label is the pruner's sweep
list, a closed set of ten: `topology_events`, `audit_log`, `check_results`,
`check_runs`, `mtr_path_snapshots`, `mtr_hop_enrichment`, `annotations`,
`k8s_events`, `incidents`, `maintenance_windows`. The tenth, `check_results`,
is the highest-volume table the pruner sweeps; earlier docs left it out of
this list, which made a sweep falling behind on the biggest table look idle in
any view built from them. There is no `webhooks` value on purpose: webhook
rows are configuration, not observation, and are never swept.

### Auth, RBAC and rate limits

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `kconmon_ng_console_auth_requests_total` | counter | `mode`, `result` | Authentication attempts: `ok`, `invalid`, `expired`, `error` |
| `kconmon_ng_console_authz_denied_total` | counter | `permission` | Requests denied by the authz policy |
| `kconmon_ng_console_audit_dropped_total` | counter | — | Audit entries dropped because the async write buffer was full |
| `kconmon_ng_console_rate_limited_total` | counter | `limit` | Requests refused with 429: `runs`, `login`, `promql` |
| `kconmon_ng_console_rate_limit_failopen_total` | counter | `limit` | Requests admitted because the KV backend was unreadable (fail-open) |
| `kconmon_ng_console_projection_guard_failopen_total` | counter | — | Definition writes admitted because the topology was unreadable (fail-open) |

The two `failopen` counters exist because the console deliberately fails open
in both places: a Valkey outage must not become a login outage, and a
controller outage must not become a config-write outage. Every admission they
count is a control that did not run, which is exactly why they are counted.

### Runs and the scheduler

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `kconmon_ng_console_runs_total` | counter | `type`, `status` | Diagnostics runs completed: `succeeded`, `partial`, `failed` |
| `kconmon_ng_console_run_pairs_total` | counter | `result` | Run pairs dispatched: `ok`, `failed`, `timeout` |
| `kconmon_ng_console_run_duration_seconds` | histogram | `type` | Run wall-clock duration |
| `kconmon_ng_console_scheduler_ticks_total` | counter | `result` | Schedule loop ticks: `ok`, `not-leader`, `error` |
| `kconmon_ng_console_scheduler_fired_total` | counter | `kind` | Runs started by the loop: `once`, `interval` |
| `kconmon_ng_console_scheduler_skipped_total` | counter | `reason` | Due schedules not fired: `overrun`, `disabled` |
| `kconmon_ng_console_runs_reaped_total` | counter | — | Runs force-finished as cancelled by the stuck-run reaper |

`scheduler_ticks_total{result="not-leader"}` is the **normal** case on every
replica but one (the loop is a singleton on a PostgreSQL advisory lock), so
alerting on it is alerting on correct behavior.

### Continuous external checks

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `kconmon_ng_console_external_series_projected` | gauge | — | Prometheus series the assigned continuous external checks project |
| `kconmon_ng_console_external_reconciles_total` | counter | `result` | Reconcile ticks: `pushed`, `unchanged`, `not-leader`, `error` |
| `kconmon_ng_console_external_specs_skipped_total` | counter | `reason` | Definitions left out of the assignment: `check-type`, `destination-kind`, `unrunnable` |

The skip reasons: `check-type` is a definition whose type cannot be a
continuous external check (udp, mtr; see
[External targets](scenarios/external-targets.md)); `destination-kind` is a
continuous check against cluster nodes, which is the agents' own peer mesh
already; `unrunnable` is the backstop for a definition no agent could parse
(http against a `host` target, dns without `params.query`) written before the
API started refusing those at the door.

### MTR path history and enrichment

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `kconmon_ng_console_mtr_snapshots_total` | counter | `result` | MTR traces projected into path history: `new-path`, `repeat`, `error` |
| `kconmon_ng_console_enrichment_cache_total` | counter | `result` | Hop addresses the TTL cache was asked about: `hit`, `miss` |
| `kconmon_ng_console_enrichment_lookups_total` | counter | `source`, `result` | Source lookups run for cache misses: `rdns\|asn\|city` × `ok\|miss\|error` |

`mtr_snapshots_total{result="new-path"}` is **the route-changed alerting
primitive** — it fires when a pair takes a route it has never taken before,
which is otherwise something an operator notices by diffing two traces by
hand. `repeat` is the steady state (a stable route re-confirmed) and is what
makes `new-path` meaningful: without it a silent projector and a stable
network look identical. `error` counts projections that never landed; a
projection failure never fails the pair (the `check_results` row is the
authority, the snapshot is a projection), so this counter is the only place
it is visible.

Enrichment is **two** counters instead of one, because a cache hit and a
source lookup are not the same event and cannot share a label set: one cached
row answers rdns, asn and city at once, so folding hits into a
`{source,result}` counter would mean attributing a hit to a source that never
ran. `enrichment_cache_total` increments once per requested IP, which makes
`hit/(hit+miss)` the cache hit ratio, the number that says whether
`mtr.enrichment.ttl` is doing its job. `enrichment_lookups_total` increments
once per source that actually ran for a missed IP: `ok` means the source
returned data, `miss` means it ran and knew nothing about the address (no PTR
record, or the address is not in the mmdb; an ordinary answer, not a
failure), `error` means the lookup itself failed. A source switched off in
config, or one whose file failed to open at boot, is never counted at all: a
series pinned at zero would read as "working and finding nothing".

### Kubernetes events and webhooks

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `kconmon_ng_console_k8s_events_total` | counter | `result` | Kubernetes events the reader decided about: `stored`, `duplicate`, `filtered`, `error` |
| `kconmon_ng_console_webhook_deliveries_total` | counter | `result` | Webhook deliveries reaching a terminal decision: `ok`, `failed`, `filtered` |

`k8s_events_total{result="duplicate"}` is the normal outcome of a relist, not
a failure: `kubernetesContext.resyncInterval` forces a periodic list, and
every already-stored row it returns costs one rejected INSERT and one
increment here. `filtered` is the fail-closed drop, a node event with no
topology to vouch for the node, and is the counter to watch if the timeline
looks quiet. A `filtered` rate that tracks the total usually means
`controller.url` is unset, not that the cluster is calm. Events for kinds the
reader does not handle are skipped uncounted, so `filtered` stays readable as
the one thing it means.

`webhook_deliveries_total` counts **one per delivery, never per HTTP
attempt**: a delivery that succeeds on the third rung of the retry ladder
(see [Set up alerting](scenarios/set-up-alerting.md#how-delivery-behaves)) is
one `ok`, not two `failed` and an `ok`. That is what makes
`failed/(ok+failed)` an endpoint-health ratio, not a retry-count
artefact. `filtered` is the steady state of an endpoint that does not
subscribe to the event (the equivalent of `repeat` above), and a disabled
endpoint is not counted at all, since a switched-off endpoint that kept
incrementing a series would read as a working one.

### What the Console never puts in a label

No console metric carries a node name, pod name, namespace, event reason or
message, webhook name or URL, endpoint secret, incident title/scope/notes, an
IP, a hostname, an ASN, an organization, a country, a path hash, a
destination, or an annotation's text. The temptation is real in several
places (a `{node}`/`{reason}` breakdown of cluster events, a `{webhook}`
breakdown of deliveries, per-hop enrichment values that are sitting right
there in the resolved row), and each was rejected for the same two reasons:
unbounded cardinality fed by whatever the cluster (or an operator's keyboard)
decides to emit, and operator-typed strings landing in long-term storage.
Per-endpoint outcome lives on the `webhooks` row, where a bounded
per-endpoint fact belongs. Per-hop RTT already has an agent metric with
`hop_ip` in its label set (`kconmon_ng_mtr_hop_rtt_seconds`); the Console did
not add a second one, which is why the per-hop trend chart in the MTR
Explorer reads snapshot history, not Prometheus.

## Default alerting rules

Deployed when `prometheusRule.enabled: true`. The rules live in the chart
(`charts/kconmon-ng/templates/_rules.tpl`), not in Helm values: each one has an
`enabled` toggle plus its tunable numbers under
`prometheusRule.<alertName>.{enabled,threshold,for,severity}`, and extra rules
are appended verbatim under `prometheusRule.additionalRules`. Metric names in
`expr` are printed from `config.metricsPrefix` directly. The chart README's
"Alerting rules" section documents every knob and the reasoning behind each
rule.

The Grafana dashboards in `dashboards/` get the same substitution: the chart
rewrites `kconmon_ng_` to `<config.metricsPrefix>_` in every panel as it
renders them (`templates/observability/dashboards.yaml`), so the shipped JSON
keeps the literal `kconmon_ng_` prefix and needs no hand-editing for a custom
prefix. Every surface the chart owns tracks the prefix; the only files that
keep the literal `kconmon_ng_` are the sources in the repo, which is what
makes the rewrite possible.

This static bundle is one of **two** rule layers: the Console's alerting
reconciler writes a separate, console-owned `PrometheusRule` from rules built
in the UI, and neither layer implies or touches the other. The two-layer
story, including how not to get paged twice, lives in
[Set up alerting](scenarios/set-up-alerting.md).

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

- alert: ZoneChecksFailing
  expr: >-
    sum by (source_zone, destination_zone)
    (rate({__name__=~"kconmon_ng_zone_(tcp|udp|icmp)_results_total",result="fail"}[5m]))
    /
    sum by (source_zone, destination_zone)
    (rate({__name__=~"kconmon_ng_zone_(tcp|udp|icmp)_results_total"}[5m])) > 0.05
  for: 5m
  labels:
    severity: warning
  annotations:
    summary: More than 5% of all probes between a zone pair are failing

- alert: ZoneLossHigh
  expr: >-
    (sum by (source_zone, destination_zone)
    (rate({__name__=~"kconmon_ng_zone_(udp|icmp)_packets_sent_total"}[5m]))
    -
    sum by (source_zone, destination_zone)
    (rate({__name__=~"kconmon_ng_zone_(udp|icmp)_packets_received_total"}[5m])))
    /
    sum by (source_zone, destination_zone)
    (rate({__name__=~"kconmon_ng_zone_(udp|icmp)_packets_sent_total"}[5m])) > 0.1
  for: 5m
  labels:
    severity: warning
  annotations:
    summary: A zone pair is losing more than 10% of its probe packets

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

Nine rules. `expr`/`for`/`severity` above are what the chart renders at its
default knob values; the `annotations` are abridged; what ships carries a
templated `summary` and `description` naming the pair, the zones and the
measured value.

**`PairWentSilent` is the only one that fires on an absence, and it exists
because the ratio rules cannot.** A rule like `TCPChecksFailing` divides a
pair's failing probes by that same pair's total, so a link that stops
reporting altogether has neither a numerator nor a denominator: the division
produces no sample at all, the rule stays quiet, and the worst failure, a
link nobody is measuring any more, reads exactly like a link that never
fails. It is written as `unless` rather than `rate(...) == 0` because a
series that is no longer scraped does not go to zero, it ceases to exist, and
`A unless B` is a difference of label sets: everything probed an hour ago,
minus everything still being probed. The grouping is just `(source_node,
destination_node)` on purpose: matching on the four peer labels would read a
zone relabel as one pair disappearing and another appearing, and fire on a
rename. The 1h lookback is also the alert's lifetime: once the silence is an
hour old the offset window empties, the pair leaves the left-hand side and
the alert resolves, so a cluster scaled down on purpose gets one bounded
warning while a node that is gone for good belongs to `KconmonAgentsMissing`.
The full reasoning, including why a rollout does not page anyone, is in the
chart README's "Alerting rules" section.

## Scaling and cardinality

Per-pair, per-protocol measurement is the point of the tool, and it is also
the bill. This is the arithmetic, stated up front so nobody discovers it from
a Prometheus that stopped fitting in memory.

### What one pair costs

Every directed pair keeps these peer-labelled families
(`internal/metrics/prometheus.go`; each histogram uses the 13-bucket scale):

| Families | Kind | Series per directed pair |
| --- | --- | --- |
| `tcp_connect_duration_seconds`, `tcp_total_duration_seconds`, `udp_rtt_seconds`, `icmp_rtt_seconds` | 4 histograms | 64 — each is 13 buckets + `+Inf` + `_sum` + `_count` = 16 |
| `udp_jitter_seconds`, `udp_packet_loss_ratio`, `icmp_packet_loss_ratio` | 3 gauges | 3 |
| `tcp_results_total`, `udp_results_total`, `icmp_results_total` | 3 counters | 3, split further by `result` |

Call it **~70 active series per directed pair** with the default checkers on.
Pairs are ordered (node A probes B *and* B probes A), so N nodes make N×(N−1)
directed pairs:

| Nodes | Directed pairs | Active series at ~70/pair |
| --- | --- | --- |
| 10 | 90 | ~6.3k |
| 50 | 2,450 | ~170k |
| 100 | 9,900 | ~690k |

The MTR families (`mtr_triggered_total`, `mtr_hops`, `mtr_hop_rtt_seconds`)
appear for a pair only after a failed probe triggered a trace. DNS and HTTP
scale differently (hosts × resolvers × nodes and URLs × nodes, linear in N)
and are negligible next to the mesh.

### The proven envelope

**50–100 nodes is the production-proven envelope at full detail.** At 100
nodes, budget ~0.7M active series for kconmon-ng alone and size Prometheus
accordingly. Above that the quadratic growth is unforgiving: 300 nodes is
~6.3M series. The valve below cuts what Prometheus keeps by an order of
magnitude by configuration alone; what it cannot change is that the agents
still *probe* the full N×N mesh, and probing a sparse mesh instead is still
roadmap work. Do not plan a 1000-node deployment on these defaults.

### Levers that exist today

- **The zone plane and the valve** (`agent.metrics.detail`). Every peer probe
  is also recorded into the [zone family](#agent-zone-aggregates), which
  grows as Z² zone pairs instead of N² node pairs, and the valve decides at
  scrape time how much of the per-pair detail Prometheus keeps:

  | `agent.metrics.detail` | Per directed pair | What remains |
  | --- | --- | --- |
  | `full` (default) | ~70 series | everything |
  | `counters-only` | ~10 series | drops the four per-pair histograms; gauges and result counters stay, every pair alert keeps firing |
  | `zone-only` | ~0 series | drops every series naming a `destination_node`; the zone family (~74×Z² series) and the linear DNS/HTTP/external families stay |

  At 100 nodes: ~0.7M series at `full`, ~0.1M at `counters-only`, and
  practically N-independent at `zone-only`. The valve renders as
  `metricRelabelings` on the agent `ServiceMonitor`, so it needs
  `serviceMonitor.enabled` (the chart refuses the combination otherwise).
  Plain-Prometheus equivalents:

  ```yaml
  metric_relabel_configs:
    # counters-only: drop the four per-pair histograms.
    - source_labels: [__name__]
      regex: kconmon_ng_(tcp_connect_duration|tcp_total_duration|udp_rtt|icmp_rtt)_seconds_(bucket|sum|count)
      action: drop
    # zone-only instead: a per-pair series is exactly one naming a destination node.
    # - source_labels: [destination_node]
    #   regex: .+
    #   action: drop
  ```

  Remember the version floor from the warning above: flip `zone-only` on a
  fleet of 2.0.3 agents and the per-pair series are dropped with nothing
  replacing them.
- **Disable checkers you do not need** (`config.checkers.<type>.enabled`).
  Each protocol takes its whole per-pair family with it: TCP off saves ~33
  series/pair (it owns two of the four histograms), UDP off ~19, ICMP off ~18.
- **Drop only what you never query.** `counters-only` is the broad version of
  this; for something narrower (one histogram, one protocol) write your own
  `metric_relabel_configs` as above, or bring your own `ServiceMonitor` in
  place of `serviceMonitor.enabled`. Dropping a family's `_bucket` series
  costs you quantiles on that family and nothing else.
- **A longer scrape interval** (`serviceMonitor.interval`) cuts sample ingest
  and query cost, **not** series count; head cardinality stays the same.
- **Shorter retention or downsampling** on the backend bounds history cost;
  it does nothing for active series.

## Self-monitoring

kconmon-ng monitors itself so that degradation of the monitor raises an alert
instead of a silent gap. The controller derives
`kconmon_ng_controller_expected_agents` from its node informer: the number of
schedulable nodes (`spec.unschedulable == false`), each of which should run an
agent. Two default rules cover the failure modes:

- `KconmonAgentsMissing` (warning) fires when registered agents stay below the
  expected count for 10m: agents failing to register or crash-looping.
- `KconmonControllerDown` (critical) fires when no controller reports itself
  leader for 5m: the control plane is down and no other alert would be
  evaluated.

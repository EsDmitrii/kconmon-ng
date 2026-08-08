# Metrics and alerting reference

All metric names use the configurable prefix (default `kconmon_ng`). The common
label set for peer metrics — "peer" below — is `source_node`,
`destination_node`, `source_zone`, `destination_zone`.

External checks (M4) use a **different** label set — "external" below:
`source_node`, `source_zone`, `target`, `target_kind`. There is no
`destination_node` or `destination_zone`, because the destination is not a
peer: `target` is the operator's NAME for it (never an address) and
`target_kind` is the closed set `host|url`. **The peer label set is unchanged
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
| `kconmon_ng_console_rate_limited_total`            | counter | `limit`    | Requests refused with 429, by limit (`runs`, `login`)              |
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

## Default alerting rules

Deployed when `prometheusRule.enabled: true`. The metric prefix in `expr` is
substituted automatically from `config.metricsPrefix`. Additional rules can be
appended under `prometheusRule.rules` in Helm values.

That substitution applies to the **PrometheusRule only**. The Grafana
dashboards in `dashboards/` are imported as plain JSON with `kconmon_ng_`
written out literally and nothing rewrites them, so a non-default
`config.metricsPrefix` means editing the dashboard JSON by hand — the same
caveat `values.yaml` states next to `metricsPrefix`.

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

- alert: ExternalChecksFailing
  expr: rate(kconmon_ng_external_results_total{result="fail"}[5m]) > 0
  for: 5m
  labels:
    severity: warning
  annotations:
    summary: External connectivity checks are failing

- alert: KconmonAgentsMissing
  expr: kconmon_ng_controller_registered_agents < kconmon_ng_controller_expected_agents
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

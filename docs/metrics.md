# Metrics and alerting reference

All metric names use the configurable prefix (default `kconmon_ng`). The common
label set for peer metrics — "peer" below — is `source_node`,
`destination_node`, `source_zone`, `destination_zone`.

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

## Controller

| Metric                                     | Type    | Description                                |
| ------------------------------------------ | ------- | ------------------------------------------ |
| `kconmon_ng_controller_registered_agents`  | gauge   | Currently registered agents                |
| `kconmon_ng_controller_expected_agents`    | gauge   | Schedulable nodes expected to run an agent |
| `kconmon_ng_controller_grpc_connections`   | gauge   | Active gRPC streaming connections          |
| `kconmon_ng_controller_peer_updates_total` | counter | Peer list updates broadcast to agents      |
| `kconmon_ng_controller_leader`             | gauge   | `1` if this instance is the active leader  |

## Default alerting rules

Deployed when `prometheusRule.enabled: true`. The metric prefix in `expr` is
substituted automatically from `config.metricsPrefix`. Additional rules can be
appended under `prometheusRule.rules` in Helm values.

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

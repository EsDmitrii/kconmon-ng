# Metrics and alerting reference

All metric names use the configurable prefix (default `kconmon_ng`). The common
label set for peer metrics ("peer" below) is `source_node`,
`destination_node`, `source_zone`, `destination_zone`.

External checks use a **different** label set ("external" below):
`source_node`, `source_zone`, `target`, `target_kind`, `check_type`. There is
no `destination_node` or `destination_zone`, because the destination is not a
peer: `target` is the operator's NAME for it (never an address), `target_kind`
is the closed set `host|url` and `check_type` is the probe's own type
(`icmp|tcp|dns|http`). `check_type` is what keeps two checks on one target
apart. Everything that is not http collapses to `target_kind="host"`, so
without it an icmp and a tcp check on the same target would share one series
and average each other's failures away. **The two label sets never mix**: no
external metric carries the peer labels and no peer family carries a `target`
label, so a dashboard or recording rule keyed on peer labels never picks up an
external series.

Every histogram on this page uses the same 13-bucket scale, in seconds:

```
0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0
```

plus the implicit `+Inf`, `_sum` and `_count`: 16 series per histogram.

## Agent: TCP

| Metric                                    | Type      | Labels          | Description                        |
| ----------------------------------------- | --------- | --------------- | ---------------------------------- |
| `kconmon_ng_tcp_connect_duration_seconds` | histogram | peer            | TCP connect phase duration         |
| `kconmon_ng_tcp_total_duration_seconds`   | histogram | peer            | Total TCP probe RTT                |
| `kconmon_ng_tcp_results_total`            | counter   | peer + `result` | Probe outcomes: `success` / `fail` |

## Agent: UDP

| Metric                             | Type      | Labels          | Description                  |
| ---------------------------------- | --------- | --------------- | ---------------------------- |
| `kconmon_ng_udp_rtt_seconds`       | histogram | peer            | Mean UDP round-trip time     |
| `kconmon_ng_udp_jitter_seconds`    | gauge     | peer            | Inter-packet delay variation |
| `kconmon_ng_udp_packet_loss_ratio` | gauge     | peer            | Packet loss ratio (0.0–1.0)  |
| `kconmon_ng_udp_results_total`     | counter   | peer + `result` | Probe outcomes               |

## Agent: ICMP

| Metric                              | Type      | Labels          | Description                 |
| ----------------------------------- | --------- | --------------- | --------------------------- |
| `kconmon_ng_icmp_rtt_seconds`       | histogram | peer            | ICMP round-trip time        |
| `kconmon_ng_icmp_packet_loss_ratio` | gauge     | peer            | Packet loss ratio (0.0–1.0) |
| `kconmon_ng_icmp_results_total`     | counter   | peer + `result` | Probe outcomes              |

## Agent: Path MTU

Agents 2.5.0 and newer. Once a minute per peer, a small datagram and a full-size one with
Don't Fragment set go to the peer's UDP echo port; on a loss the agent bisects
the size and confirms a black hole before it reports one. The algorithm and
the direction each series covers are in
[The path MTU plane](concepts/mesh-and-planes.md#the-path-mtu-plane); the
failure itself in [Catch an MTU black hole](scenarios/mtu-black-hole.md).

| Metric                                | Type    | Labels          | Description |
| ------------------------------------- | ------- | --------------- | ----------- |
| `kconmon_ng_pmtu_bytes`               | gauge   | peer            | Largest IP datagram in bytes that crossed the pair on the last path MTU probe; written for ok, reduced and blackhole |
| `kconmon_ng_pmtu_results_total`       | counter | peer + `result` | `result="success"` for ok and reduced, `fail` for a black hole; nothing for `unreachable` |
| `kconmon_ng_pmtu_probe_bytes`         | gauge   | peer            | The size the source probes this pair at: the MTU of its route to the peer (the route's `mtu`, never above the egress device's), or `checkers.pmtu.size`; written with `pmtu_bytes` |
| `kconmon_ng_agent_pmtu_probe_bytes`   | gauge   | `source_node`   | The largest `pmtu_probe_bytes` over this agent's pairs: the one probe-size series `agent.metrics.detail: zone-only` keeps. Compare a pair against its own `pmtu_probe_bytes` |

`unreachable` is the search ending without an MTU verdict: the 64-byte
datagram was lost twice, every size above 64 bytes was lost, the size the
bisection found was lost when sent again, or the time budget (half of
`checkers.pmtu.interval`) ran out before anything above 64 bytes crossed. It
writes neither the counter nor the gauge, so a lossy path reads as a pair
with no path MTU result rather than as a black hole; its loss shows on the
UDP plane. On Cilium the probe size, and with it every gauge on a healthy
path, is the route MTU (1450 with VXLAN), not the pod `eth0`'s 1500. Calico
sets the MTU on the pod's `eth0` instead: with IPIP that is 1480, and the
probe and the gauge read 1480 on every healthy pair.

The probe size is per pair because it comes from the route to each peer: a
hostNetwork or bare-host agent with a VPN route next to a 1500-byte LAN
probes its peers at different sizes. `agent_pmtu_probe_bytes` holds only the
largest of them, so a pair behind the smaller route would read as reduced
against it. `pmtu_probe_bytes` goes when `pmtu_bytes` goes: the peer leaves,
a zone changes, or a reload switches the pmtu plane off. The agent-level
series disappears once the agent has no pmtu pair left. Under
`agent.metrics.detail: zone-only` the per-pair gauge is dropped with every
other series that names a `destination_node`, and the agent-level one is the
probe size that remains.

### Telling a reduced path from a healthy one

A reduced path fails nothing, so no failure ratio shows it. Compare the size
that crossed with the size the source probes at:

    max by (source_node, destination_node) (kconmon_ng_pmtu_bytes)
      < on (source_node, destination_node)
    max by (source_node, destination_node) (kconmon_ng_pmtu_probe_bytes)

That reads the last probe. Behind ECMP the last probe may have crossed at
full size on a good next hop, so the bundled dashboards read the smallest size
in 10 minutes instead, as `PathMTUBlackHole` does:
`min by (source_node, destination_node) (min_over_time(kconmon_ng_pmtu_bytes[10m]))`
in place of the first operand.

A black hole is below the probe size too. To keep only the paths that say
so, drop the pairs with failed probes, as the Overview dashboard's reduced
path tile does:

    (min by (source_node, destination_node) (min_over_time(kconmon_ng_pmtu_bytes[10m]))
      < on (source_node, destination_node)
    max by (source_node, destination_node) (kconmon_ng_pmtu_probe_bytes))
      unless on (source_node, destination_node)
    (sum by (source_node, destination_node) (increase(kconmon_ng_pmtu_results_total{result="fail"}[15m])) > 0)

## Agent: DNS

| Metric                            | Type      | Labels                                           | Description                              |
| --------------------------------- | --------- | ------------------------------------------------ | ---------------------------------------- |
| `kconmon_ng_dns_duration_seconds` | histogram | `host`, `resolver`, `source_node`, `source_zone` | Resolution duration per (host, resolver) |
| `kconmon_ng_dns_results_total`    | counter   | same + `result`                                  | Resolution outcomes                      |

## Agent: HTTP

| Metric                                     | Type      | Labels                                                                 | Description            |
| ------------------------------------------ | --------- | ---------------------------------------------------------------------- | ---------------------- |
| `kconmon_ng_http_dns_duration_seconds`     | histogram | `url`, `source_node`, `source_zone`                                    | DNS phase              |
| `kconmon_ng_http_connect_duration_seconds` | histogram | same                                                                   | TCP connect phase      |
| `kconmon_ng_http_tls_duration_seconds`     | histogram | same                                                                   | TLS handshake phase    |
| `kconmon_ng_http_ttfb_seconds`             | histogram | same                                                                   | Time to first byte     |
| `kconmon_ng_http_total_duration_seconds`   | histogram | same                                                                   | Total request duration |
| `kconmon_ng_http_results_total`            | counter   | `url`, `method`, `status_code`, `source_node`, `source_zone`, `result` | Request outcomes       |

The `url` label is the target URL as configured, except that a password in
its userinfo reads `xxxxx` (`https://probe:xxxxx@example.com/health`); the
agent's `check failed` log and on-demand results mask it the same way, and
so do the config errors about a target's `url` at startup and on a hot
reload. The query string is kept verbatim, so do not put tokens in it.

## Agent: MTR

| Metric                           | Type    | Labels                                                    | Description                    |
| -------------------------------- | ------- | --------------------------------------------------------- | ------------------------------ |
| `kconmon_ng_mtr_triggered_total` | counter | peer                                                      | Number of MTR traces triggered |
| `kconmon_ng_mtr_hops`            | gauge   | peer                                                      | Hop count in the last trace    |
| `kconmon_ng_mtr_hop_rtt_seconds` | gauge   | `source_node`, `destination_node`, `hop_number`, `hop_ip` | Per-hop RTT                    |

## Agent: External

The probes of the Console's continuous external assignment: every external
target an agent checks on its own cadence. Gated on
`config.checkers.external.enabled`, which is **off by default**.

One-shot external diagnostics (`POST /api/v1/diagnostics` with
`destinationKind: external`) record no `kconmon_ng_external_*` series: their
target names are typed ad hoc, and nothing would ever retire a series minted
under one. They are counted by `kconmon_ng_controller_diagnostics_total` and
answered in the response and in the `CheckObserved` event (`MTRCompleted` for
`mtr`). A refused one-shot shows there as `success: false` with the refusal
text.

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

When a target leaves the agent's assignment, every `kconmon_ng_external_*`
series of that target goes at once, counters and histograms included.

## Agent: Zone aggregates

The zone plane: every peer probe is recorded a second time under only
`source_zone` and `destination_zone` ("zone" below). Where the per-pair
families grow as N×(N−1) directed pairs, this family grows as N×Z (one set
per agent and destination zone), linear in N, and is what the `agent.metrics.detail: zone-only` scrape mode
keeps (see [Scaling and cardinality](#scaling-and-cardinality)). Each agent
exports its own zone view, so queries aggregate with
`sum by (source_zone, destination_zone)` exactly as they would across nodes.

!!! warning "The zone family comes from the agent image"
    The zone family is exported by the **agent binary**, from v2.3.0 on; a
    chart pointed at an older agent image gets none of it. Until the fleet
    runs an agent that exports it, the two zone alerts match no series and
    stay silent, the Zone Heatmap dashboard renders empty, and flipping
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
| `kconmon_ng_zone_pmtu_results_total`          | counter   | zone + `result` | Path MTU probe outcomes            |
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

## Agent: plan and self-monitoring

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `kconmon_ng_build_info` | gauge | `version`, `commit` | Build info; the value is always 1. The controller exports it too |
| `kconmon_ng_probe_intended` | gauge | `source_node`, `destination_node` | 1 for every directed pair the topology plan assigns this agent; absent means unplanned, never failing |
| `kconmon_ng_agent_probe_cycle_duration_seconds` | histogram | `checker` | Wall-clock duration of one checker's full round over all peers; buckets 0.05s to 60s |
| `kconmon_ng_agent_probe_cycle_overruns_total` | counter | `checker` | Rounds that took longer than the checker's configured interval |
| `kconmon_ng_agent_controller_reconnects_total` | counter | none | Times the agent lost its controller stream and registered again |
| `kconmon_ng_agent_peer_list_age_seconds` | gauge | none | Seconds since the last peer-list update from the controller (process age until the first one) |
| `kconmon_ng_agent_mtr_reactive_inflight` | gauge | none | Reactive MTR traces running now |
| `kconmon_ng_agent_mtr_reactive_coalesced_total` | counter | `reason` | Failed probes that started no new trace: `cooldown`, `saturated` |

`probe_intended` is the plan made visible: under `topology.mode: sparse` it
names exactly the pairs this agent probes, and `PairWentSilent` joins on it
so that a pair the plan dropped does not page. It costs one series per
directed pair.

### When series go away

A peer that leaves the agent's peer list loses its per-pair gauges
(`udp_packet_loss_ratio`, `udp_jitter_seconds`, `icmp_packet_loss_ratio`,
`pmtu_bytes`, `mtr_hops`, `mtr_hop_rtt_seconds`, `probe_intended`) at once.
Its counters and histograms (`tcp`, `udp`, `icmp` and `pmtu`
`_results_total`, the four per-pair histograms and `mtr_triggered_total`)
are deleted 10 minutes later, so a controller failover, which re-registers
the fleet one agent at a time, does not reset live pairs; a peer back within
those 10 minutes keeps its counters. For 30s after re-registering, an agent
also keeps the peers it had, so the new leader's first peer lists, which name
only the agents already back, do not drop the gauges of the others (see
[High availability](concepts/architecture.md#high-availability)). The zone family is never deleted. When
a node moves to another zone, or a peer does, the pair's series under the
old zone go at once: the per-pair gauges and, on agents 2.5.0 and newer,
the counters and histograms listed above too. The series under the new zone
start right away. A
checker switched off by a config reload drops its gauges at once; its
counters and histograms stop growing and stay until the restart.

## Controller

| Metric                                     | Type    | Labels             | Description                                |
| ------------------------------------------ | ------- | ------------------ | ------------------------------------------ |
| `kconmon_ng_controller_registered_agents`  | gauge   | none                | Currently registered agents, external ones included |
| `kconmon_ng_controller_expected_agents`    | gauge   | none                | Schedulable nodes expected to run an agent |
| `kconmon_ng_controller_external_agents`    | gauge   | none                | Registered agents running outside the cluster (through the external gateway); a subset of `registered_agents`, since 2.4.0 |
| `kconmon_ng_controller_grpc_connections`   | gauge   | none                | Active gRPC streaming connections          |
| `kconmon_ng_controller_peer_updates_total` | counter | none                | Peer-list updates broadcast to agents      |
| `kconmon_ng_controller_leader`             | gauge   | none                | `1` if this instance is the active leader  |
| `kconmon_ng_controller_diagnostics_total`  | counter | `type`, `result`   | On-demand diagnostics dispatched. `type` is the check type (`tcp`/`udp`/`icmp`/`pmtu`/`dns`/`http`/`mtr`); `result` is `ok`, `not_found`, `unsupported`, `timeout`, `error`, `undelivered` or `cancelled` (the caller's own connection to the controller closed first; a CLI interrupted through its port-forward is not seen and counts by its outcome) |
| `kconmon_ng_controller_event_subscribers`  | gauge   | none                | Open Console `WatchEvents` subscriptions on this replica |
| `kconmon_ng_controller_events_published_total` | counter | `type`         | Domain events published to `WatchEvents` subscribers: `topology_changed`, `check_observed`, `mtr_triggered`, `mtr_completed`, `diagnostic_progress` |
| `kconmon_ng_controller_external_subscribers` | gauge | none                | Active agent `WatchExternalChecks` subscriptions on this replica |
| `kconmon_ng_controller_external_assignments` | gauge | none                | Agents with a non-empty continuous external-check assignment |

All three external gauges are unlabelled by design. `external_assignments`
counts **agents, never specs**: a per-agent series would grow with the cluster
for no operational gain. `external_agents` is updated in the same registry
callback as `registered_agents` and zeroed with it when a replica loses the
lease; it exists so that `KconmonAgentsMissing` can subtract bare hosts from
the registered count, which `expected_agents` (schedulable nodes) never
included.

### On a standby replica

A family appears on `/metrics` on its first write, and most of the table is
written only by the leader's work. A replica that has never held the lease
exports `kconmon_ng_build_info`, `expected_agents` (every replica runs the
node informer), `leader` at `0` and the Go runtime and process families, and
nothing else from the table. A replica that lost the lease keeps what it
wrote while leading: `registered_agents`, `external_agents` and
`external_assignments` drop to `0`, the counters keep their totals. A panel
or rule that wants the leader's view joins on
`kconmon_ng_controller_leader == 1`, as `KconmonAgentsMissing` does.

## Console

The Console exposes its own families under the same prefix, namespaced
`_console_`, next to the Go runtime (`go_*`) and process (`process_*`)
families the agent and controller export too. What follows is the full
current registry, grouped by what each family watches.

A family appears on `/metrics` on its first write, not at `0` from startup,
and that includes the ones whose Labels column says `none`: they are vectors
with an empty label list. `ws_topics`, `ws_dropped_clients_total`,
`audit_dropped_total`, `runs_reaped_total` or
`webhook_maintenance_read_errors_total` stay absent until the first run,
drop or error, and a feature that is off never writes its families at all.
`build_info`, set at startup, and `ws_refused_total`, whose three series
exist at `0` from startup, are the exceptions. Read an absent family as zero
(`… or vector(0)`) rather than as a broken scrape.

### HTTP, realtime and the ingester

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `kconmon_ng_console_build_info` | gauge | `version`, `commit` | Build info; the value is always 1 |
| `kconmon_ng_console_http_requests_total` | counter | `method`, `path`, `status` | Console HTTP requests |
| `kconmon_ng_console_http_request_duration_seconds` | histogram | `method`, `path` | Request duration |
| `kconmon_ng_console_events_received_total` | counter | `type` | Controller domain events received by this replica's ingester |
| `kconmon_ng_console_events_deduped_total` | counter | none | Live events dropped by the WebSocket hub as duplicates another replica already ingested |
| `kconmon_ng_console_ingester_connected` | gauge | none | 1 while this replica holds an established `WatchEvents` stream to the controller |
| `kconmon_ng_console_ingester_reconnects_total` | counter | `reason` | Reconnect attempts: `dial`, `stream`, `capability` |
| `kconmon_ng_console_ws_clients` | gauge | none | Currently connected WebSocket clients on this replica |
| `kconmon_ng_console_ws_messages_sent_total` | counter | `topic` | Envelopes handed to a client's send buffer |
| `kconmon_ng_console_ws_dropped_clients_total` | counter | none | Clients closed because their send buffer overflowed |
| `kconmon_ng_console_ws_refused_total` | counter | `limit` | `/ws` connections refused by a `websocket.*` cap: `total`, `address`, `subject`; all three series exist at 0 from startup (since 2.5.0) |
| `kconmon_ng_console_push_snapshots_total` | counter | `topic`, `result` | Server-side snapshot pushes: `ok`, `error` |
| `kconmon_ng_console_ws_topics` | gauge | none | Ephemeral `run:{id}` WebSocket topics currently registered |

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
`k8s_events`, `incidents`, `maintenance_windows`. `check_results` is the
highest-volume table of them, so a sweep falling behind shows there first.
There is no `webhooks` value on purpose: webhook
rows are configuration, not observation, and are never swept.

### Auth, RBAC and rate limits

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `kconmon_ng_console_auth_requests_total` | counter | `mode`, `result` | Authentication attempts: `ok`, `invalid`, `expired`, `error` |
| `kconmon_ng_console_authz_denied_total` | counter | `permission` | Requests denied by the authz policy |
| `kconmon_ng_console_audit_dropped_total` | counter | none | Audit entries dropped: the async write buffer (64 rows) was full, or a row found its share of it used up. Rows of failed requests and of anonymous or credential-less callers wait in an eighth of the buffer of their own on the RBAC, token, user, import, export and audit routes, and use at most half of the rest on other routes; one subject has at most 16 such rows waiting. Other rows stop at three quarters of the rest. What remains is kept for signed-in callers' successful requests on those routes and for successful sign-ins (local and OIDC) and password changes, which also wait up to 2 s for room. Also counts the failed rows of callers with no credential (a 401, a refused OIDC callback, a failed or rate-limited sign-in) past 120 a minute from one client address (/64 for IPv6), which are not stored |
| `kconmon_ng_console_rate_limited_total` | counter | `limit` | Requests refused with 429: `runs`, `login`, `promql` |
| `kconmon_ng_console_rate_limit_failopen_total` | counter | `limit` | Requests admitted because the KV backend was unreadable (fail-open) |
| `kconmon_ng_console_projection_guard_failopen_total` | counter | none | Definition writes admitted because the topology was unreadable (fail-open) |

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
| `kconmon_ng_console_runs_reaped_total` | counter | none | Runs force-finished as cancelled by the stuck-run reaper |
| `kconmon_ng_console_sweep_results_total` | counter | `source_zone`, `destination_zone`, `result` | Topology sweeper probes per zone pair: `ok`, `failed`, `timeout` |

`scheduler_ticks_total{result="not-leader"}` is the **normal** case on every
replica but one (the loop is a singleton on a PostgreSQL advisory lock), so
alerting on it is alerting on correct behavior.

### Continuous external checks

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `kconmon_ng_console_external_series_projected` | gauge | none | Prometheus series the assigned continuous external checks project |
| `kconmon_ng_console_external_reconciles_total` | counter | `result` | Reconcile ticks: `pushed`, `unchanged`, `not-leader`, `error`, `too-large` |
| `kconmon_ng_console_external_specs_skipped_total` | counter | `reason` | Definitions left out of the assignment: `check-type`, `destination-kind`, `unrunnable`, `over-budget` |

The skip reasons: `check-type` is a definition whose type cannot be a
continuous external check (udp, mtr; see
[External targets](scenarios/external-targets.md)); `destination-kind` is a
continuous check against cluster nodes, which is the agents' own peer mesh
already; `unrunnable` is the backstop for a definition no agent could parse
(http against a `host` target, dns without `params.query`) that a console
older than 2.5.0 stored, since the API refuses those at write time;
`over-budget` is a definition left out
so the assignment fits the controller's 8 MiB limit on
`PUT /api/v1/external-checks`. The reconciler sheds whole definitions, newest
first, so one added later never pushes out one already running, and logs one
WARN per skipped definition and reason, as for the other skips. `too-large` is
a tick the controller refused with 413 anyway, logged at ERROR: it refuses the whole assignment, and the agents keep the last one they
accepted until it shrinks.

### MTR path history and enrichment

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `kconmon_ng_console_mtr_snapshots_total` | counter | `result` | MTR traces projected into path history: `new-path`, `repeat`, `error` |
| `kconmon_ng_console_enrichment_cache_total` | counter | `result` | Hop addresses the TTL cache was asked about: `hit`, `miss` |
| `kconmon_ng_console_enrichment_lookups_total` | counter | `source`, `result` | Source lookups run for cache misses: `rdns\|asn\|city` × `ok\|miss\|error` |

`mtr_snapshots_total{result="new-path"}` is **the route-changed alerting
primitive**: it fires when a pair takes a route it has never taken before,
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
| `kconmon_ng_console_webhook_suppressed_total` | counter | `event` | Alert edges a maintenance window held back: `alert.fired`, `alert.resolved` |
| `kconmon_ng_console_webhook_maintenance_read_errors_total` | counter | none | Alert watcher polls whose maintenance-window read failed |

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

`webhook_suppressed_total` counts each edge once, when the window holds it: a
held `alert.fired` delivered after the window closes is not counted again.
The counter is per process, so a console restarted inside a window counts the
edges it still holds once more. `webhook_maintenance_read_errors_total` is the
alert watcher's own failed reads of the maintenance windows, apart from
`store_queries_total{query="ListMaintenanceWindows"}`, which the HTTP API
feeds too. On a failed read new alert edges go out unsuppressed and edges
already held stay held until a read succeeds.

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

- alert: PathMTUBlackHole
  # The value is the smallest path MTU that crossed in the last 10m, not the ratio. The second arm
  # (sustainedThreshold over 30m, at least two losses, one in the last 10m) catches one black-holed
  # ECMP path.
  expr: >-
    min by (source_node, destination_node, source_zone, destination_zone)
    (min_over_time(kconmon_ng_pmtu_bytes[10m]))
    and on (source_node, destination_node, source_zone, destination_zone)
    (
      (
        sum by (source_node, destination_node, source_zone, destination_zone)
        (rate(kconmon_ng_pmtu_results_total{result="fail"}[10m]))
        /
        sum by (source_node, destination_node, source_zone, destination_zone)
        (rate(kconmon_ng_pmtu_results_total[10m]))
        > 0.5
      )
      or
      (
        sum by (source_node, destination_node, source_zone, destination_zone)
        (rate(kconmon_ng_pmtu_results_total{result="fail"}[30m]))
        /
        sum by (source_node, destination_node, source_zone, destination_zone)
        (rate(kconmon_ng_pmtu_results_total[30m]))
        > 0.1
        and on (source_node, destination_node, source_zone, destination_zone)
        sum by (source_node, destination_node, source_zone, destination_zone)
        (increase(kconmon_ng_pmtu_results_total{result="fail"}[30m]))
        >= 2
        and on (source_node, destination_node, source_zone, destination_zone)
        sum by (source_node, destination_node, source_zone, destination_zone)
        (increase(kconmon_ng_pmtu_results_total{result="fail"}[10m]))
        > 0
      )
    )
  for: 5m
  labels:
    severity: warning
  annotations:
    summary: Full-size datagrams on a pair are lost with no ICMP frag-needed

- alert: ZonePathMTUBlackHole
  # The same verdict per zone pair, only where Prometheus holds no per-pair pmtu series
  # (agent.metrics.detail: zone-only); shares the prometheusRule.pathMtuBlackHole knobs.
  expr: >-
    (
      (
        sum by (source_zone, destination_zone) (rate(kconmon_ng_zone_pmtu_results_total{result="fail"}[10m]))
        /
        sum by (source_zone, destination_zone) (rate(kconmon_ng_zone_pmtu_results_total[10m]))
        > 0.5
      )
      or
      (
        sum by (source_zone, destination_zone) (rate(kconmon_ng_zone_pmtu_results_total{result="fail"}[30m]))
        /
        sum by (source_zone, destination_zone) (rate(kconmon_ng_zone_pmtu_results_total[30m]))
        > 0.1
        and on (source_zone, destination_zone)
        sum by (source_zone, destination_zone) (increase(kconmon_ng_zone_pmtu_results_total{result="fail"}[30m]))
        >= 2
        and on (source_zone, destination_zone)
        sum by (source_zone, destination_zone) (increase(kconmon_ng_zone_pmtu_results_total{result="fail"}[10m]))
        > 0
      )
    )
    unless on (source_zone, destination_zone)
    count by (source_zone, destination_zone) (kconmon_ng_pmtu_results_total)
  for: 5m
  labels:
    severity: warning
  annotations:
    summary: Full-size datagrams between a zone pair are lost with no ICMP frag-needed

- alert: NodeUnreachable
  # A pair counts as failing above 0.5 of its TCP probes (fixed); the last clause is minPeers.
  expr: >-
    (
      count by (destination_node, destination_zone) (
        (
          sum by (source_node, destination_node, destination_zone) (rate(kconmon_ng_tcp_results_total{result="fail"}[5m]))
          /
          sum by (source_node, destination_node, destination_zone) (rate(kconmon_ng_tcp_results_total[5m]))
        ) > 0.5
      )
      /
      count by (destination_node, destination_zone) (
        sum by (source_node, destination_node, destination_zone) (rate(kconmon_ng_tcp_results_total[5m])) > 0
      )
    ) > 0.5
    and on (destination_node, destination_zone)
    count by (destination_node, destination_zone) (
      sum by (source_node, destination_node, destination_zone) (rate(kconmon_ng_tcp_results_total[5m])) > 0
    ) >= 2
  for: 5m
  labels:
    severity: critical
  annotations:
    summary: Most peers cannot reach one node over TCP

- alert: NodeIsolated
  # The same, grouped by source_node: one node that cannot reach most of the peers it probes.
  expr: >-
    (
      count by (source_node, source_zone) (
        (
          sum by (source_node, destination_node, source_zone) (rate(kconmon_ng_tcp_results_total{result="fail"}[5m]))
          /
          sum by (source_node, destination_node, source_zone) (rate(kconmon_ng_tcp_results_total[5m]))
        ) > 0.5
      )
      /
      count by (source_node, source_zone) (
        sum by (source_node, destination_node, source_zone) (rate(kconmon_ng_tcp_results_total[5m])) > 0
      )
    ) > 0.5
    and on (source_node, source_zone)
    count by (source_node, source_zone) (
      sum by (source_node, destination_node, source_zone) (rate(kconmon_ng_tcp_results_total[5m])) > 0
    ) >= 2
  for: 5m
  labels:
    severity: critical
  annotations:
    summary: One node cannot reach most of its peers over TCP

- alert: PairWentSilent
  # First half: pairs the plan still assigns. Second half: sources that export no plan at all
  # (an agent older than 2.3.0, or one that stopped being scraped).
  expr: >-
    (
      (
        sum by (source_node, destination_node)
        (rate(kconmon_ng_tcp_results_total[1h] offset 5m)) > 0
        unless
        sum by (source_node, destination_node)
        (rate(kconmon_ng_tcp_results_total[5m])) > 0
      )
      and on (source_node, destination_node)
      (kconmon_ng_probe_intended == 1)
    )
    or
    (
      (
        sum by (source_node, destination_node)
        (rate(kconmon_ng_tcp_results_total[1h] offset 5m)) > 0
        unless
        sum by (source_node, destination_node)
        (rate(kconmon_ng_tcp_results_total[5m])) > 0
      )
      unless on (source_node)
      kconmon_ng_probe_intended
    )
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

# The three protocol families are joined with label_replace/or, not a __name__ regex union:
# rate() drops __name__, so a union collapses the families into duplicate labelsets and the
# whole expression fails evaluation with "vector cannot contain metrics with the same labelset".
- alert: ZoneChecksFailing
  expr: >-
    sum by (source_zone, destination_zone) (
    label_replace(rate(kconmon_ng_zone_tcp_results_total{result="fail"}[5m]), "proto", "tcp", "", "")
    or label_replace(rate(kconmon_ng_zone_udp_results_total{result="fail"}[5m]), "proto", "udp", "", "")
    or label_replace(rate(kconmon_ng_zone_icmp_results_total{result="fail"}[5m]), "proto", "icmp", "", "")
    )
    /
    sum by (source_zone, destination_zone) (
    label_replace(rate(kconmon_ng_zone_tcp_results_total[5m]), "proto", "tcp", "", "")
    or label_replace(rate(kconmon_ng_zone_udp_results_total[5m]), "proto", "udp", "", "")
    or label_replace(rate(kconmon_ng_zone_icmp_results_total[5m]), "proto", "icmp", "", "")
    )
    > 0.05
  for: 5m
  labels:
    severity: warning
  annotations:
    summary: More than 5% of all probes between a zone pair are failing

- alert: ZoneLossHigh
  expr: >-
    (sum by (source_zone, destination_zone) (
    label_replace(rate(kconmon_ng_zone_udp_packets_sent_total[5m]), "proto", "udp", "", "")
    or label_replace(rate(kconmon_ng_zone_icmp_packets_sent_total[5m]), "proto", "icmp", "", "")
    )
    -
    sum by (source_zone, destination_zone) (
    label_replace(rate(kconmon_ng_zone_udp_packets_received_total[5m]), "proto", "udp", "", "")
    or label_replace(rate(kconmon_ng_zone_icmp_packets_received_total[5m]), "proto", "icmp", "", "")
    ))
    /
    sum by (source_zone, destination_zone) (
    label_replace(rate(kconmon_ng_zone_udp_packets_sent_total[5m]), "proto", "udp", "", "")
    or label_replace(rate(kconmon_ng_zone_icmp_packets_sent_total[5m]), "proto", "icmp", "", "")
    )
    > 0.1
  for: 5m
  labels:
    severity: warning
  annotations:
    summary: A zone pair is losing more than 10% of its probe packets

- alert: KconmonAgentsMissing
  # Standbys hold no agents by design, so only the lease holder's counts are evidence.
  # External agents are registered but never expected, so they are subtracted; the
  # `or registered * 0` keeps the rule firing on a controller too old to export the gauge.
  expr: >-
    (kconmon_ng_controller_expected_agents
    - (kconmon_ng_controller_registered_agents
    - (kconmon_ng_controller_external_agents or kconmon_ng_controller_registered_agents * 0)) > 0)
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

- alert: KconmonExternalAgentDown
  # Off by default (prometheusRule.externalAgentDown.enabled): the job exists only with
  # the chart's ScrapeConfig or the plain-Prometheus job from the external-agents page.
  expr: up{job=~".*agent-external.*"} == 0
  for: 5m
  labels:
    severity: warning
  annotations:
    summary: External kconmon-ng agent {{ $labels.node }} is not answering scrapes
```

Fourteen rules, thirteen of them on by default; `KconmonExternalAgentDown` ships off
because its job only exists once external agents are
[scraped](external-agents.md#scraping-external-agents). `expr`/`for`/`severity`
above are what the chart renders at its default knob values; the
`annotations` are abridged; what ships carries a templated `summary` and
`description` naming the pair, the zones and the measured value.

`KconmonAgentsMissing` subtracts `kconmon_ng_controller_external_agents`
from `registered`: `registered` counts external agents while `expected`
(schedulable nodes) does not, so without it one bare host would mask one
missing cluster node. `or registered * 0` stands in for that gauge on a
controller image older than 2.4.0, so the rule keeps firing there rather than
matching nothing. `KconmonExternalAgentDown` reads Prometheus' own `up` for
the external-agent job and carries the SD labels (`node`, `zone`, `instance`)
into its annotations. It also matches a custom
`scrapeConfig.externalAgents.jobName` that lacks `agent-external`; a
hand-written plain-Prometheus job still needs `agent-external` in its name.

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
rename.

The join on `kconmon_ng_probe_intended` (since 2.3.0) keeps the rule to
pairs the plan still assigns. A pair the plan drops, because a node left or
a sparse plan reshuffled, loses its `probe_intended` series at once while its
counters linger for up to 10 minutes and its rate for an hour; without the
join it would page for that hour. The second half covers a source that
exports no plan at all: an agent older than 2.3.0, or an agent that stopped
being scraped, whose `probe_intended` goes stale with it. The 1h lookback is
also the alert's lifetime: once the silence is an hour old the offset window
empties, the pair leaves the left-hand side and the alert resolves, so a node
removed on purpose gets one bounded warning for its own outbound pairs, while
a node that is gone for good belongs to `KconmonAgentsMissing`. The full
reasoning, including why a rollout does not page anyone, is in the chart
README's "Alerting rules" section.

### One alert per node instead of one per pair

A node that stays registered while its peers cannot reach it (a host
firewall, a NetworkPolicy, the node's CNI datapath) fails every pair that
touches it, so one such node on a 100-node cluster raises about 200 pair
alerts next to one `NodeUnreachable`. These inhibit rules let the node-level
alert stand for the pairs it explains:

```yaml
inhibit_rules:
  - source_matchers: [alertname="NodeUnreachable"]
    target_matchers: [alertname=~"TCPChecksFailing|UDPLossHigh|PathMTUBlackHole"]
    equal: [destination_node]
  - source_matchers: [alertname="NodeIsolated"]
    target_matchers: [alertname=~"TCPChecksFailing|UDPLossHigh|PathMTUBlackHole"]
    equal: [source_node]
```

The chart does not configure Alertmanager; paste this into your Alertmanager
configuration (with kube-prometheus-stack: `alertmanager.config.inhibit_rules`).

A node that stops altogether is a different signal, and `NodeUnreachable`
does not page for it. Its agent stops heartbeating, the controller drops it
after `controller.agentTtl` (Helm: `config.controllerAgentTtl`, 30s) and the
peers stop probing it. Their counters towards it freeze after a few failed
probes, so the pair failure ratio sits above 0.5 for about a minute, short of
the rule's five, and then the pairs have no rate at all. The dead node pages as `KconmonAgentsMissing`
after its 10 minutes, and its own outbound pairs as `PairWentSilent`.
`NodeUnreachable` is for the node that stays registered while traffic to it
fails.

## Scaling and cardinality

Per-pair, per-protocol measurement is the point of the tool, and it is also
the bill. This is the arithmetic, stated up front so nobody discovers it from
a Prometheus that stopped fitting in memory.

### What one pair costs

Every directed pair keeps these peer-labelled families
(`internal/metrics/prometheus.go`; each histogram uses the 13-bucket scale):

| Families | Kind | Series per directed pair |
| --- | --- | --- |
| `tcp_connect_duration_seconds`, `tcp_total_duration_seconds`, `udp_rtt_seconds`, `icmp_rtt_seconds` | 4 histograms | 64: each is 13 buckets + `+Inf` + `_sum` + `_count` = 16 |
| `udp_jitter_seconds`, `udp_packet_loss_ratio`, `icmp_packet_loss_ratio` | 3 gauges | 3 |
| `pmtu_bytes`, `pmtu_probe_bytes` | 2 gauges | 2 |
| `probe_intended` | 1 gauge | 1 |
| `tcp_results_total`, `udp_results_total`, `icmp_results_total`, `pmtu_results_total` | 4 counters | 8, two per family by `result` |

That is **78 active series per directed pair** with the default checkers on.
Pairs are ordered
(node A probes B *and* B probes A), so N nodes make N×(N−1) directed pairs:

| Nodes | Directed pairs | Active series at 78/pair |
| --- | --- | --- |
| 10 | 90 | ~7.0k |
| 50 | 2,450 | ~191k |
| 100 | 9,900 | ~772k |

The MTR families (`mtr_triggered_total`, `mtr_hops`, `mtr_hop_rtt_seconds`)
appear for a pair only after a failed probe triggered a trace. DNS and HTTP
scale differently (hosts × resolvers × nodes and URLs × nodes, linear in N)
and are negligible next to the mesh.

### The proven envelope

**50–100 nodes is the production-proven envelope at full detail.** At 100
nodes, budget ~0.77M active series for kconmon-ng alone and size Prometheus
accordingly. Above that the quadratic growth is unforgiving: 300 nodes is
~7.0M series. The valve below cuts what Prometheus keeps by an order of
magnitude by configuration alone; what it cannot change is that the agents
still *probe* the full N×N mesh, which is what `topology.mode: sparse`
(since v2.3.0) trims. Do not plan a 1000-node deployment on these defaults.

### Levers that exist today

- **The zone plane and the valve** (`agent.metrics.detail`). Every peer probe
  is also recorded into the [zone family](#agent-zone-aggregates), which
  grows as N×Z (one set per agent and destination zone) instead of N² node
  pairs, and the valve decides at
  scrape time how much of the per-pair detail Prometheus keeps:

  | `agent.metrics.detail` | Per directed pair | What remains |
  | --- | --- | --- |
  | `full` (default) | 78 series | everything |
  | `counters-only` | 14 series | drops the four per-pair histograms; gauges and result counters stay, every pair alert keeps firing |
  | `zone-only` | 0 series | drops every series naming a `destination_node`; the zone family (76 series per agent and destination zone), the per-agent `agent_pmtu_probe_bytes` gauge and the linear DNS/HTTP/external families stay |

  At 100 nodes: ~0.77M series at `full`, ~0.14M at `counters-only`, and at
  `zone-only` about 30k for the zone family in four zones, linear in N. Each
  agent exports its own view of the zone family (64 histogram series, 8
  result counters, 4 packet counters per destination zone), and Prometheus
  keeps one set per scraped agent. The valve renders as
  `metricRelabelings` on the agent `ServiceMonitor` and, since 2.4.0, on the
  external-agent `ScrapeConfig` as well (one shared template, so a bare host
  never returns detail the valve dropped for the pods); it needs
  `serviceMonitor.enabled` or `scrapeConfig.externalAgents.enabled`, and the
  chart refuses the valve without either. Plain-Prometheus equivalents, which
  the [external-agents scrape job](external-agents.md#plain-prometheus)
  carries verbatim:

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
  fleet of agents older than v2.3.0 and the per-pair series are dropped with
  nothing replacing them.

  Under `zone-only` the per-pair panels of the bundled dashboards go empty.
  The Overview dashboard's *Black-hole pairs (15m)* tile then counts zone
  pairs with a failed probe, from `zone_pmtu_results_total`, instead of
  reading 0.
- **Disable checkers you do not need** (`config.checkers.<type>.enabled`).
  Each protocol takes its whole per-pair family with it once the agents
  restart, which a `helm upgrade` does: TCP off saves 34 series/pair (it
  owns two of the four histograms), UDP off 20, ICMP off 19, path MTU off 4.
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
agent. Two default rules cover the failure modes, and a third, off by
default, watches the hosts outside the cluster:

- `KconmonAgentsMissing` (warning) fires when registered in-cluster agents
  stay below the expected count for 10m: agents failing to register or
  crash-looping. Since 2.4.0 the registered count has
  `kconmon_ng_controller_external_agents` subtracted first, so a bare host
  through the gateway cannot stand in for a missing node.
- `KconmonControllerDown` (critical) fires when no controller reports itself
  leader for 5m: the control plane is down and no other alert would be
  evaluated.
- `KconmonExternalAgentDown` (warning, `prometheusRule.externalAgentDown.enabled`)
  fires when an external agent the controller's SD endpoint lists sits at
  `up == 0` for 5m. The agent still registers, or the target would have left
  the list, so the usual cause is the host firewall or the monitoring
  namespace's egress policy blocking `metricsPort`; on most CNIs that egress
  is NATed to a node IP, so the host must admit the node CIDR.

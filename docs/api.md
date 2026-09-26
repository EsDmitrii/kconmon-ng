# HTTP API reference

## Ports and listeners

Before the endpoint tables, the port map. Knowing which listener serves what
is half of this page:

| Port | Default | Serves | Who talks to it |
| --- | --- | --- | --- |
| `httpPort` | 8080 | the HTTP API below, plus `/metrics` for backward compatibility | Console, `kubectl-kconmon`, curl |
| `metricsPort` | 9091 | `/metrics`, `/healthz`, `/readyz`; on the controller also `GET /api/v1/prometheus/sd`, the external-agent target list | Prometheus (this is the port the chart scrapes, and the one its `ScrapeConfig` reads discovery from) |
| `grpcPort` | 9090 | agent-controller gRPC; on agents, also the UDP probe server | the fleet itself |
| `controller.externalGateway.port` | 9443 | a second gRPC listener: TLS plus a bearer token, for [external agents](external-agents.md) | bare-host agents |

The gateway adds **no HTTP surface**. It is gRPC only, serving the same
services on the same registry as the in-cluster listener; there is nothing on
it to port-scan for.

None of the HTTP endpoints authenticate. The only gate is leader election,
which is availability, not authorization: a standby's registry is empty by
design, so it answers `503` and clients ask another replica. That is exactly
why `/metrics` got a listener of its own: a NetworkPolicy cannot say "this
port, but only these paths", so admitting a scraper to the API port admitted
the scraper's whole namespace to the fleet's control plane. Two listeners make
"let Prometheus in" and "let this caller drive the fleet" two separate
decisions. The one API route on the metrics listener follows the same logic: the external-agent target list is served there precisely
because that is the port the scrape rule opens, so discovery needs no second
hole in the policy. It is read-only and discloses only what a scraper is
about to scrape anyway.

## Agent

| Endpoint          | Method | Description                                           |
| ----------------- | ------ | ----------------------------------------------------- |
| `/healthz`        | GET    | Liveness probe; always `200 ok`                       |
| `/readyz`         | GET    | Readiness probe; `503` until peer watch is confirmed  |
| `/metrics`        | GET    | Prometheus metrics (also on `metricsPort`)            |
| `/api/v1/version` | GET    | `{"version":"…","commit":"…"}`; no capability field   |

## Controller

| Endpoint           | Method | Description                                                              |
| ------------------ | ------ | ------------------------------------------------------------------------ |
| `/healthz`         | GET    | Liveness probe; always `200 ok`                                          |
| `/readyz`          | GET    | Readiness probe; `503` until gRPC server is bound                        |
| `/metrics`         | GET    | Prometheus metrics (also on `metricsPort`)                               |
| `/api/v1/topology`    | GET  | JSON snapshot of all registered agents and cluster nodes with zone info (leader only) |
| `/api/v1/version`     | GET  | Build info plus capability flags; see below                              |
| `/api/v1/diagnostics` | POST | Run a one-shot connectivity check between two nodes (leader only)        |
| `/api/v1/external-checks` | PUT  | Replace the fleet's continuous external-check assignment (leader only) |
| `/api/v1/prometheus/sd` | GET  | Prometheus HTTP SD body listing external agents as scrape targets (leader only; also served on `metricsPort`) |

### `GET /api/v1/version`

```json
{
  "version": "…",
  "commit": "…",
  "capabilities": ["events"],
  "externalAllowedCidrs": ["10.0.0.0/8"]
}
```

`capabilities` advertises what this build *serves*, and the Console
feature-detects on it instead of version-sniffing. The set a controller can
advertise today is exactly one flag: `events`, present when
`controller.events.enabled` is on. `external-checks` is a different animal:
an **agent** capability, asserted at registration and visible per agent in
the topology snapshot, never on this endpoint. `externalAllowedCidrs` echoes
the agent-side allowlist (`config.checkers.external.allowedCidrs`), published
because the Console cannot otherwise know it: a target outside those CIDRs
can never be probed, and that is worth saying at creation time instead of as
a timeout later. Empty when the external checker is off.

### `GET /api/v1/topology`

Leader only; a non-leader returns `503 not the leader`, which the Console and
CLI treat as "ask another replica", since answering from a standby would
report a topology with no agents.

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
      "zone": "us-east-1a",
      "capabilities": ["external-checks", "plane:tcp", "plane:udp", "plane:icmp", "plane:pmtu", "plane:dns", "plane:mtr"],
      "httpPort": 8080,
      "udpPort": 9090,
      "metricsPort": 9091
    },
    {
      "id": "edge-host-01-edge-host-01",
      "nodeName": "edge-host-01",
      "podIP": "203.0.113.10",
      "zone": "external",
      "labels": { "kconmon-ng.io/external": "true" },
      "capabilities": ["external-checks", "plane:tcp", "plane:udp", "plane:icmp", "plane:pmtu", "plane:dns", "plane:mtr"],
      "httpPort": 18080,
      "udpPort": 19090,
      "metricsPort": 19091
    }
  ],
  "timestamp": "2025-01-01T00:00:00Z"
}
```

`httpPort`, `udpPort` and `metricsPort` are the listener ports the agent
reported at registration (2.4.0 and newer): the TCP probe target, the UDP
echo target, and the `/metrics` listener that is never probed but feeds
[scrape discovery](#get-apiv1prometheussd). They are omitted for an agent
that reported none, and a probing peer then falls back to its own configured
ports, which is why a mixed fleet keeps one port set
([External agents](external-agents.md#ports)). `labels` is the agent's own
registration map, verbatim: `kconmon-ng.io/external: "true"` on any agent
running outside a Pod, and `kconmon-ng.io/host-network: "true"` on a 2.4.0
agent image running under `agent.hostNetwork`, whose `podIP` is then the
node's address.

While a sparse topology plan is in force (`topology.mode: sparse`, fleet at or
above `topology.sparse.autoThreshold`), the snapshot also carries `probePlan`:
source node name → the sorted node names it is planned to probe. The field is
**absent** on a full-mesh fleet: absence means "every pair is intended", which
keeps pre-sparse payloads unchanged. A node mapped to an empty list is planned
to probe nobody (the plan's fail-closed state until its agent re-registers).

```json
  "probePlan": {
    "node-1": ["node-2", "node-3"],
    "node-2": ["node-3"],
    "node-3": ["node-1", "node-2"]
  }
```

An [external agent](external-agents.md) appears as an ordinary fleet member
with two tells: its `labels` carry `kconmon-ng.io/external: "true"` (stamped
automatically on any agent running outside a Pod), and `podIP` holds its
advertised address rather than a pod IP (the field name predates bare-host
agents, but the value is always "the address peers probe"). `capabilities`
lists what each agent build advertised at registration; a pre-v1.6.0 agent
sends none. Since 2.4.0 the list also carries `plane:<protocol>` entries
naming the probe planes the agent runs (one per enabled checker, `plane:mtr`
always; `plane:pmtu` since 2.5.0): an agent with no `plane:` entry at all is
older than 2.4.0 and must be read as running every plane, never as running
none. The Console applies
exactly that fail-open rule.

### `GET /api/v1/prometheus/sd`

The Prometheus
[HTTP service discovery](https://prometheus.io/docs/prometheus/latest/configuration/configuration/#http_sd_config)
body for [external agents](external-agents.md#scraping-external-agents): one
target group per agent registered from outside the cluster, so that a bare
host, which no `ServiceMonitor` can select, is still scraped. Mounted on
`httpPort` and, because that is the port the scrape NetworkPolicy opens, on
`metricsPort` too. `GET` only; the mux answers `405` to anything else.

```json
[
  {
    "targets": ["203.0.113.10:19091"],
    "labels": {
      "node": "edge-host-01",
      "zone": "external",
      "external": "true",
      "agent_id": "edge-host-01-edge-host-01"
    }
  }
]
```

Headers are `Content-Type: application/json` and `Cache-Control: no-store`:
every refresh must see the registry as it is now, since a cached body would
outlive an eviction. The rules:

- **One group per external agent**, ordered by node name (agent ID breaks
  ties) and deduplicated by target address, first in that order wins: a
  rolling restart on a host can briefly leave two records at one address,
  which Prometheus would otherwise scrape twice under two label sets. IPv6
  addresses are bracketed (`[fd00::1]:9091`). In-cluster agents never appear.
- **The port** is the agent's reported `metricsPort` (agents 2.4.0 and
  newer). An older agent reports none, and the controller substitutes its own
  `config.metricsPort` as captured at startup, logging
  `metrics port assumed from controller config` once per agent ID (fields
  `agent`, `node`, `address`, `port`), so a host on a non-default port is
  diagnosable instead of silently `up == 0`.
- **The label set is fixed**: `node`, `zone`, `external` (always `"true"`)
  and `agent_id`. The agent's own registration labels are never copied
  through: an agent owns its labels map, and copying it would let a host
  inject arbitrary target labels, `__address__` included, into Prometheus.
- **No external agents** is the literal `[]` with `200`, the same convention
  as every other list this API serves.
- **Leader only**, through the same gate as `/api/v1/topology`: a standby
  answers `503 not the leader` as `text/plain`. Never `200 []`: Prometheus
  reads every `200` as the complete new target list, so an empty body from a
  standby would drop every external target, whereas on a non-200 it keeps
  the list it has, which is exactly right until the next refresh lands on
  the leader. With more than one replica behind the Service, expect
  `prometheus_sd_http_failures_total` to climb on the standby's share of
  refreshes while the targets stay right.
- **Off** (`controller.prometheusSD.enabled: false`, default `true`) means
  `404` on both ports, for operators who would rather not disclose external
  hosts' addresses to everything admitted to `metricsPort`. The chart writes
  the key into the shared ConfigMap only when it is `false`, so a controller
  image older than 2.4.0 never sees it. The trust consequences are in
  [SECURITY.md](https://github.com/EsDmitrii/kconmon-ng/blob/main/SECURITY.md).

### `POST /api/v1/diagnostics`

Runs a single on-demand check from a source node's agent to a destination node
and returns the resulting `CheckResult` verbatim. An agent that reported no
payload gets a `CheckResult` built from the task instead: `type`, `source`,
`destination`, `success`, `error` and a timestamp (the agent's, or the
controller's clock when the agent sent none), so the body is never empty and
the terminal `CheckObserved` or `MTRCompleted` event is published either way.
This is the endpoint the `kubectl-kconmon` plugin drives. Only the controller **leader** serves it (a
non-leader replica returns `503`), because only the leader holds the
authoritative agent registry and their task streams.

Request body:

| Field                | Type   | Required | Description |
| -------------------- | ------ | -------- | ----------- |
| `source`             | string | yes      | Node name whose agent runs the probe |
| `destination`        | string | *        | Node name to probe (`destinationKind=node`), or the target's NAME for an external one |
| `type`               | string | yes      | One of `tcp`, `udp`, `icmp`, `pmtu`, `dns`, `http`, `mtr` |
| `plane`              | string | no       | Traffic plane; only `pod`, the default, is accepted |
| `destinationKind`    | string | no       | `node` (default) or `external` |
| `destinationAddress` | string | *        | Required for `destinationKind=external`; the address to probe |

An optional `?timeout=<seconds>` query parameter caps the dispatch wait. It
defaults to `60` and is capped at `120`; invalid values fall back to `60`.

The negotiated timeout also governs the **response**: this endpoint extends
its own connection write deadline to cover it. Every other route on the
controller's HTTP server keeps the short server-wide 10s write budget, which
is why an `mtr` trace that spends 30s on silent TTLs still gets its answer
delivered here and nowhere else. A client must therefore be prepared to wait
the full negotiated timeout for the first response byte, and must not impose a
shorter whole-request timeout of its own.

**External destinations.** With `destinationKind=external` the
destination is not resolved against the agent registry: `destinationAddress`
carries the address and `destination`, when present, only names it. The name,
never the address, is what published events and metrics report as the
destination, since an address must not become an identifier downstream. The
address is treated as `kind=host` (an address with no scheme is a host; `url`
targets arrive through the Console's stored target objects, not this body),
and the port is left to the check type's own default.

Two gates apply and both are the agent's, not the controller's:

- The **source agent must advertise the `external-checks` capability**. A
  pre-v1.6.0 agent silently ignores the external field, so the controller
  refuses up front with `501` instead of letting the request time out
  mysteriously.
- The source agent's own `checkers.external` allowlist decides whether the
  probe happens. The controller does not consult it and cannot override it:
  the split exists because the socket carries the same bytes the REST routes
  do and must ask the same permission.

Only `tcp`, `icmp` and `mtr` reach an external destination, as the agent
already enforced. `pmtu` probes another agent's UDP echo listener, so
`type=pmtu` with `destinationKind=external` is a `400`, and so are `udp`,
`dns` and `http`, which the agent would refuse.
The body starts `external destinations support tcp, icmp and mtr checks
only; ` and gives the reason: for `udp`, `udp counts replies from the kconmon
probe server, which only another agent runs, so destinationKind must be
node`; for `dns` and `http`, `<type> is not a one-off external check; use
destinationKind node, or a continuous external check for an external
resolver or URL`.

A `plane` other than `pod` or empty is a `400` too, `plane must be "pod":
agents probe the pod network only`. `kubectl kconmon check --plane` keeps the flag for scripts, accepts only
`pod` and exits 1 on anything else without contacting the controller.

Every check also needs a source agent that runs that probe type. An agent
that lists its planes without `plane:<type>` gets a `501` instead of a task
it would refuse, for peer and external destinations alike: `agent on node
<source> does not run <type> probes (checkers.<type>.enabled=false)`. For
`pmtu` the reason reads `older than 2.5.0 or checkers.pmtu.enabled=false`;
`http` is off in the default chart, so an `http` check on a default install
gets this `501`. An agent that lists no planes at all is dispatched as
before. The `501` is counted as `result="unsupported"`, a Console run or a
scheduled check of that type ends as an error rather than a failed probe,
and `kubectl kconmon check` exits 1 on it with `controller returned HTTP 501:
...` instead of printing FAIL and exiting 2.

Status codes:

| Code  | Meaning                                                                       |
| ----- | ----------------------------------------------------------------------------- |
| `200` | Check dispatched and completed; body is the `CheckResult` JSON                |
| `400` | Malformed JSON, missing `source`/`destination`/`type`, an invalid `type`, a `type` other than `tcp`, `icmp` or `mtr` with `destinationKind=external`, or a `plane` other than `pod` |
| `413` | The body is over 64 KiB (`request body exceeds 65536 bytes`) |
| `404` | No agent registered on the source/destination node, or no active task stream  |
| `501` | The source agent does not run probes of this `type` (it lists its planes without `plane:<type>`), or `destinationKind=external` and it does not advertise `external-checks` |
| `502` | The dispatch failed for a reason other than timeout or a missing task stream  |
| `503` | This replica is not the leader, or it lost leadership while the check was in flight (`leadership lost`); retry as for `not the leader` |
| `504` | The check did not complete before the timeout                                 |

A caller whose own connection to the controller closes before the answer (a
cancelled Console run, an HTTP client that gives up) gets no response, and
the attempt is counted as `result="cancelled"` in
`kconmon_ng_controller_diagnostics_total` rather than as an error. Ctrl-C in
`kubectl kconmon` is not one of them: the CLI reaches the controller through
a Kubernetes port-forward, which keeps the pod-side connection open after the
CLI exits, so the controller never sees it go and counts the check by its
outcome, usually `result="ok"`. A check that ran but whose answer could not be written is
counted as `result="undelivered"`, logged at ERROR, and followed by a
`DiagnosticProgress` event with `state: undelivered` after its
`CheckObserved` or `MTRCompleted`.

A `200` only means the check *ran*; inspect `success` to see whether it
passed. Durations are serialized as integer nanoseconds (Go `time.Duration`).

ICMP example (`{"source":"node-1","destination":"node-2","type":"icmp"}`):

```json
{
  "type": "icmp",
  "success": true,
  "source": "node-1",
  "destination": "node-2",
  "sourceZone": "us-east-1a",
  "destZone": "us-east-1b",
  "duration": 1520000,
  "details": {
    "rtt": 2100000,
    "lossRatio": 0
  }
}
```

MTR example (`{"source":"node-1","destination":"node-2","type":"mtr"}`):

```json
{
  "type": "mtr",
  "success": true,
  "source": "node-1",
  "destination": "node-2",
  "sourceZone": "us-east-1a",
  "destZone": "us-east-1b",
  "duration": 8300000,
  "details": {
    "target": "10.244.0.12",
    "hops": [
      { "number": 1, "ip": "10.244.0.1", "rtt": 480000, "lossRatio": 0 },
      { "number": 2, "ip": "", "rtt": 0, "lossRatio": 1 },
      { "number": 3, "ip": "10.244.0.12", "rtt": 2100000, "lossRatio": 0 }
    ]
  }
}
```

### `PUT /api/v1/external-checks`

Replaces the fleet's **continuous** external-check assignment. This is the
endpoint the Console's reconciler drives; you can also call it directly.
Leader only, for the same registry reason as diagnostics.

The body carries the *whole* desired state, keyed by agent ID: an absolute
assignment, never a delta, mirroring how the assignment is fanned out to
agents (`ExternalCheckAssignment` is a complete replacement too):

```json
{
  "agents": {
    "node-1-kconmon-ng-agent-xxxxx": [
      {
        "definitionId": "…",
        "target": { "name": "corp-dns", "kind": "host", "address": "10.20.0.53", "port": 0 },
        "checkType": "dns",
        "intervalNs": 30000000000,
        "timeoutNs": 5000000000,
        "params": { "query": "example.internal" }
      }
    ]
  }
}
```

`checkType` must be one of `tcp`, `icmp`, `dns`, `http`. `udp`, `pmtu` and
`mtr` are refused with `400`. The UDP and path MTU probes speak a peer-to-peer
protocol against another agent's probe server, which no external host runs,
and a continuous MTR
against an internet destination is a traffic and cardinality decision
(`mtr_hop_rtt_seconds` is labelled by `hop_ip`, unbounded for internet paths).
One-shot MTR to a target still works through diagnostics above. `port: 0`
means the check type's own default.

Status codes: `503` when not the leader; `400` for invalid JSON, an invalid
`checkType`, or unparseable `params`; `413` for a body over 8 MiB (about
30,000 specs); a bad spec fails the **whole**
body, which is why the Console's reconciler filters ineligible definitions
before calling. It also leaves out whole definitions, newest first, when the
body would pass 8 MiB
(`kconmon_ng_console_external_specs_skipped_total{reason="over-budget"}`). Agent IDs the registry does not know are *not* an error:
the Console's topology view can legitimately lag the registry. An unknown
agent that already holds an assignment gets this PUT's specs, so an agent
evicted for a moment keeps probing on the current list; one named with an
empty list is cleared, like one the PUT leaves out. An ID that holds nothing
is not recorded. Unknown IDs are logged with a warning and reported back
instead of blocking every other agent's assignment. The `200` response says what
happened:

```json
{ "agents": 3, "changed": 1, "unknown": ["node-9-kconmon-ng-agent-zzzzz"] }
```

`agents` is how many agents ended up with a non-empty assignment, `changed`
how many were actually pushed (0 on a retried identical PUT), `unknown` the
IDs the registry did not know, whether or not their specs were stored.

## Console run planning (`POST /api/v1/runs`)

Diagnostics *runs* (many pairs, optionally repeated over a duration) are a
**Console** endpoint, documented in the
[Console API reference](reference/console-api.md) with the object model in
[Checks, runs and schedules](concepts/checks-runs-schedules.md). Two pieces of
its behavior are worth knowing from the controller side, because the
controller's dispatch limits shape them.

**Cadence.** A duration run re-probes each pair on a cadence derived from the
duration (`duration / 500`, floored at 5s). That base cadence is not a field
an operator can set, so a check type too slow to keep it is re-planned, never
refused: the effective interval is the base cadence stretched to one round's
floor. Only `mtr` and `pmtu` stretch, and both are planned around their
per-pair timeout: a traceroute walks up to 30 hops in sequence (90s floor),
and a `pmtu` probe of a black-holed pair waits out a datagram timeout per lost
datagram, up to about 10s (15s floor). Every other type is planned as if it
answered in milliseconds, its per-pair timeout only bounding a probe that has
already failed. One round's floor also counts the fan-out: 90 mtr pairs are 12 batches of 90s, so
a large run can stretch past its own duration and settle at a single full
pass. Rounds repeat until the duration elapses, back to back but no more often
than the base cadence; a round slower than the remaining time is not cut
short, it finishes and the run ends there. `MaxSamplesPerPair` (500) is the
true upper bound on rounds. `plannedSampleIntervalNs` and
`plannedSamplesPerPair`, returned on `POST /api/v1/runs` and snapshotted onto
the run's `spec` (both omitted for instant runs), are a worst-case floor, not
a target: read `plannedSamplesPerPair` as "at least N per pair, more when
probes run fast". Nothing is refused for cadence reasons (every shape yields
at least one sample per pair), though a spec that cannot expand at all is
still rejected `422` (`too many pairs`, `no pairs to check`, `run duration out
of range`).

**Failover.** A run in flight when the controller leader changes loses the
pairs dispatched into the takeover window. The new leader starts with an empty
agent registry and agents re-register over the following seconds (roughly 15s
end to end: lease acquisition plus the agents' own reconnect backoff), so
pairs dispatched in that window come back as `503`, `404` or a dispatch
timeout depending on how far they got. Those pairs are recorded as failed with
the reason the controller gave, the run reaches a terminal status normally,
and its summary says it is partial rather than quietly coming up short. There
is no repair path and none is wanted: re-run it once the topology page shows
every node again.

# HTTP API reference

## Ports and listeners

Before the endpoint tables, the port map. Knowing which listener serves what
is half of this page:

| Port | Default | Serves | Who talks to it |
| --- | --- | --- | --- |
| `httpPort` | 8080 | the HTTP API below, plus `/metrics` for backward compatibility | Console, `kubectl-kconmon`, curl |
| `metricsPort` | 9091 | `/metrics`, `/healthz`, `/readyz` and nothing else | Prometheus (this is the port the chart scrapes) |
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
decisions.

## Agent

| Endpoint          | Method | Description                                           |
| ----------------- | ------ | ----------------------------------------------------- |
| `/healthz`        | GET    | Liveness probe — always `200 ok`                      |
| `/readyz`         | GET    | Readiness probe — `503` until peer watch is confirmed |
| `/metrics`        | GET    | Prometheus metrics (also on `metricsPort`)            |
| `/api/v1/version` | GET    | `{"version":"…","commit":"…"}` — no capability field  |

## Controller

| Endpoint           | Method | Description                                                              |
| ------------------ | ------ | ------------------------------------------------------------------------ |
| `/healthz`         | GET    | Liveness probe — always `200 ok`                                         |
| `/readyz`          | GET    | Readiness probe — `503` until gRPC server is bound                       |
| `/metrics`         | GET    | Prometheus metrics (also on `metricsPort`)                               |
| `/api/v1/topology`    | GET  | JSON snapshot of all registered agents and cluster nodes with zone info (leader only) |
| `/api/v1/version`     | GET  | Build info plus capability flags; see below                              |
| `/api/v1/diagnostics` | POST | Run a one-shot connectivity check between two nodes (leader only)        |
| `/api/v1/external-checks` | PUT  | Replace the fleet's continuous external-check assignment (leader only) |

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
      "capabilities": ["external-checks"]
    },
    {
      "id": "edge-host-01-edge-host-01",
      "nodeName": "edge-host-01",
      "podIP": "203.0.113.10",
      "zone": "external",
      "labels": { "kconmon-ng.io/external": "true" },
      "capabilities": ["external-checks"]
    }
  ],
  "timestamp": "2025-01-01T00:00:00Z"
}
```

An [external agent](external-agents.md) appears as an ordinary fleet member
with two tells: its `labels` carry `kconmon-ng.io/external: "true"` (stamped
automatically on any agent running outside a Pod), and `podIP` holds its
advertised address rather than a pod IP (the field name predates bare-host
agents, but the value is always "the address peers probe"). `capabilities`
lists what each agent build advertised at registration; a pre-v1.6.0 agent
sends none.

### `POST /api/v1/diagnostics`

Runs a single on-demand check from a source node's agent to a destination node
and returns the resulting `CheckResult` verbatim. This is the endpoint the
`kubectl-kconmon` plugin drives. Only the controller **leader** serves it (a
non-leader replica returns `503`), because only the leader holds the
authoritative agent registry and their task streams.

Request body:

| Field                | Type   | Required | Description |
| -------------------- | ------ | -------- | ----------- |
| `source`             | string | yes      | Node name whose agent runs the probe |
| `destination`        | string | *        | Node name to probe (`destinationKind=node`), or the target's NAME for an external one |
| `type`               | string | yes      | One of `tcp`, `udp`, `icmp`, `dns`, `http`, `mtr` |
| `plane`              | string | no       | Traffic plane; defaults to `pod` |
| `destinationKind`    | string | no       | `node` (default) or `external` — added in v1.6.0 |
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

**External destinations (v1.6.0).** With `destinationKind=external` the
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

Status codes:

| Code  | Meaning                                                                       |
| ----- | ----------------------------------------------------------------------------- |
| `200` | Check dispatched and completed; body is the `CheckResult` JSON                |
| `400` | Malformed JSON, missing `source`/`destination`/`type`, or an invalid `type`   |
| `404` | No agent registered on the source/destination node, or no active task stream  |
| `501` | `destinationKind=external` and the source agent does not advertise `external-checks` |
| `502` | The dispatch failed for a reason other than timeout or a missing task stream  |
| `503` | This replica is not the leader                                                |
| `504` | The check did not complete before the timeout                                 |

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

`checkType` must be one of `tcp`, `icmp`, `dns`, `http`. `udp` and `mtr` are
refused with `400`. UDP's probe is a peer-to-peer protocol against another
agent's probe server, which no external host runs, and a continuous MTR
against an internet destination is a traffic and cardinality decision
(`mtr_hop_rtt_seconds` is labelled by `hop_ip`, unbounded for internet paths).
One-shot MTR to a target still works through diagnostics above. `port: 0`
means the check type's own default.

Status codes: `503` when not the leader; `400` for invalid JSON, an invalid
`checkType`, or unparseable `params`; a bad spec fails the **whole**
body, which is why the Console's reconciler filters ineligible definitions
before calling. Agent IDs the registry does not know are *not* an error:
the Console's topology view can legitimately lag the registry, so they are
skipped with a warning and reported back instead of blocking every other
agent's assignment. The `200` response says what happened:

```json
{ "agents": 3, "changed": 1, "unknown": ["node-9-kconmon-ng-agent-zzzzz"] }
```

`agents` is how many agents ended up with a non-empty assignment, `changed`
how many were actually pushed (0 on a retried identical PUT), `unknown` the
IDs that were dropped.

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
floor. Only `mtr` stretches: a traceroute walks up to 30 hops in sequence and
is budgeted at 90s per pair, while every other type answers in milliseconds
and its per-pair timeout only bounds a probe that has already failed. One
round's floor also counts the fan-out: 90 mtr pairs are 12 batches of 90s, so
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

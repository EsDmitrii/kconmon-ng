# HTTP API reference

Both binaries expose a small HTTP API next to their Prometheus metrics.

## Agent

| Endpoint          | Method | Description                                           |
| ----------------- | ------ | ----------------------------------------------------- |
| `/healthz`        | GET    | Liveness probe — always `200 ok`                      |
| `/readyz`         | GET    | Readiness probe — `503` until peer watch is confirmed |
| `/metrics`        | GET    | Prometheus metrics                                    |
| `/api/v1/version` | GET    | `{"version":"…","commit":"…"}`                        |

## Controller

| Endpoint           | Method | Description                                                              |
| ------------------ | ------ | ------------------------------------------------------------------------ |
| `/healthz`         | GET    | Liveness probe — always `200 ok`                                         |
| `/readyz`          | GET    | Readiness probe — `503` until gRPC server is bound                       |
| `/metrics`         | GET    | Prometheus metrics                                                       |
| `/api/v1/topology`    | GET  | JSON snapshot of all registered agents and cluster nodes with zone info (leader only) |
| `/api/v1/version`     | GET  | `{"version":"…","commit":"…","capabilities":["events"],"externalAllowedCidrs":["10.0.0.0/8"]}` — `capabilities` advertises what this build serves (the console feature-detects on it) and `externalAllowedCidrs` is the agent-side allowlist, empty when the external checker is off |
| `/api/v1/diagnostics` | POST | Run a one-shot connectivity check between two nodes (leader only)        |
| `/api/v1/external-checks` | PUT  | Replace the fleet's external-check set (leader only)                     |

Example topology response:

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
      "zone": "us-east-1a"
    },
    {
      "id": "node-2-kconmon-ng-agent-yyyyy",
      "nodeName": "node-2",
      "podIP": "10.0.0.2",
      "zone": "us-east-1b"
    }
  ],
  "timestamp": "2025-01-01T00:00:00Z"
}
```

### `POST /api/v1/diagnostics`

Runs a single on-demand check from a source node's agent to a destination node
and returns the resulting `CheckResult` verbatim. This is the endpoint the
`kubectl-kconmon` plugin drives. Only the controller **leader** serves it — a
non-leader replica returns `503` — because only the leader holds the
authoritative agent registry and their task streams.

Request body:

| Field         | Type   | Required | Description                                            |
| ------------- | ------ | -------- | ------------------------------------------------------ |
| `source`                | string | yes | Node name whose agent runs the probe                                     |
| `destination`           | string | *   | Node name to probe (`destinationKind=node`), or the target's NAME for an external one |
| `type`                  | string | yes | One of `tcp`, `udp`, `icmp`, `dns`, `http`, `mtr`                        |
| `plane`                 | string | no  | Traffic plane; defaults to `pod`                                         |
| `destinationKind`       | string | no  | `node` (default) or `external` — added in v1.6.0                         |
| `destinationAddress`    | string | *   | Required for `destinationKind=external`; the address to probe            |

An optional `?timeout=<seconds>` query parameter caps the dispatch wait. It
defaults to `60` and is capped at `120`; invalid values fall back to `60`.

The negotiated timeout also governs the **response**: this endpoint extends its
own connection write deadline to cover it. Every other route on the controller's
HTTP server keeps the short server-wide 10s write budget, which is why an `mtr`
trace that spends 30s on silent TTLs still gets its answer delivered here and
nowhere else. A client must therefore be prepared to wait the full negotiated
timeout for the first response byte, and must not impose a shorter whole-request
timeout of its own.

**External destinations (v1.6.0).** With `destinationKind=external` the
destination is not resolved against the agent registry: `destinationAddress`
carries the address and `destination`, when present, only names it. The name —
never the address — is what published events and metrics report as the
destination, since an address must not become an identifier downstream. The
address is treated as `kind=host` (an address with no scheme is a host; `url`
targets arrive through the Console's stored target objects, not this body),
and the port is left to the check type's own default.

Two gates apply and both are the agent's, not the controller's:

- The **source agent must advertise the `external-checks` capability**. A
  pre-v1.6.0 agent silently ignores the external field, so the controller
  refuses up front with `501` rather than letting the request time out
  mysteriously.
- The source agent's own `checkers.external` allowlist decides whether the
  probe happens. The controller does not consult it and cannot override it —
  the split exists because the socket carries the same bytes the REST routes do and must ask the
  same permission.

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

A `200` only means the check *ran* — inspect `success` to see whether it passed.
Durations are serialized as integer nanoseconds (Go `time.Duration`).

#### Duration runs and the effective cadence

A duration run re-probes each pair on a cadence derived from the duration
(`duration / 500`, floored at 5s). That base cadence is not a field an operator
can set, so a check type too slow to keep it is **re-planned, never refused**:
the effective interval is the base cadence stretched to one round's floor.

Only `mtr` stretches. A traceroute walks up to 30 hops in sequence and is
budgeted at 90s per pair; every other type answers in milliseconds and its
per-pair timeout only bounds a probe that has already failed, so planning around
it would slow healthy runs down for nothing. One round's floor also counts the
fan-out — 90 mtr pairs are 12 batches of 90s — so a large run can stretch past
its own duration and settle at a single honest pass.

A duration run runs for the wall clock it asked for. Rounds repeat until the
duration elapses: the next round starts when the previous one finishes, but no
more often than the base cadence. A round slower than the remaining time is not
cut short — it finishes and the run ends there — so a duration shorter than one
round is one honest pass. `MaxSamplesPerPair` (500) is the true upper bound on
rounds.

`plannedSampleIntervalNs` and `plannedSamplesPerPair` are returned on
`POST /api/v1/runs` and snapshotted onto the run's `spec` (both omitted for
instant runs). They are a worst-case **floor**, not a target: read
`plannedSamplesPerPair` as "at least N per pair, more when probes run fast".
A 90-pair mtr run planned at one sample will happily take twenty if each round
completes in seconds.

Nothing is refused for cadence reasons: every shape yields at least one sample
per pair. A run is still rejected `422` for a spec that cannot expand at all
(`too many pairs`, `no pairs to check`, `run duration out of range`).

#### Controller failover during a run

A Console diagnostics run in flight when the controller leader changes loses the
pairs dispatched into the takeover window. The new leader starts with an empty
agent registry and agents re-register over the following seconds (roughly 15s
end to end: lease acquisition plus the agents' own reconnect backoff), so pairs
dispatched in that window come back as `503`, `404` or a dispatch timeout
depending on how far they got. Those pairs are recorded as failed with the
reason the controller gave, the run reaches a terminal status normally, and its
summary is honestly partial rather than silently short. There is no repair path
and none is wanted: re-run it once the topology page shows every node again.

Example — ICMP (`{"source":"node-1","destination":"node-2","type":"icmp"}`):

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

Example — MTR (`{"source":"node-1","destination":"node-2","type":"mtr"}`):

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

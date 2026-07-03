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
| `/api/v1/topology`    | GET  | JSON snapshot of all registered agents and cluster nodes with zone info |
| `/api/v1/version`     | GET  | `{"version":"…","commit":"…"}`                                          |
| `/api/v1/diagnostics` | POST | Run a one-shot connectivity check between two nodes (leader only)        |

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
| `source`      | string | yes      | Node name whose agent runs the probe                   |
| `destination` | string | yes      | Node name to probe                                     |
| `type`        | string | yes      | One of `tcp`, `udp`, `icmp`, `dns`, `http`, `mtr`      |
| `plane`       | string | no       | Traffic plane; defaults to `pod`                       |

An optional `?timeout=<seconds>` query parameter caps the dispatch wait. It
defaults to `60` and is capped at `120`; invalid values fall back to `60`.

Status codes:

| Code  | Meaning                                                                       |
| ----- | ----------------------------------------------------------------------------- |
| `200` | Check dispatched and completed; body is the `CheckResult` JSON                |
| `400` | Malformed JSON, missing `source`/`destination`/`type`, or an invalid `type`   |
| `404` | No agent registered on the source/destination node, or no active task stream  |
| `502` | The dispatch failed for a reason other than timeout or a missing task stream  |
| `503` | This replica is not the leader                                                |
| `504` | The check did not complete before the timeout                                 |

A `200` only means the check *ran* — inspect `success` to see whether it passed.
Durations are serialized as integer nanoseconds (Go `time.Duration`).

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

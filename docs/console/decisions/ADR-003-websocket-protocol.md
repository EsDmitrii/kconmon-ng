# ADR-003 — Multiplexed WebSocket protocol

- Status: accepted
- Date: 2026-07-14
- Deciders: @EsDmitrii

## Context

The Console live surface (event feed, matrix deltas, run progress, topology
changes, MTR) needs low-latency push to the browser. Opening a socket per
feature would multiply connections and auth/resume logic. Some environments
block WebSocket upgrades (DESIGN.md §7.1, §8).

## Decision

We will use a **single multiplexed WebSocket** per client at `/ws`
(authenticated). Clients subscribe to topics (`matrix:tcp:pod`, `run:{id}`,
`topology`, `live`, `mtr`). Messages are
`{"topic","type":"snapshot|delta|event","seq","data"}` with 30s ping/pong.
On (re)connect the client resumes by last-seen `seq` per topic — replayed from
Valkey or served a fresh snapshot. Where WebSocket is unavailable we fall back
to SSE, and where the controller cannot stream events we degrade to 15s
Prometheus polling with a "delayed data" badge, feature-detected via capability
flags (§9.4).

## Consequences

### Positive

- One connection, one auth handshake, one resume mechanism for all realtime.
- Per-topic sequence numbers make reconnect deterministic and gap-free.
- Graceful degradation keeps the UI honest under constrained networks.

### Negative / trade-offs

- A topic registry and per-topic sequencing add server-side bookkeeping.
- Multiplexing requires careful backpressure handling (coalesce ≤1
  update/pair/5s, §12).

### Follow-ups

- Protocol lands in M2; document exact message schemas in
  `architecture/WEBSOCKET.md` as they solidify.

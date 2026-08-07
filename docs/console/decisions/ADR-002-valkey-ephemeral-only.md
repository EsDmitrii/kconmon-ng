# ADR-002 — Valkey is ephemeral only

- Status: accepted
- Date: 2026-07-14
- Deciders: @EsDmitrii

## Context

Console runs multiple replicas and needs cross-replica fan-out for realtime
delivery, a short-TTL cache for matrix/topology snapshots and PromQL responses,
session storage with instant revocation, and coordination primitives
(scheduler singleton ticks, per-user rate limits, a background job queue)
(DESIGN.md §5.3). These are latency- and coordination-oriented, not a system
of record.

## Decision

We will use Valkey (via `rueidis`) strictly as an **ephemeral** layer:
pub/sub (`events:*`), live caches, the PromQL response cache, sessions
(`sess:{id}`), and locks/queues. A Valkey flush must lose **zero** durable
data — anything that must survive lives in PostgreSQL (ADR-001).

Valkey is optional: bundled single-replica in Helm, external, or disabled.
When disabled, Console runs single-replica with documented in-process
equivalents (in-memory cache, local locks, no cross-replica pub/sub).

## Consequences

### Positive

- Clear durability boundary: losing Valkey degrades performance/liveness, never
  correctness of stored state.
- Enables horizontal scaling of the WebSocket tier without a message broker.
- Optional bundling keeps small deployments simple.

### Negative / trade-offs

- Features that assume pub/sub (push matrix, live feed) degrade to polling or
  single-replica when Valkey is disabled — must be feature-detected and badged.
- Session-in-Valkey means a flush logs users out (acceptable; not data loss).

### Follow-ups

- Document the single-replica degradation path precisely when M2 lands pub/sub.

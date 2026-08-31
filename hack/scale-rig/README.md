# scale-rig — control-plane saturation measurements (roadmap M10-4)

One process, one machine: a **real controller** (`internal/controller`, wired exactly like
`cmd/controller` — `config.DefaultConfig()` + `controller.New` + `Run`, leader election off,
plaintext localhost listeners on free ports) against **N real agent gRPC clients**
(`internal/agent.GRPCClient`: real dial, `Register`, `WatchPeers`, `StartHeartbeat` at the agent's
production 5s interval, and the same disconnect→re-register→re-subscribe recovery loop
`internal/agent.Run` uses). No probe schedulers, no probe sockets: the rig measures the CONTROL
PLANE — registration storm, FULL_SYNC fan-out, broadcast coalescing under churn, heartbeat load —
not the data plane.

## Run

```
go run ./hack/scale-rig -n 1000
```

One N per process (so peak RSS and heap describe that N alone). Defaults implement the roadmap
scenarios; every window is a flag (see `-help`). A run takes ~2.5 minutes:

1. **cold start** — N agents register over 10s (`-cold`), each starting its watch + heartbeat loops;
2. **rolling churn** — 10% of the fleet restarts over 30s (`-churn-frac`, `-churn`): graceful
   `Deregister`, transport closed, re-`Register` under a new pod identity (new pod name ⇒ new agent
   ID, same node), exactly what a DaemonSet rollout produces — 2 registry changes per event;
3. **steady heartbeats** — 60s (`-steady`) with no topology changes;
4. **propagation probes** — 40 sequential registrations spaced 300ms (`-probes`,
   `-probe-spacing` > the 200ms coalescing window), measuring *registration → EVERY watcher received
   the updated list*.

## How the numbers are obtained (and what they honestly mean)

- **changes** — counted by the rig itself: successful `Register` + `Deregister` RPCs it drove. The
  registry is unexported; the driver's own count is exact.
- **broadcasts** — two *observer* streams (raw `WatchPeers` subscribers outside the fleet) count
  post-initial receives; the max of the two is the flush count. The same received messages give the
  real **FULL_SYNC wire size** (`proto.Size` of the received update = its marshaled size; the narrow
  `peerToProto` projection: id, nodeName, podIp, zone) and the flush→receive delivery delay (one
  process, one clock). `coalesce` = changes : broadcasts — the M9 trailing-edge window at work.
- **propagation p95** — probes register one at a time into a stable fleet, so every watcher's peer
  list length crosses a strictly increasing threshold; a watcher proves receipt by an O(1) length
  check in its `OnPeersUpdate` callback (no per-peer scanning that would distort CPU). The clock
  starts before the `Register` RPC and stops when the last of the N watchers saw the grown list, so
  the number *includes* the intended ≤200ms coalescing delay. Incomplete probes are reported, never
  dropped.
- **CPU (rusage) and heap** — `getrusage(RUSAGE_SELF)` deltas per phase and `runtime.MemStats`
  after a boundary GC. The controller and the N thin clients share the process by design
  ("in-process"), so CPU/heap are **process-wide upper bounds on the controller's cost**; the client
  side is N recv loops + heartbeat tickers, no probing. Peak RSS likewise covers both sides plus
  ~N×2 localhost TCP endpoints.
- **queued** — cross-check scraped from the controller's real `/metrics` listener
  (`kconmon_ng_controller_peer_updates_total` delta = per-watcher updates queued server-side).
- **warn/error log lines** — a counting `slog` handler taps the real log stream, so server-side
  desync warnings ("peer update could not be queued…") and heartbeat failures surface in the report
  with counts instead of scrolling by.

Phases are separated by a quiesce wait (no flushes/callbacks/registrations for 1.2s, capped 30s),
so one scenario's tail does not leak into the next; a phase that never settles is flagged in the
report.

## Tests

`go test -race ./hack/scale-rig/...` — unit tests for the tracker/percentile/identity properties
plus `TestRigSmoke`, which runs the full rig (real controller, real clients) at N=6 with shortened
windows under the race detector.

<!--
Status: current
Owner: @EsDmitrii
Source: extracted from root DESIGN.md §8 in M0 (2026-07-14); rewritten from the
as-built M2 implementation (2026-08-02); `run:{id}` section added from the
as-built M3 implementation (2026-08-06): internal/console/ws/hub.go,
internal/console/checks/runner.go.
This document is the source of truth for WebSocket Protocol. Update it (and the ADRs) in the same PR as any deviation.
-->

# WebSocket Protocol

Delivered in M2, extended with `run:{id}` in M3. One multiplexed socket per browser tab at **`GET /ws`** — a
top-level route, deliberately not under `/api/v1` (ADR-003). Server side:
`internal/console/ws` (`Hub`, `ServeWS`), fed by `internal/console/events`
(controller gRPC stream → `cache.Bus`) and `internal/console/push` (server-side
snapshot timers). Browser side: `web/src/lib/ws.ts` (`WsClient`), one
module-level singleton shared by every consumer.

Every constant below was read out of the code, not remembered. If you change one,
change this table in the same PR.

## Envelope (server → client)

```json
{"topic": "live", "type": "event", "seq": 42, "data": {"id": "17-1753400000000000000", "…": "…"}}
```

| Field | Meaning |
| ----- | ------- |
| `topic` | One of the allowlisted topics below, or a `run:{id}` ephemeral topic; on an `error` frame it is echoed back verbatim from the client's request |
| `type`  | `snapshot` \| `delta` \| `event` \| `error` \| `closed` (M3, `run:{id}` only — see "Ephemeral `run:{id}` topics" below) |
| `seq`   | Per-topic monotonic counter assigned by the Hub as it sends (see Sequence semantics). Error frames carry `0` and do not advance the counter — they are not data |
| `data`  | Topic-specific JSON (see Payloads); for `error`, `{"error": "<detail>"}` |

Go: `ws.Envelope`. TypeScript: `WsEnvelope<T>` in `web/src/lib/ws.ts` —
hand-mirrored, no codegen (the repo-wide convention, see
[API.md](API.md) "Deferred: OpenAPI + codegen").

The console emits `snapshot`, `event`, `error` and `closed` (`ws.TypeClosed`,
M3's `run:{id}` terminal control frame, see below). **No `delta` frames are
produced**: matrix and topology updates are always full snapshots. `delta`
stays in the type union because coalesced per-pair deltas are the documented
scale path (SECURITY.md §12, ≤1 update/pair/5s).

## Client → server

```json
{"action": "subscribe",   "topic": "matrix:tcp:pod", "lastSeq": 41}
{"action": "unsubscribe", "topic": "matrix:tcp:pod"}
```

`subscribe` and `unsubscribe` are the only actions; anything else gets an error
frame naming the two. Client frames are tiny by design, so the read limit is
4 KiB — a larger frame closes the connection rather than allocating. A malformed
JSON frame is answered with an error and the socket stays up: one bad frame must
not cost a browser the other N-1 topics it multiplexes.

A topic outside the allowlist gets an `{"topic": "<requested>", "type": "error",
…}` reply and is **not** subscribed. It is never a silent no-op, because a
silently-ignored subscribe looks exactly like a healthy-but-idle topic from the
browser.

## Topics

| Topic | Frame types | `data` payload | Produced by | Browser consumer |
| ----- | ----------- | -------------- | ----------- | ---------------------- |
| `live` | `event` | `LiveEvent` | controller `EventStream.WatchEvents` → `events.Ingester` → bus → Hub | Live page (`web/src/pages/live.tsx`) |
| `topology` | `snapshot` | same body as `GET /api/v1/topology` | `push.TopologyPusher` (15 s timer + nudge) | **none** — `useTopology` still polls REST every 15 s |
| `matrix:tcp:pod` | `snapshot` | same body as `GET /api/v1/matrix?protocol=tcp&plane=pod` | `push.MatrixPusher` (15 s timer + nudge) | `useMatrix` |
| `matrix:udp:pod` | `snapshot` | as above, `protocol=udp` | `push.MatrixPusher` | `useMatrix` |
| `matrix:icmp:pod` | `snapshot` | as above, `protocol=icmp` | `push.MatrixPusher` | `useMatrix` |
| `run:{id}` | `event` (progress), `closed` (terminal) | `RunProgressFrame` / `RunFinishedFrame`, then an empty `{}` `closed` control frame | `checks.Runner` (M3), one specific run's execution | Run detail / permalink page (`useRun`, task-24) |

The `topology` topic is served but unconsumed: the pusher runs and the frames go
out, and the Topology page was left on its M1 polling path. Wiring it is a
frontend change with no protocol work behind it.

The `:pod` suffix is part of the topic *name* only. `matrix.Compute` has no plane
parameter — `plane=pod` is the only plane that exists (API.md), so the suffix
reserves room in the topic namespace rather than selecting anything.

`mtr`, named by ADR-003, is still **deferred**: no consumer until the M5 MTR
Explorer. MTR activity is not invisible in the meantime — `mtr_triggered` /
`mtr_completed` events flow through `live` like everything else.

## Ephemeral `run:{id}` topics (M3)

Unlike the allowlisted topics above, `run:{id}` topics are **opened and
closed at runtime**, one per diagnostics run (`ws.RunTopic(runID) =
"run:" + runID`), by `checks.Runner.Start` calling `Hub.OpenTopic` and
`Hub.CloseTopic`/`CloseTopicWithFinal` — never present in the static
allowlist a `subscribe` is checked against.

- **Registry bounds**: at most **256** ephemeral topics open at once
  (`maxEphemeralTopics`) — past the cap, `OpenTopic` evicts the oldest
  *closed* topic to make room, or returns `false` if none is closed yet.
  Refusing to open a topic never refuses to run the check itself: a run
  still executes and `GET /api/v1/runs/{id}` still reports it, the browser
  just gets no live socket for it.
- **Replay ring**: **64** frames per `run:{id}` topic (`runRingSize`) — small
  by design, since a run's progress frames are bounded by its pair count
  (`checks.maxPairs = 400`) and a reconnecting browser mostly cares about the
  terminal state, not the full history.
- **Reap delay**: a closed topic keeps serving replay for **5 minutes**
  (`reapDelay`) after `CloseTopic`, checked every 30s (`reapInterval`), so a
  browser reconnecting just after a run finished still sees the terminal
  frame. After that window the registry entry, its seq counter and its ring
  are freed.
- **Terminal contract: a `finished` event frame, then exactly one `closed`
  control frame.** `CloseTopicWithFinal` publishes the run's finished-run
  summary (`RunFinishedFrame`) and the `TypeClosed` control frame
  (`json.RawMessage("{}")`) with the finished frame strictly ordered ahead of
  `closed` in per-topic `seq` for every subscriber — a client that treats
  `closed` as "nothing more is coming, and I have already seen everything
  that matters" is correct by construction, never a race.

### The single-replica asymmetry, and its honest fallback

Unlike `live` (which fans out over the Valkey bus to every replica) and
unlike the snapshot topics (which every replica computes independently),
**`run:{id}` is bus-carried but effectively single-replica**, because
exactly one console replica — whichever one answered
`POST /api/v1/runs` — executes that run and calls `Hub.OpenTopic` for it
(Decision 14). `checks.Runner.publishFrame` publishes progress frames
through the shared `cache.Bus`, but only the executing replica's own `Hub`
ever registered the topic in its ephemeral registry; a different replica's
`Hub` has no entry for it at all.

A browser whose `subscribe` for `run:{id}` lands on a **non-runner
replica** — a real possibility under a load balancer with more than one
console replica — gets the same M2 "topic not in the allowlist" `error`
frame an unknown topic name would produce, and is **not** subscribed. This
is the documented, honest degradation: the frontend's fallback is exactly
what `useRun` already does for the "no realtime capability" and "socket
never opens" cases — **REST-poll `GET /api/v1/runs/{id}` every 5 seconds
until the run reaches a terminal status.** REST polling is not a fallback
of last resort here, it is the correctness backstop: a client that never
receives a single progress frame still reaches the run's final state
through polling alone.

**Progress-frame lossiness under bus overload is bounded and logged, not
silent.** Before closing a run's topic, the runner waits (`relayWaitTimeout
= 2s`) for the hub's per-topic `seq` to catch up to the number of frames it
published, polling every 2ms — the in-process bus drops frames when a
subscriber's buffer overflows (the same documented lossiness the "What the
envelope sequence cannot tell you" section above describes for `live`), and
a dropped frame never reaches the hub to advance `seq`. After the 2s
deadline the close proceeds regardless, and a WARN log records how many
frames were published versus actually relayed
(`"some progress frames were dropped by the bus"`). The close is
deliberately never held open indefinitely waiting for a slow bus — the
terminal `closed` frame is a stronger guarantee (published, not raced) than
any individual progress frame.

The push interval is 15 s for both pushers (`pushInterval` in
`cmd/console/main.go`), matching the frontend's `MATRIX_POLL_MS` /
`TOPOLOGY_POLL_MS`, so a browser on the socket never sees staler data than one on
the polling path. It is **not operator-tunable** in M2. The one latency case that
matters is covered out of band: `push.RunNudgeRelay` subscribes to the `live` bus
topic and nudges both pushers on `topology_changed` (and only on that — the
high-volume `check_observed` stream must never drive snapshot recomputes). Nudges
are coalescing and non-blocking.

## Sequence semantics

`seq` is **per topic, per replica, per process** — a counter the Hub increments
under its own lock as it hands a frame out, starting at 1. It is not the
controller's sequence: `pb.Event.Seq` travels inside the `live` payload as the
`LiveEvent` field `seq`, and the two numbers are unrelated.

Per-topic `seq` is the authoritative **order**. It is not a licence to drop.
Sends happen outside the hub lock, so delivery is **exactly-once but not
ordered**: a `Broadcast` racing a `subscribe` can enqueue its newer-seq frame
ahead of the replay's older ones, and two concurrent same-topic broadcasts can
arrive inverted. (Task 13's pushers each run one goroutine per topic, which is
what keeps same-topic broadcasts serialized in practice — an assumption, not an
invariant the Hub enforces.) The consumer rule therefore differs by topic class,
and `web/src/lib/ws.ts` implements exactly this:

- **Snapshot topics** (`topology`, `matrix:*`): every frame is the whole state.
  Keep only the highest seq seen on the current connection and discard lower —
  an inverted pair would otherwise leave the OLDER state rendered until the next
  push.
- **`live`**: frames are an append-only **set**, not successive states. Dedupe by
  `LiveEvent.id` and insert in order; **never discard an unseen lower seq**. A
  broadcast racing the replay can deliver seq 6 before replayed 1..5, and
  dropping those five would lose event rows permanently.

Consequence to be honest about: after a console restart, or when a reconnecting
client is load-balanced onto a different replica, the counter it sees restarts or
jumps. `lastSeq` is a best-effort hint, never a contract. The browser's
"delivered" watermark is deliberately reset per connection for that reason; its
resume cursor is a max, never a last-write.

## What the envelope sequence cannot tell you

`Envelope.seq` is **gapless by construction**, so it is not a loss detector. Both
bus implementations shed load by dropping messages *before* they reach the Hub
(`InProcessBus` when a subscriber channel is full — buffer 32; `ValkeyBus` under
Valkey's own client-output limits), and the Hub numbers only what it actually
sends. A dropped live event therefore produces a perfectly gapless envelope
sequence. It is not detectable server-side either: the subscriber channel carries
no "something was dropped" signal.

The real loss signal is the **controller-assigned `LiveEvent.seq` inside the
payload**, which only a consumer that decodes the payload can inspect. The Live
page does exactly that (`countMissedEvents`), and the accounting is worth
understanding before trusting the number:

- It walks a copy sorted **by seq**, never display order. Display order is
  timestamp-primary, and observations from different agents legitimately arrive
  time-shuffled — walking the displayed array would read that shuffle as holes.
- A hole and a **restarted controller counter** are told apart by direction, not
  magnitude: at an era boundary the lower-seq side carries the *newer* timestamp,
  because the counter went back to 1 while time kept moving. Inside one era, seq
  and time climb together. There is no size threshold, so a genuine hole is
  reported however large it is.
- The page separately counts events **it discarded itself** (a hidden tab gets no
  animation frames, so its arrival queue is trimmed in slabs at 2× the ring cap).
  Those leave no hole — they truncate the bottom of the range — so they are added
  to the same notice rather than vanishing quietly.
- Residual, accepted: a time-shuffled pair straddling a real gap boundary reads
  as a restart and is skipped. The detector under-counts; it never false-alarms.

## Replay, and its cross-replica limits

Each `Hub` keeps a bounded in-memory ring per topic — `live`: 200 entries; every
other topic: 1. One entry is enough for a snapshot topic because every
`topology`/`matrix:*` frame is a complete state that fully supersedes its
predecessor. On `subscribe` the Hub replays ring entries with `seq > lastSeq`
before any frame this subscribe would otherwise miss, atomically with the
subscription itself. That atomicity buys exactly-once, not ordering — a
concurrent `Broadcast` can still reach the client ahead of the replay, which is
why the consumer rules above exist.

The client only sends `lastSeq` for `live`. **Snapshot topics deliberately
subscribe without one** (Decision 6): their ring holds a single entry — the
current whole state — which a reconnecting tab always wants. Sending the sticky
cursor would let one failover onto a lower-seq replica trip the `seq > lastSeq`
filter, and because the cursor only ever grows, that suppression would repeat on
every later reconnect, leaving the page up to a full push interval stale each
time. Re-receiving a state we already hold costs nothing.

The ring lives in **that replica's memory only**. There is no Valkey-backed replay
log — that has not changed since M2. So, for the **socket's own** resume
mechanism (`lastSeq`, replayed from the ring):

- Reconnecting to the **same** replica within 200 `live` events loses nothing.
- Landing on a **different** replica, or on a restarted one, yields a fresh start:
  snapshot topics immediately re-send a full current snapshot (nothing lost),
  while `live` resumes from now. Hub seq counters are per-replica, so there is no
  cross-replica gap-fill via the socket itself — any events in the hole are
  gone from `lastSeq`'s perspective, permanently.

This is the same "acceptable, documented degradation" posture as the
Valkey-disabled case (ADR-002), not an oversight — the socket protocol was
never meant to be the durable record. **`GET /api/v1/events` (history for
Live scrollback), backed by `topology_events` in PostgreSQL, landed in M3**
(ADR-001; API.md "Implemented in M3") and is exactly the escape hatch this
gap always pointed at: the Live page's own scrollback UI reads it directly
rather than relying on the socket's in-memory ring to have kept anything,
so a hole in the *live* delivery no longer means the event is gone from the
console entirely — only that the socket itself will not hand it back on
reconnect.

## The deliberate asymmetry: `live` crosses replicas, snapshots do not

```
controller (leader only)
  └─ EventStream.WatchEvents (gRPC server-stream, pb.Event, controller-assigned Seq)
       └─ console replica N: events.Ingester (capability precheck + dial + reconnect)
            └─ cache.Bus.Publish("live", …)
                 ├─ ValkeyBus → PUBSUB channel "events:live" → EVERY replica (incl. self)
                 └─ InProcessBus → this replica only
                      └─ ws.Hub (subscribed to bus topic "live")
                           └─ dedupe by LiveEvent.ID → per-topic seq → Envelope → browser
```

**Every** replica runs its own `Ingester`, and the controller's subscriber map
serves them all. With two console replicas the same `pb.Event` is therefore
ingested twice and published to Valkey twice, arriving at every replica twice.
The Hub de-duplicates the `live` topic by `LiveEvent.ID` (`"<seq>-<unixNano>"`,
built only from controller-assigned values, so it is identical on every replica)
through a bounded 512-entry insertion-ordered set. Redundant ingestion is the
*feature*: a replica whose own gRPC stream is down — reconnecting, or talking to
a non-leader — still serves events another replica ingested.

Snapshot topics work the opposite way. `topology` and `matrix:*` are
**local-only**: the pushers call `hub.Broadcast(...)` directly and never touch the
bus. Each replica computes its own snapshots on its own timer. Routing them
through the bus would make N replicas each publish a full snapshot to all clients
N times, for no gain — every replica can already compute the same answer from
Prometheus and the controller's HTTP API.

## The ingester

`events.Ingester` (one per replica) loops forever: capability precheck → dial →
consume → back off → repeat, on the repo's standard 1 s doubling to a 15 s ceiling
(the shape in `internal/agent/agent.go`'s `WatchTasks`). The backoff is not reset
after a successful connection; a stream that flaps is better served by a calm
retry rate.

- **Feature detection before every dial**, including every reconnect: the
  controller's own `GET /api/v1/version` must advertise `"events"`. A controller
  with events off, or a pre-M2 controller, is a supported deployment, so that
  case logs at **Info**. Every other precheck failure — unreachable controller,
  5xx, DNS, missing HTTP client — logs at **Warn**, because it is a real problem.
  Both share the `reason="capability"` metric label; the label is bounded on
  purpose and the log level is what separates them.
- **`Healthy()` is not "the stream object exists".** A Go server-streaming client
  returns before the server has accepted the stream, so a non-leader controller
  answering `codes.Unavailable` would otherwise flap the console's advertised
  capability on every retry. The ingester reports healthy at the first real proof:
  **the first event received, or 2 s of grace survived without the server hanging
  up**, whichever comes first. It demotes the moment the attempt ends.
- One case the grace cannot catch: a blackholed connection (peer gone without a
  FIN) satisfies the grace and reads healthy until the gRPC keepalive
  (10 s + 5 s) declares it dead.
- A single unconvertible or unmarshalable event is logged and dropped; it never
  tears down the stream.

`ValkeyBus`'s own `receiveLoop` is a **backstop, not the reconnect path** —
rueidis retries transient connection loss internally inside `Receive`, and the
loop's 1 s→15 s backoff only covers what rueidis declines to retry plus benign
subscription-end returns. Its owner must call `Close()`: cancelling the context
stops the loop but does **not** release the rueidis client. `cmd/console` does
this last, after every publisher has stopped.

If Valkey cannot be dialled at startup the console falls back to `InProcessBus`
and logs a warning, rather than refusing to boot — and that fallback lasts until
the process restarts, since only the initial dial can fail.

## Liveness, backpressure, limits

| Knob | Value | Why |
| ---- | ----- | --- |
| Ping interval | 30 s | ADR-003; keeps idle proxies from reaping the socket |
| Pong wait | 60 s | Two missed pings ends the connection |
| Write deadline | 10 s | A stuck TCP write must not pin a client goroutine |
| Read limit | 4 KiB | Clients only ever send subscribe/unsubscribe frames |
| Per-client send buffer | 256 frames | On overflow the client is **dropped and closed** — the hub never blocks on a slow browser |
| Upgrader buffers | 1024 read / 4096 write | Frames out are much larger than frames in |
| `live` dedupe set | 512 IDs | Bounded, oldest-out; covers multi-replica duplicates comfortably |
| `live` replay ring | 200 | Per replica, in memory |
| Snapshot replay ring | 1 | A whole state supersedes its predecessor |
| Bus subscriber buffer | 32 | `InProcessBus`; a full channel drops the message for that subscriber (logged) |
| Client reconnect backoff | 1 s → 15 s doubling | `web/src/lib/ws.ts`, same shape as every other reconnect loop in the repo |

Dropping a slow client is deliberate: the alternative — an unbounded queue — turns
one wedged browser into console-wide memory growth. The client's own reconnect
plus snapshot resend makes a drop cheap and self-healing, and it is counted
(`_ws_dropped_clients_total`).

## Origin check

`CheckOrigin` accepts a request when the `Origin` header host matches the request
host (same origin), and when `Origin` is **absent** — non-browser clients
(`websocat`, tests, probes) do not send one, and they are not subject to the
ambient-credential problem `Origin` exists to solve. Everything else is refused
before the upgrade (gorilla answers 403). Browsers cannot be talked out of sending
`Origin`, so this is the socket's CSRF story and the direct analogue of the
"same-origin CORS default" in [SECURITY.md](SECURITY.md) §12.

Operational consequence worth knowing before debugging a 403: a reverse proxy in
front of the console must preserve `Host`/`Origin`, or every upgrade is refused.

## Payloads

`live` — `LiveEvent`, the browser-facing projection of `pb.Event`
(`internal/console/events/live_event.go`, mirrored by hand in
`web/src/lib/types.ts`):

```json
{
  "id": "17-1753400000000000000",
  "seq": 17,
  "type": "check_observed",
  "severity": "error",
  "scope": "node-a→node-b",
  "timestamp": "2026-07-25T12:00:00Z",
  "summary": "tcp check node-a→node-b failed",
  "details": {"taskId": "t-1", "checkType": "tcp", "plane": "pod", "success": false, "durationNs": 1200000, "error": "dial timeout"}
}
```

| `type` | `severity` | `scope` | `details` keys |
| ------ | ---------- | ------- | -------------- |
| `topology_changed` | `info` | node name, else `cluster` | `reason`, `nodeName`, `agentId` |
| `check_observed` | `info` on success, else `error` | `<src>→<dst>` | `taskId`, `checkType`, `plane`, `success`, `durationNs`, `error` |
| `mtr_triggered` | `info` | `<src>→<dst>` | `taskId` |
| `mtr_completed` | `info` on success, else `error` | `<src>→<dst>` | `taskId`, `success`, `error`, `hops[]` (`number`, `ip`, `hostname`, `rttNs`, `lossRatio`) |
| `diagnostic_progress` | `info` for `dispatched`, `warn` for `timeout`/`error` | `<src>→<dst>` | `taskId`, `checkType`, `state` |

`id` is `"<seq>-<unixNano>"`, stable across replicas, which is what makes it
usable both as the Hub's dedupe key and as the React list key. The timestamp comes
from the controller and nowhere else — substituting a local clock would give the
same event a different id per replica and defeat the dedupe.

Durations are integer **nanoseconds** (`durationNs`, `rttNs`), matching every
other JSON surface in this repo.

Three as-built details the shape above does not show:

- **`topology_changed` only ever populates `reason`.** The controller's registry
  callback carries a reason string and nothing else, so `nodeName` and `agentId`
  are always empty and `scope` is therefore always `cluster`. The reason vocabulary
  is `agent_registered`, `agent_deregistered`, `agent_evicted`, `zone_updated`.
  Attributing a change to a node is an M3 controller change, not a protocol one.
- **A failed MTR emits no `mtr_completed`.** The dispatch emits `mtr_triggered`;
  if the agent never answers or errors out, the terminal frame is
  `diagnostic_progress` with `state: "timeout"` or `"error"`. `mtr_completed`
  (with `success:false` inside it) only appears when the agent actually returned a
  result. The same holds for non-MTR checks: a dispatch that never comes back
  yields `diagnostic_progress`, not `check_observed`. Consumers must treat
  `diagnostic_progress{state:"timeout"|"error"}` as terminal, not as a step before
  a completion that is coming.
- **`live` does not carry continuous background probe results.** Agents record
  TCP/UDP/ICMP/DNS/HTTP probes straight into their own Prometheus registry and
  never report them to the controller, so `check_observed` is a rollup of
  **on-demand diagnostic** completions (`POST /api/v1/diagnostics`) only.
  Continuous connectivity data reaches the UI through Prometheus, i.e. the matrix
  topics.

`topology` — the controller's snapshot verbatim, byte-identical to
`GET /api/v1/topology`. A `topology_changed` event is a refetch *signal*, not a
payload: the pusher re-reads the authoritative snapshot rather than reconstructing
state from event deltas.

`matrix:{protocol}:pod` — identical to
`GET /api/v1/matrix?protocol=…&plane=pod`, recomputed server-side once per
interval and fanned out, instead of every browser polling REST every 15 s.

## Capability detection and fallback

The console's own `GET /api/v1/version` returns `capabilities: ["events"]` only
while **this replica's** ingester holds a proven stream to a controller that
advertised `events`, and `[]` otherwise — computed per request, never a boot-time
echo of config. The browser polls it every 15 s (`useCapabilities`,
`CAPABILITIES_POLL_MS`) and renders `RealtimeBadge`: **Live** or **Delayed data**.

Two honest asymmetries live here:

- **The capability is per-replica, the bus is not.** A replica whose own gRPC
  stream is down can still fan out live events that other replicas published to
  Valkey, yet it advertises no `"events"`. A browser pinned to it falls back to
  15 s polling with the "Delayed data" badge while its socket would in fact have
  kept delivering. Safe-but-conservative by choice: under-claiming costs
  freshness, over-claiming would strand a browser on a silent socket. The ≤2 s
  post-reconnect grace window reads the same way — "not yet".
- **The Live page subscribes unconditionally** for exactly that reason: `realtime`
  drives the badge and the explanatory copy, never the subscription.

`useMatrix` silences its polling only when the capability **and** the socket are
both up (`realtime && push.connected`); losing either leg puts it straight back on
the M1 interval. First paint always comes from REST, so a socket that never opens
costs nothing but freshness.

Two switches turn the whole path off, both tested as working degraded states:
`controller.events.enabled=false` (no capability → console stays on polling) and
`console.valkey.mode=disabled` (in-process bus; correct for one replica only).

## Shutdown

Order matters and `cmd/console/main.go` documents it inline:

1. `srv.Run` returns — listener closed, plain HTTP drained. Hijacked WebSocket
   connections are invisible to `http.Server.Shutdown`, so this step does **not**
   release them.
2. `stopBackground` cancels the ingester, the pushers, the nudge relay and the
   Hub. `Hub.Run`'s deferred `closeAllClients` is what actually releases the
   sockets, and it also marks the hub closed so an upgrade racing shutdown is
   refused rather than leaked.
3. `wg.Wait` guarantees nobody is inside `Bus.Publish` any more. It covers **only**
   the spawned components — the per-client read/write pumps are tracked by nothing
   and are abandoned at process exit, so the close frame is best-effort (a wedged
   peer may see an RST instead of a 1001).
4. `closeBus` releases the Valkey client last; closing it earlier would make
   in-flight publishes fail against a closed client during an otherwise clean
   shutdown.

## Metrics

All `<metricsPrefix>_console_*`, bounded cardinality — no per-connection,
per-node or per-query labels.

| Metric | Type | Labels |
| ------ | ---- | ------ |
| `_events_received_total` | counter | `type` |
| `_events_deduped_total` | counter | — |
| `_ingester_connected` | gauge | — |
| `_ingester_reconnects_total` | counter | `reason` (`dial`, `stream`, `capability`) |
| `_ws_clients` | gauge | — |
| `_ws_messages_sent_total` | counter | `topic` |
| `_ws_dropped_clients_total` | counter | — |
| `_push_snapshots_total` | counter | `topic`, `result` (`ok`, `error`) |

`_ws_messages_sent_total` counts **allowlisted topics only**. Error frames echo
the client-chosen topic string, so counting every frame would let any browser mint
unbounded series by looping subscribes to random topics.

Controller side, for the other end of the stream:
`<prefix>_controller_event_subscribers` (gauge) and
`<prefix>_controller_events_published_total{type}` (counter).

Two traps in the HTTP middleware series, both consequences of hijacking:

- A **successful** upgrade is recorded as `status="200"`. The hijack bypasses
  `WriteHeader`, so the recorder's optimistic default is what lands — not 101.
- `_http_request_duration_seconds{path="/ws"}` measures the connection
  **lifetime**, so every long-lived socket falls into the `+Inf` bucket and
  inflates `_sum` by connection-seconds. Do not read request latency from it. Live
  connection count is the `_ws_clients` gauge.

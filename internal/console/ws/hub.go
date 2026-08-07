package ws

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/events"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
)

const (
	// sendBuffer is the per-client outbound queue depth. On overflow the client
	// is dropped and closed rather than the hub blocking: an unbounded queue
	// would turn one wedged browser into console-wide memory growth, and the
	// browser's own reconnect plus a fresh snapshot makes a drop cheap.
	sendBuffer = 256

	// liveRingSize / snapshotRingSize bound the per-topic replay ring
	// (Decision 6). One entry is enough for a snapshot topic because every
	// topology/matrix frame is a complete state that fully supersedes the
	// previous one.
	liveRingSize     = 200
	snapshotRingSize = 1

	// dedupeCacheSize bounds the live-event id set used to drop the duplicates
	// that multi-replica ingestion produces.
	dedupeCacheSize = 512

	// runRingSize bounds the replay ring for run:{id} topics (Task 20). Run
	// progress frames are append-only, like live, not successive whole
	// states, so — like liveRingSize — one entry would lose progress on
	// reconnect.
	runRingSize = 64

	// maxEphemeralTopics bounds the run:{id} registry (Decision 14). Past the
	// cap, OpenTopic evicts the oldest CLOSED topic to make room; if none is
	// closed, it returns false. Refusing a topic must never refuse a run —
	// the fallback is the caller polling GET /api/v1/runs/{id} instead.
	maxEphemeralTopics = 256

	// reapDelay is how long a CLOSED ephemeral topic keeps serving replay
	// before its subscription, seq counter and ring are freed — long enough
	// that a browser reconnecting just after a run finished still sees the
	// terminal frames.
	reapDelay = 5 * time.Minute

	// reapInterval is how often Hub.Run checks for ephemeral topics past
	// reapDelay. It runs inside Run's own select loop (a time.Ticker), not a
	// dedicated goroutine, so shutdown ordering is unchanged.
	reapInterval = 30 * time.Second
)

// client is one connected browser. Its topic set is guarded by Hub.mu, so
// subscribe and Broadcast can be made atomic against each other.
type client struct {
	send   chan Envelope
	topics map[string]bool

	done     chan struct{}
	doneOnce sync.Once
}

// close signals the client's pumps to shut down. Idempotent.
func (c *client) close() { c.doneOnce.Do(func() { close(c.done) }) }

// ephemeralTopic is one run:{id} topic's registry entry. Guarded by h.mu, like
// every other piece of topic state — seq, rings and ephemeral membership move
// together under one lock (see Broadcast and subscribe's doc comments; a
// second mutex here would quietly break the exactly-once invariant they
// depend on).
type ephemeralTopic struct {
	// cancel stops the per-topic bus-subscription goroutine started by
	// OpenTopic. It is nil for the brief window between OpenTopic reserving
	// this entry's map slot and h.bus.Subscribe (called without h.mu held,
	// deliberately — see OpenTopic) actually returning; every code path that
	// invokes it (reapTopicLocked) must nil-check first.
	cancel   context.CancelFunc
	closed   bool      // true once CloseTopic marked this topic terminal
	closedAt time.Time // when CloseTopic was called; zero while open, used by the reaper and by eviction's "oldest closed first" order
}

// Hub fans messages out to the WebSocket clients of one console replica.
type Hub struct {
	bus     cache.Bus
	metrics *metrics.Metrics
	dedupe  *idSet

	mu        sync.Mutex
	closed    bool // set once Run has shut the hub down; one-way — a Hub is per-process and cannot be restarted after Run returns
	clients   map[*client]struct{}
	seq       map[string]uint64
	rings     map[string][]Envelope
	ephemeral map[string]*ephemeralTopic // run:{id} topics opened via OpenTopic (Task 20)
}

// NewHub returns a hub that will read the live topic from bus once Run is
// called. Broadcast and ServeWS work without Run — snapshot topics do not
// depend on the bus at all.
func NewHub(bus cache.Bus, m *metrics.Metrics) *Hub {
	return &Hub{
		bus:       bus,
		metrics:   m,
		dedupe:    newIDSet(dedupeCacheSize),
		clients:   make(map[*client]struct{}),
		seq:       make(map[string]uint64),
		rings:     make(map[string][]Envelope),
		ephemeral: make(map[string]*ephemeralTopic),
	}
}

// Run subscribes to the bus's live topic, de-duplicates, and fans events out to
// subscribed clients. It blocks until ctx is cancelled (or the bus closes the
// subscription) and then closes every connected client, which is what actually
// releases the hijacked WebSocket connections at shutdown —
// http.Server.Shutdown does not track them.
//
// Accepted limitation: a bus-side drop is invisible in Envelope.Seq. Both bus
// implementations shed load by dropping messages BEFORE they reach this loop
// (InProcessBus when the hub's subscriber channel is full, ValkeyBus under
// Valkey's own client-output limits), and Envelope.Seq is assigned here,
// after the bus — so a dropped live event produces a perfectly gapless
// envelope sequence and the browser cannot detect the loss from framing
// alone. It is not detectable here either: the subscriber channel carries no
// "something was dropped" signal. The recovery path clients do have is
// LiveEvent.Seq inside Data — controller-assigned and strictly increasing —
// so a gap THERE is a real loss signal a client may act on (M2's Live page
// merely renders what arrives; the feed is best-effort by design, ADR-003).
//
// Shutdown ordering (Task 14): stop accepting new WebSocket upgrades before,
// or alongside, cancelling this context — closeAllClients only closes the
// clients that exist at that instant. As a backstop, register on a stopped hub
// closes the client immediately instead of inserting it into a map nothing
// will ever drain again.
func (h *Hub) Run(ctx context.Context) {
	msgs, unsubscribe := h.bus.Subscribe(TopicLive)
	defer unsubscribe()
	defer h.closeAllClients()

	reapTicker := time.NewTicker(reapInterval)
	defer reapTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-msgs:
			if !ok {
				return
			}
			h.fanOutLive(msg)
		case <-reapTicker.C:
			h.reapExpiredTopics()
		}
	}
}

// fanOutLive is the dedupe gate. Every replica runs its own events.Ingester and
// publishes to the same Valkey channel, so with N replicas each pb.Event arrives
// here N times — identical bytes, identical LiveEvent.ID, because the id is
// built only from controller-assigned values. Dropping the repeats here is what
// keeps that redundancy (a replica whose own stream is down still serves events)
// from becoming N copies of every row in the browser.
func (h *Hub) fanOutLive(msg cache.Message) {
	var live events.LiveEvent
	if err := json.Unmarshal(msg.Data, &live); err != nil {
		slog.Warn("ws hub: dropping malformed live event", "error", err)
		return
	}
	if live.ID == "" {
		slog.Warn("ws hub: dropping live event with an empty id", "type", live.Type)
		return
	}
	if !h.dedupe.add(live.ID) {
		h.metrics.EventsDeduped.WithLabelValues().Inc()
		return
	}

	msgType := msg.Type
	if msgType == "" {
		msgType = TypeEvent
	}
	h.Broadcast(TopicLive, msgType, msg.Data)
}

// Broadcast assigns the next per-topic seq, records the frame in the topic's
// replay ring, and delivers it to every LOCAL subscriber of topic. It never goes
// through cache.Bus: snapshot pushers call it directly, and Run calls it for
// already-deduplicated live events.
//
// Seq assignment and the ring append are atomic under h.mu, so per-topic Seq is
// the authoritative order and the ring is always seq-sorted. The sends happen
// OUTSIDE the lock, so two concurrent Broadcasts on the same topic can reach a
// client's buffer inverted (see subscribe for the consumer contract). Task 13's
// pushers each run one goroutine per topic, which is the operative assumption
// that keeps same-topic Broadcasts serialized in practice. Topic is not
// validated here — callers are trusted server code. M2's fixed topic set made
// seq/rings implicitly bounded; Task 20's run:{id} topics are the caller-chosen
// set that debt named, and OpenTopic/CloseTopic/the reaper are what keeps them
// from growing forever (maxEphemeralTopics, reapDelay).
//
// Reap-vs-in-flight-Broadcast: runEphemeralTopic's bus subscription can have
// one message already read and mid-flight into this call when reapTopicLocked
// deletes the topic's registry entry, seq counter and ring — the two run on
// different goroutines and are ordered only by whoever gets h.mu next. Without
// a check here, that message would silently recreate h.seq[topic]/h.rings[topic]
// right after the reaper freed them: a mutex-ordered TOCTOU -race cannot see
// (both sides individually lock h.mu correctly), but unbounded growth all the
// same — every future message on a long-dead run:{id} would keep reviving it
// forever, which is exactly what the reaper exists to prevent. So a run:{id}
// topic (only — the static live/topology/matrix topics have no ephemeral
// registry entry to check and must never be gated by one) is broadcast only
// while it still has one.
func (h *Hub) Broadcast(topic, msgType string, data json.RawMessage) {
	h.mu.Lock()
	if strings.HasPrefix(topic, runTopicPrefix) {
		if _, open := h.ephemeral[topic]; !open {
			h.mu.Unlock()
			return
		}
	}
	h.seq[topic]++
	env := Envelope{Topic: topic, Type: msgType, Seq: h.seq[topic], Data: data}
	h.appendRingLocked(env)
	targets := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		if c.topics[topic] {
			targets = append(targets, c)
		}
	}
	h.mu.Unlock()

	for _, c := range targets {
		h.deliver(c, env)
	}
}

// ClientCount reports the number of connected clients on this replica.
func (h *Hub) ClientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// register adds a client. Called by ServeWS once the upgrade succeeded. On a
// hub that Run has already shut down, the client is refused: its done channel
// is closed immediately and it never enters the clients map — otherwise an
// upgrade racing shutdown would leak a hijacked connection forever (nothing
// runs closeAllClients twice) and push the WSClients gauge back up after
// shutdown zeroed it.
func (h *Hub) register() *client {
	c := &client{
		send:   make(chan Envelope, sendBuffer),
		topics: make(map[string]bool),
		done:   make(chan struct{}),
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		c.close()
		return c
	}
	h.clients[c] = struct{}{}
	h.metrics.WSClients.WithLabelValues().Set(float64(len(h.clients)))
	h.mu.Unlock()
	return c
}

// unregister removes a client and signals its pumps. Idempotent. The gauge is
// set inside the critical section so concurrent register/unregister/drop calls
// cannot latch a stale count.
func (h *Hub) unregister(c *client) {
	h.mu.Lock()
	if _, present := h.clients[c]; present {
		delete(h.clients, c)
		h.metrics.WSClients.WithLabelValues().Set(float64(len(h.clients)))
	}
	h.mu.Unlock()

	c.close()
}

// closeAllClients drops every client, drains the ephemeral topic registry and
// marks the hub closed, used by Run on shutdown. After it runs, register
// refuses new clients (see register) and OpenTopic refuses new topics.
// Draining the registry cancels every OpenTopic subscription goroutine — left
// running, they would leak and the cmd/console wg.Wait shutdown sequence
// waits on nothing that stops them otherwise.
func (h *Hub) closeAllClients() {
	h.mu.Lock()
	h.closed = true
	doomed := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		doomed = append(doomed, c)
	}
	h.clients = make(map[*client]struct{})
	h.metrics.WSClients.WithLabelValues().Set(0)

	for topic := range h.ephemeral {
		h.reapTopicLocked(topic)
	}
	h.mu.Unlock()

	for _, c := range doomed {
		c.close()
	}
	if len(doomed) > 0 {
		slog.Info("ws hub stopped, closed websocket clients", "clients", len(doomed))
	}
}

// handleClientMessage applies one decoded client frame. Called from the read
// pump, so it must not block.
func (h *Hub) handleClientMessage(c *client, msg ClientMessage) {
	switch msg.Action {
	case ActionSubscribe:
		if !h.topicAllowed(msg.Topic) {
			h.sendError(c, msg.Topic, "unknown topic; subscribable topics are "+
				"live, topology, matrix:tcp:pod, matrix:udp:pod, matrix:icmp:pod")
			return
		}
		for _, env := range h.subscribe(c, msg.Topic, msg.LastSeq) {
			h.deliver(c, env)
		}
	case ActionUnsubscribe:
		h.unsubscribe(c, msg.Topic)
	default:
		h.sendError(c, msg.Topic, "unknown action "+quote(msg.Action)+"; expected subscribe or unsubscribe")
	}
}

// subscribe registers c for topic and returns the replay frames it missed, both
// under one lock. That atomicity is what makes delivery exactly-once against a
// concurrent Broadcast: if the Broadcast wins the lock, its frame is already in
// the ring and c is not yet a target, so the replay carries it; if subscribe
// wins, c is a target and gets it live, and the replay snapshot predates it.
//
// Exactly-once is NOT ordered. Because frames are handed to c.send outside the
// hub lock, a Broadcast racing this subscribe can enqueue its (newer-seq) frame
// before the replay's older frames, and two concurrent same-topic Broadcasts
// can likewise arrive inverted. Per-topic Seq is the authoritative ORDER — not
// a licence to drop — and the consumer rule differs by topic class
// (web/src/lib/ws.ts, Task 12):
//
//   - Snapshot topics (topology, matrix:*): every frame is the whole state, so
//     keep only the highest seq seen and discard lower — an inverted pair would
//     otherwise leave the OLDER state rendered until the next push.
//   - live: frames are an append-only SET, not successive states. Dedupe by
//     LiveEvent.ID (or envelope seq) and insert in seq order; NEVER discard an
//     unseen lower seq — a Broadcast racing the replay can deliver seq 6 before
//     replayed 1..5, and dropping those five would lose event rows permanently.
func (h *Hub) subscribe(c *client, topic string, lastSeq uint64) []Envelope {
	h.mu.Lock()
	defer h.mu.Unlock()

	c.topics[topic] = true
	ring := h.rings[topic]
	replay := make([]Envelope, 0, len(ring))
	for _, env := range ring {
		if env.Seq > lastSeq {
			replay = append(replay, env)
		}
	}
	return replay
}

func (h *Hub) unsubscribe(c *client, topic string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(c.topics, topic)
}

// topicAllowed reports whether topic may be subscribed to: the static M2
// allowlist, or a topic currently present in the ephemeral run:{id} registry
// (open or closed-awaiting-reap — replay must keep working right up to the
// moment the reaper actually frees the entry). A subscribe to a run:{id} that
// is not, or no longer, streaming here falls through to the M2 error frame,
// which is the right answer: the browser's fallback is REST polling.
func (h *Hub) topicAllowed(topic string) bool {
	if _, ok := allowedTopics[topic]; ok {
		return true
	}
	h.mu.Lock()
	_, ok := h.ephemeral[topic]
	h.mu.Unlock()
	return ok
}

// OpenTopic registers an ephemeral topic and starts a bus subscription
// feeding it. Unlike the snapshot topics (which every replica recomputes
// locally and therefore broadcast hub-locally), a run executes on EXACTLY ONE
// replica while the browser that started it may be pinned to another, and no
// other replica can recompute it — so run progress is bus-carried like
// "live". Unlike "live" it needs no dedupe: there is exactly one publisher
// per run.
//
// Bounded, per Broadcast's own warning: at most maxEphemeralTopics (256) may
// be open. Over the cap the oldest CLOSED topic is evicted first; if none is
// closed, OpenTopic returns false and the caller proceeds without a socket
// topic — the run still executes and GET /api/v1/runs/{id} still reports it.
// Refusing to open a topic must never refuse to run a check.
//
// ctx MUST be the RUN's own lifetime — background-derived (e.g.
// context.Background(), or a context tied to the run's own goroutine/job),
// NEVER a request-scoped context such as an HTTP handler's r.Context(). The
// subscription goroutine's context is a child of ctx, so a caller that passes
// r.Context() gets a subscription that dies the instant the HTTP request that
// happened to call OpenTopic returns — typically milliseconds later — even
// though the run itself keeps executing for minutes. That failure is silent
// from OpenTopic's own return value (it still returns true; the subscription
// forms and is torn down on its own schedule after) and only shows up later as
// "the run:{id} topic went idle" with no error anywhere. Hub can also force
// the subscription to stop independently of ctx — eviction over the cap, the
// reaper 5 minutes after CloseTopic, or Run returning at shutdown — which is
// why the goroutine is additionally cancellable from inside the registry, not
// solely a derivative of ctx; that mechanism does not rescue a request-scoped
// ctx, it only ADDS more ways the subscription can end early.
//
// Idempotent: calling OpenTopic again for a topic already in the registry
// (open or closed-awaiting-reap) is a no-op that returns true without
// touching the existing subscription. This assumes run IDs — and therefore
// RunTopic(runID) strings — are never reused for two different runs; if a
// caller ever did reuse one, the second OpenTopic would silently attach to
// the FIRST run's (possibly already-closed) topic instead of opening a fresh
// one for the second run. Nothing in this package enforces that uniqueness —
// it is the run ID generator's contract to keep, not the Hub's to check.
//
// Concurrency: two OpenTopic calls for the SAME topic can race in from
// different goroutines. h.bus.Subscribe must not run while h.mu is held (it
// can block on the bus implementation, exactly like Run's own unlocked
// Subscribe call), so the cap check and the "already open" idempotence check
// cannot simply be re-verified atomically with the subscribe itself. Instead,
// under the FIRST lock (before Subscribe) this reserves the map slot with a
// placeholder ephemeralTopic (cancel == nil) — that single write is what
// makes both invariants atomic with respect to a concurrent OpenTopic:
// len(h.ephemeral) already reflects the reservation for the cap check, and a
// second OpenTopic(topic) sees `exists` immediately and returns true without
// ever calling Subscribe a second time. After Subscribe returns, OpenTopic
// re-locks and re-checks that ITS OWN reservation is still the one in the
// registry — closeAllClients (shutdown) or a reap can have deleted it out
// from under an in-flight OpenTopic in that unlocked window — and if not,
// tears its own subscription back down (cancel + unsubscribe) and reports
// failure rather than leaking it or clobbering whatever is there now.
func (h *Hub) OpenTopic(ctx context.Context, topic string) bool {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return false
	}
	if _, exists := h.ephemeral[topic]; exists {
		h.mu.Unlock()
		return true
	}
	if len(h.ephemeral) >= maxEphemeralTopics && !h.evictOldestClosedLocked() {
		h.mu.Unlock()
		return false
	}
	// Reserve the slot now, under h.mu, before releasing it — see the doc
	// comment above for why this single write is what keeps the cap check and
	// the same-topic idempotence check correct under concurrent OpenTopic
	// calls, without holding h.mu across the Subscribe call below.
	et := &ephemeralTopic{}
	h.ephemeral[topic] = et
	h.metrics.WSTopics.WithLabelValues().Set(float64(len(h.ephemeral)))
	h.mu.Unlock()

	msgs, unsubscribe := h.bus.Subscribe(topic)
	subCtx, cancel := context.WithCancel(ctx)

	h.mu.Lock()
	if h.ephemeral[topic] != et {
		// Our reservation is gone (or replaced): the hub shut down, or the
		// topic was closed and reaped, while Subscribe was in flight. Tear this
		// subscription back down instead of leaking it or overwriting whatever
		// now occupies (or doesn't occupy) the slot.
		h.mu.Unlock()
		cancel()
		unsubscribe()
		return false
	}
	et.cancel = cancel
	h.mu.Unlock()

	go h.runEphemeralTopic(subCtx, topic, msgs, unsubscribe)
	return true
}

// CloseTopic marks topic terminal and broadcasts one TypeClosed control frame
// on it — carrying no Data, the type alone is the signal — before doing so.
// That frame IS the topic's terminal signal: a subscribed browser tab (or one
// that resumes with lastSeq before it) learns the run is over instead of the
// topic silently going idle, the same "an unknown state must be visible, not
// guessed" principle sendError already follows for a rejected subscribe. Once
// a client has that frame it has no further reason to expect a push on this
// topic, whether or not the hub ever gets around to reaping it.
//
// It then keeps serving replay for reapDelay (5 min) so a browser that
// reconnects just after a run finished still receives the terminal frame (and
// everything before it) from the ring; the reaper then unsubscribes it from
// the bus and frees the seq counter, the ring, and the topic entry in every
// still-subscribed client's c.topics (reapTopicLocked). Idempotent — a topic
// that is already closed, or was never opened, is a no-op and broadcasts
// nothing: a second terminal frame would be a false signal, not a redundant
// one, since by then the ring may already have moved past what it replays.
//
// A caller that also needs to deliver its OWN final data frame (e.g. a run's
// terminal status) should use CloseTopicWithFinal instead of publishing that
// frame separately (through the bus, say) and then calling this method — see
// CloseTopicWithFinal's doc comment for why that shape cannot guarantee the
// final frame is seen before TypeClosed.
func (h *Hub) CloseTopic(topic string) {
	if !h.markClosed(topic) {
		return
	}
	h.Broadcast(topic, TypeClosed, json.RawMessage(`{}`))
}

// CloseTopicWithFinal marks topic terminal and broadcasts final (as msgType)
// immediately followed by the TypeClosed control frame — both delivered
// synchronously, back-to-back, before this call returns.
//
// This is the seam a caller with its own terminal payload for the topic (the
// checks.Runner's finished-run summary, currently the only caller) must use
// instead of publishing that payload through the async path — a cache.Bus
// publish that some OTHER goroutine (here, the per-topic bus-subscription
// goroutine OpenTopic started, runEphemeralTopic) eventually picks up and
// hands to Broadcast — and then separately calling plain CloseTopic. That
// two-step shape races the final frame against CloseTopic's own Broadcast
// call: CloseTopic reaches Broadcast directly, synchronously, on the calling
// goroutine, while the final frame has to first cross the bus and be read by
// runEphemeralTopic before it ever reaches Broadcast — there is no ordering
// guarantee between "goroutine A calls Broadcast now" and "goroutine B reads
// a channel, then calls Broadcast," and CloseTopic's TypeClosed frame
// regularly wins, landing a LOWER Seq than the final frame that was
// logically supposed to precede it. A client that treats TypeClosed as "nothing
// more is coming for this topic" (exactly the contract CloseTopic's own doc
// comment describes) then drops the final frame outright, or renders it after
// having already told the user the run is over.
//
// CloseTopicWithFinal closes that seam structurally, not by luck: both
// Broadcast calls happen on the SAME goroutine, one right after the other,
// with no channel hop in between, so Broadcast's own per-topic seq counter
// (already the authoritative order — see Broadcast's doc comment) assigns
// final a strictly lower Seq than TypeClosed's, for every subscriber,
// every time.
//
// Idempotent, exactly like CloseTopic: a topic that is already closed, or was
// never opened, is a no-op that broadcasts nothing (neither frame).
func (h *Hub) CloseTopicWithFinal(topic, msgType string, final json.RawMessage) {
	if !h.markClosed(topic) {
		return
	}
	h.Broadcast(topic, msgType, final)
	h.Broadcast(topic, TypeClosed, json.RawMessage(`{}`))
}

// markClosed marks topic terminal (closed=true, closedAt=now) under
// h.mu and reports whether it just did so — false means topic is unknown or
// was already closed, in which case the caller (CloseTopic /
// CloseTopicWithFinal) must broadcast nothing, per their shared idempotence
// contract.
func (h *Hub) markClosed(topic string) bool {
	h.mu.Lock()
	et, ok := h.ephemeral[topic]
	if !ok || et.closed {
		h.mu.Unlock()
		return false
	}
	et.closed = true
	et.closedAt = time.Now()
	h.mu.Unlock()
	return true
}

// OpenTopicCount reports the current ephemeral registry size (for the gauge
// and for tests).
func (h *Hub) OpenTopicCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.ephemeral)
}

// TopicSeq reports the last sequence number assigned on topic (0 when the
// topic has never carried a frame). For a run:{id} topic the runner is the
// SINGLE publisher (Decision 14), so this doubles as "how many of the
// publisher's frames the hub has relayed so far" — the seam
// checks.Runner.execute uses to wait for its bus-published progress frames
// to be relayed before CloseTopicWithFinal assigns the terminal frames their
// seq, keeping every progress frame's seq below TypeClosed's.
func (h *Hub) TopicSeq(topic string) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seq[topic]
}

// runEphemeralTopic feeds one ephemeral topic's own bus subscription into
// Broadcast — the same shape as Run/fanOutLive for "live", but scoped to a
// single topic and independently cancellable. It exits when ctx is cancelled
// or the bus closes msgs.
func (h *Hub) runEphemeralTopic(ctx context.Context, topic string, msgs <-chan cache.Message, unsubscribe func()) {
	defer unsubscribe()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-msgs:
			if !ok {
				return
			}
			msgType := msg.Type
			if msgType == "" {
				msgType = TypeEvent
			}
			h.Broadcast(topic, msgType, msg.Data)
		}
	}
}

// reapExpiredTopics evicts every ephemeral topic that has been closed for at
// least reapDelay. Called from Run's select loop on reapTicker, never
// concurrently with itself.
func (h *Hub) reapExpiredTopics() {
	h.mu.Lock()
	defer h.mu.Unlock()
	cutoff := time.Now().Add(-reapDelay)
	for topic, et := range h.ephemeral {
		if et.closed && et.closedAt.Before(cutoff) {
			h.reapTopicLocked(topic)
		}
	}
}

// evictOldestClosedLocked frees one registry slot by reaping the closed topic
// with the earliest CloseTopic call ("oldest closed first" — Decision 14).
// Reports whether an eviction happened; false means every topic is still
// open, and the caller (OpenTopic, over the cap) must refuse rather than cut
// short a run that is still in progress. Caller holds h.mu.
func (h *Hub) evictOldestClosedLocked() bool {
	oldest := ""
	var oldestAt time.Time
	found := false
	for topic, et := range h.ephemeral {
		if !et.closed {
			continue
		}
		if !found || et.closedAt.Before(oldestAt) {
			oldest, oldestAt, found = topic, et.closedAt, true
		}
	}
	if !found {
		return false
	}
	h.reapTopicLocked(oldest)
	return true
}

// reapTopicLocked removes topic from the ephemeral registry: cancels its bus
// subscription goroutine, frees the seq counter and ring, and clears topic out
// of every currently-subscribed client's c.topics — so no trace of the topic
// survives past this call, in the hub OR in a long-lived connection that
// happened to be subscribed (the leak this task exists to close). By the time
// a topic is reaped its terminal "closed" frame has already gone out
// (CloseTopic), so clearing the subscription here costs a subscriber nothing.
// Caller holds h.mu. Shared by the periodic reaper, cap eviction and
// closeAllClients.
//
// et.cancel can be nil: reapTopicLocked can run against an OpenTopic
// reservation that has not yet finished subscribing (see OpenTopic's doc
// comment) — most plausibly closeAllClients draining the whole registry at
// shutdown while an OpenTopic call is still in flight.
func (h *Hub) reapTopicLocked(topic string) {
	if et, ok := h.ephemeral[topic]; ok {
		if et.cancel != nil {
			et.cancel()
		}
		delete(h.ephemeral, topic)
	}
	delete(h.seq, topic)
	delete(h.rings, topic)
	for c := range h.clients {
		delete(c.topics, topic)
	}
	h.metrics.WSTopics.WithLabelValues().Set(float64(len(h.ephemeral)))
}

// appendRingLocked records env in its topic's ring, trimming to the topic's
// bound. Caller holds h.mu.
func (h *Hub) appendRingLocked(env Envelope) {
	ring := h.rings[env.Topic]
	ring = append(ring, env)
	if size := ringSize(env.Topic); len(ring) > size {
		ring = ring[len(ring)-size:]
	}
	h.rings[env.Topic] = ring
}

func ringSize(topic string) int {
	switch {
	case topic == TopicLive:
		return liveRingSize
	case strings.HasPrefix(topic, runTopicPrefix):
		return runRingSize
	default:
		return snapshotRingSize
	}
}

// deliver hands env to one client, or drops the client if its buffer is full.
// Never blocks.
//
// Only the STATIC allowlist is counted here — deliberately narrower than
// Hub.topicAllowed's subscribe gate. Two distinct unbounded-cardinality
// sources must both be kept out of ws_messages_sent_total's topic label:
// error frames echo the client-chosen topic string verbatim (sendError), and
// — since Task 20 — run:{id} data frames carry a controller-assigned run ID
// baked into the topic string. Counting either would let the label set grow
// without bound (the closed-label-set convention StoreQueries' doc comment
// states outright: "no run-ID labels anywhere"). Static-allowlist frames are
// always counted; run:{id} traffic is observable instead through
// OpenTopicCount/WSTopics, which report a count, never a run ID.
func (h *Hub) deliver(c *client, env Envelope) {
	select {
	case c.send <- env:
		if _, ok := allowedTopics[env.Topic]; ok {
			h.metrics.WSMessagesSent.WithLabelValues(env.Topic).Inc()
		}
	default:
		h.dropSlowClient(c)
	}
}

func (h *Hub) dropSlowClient(c *client) {
	h.mu.Lock()
	_, present := h.clients[c]
	if present {
		delete(h.clients, c)
		h.metrics.WSClients.WithLabelValues().Set(float64(len(h.clients)))
		h.metrics.WSDroppedClients.WithLabelValues().Inc()
	}
	h.mu.Unlock()

	c.close()
	if present {
		slog.Warn("dropping websocket client: send buffer full", "buffer", sendBuffer)
	}
}

// sendError replies with an error envelope on the topic the client asked about,
// so a rejected subscribe is visibly rejected instead of looking like a healthy
// but idle topic. Error frames carry no sequence number and do not advance the
// topic counter — they are not data.
func (h *Hub) sendError(c *client, topic, detail string) {
	data, err := json.Marshal(errorPayload{Error: detail})
	if err != nil { // unreachable: the payload is one plain string
		data = json.RawMessage(`{"error":"internal error"}`)
	}
	h.deliver(c, Envelope{Topic: topic, Type: TypeError, Data: data})
}

// quote renders s for an error message without pulling in fmt.
func quote(s string) string { return `"` + s + `"` }

// idSet is a bounded set of live-event ids with insertion-ordered eviction:
// oldest id out first. Controller sequence numbers only grow, so oldest-first is
// the same as least-recently-used here, and the fixed cap is what keeps a
// multi-week uptime from growing the set forever.
type idSet struct {
	mu    sync.Mutex
	max   int
	order []string
	seen  map[string]struct{}
}

func newIDSet(maxEntries int) *idSet {
	return &idSet{max: maxEntries, order: make([]string, 0, maxEntries), seen: make(map[string]struct{}, maxEntries)}
}

// add records id and reports whether it was new. false means "already seen" —
// i.e. a duplicate to drop.
func (s *idSet) add(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, dup := s.seen[id]; dup {
		return false
	}
	s.seen[id] = struct{}{}
	s.order = append(s.order, id)
	if len(s.order) > s.max {
		delete(s.seen, s.order[0])
		s.order = s.order[1:]
	}
	return true
}

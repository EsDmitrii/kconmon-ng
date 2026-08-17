package ws

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/events"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
)

const (
	// sendBuffer is the per-client outbound queue depth; on overflow the client is dropped and closed
	// rather than the hub blocking.
	sendBuffer = 256

	// liveRingSize / snapshotRingSize bound the per-topic replay ring.
	liveRingSize     = 200
	snapshotRingSize = 1

	// dedupeCacheSize bounds the live-event id set used to drop the duplicates
	// that multi-replica ingestion produces.
	dedupeCacheSize = 512

	// runRingSize bounds the replay ring for run:{id} topics; run progress frames are append-only,
	// like live, not successive whole states.
	runRingSize = 64

	// maxEphemeralTopics bounds the run:{id} registry; refusing a topic must never refuse a run.
	maxEphemeralTopics = 256

	// reapDelay is how long a CLOSED ephemeral topic keeps serving replay before its subscription, seq
	// counter and ring are freed.
	reapDelay = 5 * time.Minute

	// sweepBatch bounds how many ephemeral topics ONE liveness sweep asks the store about; a backlog
	// is worked through over several ticks rather than in one long pass.
	sweepBatch = 32

	// reapInterval is how often Hub.Run checks for ephemeral topics past
	// reapDelay. It runs inside Run's own select loop (a time.Ticker), not a
	// dedicated goroutine, so shutdown ordering is unchanged.
	reapInterval = 30 * time.Second
)

// TopicAuthorizer decides, per CONNECTION, whether that connection's subject may subscribe to
// topic; a nil error means allowed.
type TopicAuthorizer func(topic string) error

// client is one connected browser. Its topic set is guarded by Hub.mu, so
// subscribe and Broadcast can be made atomic against each other.
type client struct {
	send   chan Envelope
	topics map[string]bool

	// authorize is this connection's per-topic gate.
	authorize TopicAuthorizer

	done     chan struct{}
	doneOnce sync.Once
}

// allowedToSubscribe applies c's authorizer. It returns the detail string for
// the error frame, and false, when the subscribe must be refused.
func (c *client) allowedToSubscribe(topic string) (detail string, ok bool) {
	if c.authorize == nil {
		return "", true
	}
	if err := c.authorize(topic); err != nil {
		return err.Error(), false
	}
	return "", true
}

// close signals the client's pumps to shut down. Idempotent.
func (c *client) close() { c.doneOnce.Do(func() { close(c.done) }) }

// ephemeralTopic is one run:{id} topic's registry entry; guarded by h.mu, like every other piece of
// topic state — seq.
type ephemeralTopic struct {
	// It is nil for window between OpenTopic reserving this entry's map slot and h.bus.Subscribe
	// (called without h.mu held, deliberately — see OpenTopic) actually returning.
	cancel   context.CancelFunc
	closed   bool      // true once CloseTopic marked this topic terminal
	closedAt time.Time // when CloseTopic was called; zero while open, used by the reaper and by eviction's "oldest closed first" order
}

// Hub fans messages out to the WebSocket clients of one console replica.
type Hub struct {
	bus     cache.Bus
	metrics *metrics.Metrics
	dedupe  *idSet
	/* epoch identifies THIS process's numbering. Sequence numbers are per-hub and start at 1, so a
	   cursor only means something against the hub that issued it — see Envelope.Epoch. */
	epoch string
	/* openEphemeral is asked to open a run:{id} topic this replica does not know about. A run's
	   topic used to be opened only on the replica that served the POST, while the browser's socket
	   lands on whichever replica the Service picks: with the chart's default of two console
	   replicas, about half of all run permalinks subscribed to a topic that did not exist there and
	   were answered "unknown topic". The frames are on the bus either way; what was missing was
	   somebody on this replica listening. nil leaves the old behaviour. */
	openEphemeral func(ctx context.Context, topic string) bool

	/* liveEphemeral answers "is this ephemeral topic's run still going?" for the reaper. Only the
	   replica that OWNS a run closes its topic, and it does so hub-locally, so a topic opened by
	   openEphemeral on any other replica has no other way to reach a closed state. See
	   SetEphemeralLiveness. nil means the reaper behaves exactly as it did. */
	liveEphemeral func(topic string) bool

	// sweeping keeps one liveness sweep in flight at a time; it is not guarded by h.mu because the
	// sweep itself takes and releases that lock.
	sweeping atomic.Bool

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
		epoch:     uuid.NewString(),
		dedupe:    newIDSet(dedupeCacheSize),
		clients:   make(map[*client]struct{}),
		seq:       make(map[string]uint64),
		rings:     make(map[string][]Envelope),
		ephemeral: make(map[string]*ephemeralTopic),
	}
}

// Run subscribes to the bus's live topic; both bus implementations shed load by dropping messages
// BEFORE they reach this loop (InProcessBus when the hub's subscriber channel is full, RedisBus
// under Valkey's own client-output limits).
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
			// Off the loop: the liveness predicate reads the run store, and a slow one must not stop
			// this hub relaying events. See sweepEphemeralLiveness.
			go h.sweepEphemeralLiveness()
		}
	}
}

// Every replica runs its own events.Ingester and publishes to the same Valkey channel.
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

// Broadcast assigns the next per-topic seq, records the frame in the topic's replay ring; it never
// goes through cache.Bus: snapshot pushers call it directly.
func (h *Hub) Broadcast(topic, msgType string, data json.RawMessage) {
	h.mu.Lock()
	if IsRunTopic(topic) {
		if _, open := h.ephemeral[topic]; !open {
			h.mu.Unlock()
			return
		}
	}
	h.seq[topic]++
	env := Envelope{Topic: topic, Type: msgType, Seq: h.seq[topic], Epoch: h.epoch, Data: data}
	h.appendRingLocked(&env)
	targets := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		if c.topics[topic] {
			targets = append(targets, c)
		}
	}
	h.mu.Unlock()

	for _, c := range targets {
		h.deliver(c, &env)
	}
}

// ClientCount reports the number of connected clients on this replica.
func (h *Hub) ClientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// register adds a client whose subscribes are gated by authorize (nil = every subscribable topic,
// the pre-M7 behaviour); on a hub that Run has already shut down, the client is refused.
func (h *Hub) register(authorize TopicAuthorizer) *client {
	c := &client{
		send:      make(chan Envelope, sendBuffer),
		topics:    make(map[string]bool),
		authorize: authorize,
		done:      make(chan struct{}),
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

// closeAllClients drops every client, drains the ephemeral topic registry and marks the hub closed;
// draining the registry cancels every OpenTopic subscription goroutine — left running.
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
		if !h.topicAllowed(msg.Topic) && !h.openOnDemand(msg.Topic) {
			h.sendError(c, msg.Topic, "unknown topic; subscribable topics are "+
				"live, topology, matrix:tcp:pod, matrix:udp:pod, matrix:icmp:pod")
			return
		}
		// Existence first, then permission; the order costs nothing here: every subject that got this far
		// already holds a permission that lets it enumerate run ids over REST (see the /ws upgrade gate).
		if detail, ok := c.allowedToSubscribe(msg.Topic); !ok {
			h.sendError(c, msg.Topic, detail)
			return
		}
		replay := h.subscribe(c, msg.Topic, msg.LastSeq, msg.Epoch)
		for i := range replay {
			h.deliverReplay(c, &replay[i])
		}
	case ActionUnsubscribe:
		h.unsubscribe(c, msg.Topic)
	default:
		h.sendError(c, msg.Topic, "unknown action "+quote(msg.Action)+"; expected subscribe or unsubscribe")
	}
}

// subscribe registers c for topic and returns the replay frames it missed, both under one lock;
// per-topic Seq is the authoritative ORDER — not a licence to drop.
//
// The cursor is honoured only when it came from THIS hub (epoch). A cursor from another replica is
// not a smaller number in the same series, it is a number in a different series — comparing them
// replayed nothing at all and lost the whole gap without a word.
func (h *Hub) subscribe(c *client, topic string, lastSeq uint64, epoch string) []Envelope {
	h.mu.Lock()
	defer h.mu.Unlock()

	c.topics[topic] = true
	/* A cursor carrying ANOTHER hub's epoch is not a smaller number in this series, so it is
	   discarded and the whole ring replays. An EMPTY epoch is a client that predates the field: its
	   cursor is honoured as before, which is what it has always been given. */
	if epoch != "" && epoch != h.epoch {
		lastSeq = 0
	}
	ring := h.rings[topic]
	replay := make([]Envelope, 0, len(ring))
	for _, env := range ring {
		if env.Seq > lastSeq {
			replay = append(replay, env)
		}
	}
	return replay
}

/*
 * openOnDemand tries to open a run:{id} topic this replica has not seen, reporting whether it may
 * now be subscribed.
 *
 * A run's topic was opened only where its POST landed, and the browser's socket is an independent
 * connection that lands wherever the Service sends it: with two console replicas, about half of all
 * run permalinks asked for a topic that did not exist on their replica and were told "unknown
 * topic", so the page showed no progress at all until it finished. The frames were on the bus the
 * whole time — this replica simply was not listening.
 *
 * The opener is the RUNNER's: it decides whether the id names a real run before the hub opens
 * anything, so an unknown id is still an error rather than an open topic that never speaks.
 */
func (h *Hub) openOnDemand(topic string) bool {
	if !IsRunTopic(topic) {
		return false
	}
	h.mu.Lock()
	open := h.openEphemeral
	h.mu.Unlock()
	if open == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), openOnDemandTimeout)
	defer cancel()
	return open(ctx, topic)
}

// openOnDemandTimeout bounds the store lookup + subscribe that opening a topic on demand performs;
// it runs on the read pump, which must not park.
const openOnDemandTimeout = 3 * time.Second

// SetEphemeralOpener installs the callback that opens a run:{id} topic this replica has not seen;
// see Hub.openEphemeral. Call before serving.
func (h *Hub) SetEphemeralOpener(open func(ctx context.Context, topic string) bool) {
	h.mu.Lock()
	h.openEphemeral = open
	h.mu.Unlock()
}

/*
SetEphemeralLiveness installs the "is this topic's run still going?" predicate the reaper needs.

Every reclaim path in this file — reapExpiredTopics and evictOldestClosedLocked — acts only on
CLOSED entries, and an entry is closed by CloseTopicWithFinal, which the run's OWNING replica calls
locally when the run ends. A topic opened here by the on-demand opener (a browser whose socket landed
on the replica that did not serve the run's POST — with two replicas, about half of all run
permalinks) therefore had nothing to close it: the owner's close is hub-local and never crosses the
bus, so the entry, its ring, its seq counter and its bus subscription goroutine stayed alive for the
process's lifetime. maxEphemeralTopics is 256, and once 256 such entries accumulate OpenTopic
refuses every new topic — that replica stops serving live run progress entirely, and nothing about
it is visible except runs whose pages never move.

The predicate closes those entries from the outside, so the ordinary closed → reapDelay → free path
takes over. Call before serving; nil leaves the reaper as it was.
*/
func (h *Hub) SetEphemeralLiveness(live func(topic string) bool) {
	h.mu.Lock()
	h.liveEphemeral = live
	h.mu.Unlock()
}

// Epoch identifies this hub's sequence numbering; exported for tests and for the handshake.
func (h *Hub) Epoch() string { return h.epoch }

func (h *Hub) unsubscribe(c *client, topic string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(c.topics, topic)
}

/*
regate re-applies this connection's authorizer to the topics it is ALREADY subscribed to, dropping
the ones it may no longer have, and reports which went.

The per-topic gate used to run once per subscribe and never again, so narrowing a role left an open
socket streaming the very snapshots the REST routes had begun refusing -- revocation only took effect
when the browser tab closed. Closing the whole connection instead was the first fix and it was too
blunt: it costs every OTHER topic on that socket a reconnect and a resubscribe for a change that
touched one of them. Dropping exactly the topics that are no longer permitted is the behaviour the
REST side already has, one route at a time.

The authorizer is the connection's own closure and reads the current subject, so this needs no
argument: the caller decides WHEN to re-ask, not what the answer is.
*/
func (h *Hub) regate(c *client) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c.authorize == nil {
		return nil
	}
	var dropped []string
	for topic := range c.topics {
		if err := c.authorize(topic); err != nil {
			delete(c.topics, topic)
			dropped = append(dropped, topic)
		}
	}
	slices.Sort(dropped) // a stable order keeps the error frames (and the tests) deterministic
	return dropped
}

// topicAllowed reports whether topic may be subscribed.
func (h *Hub) topicAllowed(topic string) bool {
	if _, ok := allowedTopics[topic]; ok {
		return true
	}
	h.mu.Lock()
	_, ok := h.ephemeral[topic]
	h.mu.Unlock()
	return ok
}

// OpenTopic registers an ephemeral topic and starts a bus subscription feeding it; refusing to open
// a topic must never refuse to run a check.
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
	// Reserve the slot now, under h.mu, before releasing it.
	et := &ephemeralTopic{}
	h.ephemeral[topic] = et
	h.metrics.WSTopics.WithLabelValues().Set(float64(len(h.ephemeral)))
	h.mu.Unlock()

	msgs, unsubscribe := h.bus.Subscribe(topic)
	subCtx, cancel := context.WithCancel(ctx)

	h.mu.Lock()
	if h.ephemeral[topic] != et {
		// Our reservation is gone (or replaced): the hub shut down, or the topic was closed and reaped,
		// while Subscribe was in flight.
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

// CloseTopic marks topic terminal and broadcasts one TypeClosed control frame on it; that frame IS
// the topic's terminal signal.
func (h *Hub) CloseTopic(topic string) {
	if !h.markClosed(topic) {
		return
	}
	h.Broadcast(topic, TypeClosed, json.RawMessage(`{}`))
}

// CloseTopicWithFinal marks topic terminal and broadcasts final (as msgType) immediately followed
// by the TypeClosed control frame; this is the seam a caller with its own terminal payload for the
// topic (the checks.Runner's finished-run summary, currently the only caller) must use instead of
// publishing that payload through the async path.
func (h *Hub) CloseTopicWithFinal(topic, msgType string, final json.RawMessage) {
	if !h.markClosed(topic) {
		return
	}
	h.Broadcast(topic, msgType, final)
	h.Broadcast(topic, TypeClosed, json.RawMessage(`{}`))
}

// markClosed marks topic terminal (closed=true, closedAt=now) under h.mu and reports whether it
// just did so.
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

// TopicSeq reports the last sequence number assigned on topic (0 when the topic has never carried a
// frame).
func (h *Hub) TopicSeq(topic string) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seq[topic]
}

// runEphemeralTopic feeds one ephemeral topic's own bus subscription into Broadcast.
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

/*
sweepEphemeralLiveness closes the ephemeral topics whose runs have ended, so the reclaim path above
can free them.

An entry only ever reaches `closed` through CloseTopicWithFinal, which the run's OWNING replica
calls locally. A topic this replica opened on demand (for a browser whose socket landed here rather
than on the owner) therefore never closes on its own and is invisible to both reclaim paths — a
permanent leak, and at maxEphemeralTopics of them this replica stops opening run topics at all.

It runs in a goroutine of its OWN, not on Hub.Run's select loop, and that is the point: the
predicate reads the run store, one topic at a time, with a 3s timeout each. On the loop, a slow
database turned a sweep of 256 topics into up to twelve minutes during which the hub relayed no live
events at all and its bus subscription backed up — a reclaim path taking down the thing it serves.
Off the loop it costs nothing that matters, and sweepBatch bounds one pass so a backlog is worked
through over several ticks instead of in one long run.

sweeping keeps ticks from piling up on a store slower than the tick interval: a sweep still running
when the next tick fires simply skips that tick.
*/
func (h *Hub) sweepEphemeralLiveness() {
	if !h.sweeping.CompareAndSwap(false, true) {
		return
	}
	defer h.sweeping.Store(false)

	h.mu.Lock()
	live := h.liveEphemeral
	var candidates []string
	if live != nil {
		for topic, et := range h.ephemeral {
			if !et.closed {
				candidates = append(candidates, topic)
				if len(candidates) == sweepBatch {
					break
				}
			}
		}
	}
	h.mu.Unlock()

	for _, topic := range candidates {
		// markClosed takes h.mu itself and is a no-op for anything closed in the meantime.
		if !live(topic) {
			h.markClosed(topic)
		}
	}
}

// evictOldestClosedLocked frees one registry slot by reaping the closed topic with the earliest
// CloseTopic call; reports whether an eviction happened.
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

// reapTopicLocked removes topic from the ephemeral registry: cancels its bus subscription
// goroutine.
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
func (h *Hub) appendRingLocked(env *Envelope) {
	ring := h.rings[env.Topic]
	ring = append(ring, *env)
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

// deliver hands env to one client, or drops the client if its buffer is full; only the STATIC
// allowlist is counted here.
func (h *Hub) deliver(c *client, env *Envelope) {
	select {
	case c.send <- *env:
		if _, ok := allowedTopics[env.Topic]; ok {
			h.metrics.WSMessagesSent.WithLabelValues(env.Topic).Inc()
		}
	default:
		h.dropSlowClient(c)
	}
}

// replayDeliverTimeout bounds ONE replay frame's handover to a client that is not draining. It is
// generous next to a write pump doing its job and finite next to a socket that has stopped.
const replayDeliverTimeout = 5 * time.Second

/*
 * deliverReplay hands one REPLAY frame to c, waiting rather than dropping the client.
 *
 * Replay is the hub answering a subscribe, not the hub falling behind: a subscribe pushed the whole
 * matching ring into a 256-slot channel from the read pump, faster than the write pump drains it, so
 * two subscribes to a full `live` ring on one connection overflowed the buffer and the hub closed
 * the socket — logging the client as slow for having asked a legal question. A client's own replay
 * must not count against the slow-client rule.
 */
func (h *Hub) deliverReplay(c *client, env *Envelope) {
	timer := time.NewTimer(replayDeliverTimeout)
	defer timer.Stop()
	select {
	case c.send <- *env:
	case <-c.done:
	case <-timer.C:
		// It really is not reading: this is the slow-client case, and it is judged on TIME rather
		// than on the depth of a queue the hub itself filled.
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

// sendError replies with an error envelope on the topic the client asked about.
func (h *Hub) sendError(c *client, topic, detail string) {
	data, err := json.Marshal(errorPayload{Error: detail})
	if err != nil { // unreachable: the payload is one plain string
		data = json.RawMessage(`{"error":"internal error"}`)
	}
	h.deliver(c, &Envelope{Topic: topic, Type: TypeError, Data: data})
}

// quote renders s for an error message without pulling in fmt.
func quote(s string) string { return `"` + s + `"` }

// idSet is a bounded set of live-event ids with insertion-ordered eviction: oldest id out first;
// controller sequence numbers only grow, so oldest-first is the same as least-recently-used here.
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

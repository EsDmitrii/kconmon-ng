package ws

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

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

// Hub fans messages out to the WebSocket clients of one console replica.
type Hub struct {
	bus     cache.Bus
	metrics *metrics.Metrics
	dedupe  *idSet

	mu      sync.Mutex
	closed  bool // set once Run has shut the hub down; one-way — a Hub is per-process and cannot be restarted after Run returns
	clients map[*client]struct{}
	seq     map[string]uint64
	rings   map[string][]Envelope
}

// NewHub returns a hub that will read the live topic from bus once Run is
// called. Broadcast and ServeWS work without Run — snapshot topics do not
// depend on the bus at all.
func NewHub(bus cache.Bus, m *metrics.Metrics) *Hub {
	return &Hub{
		bus:     bus,
		metrics: m,
		dedupe:  newIDSet(dedupeCacheSize),
		clients: make(map[*client]struct{}),
		seq:     make(map[string]uint64),
		rings:   make(map[string][]Envelope),
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

	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-msgs:
			if !ok {
				return
			}
			h.fanOutLive(msg)
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
// validated here — callers are trusted server code; an unbounded caller-chosen
// topic set would grow seq/rings forever, which M3's run:{id} topics will have
// to address.
func (h *Hub) Broadcast(topic, msgType string, data json.RawMessage) {
	h.mu.Lock()
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

// closeAllClients drops every client and marks the hub closed, used by Run on
// shutdown. After it runs, register refuses new clients (see register).
func (h *Hub) closeAllClients() {
	h.mu.Lock()
	h.closed = true
	doomed := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		doomed = append(doomed, c)
	}
	h.clients = make(map[*client]struct{})
	h.metrics.WSClients.WithLabelValues().Set(0)
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
		if !topicAllowed(msg.Topic) {
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
	if topic == TopicLive {
		return liveRingSize
	}
	return snapshotRingSize
}

// deliver hands env to one client, or drops the client if its buffer is full.
// Never blocks.
//
// Only allowlisted topics are counted: error frames echo the client-chosen
// topic string verbatim (sendError), so counting every frame would let any
// browser mint unbounded ws_messages_sent_total series by looping subscribes
// to random topics — the bounded-cardinality constraint forbids that. Data
// frames only ever carry allowlisted topics, so they are always counted.
func (h *Hub) deliver(c *client, env Envelope) {
	select {
	case c.send <- env:
		if topicAllowed(env.Topic) {
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

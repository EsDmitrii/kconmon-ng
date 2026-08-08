package ws

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/events"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// countingBus wraps InProcessBus and counts unsubscribe calls, so a shutdown
// test can assert every ephemeral-topic subscription was actually torn down
// rather than merely assuming it from the absence of a panic.
type countingBus struct {
	*cache.InProcessBus
	unsubscribes atomic.Int64
}

var _ cache.Bus = (*countingBus)(nil)

func newCountingBus() *countingBus {
	return &countingBus{InProcessBus: cache.NewInProcessBus()}
}

func (b *countingBus) Subscribe(topic string) (<-chan cache.Message, func()) {
	msgs, unsubscribe := b.InProcessBus.Subscribe(topic)
	return msgs, func() {
		unsubscribe()
		b.unsubscribes.Add(1)
	}
}

func newTestHub(t *testing.T, bus cache.Bus) (*Hub, *metrics.Metrics) {
	t.Helper()
	m := metrics.New("kconmon_ng", prometheus.NewRegistry())
	return NewHub(bus, m), m
}

// nextEnvelope reads one frame from the client's send buffer.
func nextEnvelope(t *testing.T, c *client) Envelope {
	t.Helper()
	select {
	case env := <-c.send:
		return env
	case <-time.After(2 * time.Second):
		t.Fatal("no envelope was delivered within 2s")
		return Envelope{}
	}
}

// expectNoEnvelope asserts nothing else is delivered.
func expectNoEnvelope(t *testing.T, c *client) {
	t.Helper()
	select {
	case env := <-c.send:
		t.Fatalf("unexpected extra envelope: %+v", env)
	case <-time.After(100 * time.Millisecond):
	}
}

func subscribeClient(h *Hub, c *client, topic string, lastSeq uint64) {
	h.handleClientMessage(c, ClientMessage{Action: ActionSubscribe, Topic: topic, LastSeq: lastSeq})
}

func liveEventBytes(t *testing.T, id string, seq uint64) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(events.LiveEvent{
		ID:        id,
		Seq:       seq,
		Type:      "topology_changed",
		Severity:  "info",
		Scope:     "cluster",
		Timestamp: time.Unix(0, 1753400000000000000).UTC(),
		Summary:   "topology changed: agent_registered",
		Details:   json.RawMessage(`{"reason":"agent_registered","nodeName":"","agentId":""}`),
	})
	if err != nil {
		t.Fatalf("marshal LiveEvent: %v", err)
	}
	return raw
}

func TestMatrixTopicShape(t *testing.T) {
	for protocol, want := range map[string]string{
		"tcp":  "matrix:tcp:pod",
		"udp":  "matrix:udp:pod",
		"icmp": "matrix:icmp:pod",
	} {
		if got := MatrixTopic(protocol); got != want {
			t.Errorf("MatrixTopic(%q) = %q, want %q", protocol, got, want)
		}
	}
}

// TopicLive must equal the bus topic internal/console/events publishes on (its
// unexported busTopicLive). The two constants cannot reference each other — ws
// imports events so the Hub can dedupe by LiveEvent.ID, so events cannot import
// ws — so each side pins the same literal: this test, and liveBusTopic in
// internal/console/events/ingester_test.go.
func TestTopicLiveIsTheBusTopicTheIngesterPublishesOn(t *testing.T) {
	if TopicLive != "live" {
		t.Errorf("TopicLive = %q, want %q", TopicLive, "live")
	}
}

func TestHubBroadcastAssignsIndependentPerTopicSequences(t *testing.T) {
	h, m := newTestHub(t, cache.NewInProcessBus())
	c := h.register(nil)
	defer h.unregister(c)

	subscribeClient(h, c, TopicTopology, 0)
	subscribeClient(h, c, MatrixTopic("tcp"), 0)

	h.Broadcast(TopicTopology, TypeSnapshot, json.RawMessage(`{"n":1}`))
	h.Broadcast(MatrixTopic("tcp"), TypeSnapshot, json.RawMessage(`{"m":1}`))
	h.Broadcast(TopicTopology, TypeSnapshot, json.RawMessage(`{"n":2}`))

	want := []Envelope{
		{Topic: TopicTopology, Type: TypeSnapshot, Seq: 1, Data: json.RawMessage(`{"n":1}`)},
		{Topic: MatrixTopic("tcp"), Type: TypeSnapshot, Seq: 1, Data: json.RawMessage(`{"m":1}`)},
		{Topic: TopicTopology, Type: TypeSnapshot, Seq: 2, Data: json.RawMessage(`{"n":2}`)},
	}
	for i, w := range want {
		got := nextEnvelope(t, c)
		if got.Topic != w.Topic || got.Type != w.Type || got.Seq != w.Seq || string(got.Data) != string(w.Data) {
			t.Errorf("frame %d = %+v, want %+v", i, got, w)
		}
	}

	if got := testutil.ToFloat64(m.WSMessagesSent.WithLabelValues(TopicTopology)); got != 2 {
		t.Errorf("ws_messages_sent_total{topic=topology} = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.WSMessagesSent.WithLabelValues(MatrixTopic("tcp"))); got != 1 {
		t.Errorf("ws_messages_sent_total{topic=matrix:tcp:pod} = %v, want 1", got)
	}
}

func TestHubBroadcastOnlyReachesSubscribers(t *testing.T) {
	h, _ := newTestHub(t, cache.NewInProcessBus())
	subscribed := h.register(nil)
	defer h.unregister(subscribed)
	idle := h.register(nil)
	defer h.unregister(idle)

	subscribeClient(h, subscribed, TopicTopology, 0)
	h.Broadcast(TopicTopology, TypeSnapshot, json.RawMessage(`{}`))

	if got := nextEnvelope(t, subscribed); got.Topic != TopicTopology {
		t.Errorf("subscriber got %+v", got)
	}
	expectNoEnvelope(t, idle)
}

func TestHubUnsubscribeStopsDelivery(t *testing.T) {
	h, _ := newTestHub(t, cache.NewInProcessBus())
	c := h.register(nil)
	defer h.unregister(c)

	subscribeClient(h, c, TopicTopology, 0)
	h.Broadcast(TopicTopology, TypeSnapshot, json.RawMessage(`{"n":1}`))
	_ = nextEnvelope(t, c)

	h.handleClientMessage(c, ClientMessage{Action: ActionUnsubscribe, Topic: TopicTopology})
	h.Broadcast(TopicTopology, TypeSnapshot, json.RawMessage(`{"n":2}`))
	expectNoEnvelope(t, c)
}

// An unknown topic must be rejected out loud: a silently-ignored subscribe is
// indistinguishable from a healthy-but-idle topic in the browser.
func TestHubUnknownTopicIsRejectedAndNotSubscribed(t *testing.T) {
	h, _ := newTestHub(t, cache.NewInProcessBus())
	c := h.register(nil)
	defer h.unregister(c)

	subscribeClient(h, c, "matrix:sctp:pod", 0)

	got := nextEnvelope(t, c)
	if got.Type != TypeError {
		t.Errorf("frame type = %q, want %q", got.Type, TypeError)
	}
	if got.Topic != "matrix:sctp:pod" {
		t.Errorf("error frame topic = %q, want the requested topic echoed back", got.Topic)
	}
	if got.Seq != 0 {
		t.Errorf("error frame seq = %d, want 0 (an error is not a data frame)", got.Seq)
	}
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(got.Data, &payload); err != nil || payload.Error == "" {
		t.Errorf("error frame data = %s (unmarshal err %v), want a non-empty {\"error\":...}", got.Data, err)
	}

	// Not subscribed: a later broadcast on that topic must not reach the client.
	h.Broadcast("matrix:sctp:pod", TypeSnapshot, json.RawMessage(`{}`))
	expectNoEnvelope(t, c)
}

// The topic in an error frame is the client's own unvalidated string echoed
// back; it must never become a Prometheus label value, or any browser could
// mint unbounded ws_messages_sent_total series by looping junk subscribes.
func TestHubErrorFramesDoNotMintTopicMetricLabels(t *testing.T) {
	h, m := newTestHub(t, cache.NewInProcessBus())
	c := h.register(nil)
	defer h.unregister(c)

	for i := 0; i < 5; i++ {
		subscribeClient(h, c, "junk:"+strconv.Itoa(i), 0)
		if got := nextEnvelope(t, c); got.Type != TypeError {
			t.Fatalf("frame %d type = %q, want %q", i, got.Type, TypeError)
		}
	}

	if got := testutil.CollectAndCount(m.WSMessagesSent); got != 0 {
		t.Errorf("ws_messages_sent_total has %d series, want 0 — a client-chosen topic minted a label", got)
	}
}

func TestHubUnknownActionIsRejected(t *testing.T) {
	h, _ := newTestHub(t, cache.NewInProcessBus())
	c := h.register(nil)
	defer h.unregister(c)

	h.handleClientMessage(c, ClientMessage{Action: "resubscribe", Topic: TopicLive})

	got := nextEnvelope(t, c)
	if got.Type != TypeError {
		t.Errorf("frame type = %q, want %q", got.Type, TypeError)
	}
	if got.Topic != TopicLive {
		t.Errorf("error frame topic = %q, want %q", got.Topic, TopicLive)
	}
}

// A snapshot topic keeps exactly one ring entry, so a fresh subscriber gets the
// current state immediately instead of waiting for the next push interval.
func TestHubSnapshotRingReplaysOnlyTheNewest(t *testing.T) {
	h, _ := newTestHub(t, cache.NewInProcessBus())

	for i := 1; i <= 3; i++ {
		h.Broadcast(TopicTopology, TypeSnapshot, json.RawMessage(`{"n":`+strconv.Itoa(i)+`}`))
	}

	c := h.register(nil)
	defer h.unregister(c)
	subscribeClient(h, c, TopicTopology, 0)

	got := nextEnvelope(t, c)
	if got.Seq != 3 || string(got.Data) != `{"n":3}` {
		t.Errorf("replayed %+v, want only the newest snapshot (seq 3)", got)
	}
	expectNoEnvelope(t, c)
}

func TestHubLiveRingReplaysOnlyFramesAfterLastSeq(t *testing.T) {
	h, _ := newTestHub(t, cache.NewInProcessBus())

	for i := 1; i <= 5; i++ {
		h.Broadcast(TopicLive, TypeEvent, json.RawMessage(`{"i":`+strconv.Itoa(i)+`}`))
	}

	c := h.register(nil)
	defer h.unregister(c)
	subscribeClient(h, c, TopicLive, 3)

	for _, wantSeq := range []uint64{4, 5} {
		if got := nextEnvelope(t, c); got.Seq != wantSeq {
			t.Errorf("replayed seq %d, want %d", got.Seq, wantSeq)
		}
	}
	expectNoEnvelope(t, c)
}

func TestHubLiveRingIsBounded(t *testing.T) {
	h, _ := newTestHub(t, cache.NewInProcessBus())

	const published = liveRingSize + 50
	for i := 1; i <= published; i++ {
		h.Broadcast(TopicLive, TypeEvent, json.RawMessage(`{"i":`+strconv.Itoa(i)+`}`))
	}

	c := h.register(nil)
	defer h.unregister(c)
	subscribeClient(h, c, TopicLive, 0)

	first := nextEnvelope(t, c)
	if want := uint64(published - liveRingSize + 1); first.Seq != want {
		t.Errorf("oldest replayed seq = %d, want %d (the ring holds %d)", first.Seq, want, liveRingSize)
	}
	count := 1
	for {
		select {
		case env := <-c.send:
			count++
			if env.Seq != uint64(published-liveRingSize+count) {
				t.Fatalf("replay is out of order at position %d: seq %d", count, env.Seq)
			}
		case <-time.After(100 * time.Millisecond):
			if count != liveRingSize {
				t.Fatalf("replayed %d frames, want %d", count, liveRingSize)
			}
			return
		}
	}
}

// The required dedupe test. Every replica ingests the controller stream and
// publishes to the same bus channel, so the same LiveEvent arrives here once per
// replica; only the first may reach a browser.
func TestHubDedupesLiveEventsByID(t *testing.T) {
	bus := cache.NewInProcessBus()
	h, m := newTestHub(t, bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); h.Run(ctx) }()

	c := h.register(nil)
	subscribeClient(h, c, TopicLive, 0)

	duplicate := liveEventBytes(t, "17-1753400000000000000", 17)
	// Publish until the hub's own bus subscription is demonstrably live, rather
	// than sleeping on a guess: the first delivered frame proves Run is reading.
	deadline := time.Now().Add(5 * time.Second)
	var first Envelope
	for {
		if err := bus.Publish(ctx, TopicLive, cache.Message{Type: TypeEvent, Data: duplicate}); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		select {
		case first = <-c.send:
		case <-time.After(20 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("the hub never delivered the first live event")
			}
			continue
		}
		break
	}
	if first.Topic != TopicLive || first.Type != TypeEvent || first.Seq != 1 {
		t.Errorf("first live frame = %+v, want topic=live type=event seq=1", first)
	}

	// Every republish of the same id is a duplicate — including the extra ones
	// the loop above may already have sent.
	for i := 0; i < 3; i++ {
		if err := bus.Publish(ctx, TopicLive, cache.Message{Type: TypeEvent, Data: duplicate}); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}
	// A different id must still get through, and it proves the duplicates above
	// were processed and dropped rather than merely delayed.
	distinct := liveEventBytes(t, "18-1753400000000000000", 18)
	if err := bus.Publish(ctx, TopicLive, cache.Message{Type: TypeEvent, Data: distinct}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	second := nextEnvelope(t, c)
	if second.Seq != 2 {
		t.Errorf("second delivered frame seq = %d, want 2 — a duplicate was fanned out", second.Seq)
	}
	var live events.LiveEvent
	if err := json.Unmarshal(second.Data, &live); err != nil {
		t.Fatalf("second frame payload: %v", err)
	}
	if live.ID != "18-1753400000000000000" {
		t.Errorf("second frame id = %q, want the distinct event", live.ID)
	}
	expectNoEnvelope(t, c)

	if got := testutil.ToFloat64(m.EventsDeduped.WithLabelValues()); got < 3 {
		t.Errorf("events_deduped_total = %v, want at least 3", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestHubDropsMalformedAndIDLessLiveMessages(t *testing.T) {
	bus := cache.NewInProcessBus()
	h, m := newTestHub(t, bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); h.Run(ctx) }()

	c := h.register(nil)
	subscribeClient(h, c, TopicLive, 0)

	// Barrier: publish valid events with UNIQUE ids until one is delivered, so
	// the hub's bus subscription is demonstrably live before the malformed
	// publishes — otherwise those could be dropped by the bus itself (no
	// subscriber yet) and the test would pass vacuously. Unique ids keep
	// events_deduped_total at zero.
	deadline := time.Now().Add(5 * time.Second)
	for i := 1; ; i++ {
		barrier := liveEventBytes(t, "barrier-"+strconv.Itoa(i), uint64(i))
		if err := bus.Publish(ctx, TopicLive, cache.Message{Type: TypeEvent, Data: barrier}); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		delivered := false
		select {
		case <-c.send:
			delivered = true
		case <-time.After(20 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("the hub never delivered the first live event")
			}
		}
		if delivered {
			break
		}
	}

	// Each malformed pair is followed by a uniquely-ID'd valid probe, and the
	// loop waits for that probe before publishing the next pair. The hub reads
	// its subscription serially, so a delivered probe proves the two frames
	// ahead of it were processed — and dropped — rather than merely still queued.
	//
	// The pacing is load-bearing, not style. cache.InProcessBus gives each
	// subscriber a 32-slot channel and drops on full instead of blocking the
	// publisher (inprocess.go: localSubscriberBuffer). The earlier shape of this
	// test fired 41 publishes in one tight loop past those 32 slots and then
	// waited on a single trailing sentinel, so whenever the hub goroutine was
	// not scheduled fast enough the bus threw the sentinel away and the test
	// timed out. That is a bounded-channel overflow by construction, not a race
	// in the hub: it merely looked like a rare flake on a fast machine while
	// failing constantly on a loaded CI runner. Waiting per iteration keeps at
	// most three messages in flight, and asserting per pair is strictly stronger
	// than one sentinel for the whole batch.
	for i := 0; i < 20; i++ {
		if err := bus.Publish(ctx, TopicLive, cache.Message{Type: TypeEvent, Data: json.RawMessage(`{not json`)}); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		if err := bus.Publish(ctx, TopicLive, cache.Message{Type: TypeEvent, Data: json.RawMessage(`{"seq":1}`)}); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		probeID := "probe-" + strconv.Itoa(i)
		probe := liveEventBytes(t, probeID, uint64(1000+i))
		if err := bus.Publish(ctx, TopicLive, cache.Message{Type: TypeEvent, Data: probe}); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		for {
			env := nextEnvelope(t, c)
			var live events.LiveEvent
			if err := json.Unmarshal(env.Data, &live); err != nil {
				t.Fatalf("a delivered frame is not a LiveEvent (%s): %v", env.Data, err)
			}
			if live.ID == probeID {
				break
			}
			// Only barrier frames may precede a probe: barriers were published
			// before this loop, and every earlier probe was already consumed by
			// the iteration that published it. A delivered ID-less frame
			// ({"seq":1}) would unmarshal cleanly, so it has to be rejected
			// explicitly here instead of being skipped as a "straggler".
			if !strings.HasPrefix(live.ID, "barrier-") {
				t.Fatalf("non-barrier frame %q was delivered — malformed/ID-less events must be dropped", live.ID)
			}
		}
	}
	expectNoEnvelope(t, c)
	if got := testutil.ToFloat64(m.EventsDeduped.WithLabelValues()); got != 0 {
		t.Errorf("events_deduped_total = %v, want 0 — undecodable events are dropped, not deduped", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestHubDropsSlowClientInsteadOfBlocking(t *testing.T) {
	h, m := newTestHub(t, cache.NewInProcessBus())
	c := h.register(nil)
	subscribeClient(h, c, TopicTopology, 0)

	// Nothing drains c.send, so one frame past the buffer must drop the client.
	blocked := make(chan struct{})
	go func() {
		defer close(blocked)
		for i := 0; i <= sendBuffer; i++ {
			h.Broadcast(TopicTopology, TypeSnapshot, json.RawMessage(`{}`))
		}
	}()
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("Broadcast blocked on a client that is not reading")
	}

	if got := h.ClientCount(); got != 0 {
		t.Errorf("ClientCount = %d, want 0 after the drop", got)
	}
	select {
	case <-c.done:
	case <-time.After(time.Second):
		t.Error("the dropped client's done channel was not closed")
	}
	if got := testutil.ToFloat64(m.WSDroppedClients.WithLabelValues()); got != 1 {
		t.Errorf("ws_dropped_clients_total = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.WSClients.WithLabelValues()); got != 0 {
		t.Errorf("ws_clients = %v, want 0", got)
	}

	// Dropping is idempotent: a later broadcast must not count a second drop.
	h.Broadcast(TopicTopology, TypeSnapshot, json.RawMessage(`{}`))
	if got := testutil.ToFloat64(m.WSDroppedClients.WithLabelValues()); got != 1 {
		t.Errorf("ws_dropped_clients_total = %v after a second broadcast, want 1", got)
	}
}

func TestHubClientCountAndGaugeTrackRegistration(t *testing.T) {
	h, m := newTestHub(t, cache.NewInProcessBus())

	a := h.register(nil)
	b := h.register(nil)
	if got := h.ClientCount(); got != 2 {
		t.Fatalf("ClientCount = %d, want 2", got)
	}
	if got := testutil.ToFloat64(m.WSClients.WithLabelValues()); got != 2 {
		t.Errorf("ws_clients = %v, want 2", got)
	}

	h.unregister(a)
	h.unregister(a) // idempotent
	if got := h.ClientCount(); got != 1 {
		t.Errorf("ClientCount = %d, want 1", got)
	}
	if got := testutil.ToFloat64(m.WSClients.WithLabelValues()); got != 1 {
		t.Errorf("ws_clients = %v, want 1", got)
	}

	h.unregister(b)
	if got := testutil.ToFloat64(m.WSClients.WithLabelValues()); got != 0 {
		t.Errorf("ws_clients = %v, want 0", got)
	}
}

// Task 14's shutdown ordering depends on this: http.Server.Shutdown does not
// track hijacked connections, so cancelling the Hub's context is what releases
// live WebSocket clients.
func TestHubRunClosesEveryClientOnContextCancel(t *testing.T) {
	h, m := newTestHub(t, cache.NewInProcessBus())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); h.Run(ctx) }()

	a := h.register(nil)
	b := h.register(nil)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}

	for name, c := range map[string]*client{"a": a, "b": b} {
		select {
		case <-c.done:
		case <-time.After(time.Second):
			t.Errorf("client %s was not closed when the hub stopped", name)
		}
	}
	if got := h.ClientCount(); got != 0 {
		t.Errorf("ClientCount = %d, want 0", got)
	}
	if got := testutil.ToFloat64(m.WSClients.WithLabelValues()); got != 0 {
		t.Errorf("ws_clients = %v, want 0", got)
	}
}

// An upgrade racing shutdown: a register after Run returned must be refused —
// closed immediately, never inserted — or the hijacked connection leaks forever
// (nothing runs closeAllClients twice) and the gauge climbs after shutdown.
func TestHubRegisterAfterShutdownClosesClientImmediately(t *testing.T) {
	h, m := newTestHub(t, cache.NewInProcessBus())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); h.Run(ctx) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}

	c := h.register(nil)
	select {
	case <-c.done:
	case <-time.After(time.Second):
		t.Error("a client registered after shutdown was not closed immediately")
	}
	if got := h.ClientCount(); got != 0 {
		t.Errorf("ClientCount = %d, want 0", got)
	}
	if got := testutil.ToFloat64(m.WSClients.WithLabelValues()); got != 0 {
		t.Errorf("ws_clients = %v, want 0", got)
	}
}

// --- Task 20: ephemeral run:{id} topics ---

func TestRunTopicShape(t *testing.T) {
	if got, want := RunTopic("abc-123"), "run:abc-123"; got != want {
		t.Errorf("RunTopic(%q) = %q, want %q", "abc-123", got, want)
	}
}

// Without OpenTopic, a run:{id} subscribe is the same M2 rejection as any
// other unknown topic. This is the "before" half of the required test: the
// UI's fallback (REST polling) depends on this staying an out-loud error, not
// a silently-idle topic.
func TestSubscribeToUnopenedRunTopicIsRejected(t *testing.T) {
	h, _ := newTestHub(t, cache.NewInProcessBus())
	c := h.register(nil)
	defer h.unregister(c)

	topic := RunTopic("never-opened")
	subscribeClient(h, c, topic, 0)

	got := nextEnvelope(t, c)
	if got.Type != TypeError {
		t.Errorf("frame type = %q, want %q", got.Type, TypeError)
	}
	if got.Topic != topic {
		t.Errorf("error frame topic = %q, want %q", got.Topic, topic)
	}
}

// OpenTopic makes the same subscribe succeed, and a message published to the
// bus on that topic reaches the client with a per-topic seq starting at 1 --
// the two halves of the required "before/after OpenTopic" test.
func TestOpenTopicMakesSubscribeSucceedAndDeliversBusMessages(t *testing.T) {
	bus := cache.NewInProcessBus()
	h, m := newTestHub(t, bus)
	topic := RunTopic("run-1")

	if !h.OpenTopic(context.Background(), topic) {
		t.Fatal("OpenTopic returned false")
	}
	if got := h.OpenTopicCount(); got != 1 {
		t.Errorf("OpenTopicCount = %d, want 1", got)
	}
	if got := testutil.ToFloat64(m.WSTopics.WithLabelValues()); got != 1 {
		t.Errorf("ws_topics = %v, want 1", got)
	}

	c := h.register(nil)
	defer h.unregister(c)
	subscribeClient(h, c, topic, 0)
	expectNoEnvelope(t, c) // subscribing alone delivers nothing; the ring is empty

	// Publish until the per-topic subscription is demonstrably live, the same
	// barrier pattern the live-topic tests use rather than sleeping on a guess.
	deadline := time.Now().Add(5 * time.Second)
	var got Envelope
	for {
		if err := bus.Publish(context.Background(), topic, cache.Message{Type: TypeEvent, Data: json.RawMessage(`{"step":1}`)}); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		select {
		case got = <-c.send:
		case <-time.After(20 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("no envelope delivered from the ephemeral topic's bus subscription")
			}
			continue
		}
		break
	}
	if got.Topic != topic || got.Type != TypeEvent || got.Seq != 1 || string(got.Data) != `{"step":1}` {
		t.Errorf("delivered %+v, want topic=%s type=event seq=1 data={\"step\":1}", got, topic)
	}
}

// run:{id} data frames must never mint a ws_messages_sent_total series keyed
// by run ID -- the same closed-label-set constraint
// TestHubErrorFramesDoNotMintTopicMetricLabels enforces for client-chosen
// junk topics, now for a controller-assigned but still per-run-unique one.
func TestOpenTopicDataFramesDoNotMintTopicMetricLabels(t *testing.T) {
	bus := cache.NewInProcessBus()
	h, m := newTestHub(t, bus)
	topic := RunTopic("cardinality-check")

	if !h.OpenTopic(context.Background(), topic) {
		t.Fatal("OpenTopic returned false")
	}
	c := h.register(nil)
	defer h.unregister(c)
	subscribeClient(h, c, topic, 0)

	h.Broadcast(topic, TypeEvent, json.RawMessage(`{}`))
	_ = nextEnvelope(t, c)

	if got := testutil.CollectAndCount(m.WSMessagesSent); got != 0 {
		t.Errorf("ws_messages_sent_total has %d series, want 0 -- a run ID minted a label", got)
	}
}

// CloseTopic keeps replay working immediately after (the browser reconnect
// case its doc comment describes), and only after reapDelay has actually
// elapsed is the topic gone -- subscribe errors again, OpenTopicCount drops,
// and h.seq/h.rings hold no trace of the key. That last assertion is the
// leak test this task exists to pass: a topic that is merely inaccessible but
// still present in those maps would still be the unbounded growth Broadcast's
// doc comment warned about.
func TestCloseTopicKeepsReplayThenReapsAfterDelay(t *testing.T) {
	h, m := newTestHub(t, cache.NewInProcessBus())
	topic := RunTopic("closes-then-reaps")

	if !h.OpenTopic(context.Background(), topic) {
		t.Fatal("OpenTopic returned false")
	}
	h.Broadcast(topic, TypeEvent, json.RawMessage(`{"n":1}`))

	h.CloseTopic(topic)
	h.CloseTopic(topic) // idempotent

	// Immediately after close: replay still works.
	c := h.register(nil)
	defer h.unregister(c)
	subscribeClient(h, c, topic, 0)
	if got := nextEnvelope(t, c); got.Seq != 1 || string(got.Data) != `{"n":1}` {
		t.Errorf("replay after close = %+v, want seq=1 data={\"n\":1}", got)
	}
	if got := h.OpenTopicCount(); got != 1 {
		t.Errorf("OpenTopicCount right after close = %d, want 1 (still registered, awaiting reap)", got)
	}

	// Force the reap without sleeping reapDelay: back-date closedAt and run
	// the reaper directly (white-box, same package).
	h.mu.Lock()
	h.ephemeral[topic].closedAt = time.Now().Add(-reapDelay - time.Second)
	h.mu.Unlock()
	h.reapExpiredTopics()

	if got := h.OpenTopicCount(); got != 0 {
		t.Errorf("OpenTopicCount after reap = %d, want 0", got)
	}
	if got := testutil.ToFloat64(m.WSTopics.WithLabelValues()); got != 0 {
		t.Errorf("ws_topics after reap = %v, want 0", got)
	}
	h.mu.Lock()
	_, seqPresent := h.seq[topic]
	_, ringPresent := h.rings[topic]
	_, ephemeralPresent := h.ephemeral[topic]
	h.mu.Unlock()
	if seqPresent {
		t.Error("h.seq still holds the reaped topic's key")
	}
	if ringPresent {
		t.Error("h.rings still holds the reaped topic's key")
	}
	if ephemeralPresent {
		t.Error("h.ephemeral still holds the reaped topic's key")
	}

	c2 := h.register(nil)
	defer h.unregister(c2)
	subscribeClient(h, c2, topic, 0)
	if got := nextEnvelope(t, c2); got.Type != TypeError {
		t.Errorf("post-reap subscribe frame type = %q, want %q", got.Type, TypeError)
	}
}

// A run:{id} topic's ring is append-only like live's, not last-write-wins
// like a snapshot topic: a reconnecting browser must get every progress frame
// after its lastSeq, not just the newest one.
func TestRunTopicRingIsAppendOnlyAndBounded(t *testing.T) {
	h, _ := newTestHub(t, cache.NewInProcessBus())
	topic := RunTopic("append-only")
	if !h.OpenTopic(context.Background(), topic) {
		t.Fatal("OpenTopic returned false")
	}

	const published = runRingSize + 10
	for i := 1; i <= published; i++ {
		h.Broadcast(topic, TypeEvent, json.RawMessage(`{"i":`+strconv.Itoa(i)+`}`))
	}

	c := h.register(nil)
	defer h.unregister(c)
	subscribeClient(h, c, topic, 0)

	first := nextEnvelope(t, c)
	if want := uint64(published - runRingSize + 1); first.Seq != want {
		t.Errorf("oldest replayed seq = %d, want %d (the ring holds %d)", first.Seq, want, runRingSize)
	}
	count := 1
	for {
		select {
		case env := <-c.send:
			count++
			if env.Seq != uint64(published-runRingSize+count) {
				t.Fatalf("replay is out of order at position %d: seq %d", count, env.Seq)
			}
		case <-time.After(100 * time.Millisecond):
			if count != runRingSize {
				t.Fatalf("replayed %d frames, want %d", count, runRingSize)
			}
			return
		}
	}
}

// Opening 256 topics succeeds; the 257th with all of them still open must
// fail closed (the run still executes, per OpenTopic's doc comment); the
// 257th with one CLOSED topic among the 256 evicts that one and succeeds.
func TestOpenTopicCapAndEviction(t *testing.T) {
	h, m := newTestHub(t, cache.NewInProcessBus())
	ctx := context.Background()

	for i := 0; i < maxEphemeralTopics; i++ {
		topic := RunTopic("cap-" + strconv.Itoa(i))
		if !h.OpenTopic(ctx, topic) {
			t.Fatalf("OpenTopic(%d) returned false before the cap", i)
		}
	}
	if got := h.OpenTopicCount(); got != maxEphemeralTopics {
		t.Fatalf("OpenTopicCount = %d, want %d", got, maxEphemeralTopics)
	}
	if got := testutil.ToFloat64(m.WSTopics.WithLabelValues()); got != float64(maxEphemeralTopics) {
		t.Errorf("ws_topics = %v, want %d", got, maxEphemeralTopics)
	}

	// All 256 still open: the 257th must be refused.
	overflowTopic := RunTopic("overflow")
	if h.OpenTopic(ctx, overflowTopic) {
		t.Fatal("OpenTopic succeeded past the cap with nothing closed")
	}
	if got := h.OpenTopicCount(); got != maxEphemeralTopics {
		t.Errorf("OpenTopicCount after a refused open = %d, want %d (unchanged)", got, maxEphemeralTopics)
	}

	// Close one, freeing a slot for the oldest-closed-first eviction.
	evictable := RunTopic("cap-0")
	h.CloseTopic(evictable)

	if !h.OpenTopic(ctx, overflowTopic) {
		t.Fatal("OpenTopic still refused after a topic was closed")
	}
	if got := h.OpenTopicCount(); got != maxEphemeralTopics {
		t.Errorf("OpenTopicCount after eviction = %d, want %d (still at the cap)", got, maxEphemeralTopics)
	}
	if h.topicAllowed(evictable) {
		t.Error("the evicted topic is still in the registry")
	}
	h.mu.Lock()
	_, seqPresent := h.seq[evictable]
	_, ringPresent := h.rings[evictable]
	h.mu.Unlock()
	if seqPresent || ringPresent {
		t.Error("the evicted topic's seq/ring were not freed")
	}
	if !h.topicAllowed(overflowTopic) {
		t.Error("the newly opened topic is not registered")
	}
}

// Hub.Run's shutdown must cancel every ephemeral subscription goroutine, not
// just close WebSocket clients -- otherwise cmd/console's wg.Wait waits on
// goroutines nothing ever tells to stop.
func TestHubRunShutdownClosesEveryEphemeralSubscription(t *testing.T) {
	bus := newCountingBus()
	h, _ := newTestHub(t, bus)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); h.Run(ctx) }()

	const topics = 5
	for i := 0; i < topics; i++ {
		if !h.OpenTopic(context.Background(), RunTopic("shutdown-"+strconv.Itoa(i))) {
			t.Fatalf("OpenTopic(%d) returned false", i)
		}
	}
	if got := h.OpenTopicCount(); got != topics {
		t.Fatalf("OpenTopicCount = %d, want %d", got, topics)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}

	if got := h.OpenTopicCount(); got != 0 {
		t.Errorf("OpenTopicCount after shutdown = %d, want 0", got)
	}
	// The per-topic goroutines' deferred unsubscribe runs asynchronously to
	// closeAllClients cancelling their context, so poll for it rather than
	// asserting immediately. +1 is Run's own TopicLive subscription, torn
	// down by the same shutdown through the same countingBus.
	const wantUnsubscribes = topics + 1
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := bus.unsubscribes.Load(); got == wantUnsubscribes {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("countingBus saw %d unsubscribe calls, want %d", got, wantUnsubscribes)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The registry gauge must track OpenTopic/CloseTopic/reap, not just OpenTopic.
func TestWSTopicsGaugeTracksRegistryAcrossOpenCloseReap(t *testing.T) {
	h, m := newTestHub(t, cache.NewInProcessBus())
	topicA, topicB := RunTopic("gauge-a"), RunTopic("gauge-b")

	if !h.OpenTopic(context.Background(), topicA) {
		t.Fatal("OpenTopic(a) returned false")
	}
	if got := testutil.ToFloat64(m.WSTopics.WithLabelValues()); got != 1 {
		t.Errorf("ws_topics after opening a = %v, want 1", got)
	}

	if !h.OpenTopic(context.Background(), topicB) {
		t.Fatal("OpenTopic(b) returned false")
	}
	if got := testutil.ToFloat64(m.WSTopics.WithLabelValues()); got != 2 {
		t.Errorf("ws_topics after opening b = %v, want 2", got)
	}

	// CloseTopic alone does not free the slot -- only the reaper does.
	h.CloseTopic(topicA)
	if got := testutil.ToFloat64(m.WSTopics.WithLabelValues()); got != 2 {
		t.Errorf("ws_topics right after close = %v, want 2 (still registered)", got)
	}

	h.mu.Lock()
	h.ephemeral[topicA].closedAt = time.Now().Add(-reapDelay - time.Second)
	h.mu.Unlock()
	h.reapExpiredTopics()

	if got := testutil.ToFloat64(m.WSTopics.WithLabelValues()); got != 1 {
		t.Errorf("ws_topics after reaping a = %v, want 1", got)
	}
}

// Concurrent OpenTopic/CloseTopic/Broadcast/subscribe on overlapping topics,
// meant to run under -race. No assertions on delivery outcome -- this is
// purely a data-race and deadlock check for the shared h.mu across the new
// ephemeral-registry paths and the pre-existing client/ring paths.
func TestHubEphemeralTopicsConcurrentAccessRace(t *testing.T) {
	bus := cache.NewInProcessBus()
	h, _ := newTestHub(t, bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan struct{})
	go func() { defer close(runDone); h.Run(ctx) }()

	topics := make([]string, 5)
	for i := range topics {
		topics[i] = RunTopic("race-" + strconv.Itoa(i))
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	for _, topic := range topics {
		wg.Add(1)
		go func(topic string) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				h.OpenTopic(ctx, topic)
				_ = bus.Publish(ctx, topic, cache.Message{Type: TypeEvent, Data: json.RawMessage(`{}`)})
				h.CloseTopic(topic)
			}
		}(topic)
	}

	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := h.register(nil)
			defer h.unregister(c)
			drain := func() {
				for {
					select {
					case <-c.send:
					default:
						return
					}
				}
			}
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, topic := range topics {
					subscribeClient(h, c, topic, 0)
				}
				drain()
			}
		}()
	}

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, topic := range topics {
					h.Broadcast(topic, TypeEvent, json.RawMessage(`{}`))
				}
			}
		}()
	}

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()

	cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// --- Fix pass regression tests (code review on Task 20) ---

// subscribeCountingBus wraps InProcessBus and counts Subscribe calls per
// topic. It exists for TestOpenTopicConcurrentSameTopicSubscribesOnlyOnce:
// OpenTopic must reserve a topic's registry slot atomically with the cap/
// idempotence check BEFORE calling bus.Subscribe, precisely so two racing
// OpenTopic(sameTopic) calls cannot each start their own bus subscription.
type subscribeCountingBus struct {
	*cache.InProcessBus
	mu     sync.Mutex
	counts map[string]int
}

func newSubscribeCountingBus() *subscribeCountingBus {
	return &subscribeCountingBus{InProcessBus: cache.NewInProcessBus(), counts: make(map[string]int)}
}

func (b *subscribeCountingBus) Subscribe(topic string) (<-chan cache.Message, func()) {
	b.mu.Lock()
	b.counts[topic]++
	b.mu.Unlock()
	return b.InProcessBus.Subscribe(topic)
}

func (b *subscribeCountingBus) subscribeCount(topic string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.counts[topic]
}

// OpenTopic must not hold h.mu across h.bus.Subscribe, but two concurrent
// OpenTopic calls for the SAME topic must still result in exactly one bus
// subscription: OpenTopic's reservation (a placeholder ephemeralTopic
// inserted under the lock before Subscribe runs) is what a second, racing
// caller sees as "already open" and returns on without subscribing again.
func TestOpenTopicConcurrentSameTopicSubscribesOnlyOnce(t *testing.T) {
	bus := newSubscribeCountingBus()
	h, _ := newTestHub(t, bus)
	topic := RunTopic("concurrent-open")

	const callers = 20
	results := make([]bool, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = h.OpenTopic(context.Background(), topic)
		}(i)
	}
	wg.Wait()

	for i, ok := range results {
		if !ok {
			t.Errorf("OpenTopic call %d returned false, want true (idempotent open)", i)
		}
	}
	if got := bus.subscribeCount(topic); got != 1 {
		t.Errorf("bus.Subscribe(%q) called %d times, want exactly 1 -- a concurrent OpenTopic race must not double-subscribe", topic, got)
	}
	if got := h.OpenTopicCount(); got != 1 {
		t.Errorf("OpenTopicCount = %d, want 1", got)
	}
}

// The cap check and the registry insert must be atomic across concurrent
// OpenTopic calls for DIFFERENT topics too, or more than one caller could read
// len(h.ephemeral) < maxEphemeralTopics before either had reserved a slot and
// all of them would squeeze past the cap.
func TestOpenTopicConcurrentDifferentTopicsRespectsCapExactly(t *testing.T) {
	h, _ := newTestHub(t, cache.NewInProcessBus())
	ctx := context.Background()

	for i := 0; i < maxEphemeralTopics-1; i++ {
		if !h.OpenTopic(ctx, RunTopic("precap-"+strconv.Itoa(i))) {
			t.Fatalf("OpenTopic(%d) returned false before the cap", i)
		}
	}
	if got := h.OpenTopicCount(); got != maxEphemeralTopics-1 {
		t.Fatalf("OpenTopicCount = %d, want %d", got, maxEphemeralTopics-1)
	}

	// Exactly one slot remains and nothing is closed (so eviction cannot free
	// more). Race several distinct-topic OpenTopic calls for that one slot.
	const racers = 8
	results := make([]bool, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = h.OpenTopic(ctx, RunTopic("racer-"+strconv.Itoa(i)))
		}(i)
	}
	wg.Wait()

	succeeded := 0
	for _, ok := range results {
		if ok {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Errorf("%d of %d racers succeeded, want exactly 1 -- the cap must be enforced atomically", succeeded, racers)
	}
	if got := h.OpenTopicCount(); got != maxEphemeralTopics {
		t.Errorf("OpenTopicCount = %d, want %d (exactly at the cap, never over)", got, maxEphemeralTopics)
	}
}

// The reap-vs-in-flight-Broadcast resurrection race: a Broadcast call that
// read its message off the topic's bus subscription before reapTopicLocked
// ran must NOT recreate h.seq/h.rings once it finally gets h.mu. This test
// forces the interleaving deterministically by holding h.mu itself across the
// publish and the reap, guaranteeing that whenever runEphemeralTopic's
// goroutine gets around to calling Broadcast, it blocks on h.mu until AFTER
// the reap has already run and released it -- so Broadcast's guard is
// exercised against a topic that is provably already gone, not merely
// probably gone by scheduling luck.
func TestBroadcastDoesNotResurrectAReapedRunTopic(t *testing.T) {
	bus := cache.NewInProcessBus()
	h, _ := newTestHub(t, bus)
	topic := RunTopic("resurrection")

	if !h.OpenTopic(context.Background(), topic) {
		t.Fatal("OpenTopic returned false")
	}

	h.mu.Lock()
	if err := bus.Publish(context.Background(), topic, cache.Message{Type: TypeEvent, Data: json.RawMessage(`{"n":1}`)}); err != nil {
		h.mu.Unlock()
		t.Fatalf("Publish: %v", err)
	}
	// Give runEphemeralTopic's goroutine a chance to read the message and reach
	// its own h.mu.Lock() call inside Broadcast -- there is no signal for
	// "blocked on a mutex" to wait on instead, but correctness here does not
	// actually depend on the goroutine having reached that point yet: h.mu is
	// held for the whole publish-then-reap sequence below, so whenever the
	// goroutine's Broadcast call does acquire the lock -- before this sleep,
	// during it, or after -- it can only do so once the reap (also inside this
	// critical section) has already completed.
	time.Sleep(50 * time.Millisecond)

	et := h.ephemeral[topic]
	et.closed = true
	et.closedAt = time.Now().Add(-reapDelay - time.Second)
	h.reapTopicLocked(topic)
	h.mu.Unlock()

	// Let the (now-guarded) Broadcast call run to completion.
	time.Sleep(100 * time.Millisecond)

	h.mu.Lock()
	_, seqPresent := h.seq[topic]
	_, ringPresent := h.rings[topic]
	_, ephemeralPresent := h.ephemeral[topic]
	h.mu.Unlock()
	if seqPresent {
		t.Error("h.seq was resurrected by a Broadcast racing the reap")
	}
	if ringPresent {
		t.Error("h.rings was resurrected by a Broadcast racing the reap")
	}
	if ephemeralPresent {
		t.Error("h.ephemeral was resurrected by a Broadcast racing the reap")
	}
}

// CloseTopic's terminal frame is what tells an already-subscribed client the
// run is over instead of the topic silently going idle. A second CloseTopic
// call must not send a second one -- idempotence applies to the broadcast,
// not just the registry state.
func TestCloseTopicBroadcastsTerminalFrameToSubscribedClient(t *testing.T) {
	h, _ := newTestHub(t, cache.NewInProcessBus())
	topic := RunTopic("terminal-frame")
	if !h.OpenTopic(context.Background(), topic) {
		t.Fatal("OpenTopic returned false")
	}

	c := h.register(nil)
	defer h.unregister(c)
	subscribeClient(h, c, topic, 0)
	expectNoEnvelope(t, c) // nothing yet -- the ring is empty

	h.CloseTopic(topic)

	got := nextEnvelope(t, c)
	if got.Topic != topic {
		t.Errorf("terminal frame topic = %q, want %q", got.Topic, topic)
	}
	if got.Type != TypeClosed {
		t.Errorf("terminal frame type = %q, want %q", got.Type, TypeClosed)
	}
	if got.Seq != 1 {
		t.Errorf("terminal frame seq = %d, want 1 (it is a real data frame, unlike an error)", got.Seq)
	}

	h.CloseTopic(topic) // idempotent: no second terminal frame
	expectNoEnvelope(t, c)
}

// CloseTopicWithFinal must deliver its final data frame strictly before the
// TypeClosed control frame -- lower Seq, and first in delivery order -- to a
// subscribed client, and must do so as one atomic pair (a second call after
// the topic is already closed sends neither frame). This is the ordering
// guarantee task-22-brief.md's I-2 exists to make structural instead of a
// race between an async bus-fed publish and CloseTopic's own synchronous
// Broadcast (see CloseTopicWithFinal's doc comment).
func TestCloseTopicWithFinalOrdersFinalFrameStrictlyBeforeClosed(t *testing.T) {
	h, _ := newTestHub(t, cache.NewInProcessBus())
	topic := RunTopic("final-before-closed")
	if !h.OpenTopic(context.Background(), topic) {
		t.Fatal("OpenTopic returned false")
	}

	c := h.register(nil)
	defer h.unregister(c)
	subscribeClient(h, c, topic, 0)
	expectNoEnvelope(t, c) // nothing yet -- the ring is empty

	final := json.RawMessage(`{"state":"finished","status":"succeeded"}`)
	h.CloseTopicWithFinal(topic, TypeEvent, final)

	gotFinal := nextEnvelope(t, c)
	if gotFinal.Type != TypeEvent {
		t.Errorf("first frame type = %q, want %q", gotFinal.Type, TypeEvent)
	}
	if string(gotFinal.Data) != string(final) {
		t.Errorf("first frame data = %s, want %s", gotFinal.Data, final)
	}

	gotClosed := nextEnvelope(t, c)
	if gotClosed.Type != TypeClosed {
		t.Errorf("second frame type = %q, want %q", gotClosed.Type, TypeClosed)
	}

	if gotFinal.Seq >= gotClosed.Seq {
		t.Errorf("final frame seq %d, TypeClosed seq %d -- want final strictly lower", gotFinal.Seq, gotClosed.Seq)
	}
	expectNoEnvelope(t, c)

	// Idempotent, exactly like CloseTopic: a second call after the topic is
	// already closed broadcasts neither frame.
	h.CloseTopicWithFinal(topic, TypeEvent, final)
	expectNoEnvelope(t, c)
}

// reapTopicLocked must clear the reaped topic out of every currently
// subscribed client's c.topics map, or a long-lived connection that
// subscribed to many short-lived run:{id} topics over its lifetime would
// accumulate a dead entry per run forever.
func TestReapTopicLockedClearsTopicFromSubscribedClientsTopicsMap(t *testing.T) {
	h, _ := newTestHub(t, cache.NewInProcessBus())
	topic := RunTopic("reap-clears-client")
	if !h.OpenTopic(context.Background(), topic) {
		t.Fatal("OpenTopic returned false")
	}

	c := h.register(nil)
	defer h.unregister(c)
	subscribeClient(h, c, topic, 0)

	h.mu.Lock()
	_, subscribed := c.topics[topic]
	h.mu.Unlock()
	if !subscribed {
		t.Fatal("test setup: client is not subscribed to the topic")
	}

	h.CloseTopic(topic)
	_ = nextEnvelope(t, c) // drain the terminal frame

	h.mu.Lock()
	h.ephemeral[topic].closedAt = time.Now().Add(-reapDelay - time.Second)
	h.mu.Unlock()
	h.reapExpiredTopics()

	h.mu.Lock()
	_, stillSubscribed := c.topics[topic]
	h.mu.Unlock()
	if stillSubscribed {
		t.Error("reap left the topic in the client's topics map -- a long-lived connection would accumulate dead entries")
	}
}

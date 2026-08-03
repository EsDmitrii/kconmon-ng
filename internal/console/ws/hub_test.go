package ws

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/events"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

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
	c := h.register()
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
	subscribed := h.register()
	defer h.unregister(subscribed)
	idle := h.register()
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
	c := h.register()
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
	c := h.register()
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
	c := h.register()
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
	c := h.register()
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

	c := h.register()
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

	c := h.register()
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

	c := h.register()
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

	c := h.register()
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

	c := h.register()
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
	c := h.register()
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

	a := h.register()
	b := h.register()
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

	a := h.register()
	b := h.register()

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

	c := h.register()
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

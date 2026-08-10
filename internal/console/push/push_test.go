package push_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/events"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/push"
	"github.com/EsDmitrii/kconmon-ng/internal/console/ws"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// errQuerierDown is what the fake Prometheus returns in the failure tests.
var errQuerierDown = errors.New("prometheus is down")

// newTestHub builds a real Hub with a fresh registry and no WebSocket clients.
func newTestHub(t *testing.T) (*ws.Hub, *metrics.Metrics) {
	t.Helper()
	hub, m, _ := newTestHubOnBus(t)
	return hub, m
}

// newTestHubOnBus is newTestHub with the Hub's underlying bus exposed, for the
// tests that pin design point 1: snapshot topics are hub-local and must never
// travel over cache.Bus.
func newTestHubOnBus(t *testing.T) (*ws.Hub, *metrics.Metrics, *cache.InProcessBus) {
	t.Helper()
	m := metrics.New("kconmon_ng", prometheus.NewRegistry())
	bus := cache.NewInProcessBus()
	return ws.NewHub(bus, m), m, bus
}

// assertNoBusMessages fails if anything at all is queued on ch; it is a bounded non-blocking drain
// rather than a sleep.
func assertNoBusMessages(t *testing.T, topic string, ch <-chan cache.Message) {
	t.Helper()
	select {
	case msg := <-ch:
		t.Fatalf("snapshot topic %q must never go through cache.Bus (N replicas would each fan a full snapshot to every browser); got a %q message: %s",
			topic, msg.Type, msg.Data)
	default:
	}
}

// waitForCounter polls until c reaches at least want, or fails the test.
func waitForCounter(t *testing.T, c prometheus.Counter, want float64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if testutil.ToFloat64(c) >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("counter never reached %v within 5s, last value %v", want, testutil.ToFloat64(c))
}

// gatedQuerier is a matrix.Querier whose every call announces itself on entered (coalescing,
// capacity 1) and then blocks until release is closed.
type gatedQuerier struct {
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int64
	failing atomic.Bool
}

func newGatedQuerier() *gatedQuerier {
	return &gatedQuerier{entered: make(chan struct{}, 1), release: make(chan struct{})}
}

func (q *gatedQuerier) Query(ctx context.Context, _ string, _ time.Time) (json.RawMessage, error) {
	q.calls.Add(1)
	select {
	case q.entered <- struct{}{}:
	default:
	}
	select {
	case <-q.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if q.failing.Load() {
		return nil, errQuerierDown
	}
	return json.RawMessage(`{"status":"success","data":{"result":[]}}`), nil
}

func TestMatrixPusherPushesEveryProtocolImmediately(t *testing.T) {
	hub, m := newTestHub(t)
	q := newGatedQuerier()
	close(q.release) // never block

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := push.NewMatrixPusher(q, hub, "kconmon_ng", time.Hour, m)
	done := make(chan struct{})
	go func() { defer close(done); p.Run(ctx) }()

	for _, protocol := range []string{"tcp", "udp", "icmp"} {
		waitForCounter(t, m.PushSnapshots.WithLabelValues(ws.MatrixTopic(protocol), "ok"), 1)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}

	// Shutdown must not look like a failure: a query aborted by ctx cancel is
	// not a push error, or every rolling restart would spike PushSnapshots.
	for _, protocol := range []string{"tcp", "udp", "icmp"} {
		topic := ws.MatrixTopic(protocol)
		if got := testutil.ToFloat64(m.PushSnapshots.WithLabelValues(topic, "error")); got != 0 {
			t.Errorf("%s: shutdown recorded %v errors, want 0", topic, got)
		}
	}
}

func TestMatrixPusherNeverPublishesSnapshotsToTheBus(t *testing.T) {
	hub, m, bus := newTestHubOnBus(t)
	q := newGatedQuerier()
	close(q.release)

	// Subscribing before Run starts means no snapshot can slip past us.
	matrixMsgs, unsubscribeMatrix := bus.Subscribe(ws.MatrixTopic("tcp"))
	defer unsubscribeMatrix()
	topologyMsgs, unsubscribeTopology := bus.Subscribe(ws.TopicTopology)
	defer unsubscribeTopology()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := push.NewMatrixPusher(q, hub, "kconmon_ng", time.Hour, m)
	done := make(chan struct{})
	go func() { defer close(done); p.Run(ctx) }()

	okTCP := m.PushSnapshots.WithLabelValues(ws.MatrixTopic("tcp"), "ok")
	waitForCounter(t, okTCP, 1)
	// The second, nudge-driven cycle is the happens-before barrier described on
	// assertNoBusMessages: once it is counted, anything the first cycle might
	// have published is already in the subscriber buffers.
	p.Nudge()
	waitForCounter(t, okTCP, 2)

	assertNoBusMessages(t, ws.MatrixTopic("tcp"), matrixMsgs)
	assertNoBusMessages(t, ws.TopicTopology, topologyMsgs)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestMatrixPusherCancelMidQueryIsNotAPushError(t *testing.T) {
	hub, m := newTestHub(t)
	q := newGatedQuerier() // release is never closed: the query stays in flight

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := push.NewMatrixPusher(q, hub, "kconmon_ng", time.Hour, m)
	done := make(chan struct{})
	go func() { defer close(done); p.Run(ctx) }()

	// Cancel while the pusher is provably parked inside Prometheus, so
	// matrix.Compute returns context.Canceled and the guard is exercised.
	select {
	case <-q.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("MatrixPusher never started its first recompute")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}

	for _, protocol := range []string{"tcp", "udp", "icmp"} {
		topic := ws.MatrixTopic(protocol)
		if got := testutil.ToFloat64(m.PushSnapshots.WithLabelValues(topic, "error")); got != 0 {
			t.Errorf("%s: a query aborted by ctx cancel recorded %v errors, want 0", topic, got)
		}
	}
}

func TestMatrixPusherNudgeBurstCoalescesIntoOneExtraRecompute(t *testing.T) {
	hub, m := newTestHub(t)
	q := newGatedQuerier()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// An hour-long interval guarantees the ticker cannot fire during the test,
	// so every recompute after the first is attributable to a nudge.
	p := push.NewMatrixPusher(q, hub, "kconmon_ng", time.Hour, m)
	done := make(chan struct{})
	go func() { defer close(done); p.Run(ctx) }()

	select {
	case <-q.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("MatrixPusher never started its first recompute")
	}

	// The pusher is parked inside the querier, so all 50 nudges land while one
	// recompute is in flight and must collapse into a single pending one.
	for i := 0; i < 50; i++ {
		p.Nudge()
	}
	close(q.release)

	okTCP := m.PushSnapshots.WithLabelValues(ws.MatrixTopic("tcp"), "ok")
	waitForCounter(t, okTCP, 2)

	// Give the loop time to do the wrong thing, then assert it did not.
	time.Sleep(300 * time.Millisecond)
	for _, protocol := range []string{"tcp", "udp", "icmp"} {
		topic := ws.MatrixTopic(protocol)
		if got := testutil.ToFloat64(m.PushSnapshots.WithLabelValues(topic, "ok")); got != 2 {
			t.Errorf("%s: got %v snapshots, want exactly 2 (one initial + one coalesced nudge)", topic, got)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestMatrixPusherSurvivesPrometheusFailures(t *testing.T) {
	hub, m := newTestHub(t)
	q := newGatedQuerier()
	q.failing.Store(true)
	close(q.release)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := push.NewMatrixPusher(q, hub, "kconmon_ng", 20*time.Millisecond, m)
	done := make(chan struct{})
	go func() { defer close(done); p.Run(ctx) }()

	// Three consecutive failures prove the ticker loop is still alive.
	errTCP := m.PushSnapshots.WithLabelValues(ws.MatrixTopic("tcp"), "error")
	waitForCounter(t, errTCP, 3)
	if got := testutil.ToFloat64(m.PushSnapshots.WithLabelValues(ws.MatrixTopic("tcp"), "ok")); got != 0 {
		t.Errorf("no successful push may be recorded while Prometheus fails, got %v", got)
	}

	// And it recovers without a restart.
	q.failing.Store(false)
	waitForCounter(t, m.PushSnapshots.WithLabelValues(ws.MatrixTopic("tcp"), "ok"), 1)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestNewMatrixPusherDefaultsInterval(t *testing.T) {
	hub, m := newTestHub(t)
	q := newGatedQuerier()
	close(q.release)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A zero interval must not panic in time.NewTicker: the constructor
	// substitutes the 15s default.
	p := push.NewMatrixPusher(q, hub, "kconmon_ng", 0, m)
	done := make(chan struct{})
	go func() { defer close(done); p.Run(ctx) }()

	waitForCounter(t, m.PushSnapshots.WithLabelValues(ws.MatrixTopic("tcp"), "ok"), 1)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// gatedController is a stand-in controller HTTP API driven by the real
// controllerclient. GET /api/v1/topology announces itself, blocks until release
// is closed, then answers a snapshot or 500.
type gatedController struct {
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int64
	failing atomic.Bool
}

func newGatedController() *gatedController {
	return &gatedController{entered: make(chan struct{}, 1), release: make(chan struct{})}
}

func (g *gatedController) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.calls.Add(1)
		select {
		case g.entered <- struct{}{}:
		default:
		}
		select {
		case <-g.release:
		case <-r.Context().Done():
			return
		}
		if g.failing.Load() {
			// 500, not 503: controllerclient retries 503 three times with
			// backoff, and this test is about the pusher's error accounting.
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(controllerclient.Topology{
			Nodes:     []controllerclient.Node{{Name: "node-a", Zone: "zone-a", Ready: true}},
			Agents:    []controllerclient.Agent{{ID: "agent-a", NodeName: "node-a", PodIP: "10.0.0.1", Zone: "zone-a"}},
			Timestamp: time.Now().UTC(),
		})
	})
}

func TestTopologyPusherPushesImmediately(t *testing.T) {
	hub, m := newTestHub(t)
	g := newGatedController()
	close(g.release)
	srv := httptest.NewServer(g.handler())
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := push.NewTopologyPusher(controllerclient.New(srv.URL, 5*time.Second), hub, time.Hour, m)
	done := make(chan struct{})
	go func() { defer close(done); p.Run(ctx) }()

	waitForCounter(t, m.PushSnapshots.WithLabelValues(ws.TopicTopology, "ok"), 1)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}

	// Shutdown must not look like a failure: a refetch aborted by ctx cancel is
	// not a push error.
	if got := testutil.ToFloat64(m.PushSnapshots.WithLabelValues(ws.TopicTopology, "error")); got != 0 {
		t.Errorf("shutdown recorded %v errors, want 0", got)
	}
}

func TestTopologyPusherNeverPublishesSnapshotsToTheBus(t *testing.T) {
	hub, m, bus := newTestHubOnBus(t)
	g := newGatedController()
	close(g.release)
	srv := httptest.NewServer(g.handler())
	defer srv.Close()

	topologyMsgs, unsubscribeTopology := bus.Subscribe(ws.TopicTopology)
	defer unsubscribeTopology()
	matrixMsgs, unsubscribeMatrix := bus.Subscribe(ws.MatrixTopic("tcp"))
	defer unsubscribeMatrix()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := push.NewTopologyPusher(controllerclient.New(srv.URL, 5*time.Second), hub, time.Hour, m)
	done := make(chan struct{})
	go func() { defer close(done); p.Run(ctx) }()

	ok := m.PushSnapshots.WithLabelValues(ws.TopicTopology, "ok")
	waitForCounter(t, ok, 1)
	p.Nudge()
	waitForCounter(t, ok, 2)

	assertNoBusMessages(t, ws.TopicTopology, topologyMsgs)
	assertNoBusMessages(t, ws.MatrixTopic("tcp"), matrixMsgs)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestTopologyPusherCancelMidRefetchIsNotAPushError(t *testing.T) {
	hub, m := newTestHub(t)
	g := newGatedController() // release is never closed: the refetch stays in flight
	srv := httptest.NewServer(g.handler())
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := push.NewTopologyPusher(controllerclient.New(srv.URL, 5*time.Second), hub, time.Hour, m)
	done := make(chan struct{})
	go func() { defer close(done); p.Run(ctx) }()

	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("TopologyPusher never started its first refetch")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}

	if got := testutil.ToFloat64(m.PushSnapshots.WithLabelValues(ws.TopicTopology, "error")); got != 0 {
		t.Errorf("a refetch aborted by ctx cancel recorded %v errors, want 0", got)
	}
}

func TestTopologyPusherNudgeBurstCoalescesIntoOneExtraRefetch(t *testing.T) {
	hub, m := newTestHub(t)
	g := newGatedController()
	srv := httptest.NewServer(g.handler())
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := push.NewTopologyPusher(controllerclient.New(srv.URL, 5*time.Second), hub, time.Hour, m)
	done := make(chan struct{})
	go func() { defer close(done); p.Run(ctx) }()

	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("TopologyPusher never started its first refetch")
	}

	for i := 0; i < 50; i++ {
		p.Nudge()
	}
	close(g.release)

	ok := m.PushSnapshots.WithLabelValues(ws.TopicTopology, "ok")
	waitForCounter(t, ok, 2)

	time.Sleep(300 * time.Millisecond)
	if got := testutil.ToFloat64(ok); got != 2 {
		t.Errorf("got %v snapshots, want exactly 2 (one initial + one coalesced nudge)", got)
	}
	if got := g.calls.Load(); got != 2 {
		t.Errorf("controller was called %d times, want 2", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestTopologyPusherSurvivesControllerFailures(t *testing.T) {
	hub, m := newTestHub(t)
	g := newGatedController()
	g.failing.Store(true)
	close(g.release)
	srv := httptest.NewServer(g.handler())
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := push.NewTopologyPusher(controllerclient.New(srv.URL, 5*time.Second), hub, 20*time.Millisecond, m)
	done := make(chan struct{})
	go func() { defer close(done); p.Run(ctx) }()

	waitForCounter(t, m.PushSnapshots.WithLabelValues(ws.TopicTopology, "error"), 3)
	if got := testutil.ToFloat64(m.PushSnapshots.WithLabelValues(ws.TopicTopology, "ok")); got != 0 {
		t.Errorf("no successful push may be recorded while the controller fails, got %v", got)
	}

	g.failing.Store(false)
	waitForCounter(t, m.PushSnapshots.WithLabelValues(ws.TopicTopology, "ok"), 1)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// countingNudger records how many times it was nudged.
type countingNudger struct{ n atomic.Int64 }

func (c *countingNudger) Nudge() { c.n.Add(1) }

func liveEventJSON(t *testing.T, evType string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(events.LiveEvent{
		ID:        "1-1700000000000000000",
		Seq:       1,
		Type:      evType,
		Severity:  "info",
		Scope:     "cluster",
		Timestamp: time.Unix(0, 1700000000000000000).UTC(),
		Summary:   "test event",
		Details:   json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("marshal LiveEvent: %v", err)
	}
	return raw
}

func TestRunNudgeRelayNudgesOnTopologyChangedOnly(t *testing.T) {
	bus := cache.NewInProcessBus()
	n1, n2 := &countingNudger{}, &countingNudger{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() { defer close(done); push.RunNudgeRelay(ctx, bus, n1, n2) }()

	// The relay subscribes inside its own goroutine, so republish until it is
	// demonstrably subscribed rather than sleeping on a guess.
	deadline := time.Now().Add(5 * time.Second)
	for (n1.n.Load() == 0 || n2.n.Load() == 0) && time.Now().Before(deadline) {
		if err := bus.Publish(ctx, ws.TopicLive, cache.Message{Type: "event", Data: liveEventJSON(t, events.TypeTopologyChanged)}); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n1.n.Load() == 0 || n2.n.Load() == 0 {
		t.Fatalf("topology_changed must nudge every nudger, got n1=%d n2=%d", n1.n.Load(), n2.n.Load())
	}

	base1, base2 := n1.n.Load(), n2.n.Load()
	if err := bus.Publish(ctx, ws.TopicLive, cache.Message{Type: "event", Data: liveEventJSON(t, events.TypeCheckObserved)}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := bus.Publish(ctx, ws.TopicLive, cache.Message{Type: "event", Data: json.RawMessage(`{not json`)}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if n1.n.Load() != base1 || n2.n.Load() != base2 {
		t.Errorf("only topology_changed may nudge: n1 %d->%d, n2 %d->%d", base1, n1.n.Load(), base2, n2.n.Load())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunNudgeRelay did not return after ctx cancel")
	}
}

func TestRunNudgeRelayWithNoNudgersStillDrains(t *testing.T) {
	bus := cache.NewInProcessBus()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() { defer close(done); push.RunNudgeRelay(ctx, bus) }()

	for i := 0; i < 100; i++ {
		if err := bus.Publish(ctx, ws.TopicLive, cache.Message{Type: "event", Data: liveEventJSON(t, events.TypeTopologyChanged)}); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunNudgeRelay did not return after ctx cancel")
	}
}

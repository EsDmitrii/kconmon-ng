package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/events"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// liveBusTopic is ws.TopicLive spelled out. This package cannot import
// internal/console/ws (ws imports this one so the Hub can dedupe by
// LiveEvent.ID), so both sides pin the same literal: here, and in
// TestTopicLiveIsTheBusTopicTheIngesterPublishesOn in the ws package.
const liveBusTopic = "live"

// preconditionGrace is for the tests that only need the ingester to reach a
// healthy stream before getting on with their real subject. They are not about
// the promotion rule, so they do not pay the production connect grace; the two
// tests that ARE about it keep the default.
const preconditionGrace = 20 * time.Millisecond

// waitFor polls cond until it holds, or fails the test. Used instead of sleeps
// because the ingester's first reconnect only happens after a 1s backoff.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out after 10s waiting for %s", what)
}

// listenLoopback opens a loopback listener on an OS-chosen port. It goes through
// net.ListenConfig because the repo's linter (noctx) requires the context-aware
// form, exactly as internal/console/httpapi/server.go's Run does.
func listenLoopback(t *testing.T) (net.Listener, error) {
	t.Helper()
	var lc net.ListenConfig
	return lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
}

// fakeControllerAPI is the controller's HTTP side: just enough of
// GET /api/v1/version for the ingester's capability precheck, with the
// capability list swappable at runtime.
type fakeControllerAPI struct {
	mu           sync.Mutex
	capabilities []string
	calls        atomic.Int64
	down         atomic.Bool
}

func (f *fakeControllerAPI) setCapabilities(caps ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.capabilities = caps
}

// setDown makes the version endpoint fail like a controller that is down or
// broken, as opposed to one that is up and simply not offering events.
func (f *fakeControllerAPI) setDown(down bool) { f.down.Store(down) }

func (f *fakeControllerAPI) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/version" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.calls.Add(1)
		if f.down.Load() {
			// 500, not 503: 503 means "not the leader" and controllerclient
			// retries it internally, which is a different scenario.
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f.mu.Lock()
		caps := append([]string(nil), f.capabilities...)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version": "1.6.0", "commit": "abc123", "capabilities": caps,
		})
	})
}

// startFakeController starts the fake controller HTTP API and returns it plus a
// real controllerclient pointed at it.
func startFakeController(t *testing.T, caps ...string) (*fakeControllerAPI, *controllerclient.Client) {
	t.Helper()
	api := &fakeControllerAPI{capabilities: caps}
	srv := httptest.NewServer(api.handler())
	t.Cleanup(srv.Close)
	return api, controllerclient.New(srv.URL, 5*time.Second)
}

// fakeEventStream is a real gRPC EventStream server. WatchEvents forwards
// whatever the test sends on events, and returns an error on demand so the
// reconnect path can be exercised.
type fakeEventStream struct {
	pb.UnimplementedEventStreamServer
	events chan *pb.Event
	fail   chan struct{}
	calls  atomic.Int64
}

func newFakeEventStream() *fakeEventStream {
	return &fakeEventStream{events: make(chan *pb.Event), fail: make(chan struct{})}
}

func (f *fakeEventStream) WatchEvents(_ *pb.WatchEventsRequest, stream pb.EventStream_WatchEventsServer) error {
	f.calls.Add(1)
	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case <-f.fail:
			return grpcstatus.Error(codes.Internal, "induced stream failure")
		case ev := <-f.events:
			if err := stream.Send(ev); err != nil {
				return err
			}
		}
	}
}

// send hands ev to the currently-served stream, failing the test if no stream
// picks it up.
func (f *fakeEventStream) send(t *testing.T, ev *pb.Event) {
	t.Helper()
	select {
	case f.events <- ev:
	case <-time.After(5 * time.Second):
		t.Fatal("no WatchEvents stream consumed the event within 5s")
	}
}

// breakStream makes the stream currently being served return an error, exactly
// as a controller restart or a lost leader lease would.
func (f *fakeEventStream) breakStream(t *testing.T) {
	t.Helper()
	select {
	case f.fail <- struct{}{}:
	case <-time.After(5 * time.Second):
		t.Fatal("no WatchEvents stream was being served, so none could be broken")
	}
}

// startFakeEventStream serves the fake on a loopback TCP listener and returns it
// with its address. A loopback listener, not bufconn: the Ingester takes a plain
// host:port and dials it with the agent's own grpc.NewClient options, so there is
// no grpc.WithContextDialer seam to hand a bufconn to. 127.0.0.1:0 keeps it
// entirely local anyway.
func startFakeEventStream(t *testing.T) (*fakeEventStream, string) {
	t.Helper()
	fake := newFakeEventStream()
	return fake, serveEventStream(t, fake)
}

// serveEventStream runs any EventStreamServer on a loopback listener and returns
// its address.
func serveEventStream(t *testing.T, srv pb.EventStreamServer) string {
	t.Helper()
	lis, err := listenLoopback(t)
	if err != nil {
		t.Fatalf("listen on loopback: %v", err)
	}
	gs := grpc.NewServer()
	pb.RegisterEventStreamServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return lis.Addr().String()
}

// rejectDelay is how long rejectingEventStream holds an accepted stream before
// refusing it. Two orders of magnitude below the ingester's connect grace, so
// correct code still never promotes — but long enough that a regression to
// promoting on stream creation would hold Healthy() true for a whole 50ms and be
// caught with certainty rather than probabilistically. Over loopback the
// rejection would otherwise land within ~1ms of the dial.
const rejectDelay = 50 * time.Millisecond

// rejectingEventStream accepts the subscription and then refuses it, which is
// exactly what a non-leader controller replica does (the leader gate answers
// codes.Unavailable). The client sees this at its first Recv, never at the
// WatchEvents call.
type rejectingEventStream struct {
	pb.UnimplementedEventStreamServer
	calls atomic.Int64
}

func (f *rejectingEventStream) WatchEvents(_ *pb.WatchEventsRequest, stream pb.EventStream_WatchEventsServer) error {
	f.calls.Add(1)
	select {
	case <-time.After(rejectDelay):
	case <-stream.Context().Done():
		return stream.Context().Err()
	}
	return grpcstatus.Error(codes.Unavailable, "not the leader")
}

// countingListener accepts and immediately closes every connection, counting
// them. It is how a test asserts "the ingester did not dial" rather than the
// much weaker "the ingester did not succeed".
func countingListener(t *testing.T) (addr string, accepted *atomic.Int64) {
	t.Helper()
	lis, err := listenLoopback(t)
	if err != nil {
		t.Fatalf("listen on loopback: %v", err)
	}
	t.Cleanup(func() { _ = lis.Close() })
	accepted = &atomic.Int64{}
	go func() {
		for {
			conn, aerr := lis.Accept()
			if aerr != nil {
				return
			}
			accepted.Add(1)
			_ = conn.Close()
		}
	}()
	return lis.Addr().String(), accepted
}

// fakeSink is an events.EventSink test double. err makes every call return
// that error; conflict (checked only when err is nil) makes every call return
// (false, nil) — the "another replica won the race" outcome; delay, if set,
// blocks the call until it elapses OR ctx is done, whichever comes first, so
// a test can exercise the sink's independent timeout without hanging forever
// if the ingester ever regresses to not deriving one.
type fakeSink struct {
	mu       sync.Mutex
	received []events.LiveEvent
	err      error
	conflict bool
	delay    time.Duration
}

func (f *fakeSink) InsertEvent(ctx context.Context, ev events.LiveEvent) (bool, error) {
	f.mu.Lock()
	f.received = append(f.received, ev)
	f.mu.Unlock()

	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	if f.err != nil {
		return false, f.err
	}
	if f.conflict {
		return false, nil
	}
	return true, nil
}

// callCount reports how many times InsertEvent has been called so far.
func (f *fakeSink) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.received)
}

// calls returns a snapshot of every LiveEvent handed to InsertEvent so far.
func (f *fakeSink) calls() []events.LiveEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]events.LiveEvent(nil), f.received...)
}

// newTestMetrics gives every test a fresh registry: promauto panics on a
// duplicate registration.
func newTestMetrics() *metrics.Metrics {
	return metrics.New("kconmon_ng", prometheus.NewRegistry())
}

// capturedLog is one record a capturingHandler kept.
type capturedLog struct {
	level slog.Level
	msg   string
}

// capturingHandler records what the ingester logs so a test can assert the
// LEVEL, not just the text. The ingester logs from its own goroutine, so every
// access is locked.
type capturingHandler struct {
	mu      sync.Mutex
	records []capturedLog
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, capturedLog{level: r.Level, msg: r.Message})
	return nil
}

func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler      { return h }

// findLevel returns the level a message containing substr was logged at, and
// whether such a message was logged at all.
func (h *capturingHandler) findLevel(substr string) (slog.Level, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, rec := range h.records {
		if strings.Contains(rec.msg, substr) {
			return rec.level, true
		}
	}
	return 0, false
}

// captureLogs swaps in a capturing default logger for the duration of one test.
// Tests in a package run sequentially, so replacing the default logger is safe —
// same reasoning as discardLogs in internal/console/cache/inprocess_test.go.
func captureLogs(t *testing.T) *capturingHandler {
	t.Helper()
	h := &capturingHandler{}
	previous := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return h
}

// The metric label is the same (reason="capability") for a controller that is up
// and not offering events and for one that is unreachable, so the LOG LEVEL is
// the only thing that distinguishes a supported degraded state from an incident.
// That makes the level worth pinning: without this test, collapsing the two back
// into one Info line would go unnoticed.
func TestIngesterLogsAMissingCapabilityAtInfoAndABrokenPrecheckAtWarn(t *testing.T) {
	tests := []struct {
		name      string
		down      bool
		substr    string
		wantLevel slog.Level
		absent    string
	}{
		{
			name:      "controller up but not offering events logs at info",
			down:      false,
			substr:    "not offering realtime events",
			wantLevel: slog.LevelInfo,
			absent:    "precheck failed",
		},
		{
			name:      "controller down logs the precheck failure at warn",
			down:      true,
			substr:    "precheck failed",
			wantLevel: slog.LevelWarn,
			absent:    "not offering realtime events",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			api, ctrl := startFakeController(t) // never advertises "events"
			api.setDown(tc.down)
			addr, _ := countingListener(t)

			m := newTestMetrics()
			ing := events.NewIngester(ctrl, addr, cache.NewInProcessBus(), m)

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); ing.Run(ctx) }()

			waitFor(t, "a capability-gated retry", func() bool {
				return testutil.ToFloat64(m.IngesterReconnects.WithLabelValues("capability")) >= 1
			})
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Run did not return after ctx cancel")
			}

			level, ok := logs.findLevel(tc.substr)
			if !ok {
				t.Fatalf("nothing was logged containing %q", tc.substr)
			}
			if level != tc.wantLevel {
				t.Errorf("%q logged at %v, want %v", tc.substr, level, tc.wantLevel)
			}
			if _, found := logs.findLevel(tc.absent); found {
				t.Errorf("%q was logged, but this case is the other one", tc.absent)
			}
		})
	}
}

func TestIngesterWithoutGRPCAddrIsDisabled(t *testing.T) {
	ing := events.NewIngester(nil, "", cache.NewInProcessBus(), newTestMetrics())

	done := make(chan struct{})
	go func() { defer close(done); ing.Run(context.Background()) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run must return immediately when grpcAddr is empty")
	}
	if ing.Healthy() {
		t.Error("a disabled ingester must never report healthy")
	}
}

// The capability precheck runs before EVERY dial, so a controller that does not
// advertise "events" must never see a gRPC connection at all.
func TestIngesterNeverDialsWithoutTheEventsCapability(t *testing.T) {
	api, ctrl := startFakeController(t) // no capabilities advertised

	// A listener nothing should ever connect to; every accept is counted.
	addr, accepted := countingListener(t)

	m := newTestMetrics()
	ing := events.NewIngester(ctrl, addr, cache.NewInProcessBus(), m)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); ing.Run(ctx) }()

	capabilityRetries := m.IngesterReconnects.WithLabelValues("capability")
	waitFor(t, "two capability-gated retries", func() bool {
		return testutil.ToFloat64(capabilityRetries) >= 2
	})

	if got := accepted.Load(); got != 0 {
		t.Errorf("the ingester opened %d connections without the capability, want 0", got)
	}
	if got := testutil.ToFloat64(m.IngesterReconnects.WithLabelValues("dial")); got != 0 {
		t.Errorf("reason=dial counted %v times, want 0 — a missing capability is not a dial failure", got)
	}
	if got := api.calls.Load(); got < 2 {
		t.Errorf("the version endpoint was probed %d times, want at least 2 — the precheck runs before every attempt", got)
	}
	if ing.Healthy() {
		t.Error("Healthy must stay false while the capability is absent")
	}
	if got := testutil.ToFloat64(m.IngesterConnected.WithLabelValues()); got != 0 {
		t.Errorf("ingester_connected = %v, want 0", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestIngesterPublishesLiveEventsToTheBus(t *testing.T) {
	_, ctrl := startFakeController(t, "events")
	fake, addr := startFakeEventStream(t)

	bus := cache.NewInProcessBus()
	msgs, unsubscribe := bus.Subscribe(liveBusTopic)
	defer unsubscribe()

	m := newTestMetrics()
	ing := events.NewIngester(ctrl, addr, bus, m)
	ing.SetConnectGrace(preconditionGrace)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); ing.Run(ctx) }()

	waitFor(t, "the ingester to establish a stream", ing.Healthy)
	if got := testutil.ToFloat64(m.IngesterConnected.WithLabelValues()); got != 1 {
		t.Errorf("ingester_connected = %v, want 1 while the stream is up", got)
	}

	fake.send(t, &pb.Event{
		Seq:       17,
		Timestamp: timestamppb.New(time.Unix(0, 1753400000000000000).UTC()),
		Payload: &pb.Event_CheckObserved{CheckObserved: &pb.CheckObserved{
			TaskId: "t-1", CheckType: "tcp", SourceNode: "node-a", DestinationNode: "node-b",
			Plane: "pod", Success: false, DurationNs: 1200000, Error: "dial timeout",
		}},
	})

	select {
	case msg := <-msgs:
		if msg.Type != "event" {
			t.Errorf("bus message type = %q, want %q", msg.Type, "event")
		}
		var live events.LiveEvent
		if err := json.Unmarshal(msg.Data, &live); err != nil {
			t.Fatalf("bus payload is not a LiveEvent: %v (%s)", err, msg.Data)
		}
		if live.ID != "17-1753400000000000000" {
			t.Errorf("LiveEvent.ID = %q, want %q", live.ID, "17-1753400000000000000")
		}
		if live.Type != "check_observed" || live.Severity != "error" || live.Scope != "node-a→node-b" {
			t.Errorf("unexpected projection: type=%q severity=%q scope=%q", live.Type, live.Severity, live.Scope)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the event never reached the bus")
	}

	waitFor(t, "events_received_total{type=check_observed}", func() bool {
		return testutil.ToFloat64(m.EventsReceived.WithLabelValues("check_observed")) == 1
	})

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
	waitFor(t, "ingester_connected to fall back to 0 after shutdown", func() bool {
		return testutil.ToFloat64(m.IngesterConnected.WithLabelValues()) == 0
	})
}

func TestIngesterReconnectsAfterStreamError(t *testing.T) {
	api, ctrl := startFakeController(t, "events")
	fake, addr := startFakeEventStream(t)

	m := newTestMetrics()
	ing := events.NewIngester(ctrl, addr, cache.NewInProcessBus(), m)
	ing.SetConnectGrace(preconditionGrace)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); ing.Run(ctx) }()

	waitFor(t, "the first stream", ing.Healthy)
	fake.breakStream(t)

	waitFor(t, "Healthy to flip false after the stream broke", func() bool { return !ing.Healthy() })
	waitFor(t, "a reason=stream reconnect to be counted", func() bool {
		return testutil.ToFloat64(m.IngesterReconnects.WithLabelValues("stream")) >= 1
	})
	waitFor(t, "Healthy to flip back true after the backoff", ing.Healthy)

	// The server-side handler is entered asynchronously with respect to the
	// client's stream creation, so the ingester can already be Healthy before
	// gRPC has dispatched WatchEvents on the fake. Poll rather than sample.
	waitFor(t, "WatchEvents to be served a second time", func() bool {
		return fake.calls.Load() >= 2
	})
	if got := testutil.ToFloat64(m.IngesterConnected.WithLabelValues()); got != 1 {
		t.Errorf("ingester_connected = %v, want 1 after the reconnect", got)
	}
	if got := testutil.ToFloat64(m.IngesterReconnects.WithLabelValues("capability")); got != 0 {
		t.Errorf("reason=capability counted %v times, want 0 — the capability was present throughout", got)
	}
	if got := api.calls.Load(); got < 2 {
		t.Errorf("the version endpoint was probed %d times, want at least 2 — the precheck also runs before a reconnect", got)
	}

	// The reconnected stream is a working one, not just an established socket.
	fake.send(t, &pb.Event{Seq: 2, Timestamp: timestamppb.Now(), Payload: &pb.Event_TopologyChanged{
		TopologyChanged: &pb.TopologyChanged{Reason: "agent_registered", NodeName: "node-a"},
	}})
	waitFor(t, "events_received_total{type=topology_changed}", func() bool {
		return testutil.ToFloat64(m.EventsReceived.WithLabelValues("topology_changed")) >= 1
	})

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// A controller that only turns events on later must be picked up by the very
// next retry, because the precheck runs before every dial rather than once.
func TestIngesterConnectsOnceTheCapabilityAppears(t *testing.T) {
	api, ctrl := startFakeController(t) // starts with no capabilities
	fake, addr := startFakeEventStream(t)

	m := newTestMetrics()
	ing := events.NewIngester(ctrl, addr, cache.NewInProcessBus(), m)
	ing.SetConnectGrace(preconditionGrace)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); ing.Run(ctx) }()

	waitFor(t, "at least one capability-gated retry", func() bool {
		return testutil.ToFloat64(m.IngesterReconnects.WithLabelValues("capability")) >= 1
	})
	if fake.calls.Load() != 0 {
		t.Fatalf("WatchEvents was called %d times before the capability appeared, want 0", fake.calls.Load())
	}

	api.setCapabilities("events")
	waitFor(t, "the ingester to connect after the capability appeared", ing.Healthy)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// A controller that is DOWN is a different situation from one that is up and not
// offering events, but it takes the same route through the retry loop: the shared
// reason="capability" label is a frozen contract, so the distinction lives in the
// log level (Info for "no capability", Warn for everything else) rather than in
// the metric. What must hold either way is that no gRPC connection is attempted.
func TestIngesterTreatsAControllerOutageAsACapabilityRetryWithoutDialing(t *testing.T) {
	api, ctrl := startFakeController(t, "events")
	api.setDown(true)

	addr, accepted := countingListener(t)

	m := newTestMetrics()
	ing := events.NewIngester(ctrl, addr, cache.NewInProcessBus(), m)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); ing.Run(ctx) }()

	waitFor(t, "two retries while the controller is down", func() bool {
		return testutil.ToFloat64(m.IngesterReconnects.WithLabelValues("capability")) >= 2
	})

	if got := accepted.Load(); got != 0 {
		t.Errorf("the ingester opened %d gRPC connections while the controller was down, want 0", got)
	}
	for _, reason := range []string{"dial", "stream"} {
		if got := testutil.ToFloat64(m.IngesterReconnects.WithLabelValues(reason)); got != 0 {
			t.Errorf("reason=%s counted %v times, want 0 — the precheck failed before any dial", reason, got)
		}
	}
	if ing.Healthy() {
		t.Error("Healthy must stay false while the controller is down")
	}
	if got := api.calls.Load(); got < 2 {
		t.Errorf("the version endpoint was probed %d times, want at least 2", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// The flap guard. A non-leader controller accepts the subscription and rejects it
// at once, so the stream object exists but no event can ever arrive. Healthy must
// never go true, because Task 12 derives the console's browser-facing
// capabilities from it and the realtime badge would otherwise flicker on every
// retry cycle.
func TestIngesterNeverReportsHealthyAgainstARejectingController(t *testing.T) {
	_, ctrl := startFakeController(t, "events")
	rejecting := &rejectingEventStream{}
	addr := serveEventStream(t, rejecting)

	m := newTestMetrics()
	ing := events.NewIngester(ctrl, addr, cache.NewInProcessBus(), m)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); ing.Run(ctx) }()

	// Observe until three whole attempts have been rejected, asserting throughout
	// that the ingester never once reported healthy. The window is defined by the
	// retry count, not by a duration, so it scales with the backoff instead of
	// restating it — and the production connect grace is deliberately NOT
	// shortened here, because the property under test is that the rejection always
	// beats the grace timer.
	const wantRejections = 3
	deadline := time.Now().Add(30 * time.Second)
	for testutil.ToFloat64(m.IngesterReconnects.WithLabelValues("stream")) < wantRejections {
		if ing.Healthy() {
			t.Fatal("Healthy went true against a controller that rejects every stream")
		}
		if got := testutil.ToFloat64(m.IngesterConnected.WithLabelValues()); got != 0 {
			t.Fatalf("ingester_connected = %v while every stream is being rejected, want 0", got)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d rejected attempts, saw %v",
				wantRejections, testutil.ToFloat64(m.IngesterReconnects.WithLabelValues("stream")))
		}
		time.Sleep(2 * time.Millisecond)
	}

	// The rejection arrives at the first Recv, so it is a stream failure and not
	// a dial failure — the distinction the corrected comment in attempt records.
	if got := rejecting.calls.Load(); got < wantRejections {
		t.Errorf("WatchEvents was attempted %d times, want at least %d", got, wantRejections)
	}
	if got := testutil.ToFloat64(m.IngesterReconnects.WithLabelValues("dial")); got != 0 {
		t.Errorf("reason=dial counted %v times, want 0 — the stream was accepted, then refused", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// Invariant: a grace timer left over from an attempt that has already ended must
// never promote the ingester. The attempt here is cancelled while its timer is
// still pending, and Healthy must stay false well past the moment that timer
// would have fired. Sleeping is the assertion, not a synchronisation shortcut:
// the test is that nothing happens.
func TestIngesterGraceTimerCannotResurrectAFinishedAttempt(t *testing.T) {
	_, ctrl := startFakeController(t, "events")
	fake, addr := startFakeEventStream(t)

	m := newTestMetrics()
	ing := events.NewIngester(ctrl, addr, cache.NewInProcessBus(), m)
	const grace = 300 * time.Millisecond
	ing.SetConnectGrace(grace)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); ing.Run(ctx) }()

	// Cancel while the stream is being served but before the grace elapses, so
	// the attempt ends with its timer still armed.
	waitFor(t, "the stream to be served", func() bool { return fake.calls.Load() >= 1 })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
	if ing.Healthy() {
		t.Fatal("Healthy must be false immediately after Run returns")
	}

	time.Sleep(3 * grace)

	if ing.Healthy() {
		t.Error("a stale grace timer resurrected Healthy after the attempt had ended")
	}
	if got := testutil.ToFloat64(m.IngesterConnected.WithLabelValues()); got != 0 {
		t.Errorf("ingester_connected = %v after shutdown, want 0", got)
	}
}

// The other half of the promotion rule: an event received promotes the ingester
// immediately rather than making it wait out the grace period. The grace is
// stretched far past the test's own runtime so a promotion can only mean the
// event did it.
func TestIngesterBecomesHealthyOnTheFirstEventAheadOfTheGrace(t *testing.T) {
	_, ctrl := startFakeController(t, "events")
	fake, addr := startFakeEventStream(t)

	bus := cache.NewInProcessBus()
	msgs, unsubscribe := bus.Subscribe(liveBusTopic)
	defer unsubscribe()

	m := newTestMetrics()
	ing := events.NewIngester(ctrl, addr, bus, m)
	ing.SetConnectGrace(10 * time.Minute) // only an event can promote in time

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); ing.Run(ctx) }()

	fake.send(t, &pb.Event{Seq: 1, Timestamp: fixedTime(), Payload: &pb.Event_TopologyChanged{
		TopologyChanged: &pb.TopologyChanged{Reason: "agent_registered", NodeName: "node-a"},
	}})

	select {
	case <-msgs:
	case <-time.After(5 * time.Second):
		t.Fatal("the event never reached the bus")
	}

	// Promotion happens before the event is published, so by the time the bus
	// message is observable this is already true — no polling needed, which is
	// what makes the assertion about ordering rather than about timing.
	if !ing.Healthy() {
		t.Error("Healthy must be true once an event has been received and published")
	}
	if got := testutil.ToFloat64(m.IngesterConnected.WithLabelValues()); got != 1 {
		t.Errorf("ingester_connected = %v, want 1", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
	waitFor(t, "ingester_connected to fall back to 0 after shutdown", func() bool {
		return testutil.ToFloat64(m.IngesterConnected.WithLabelValues()) == 0
	})
	if ing.Healthy() {
		t.Error("Healthy must be false after Run returns")
	}
}

// topologyEvent builds a minimal pb.Event with the given seq, useful for the
// persistence tests below where the payload content itself is not the point.
func topologyEvent(seq uint64) *pb.Event {
	return &pb.Event{
		Seq:       seq,
		Timestamp: timestamppb.Now(),
		Payload: &pb.Event_TopologyChanged{TopologyChanged: &pb.TopologyChanged{
			Reason: "agent_registered", NodeName: "node-a",
		}},
	}
}

// TestIngesterPersistsAfterPublish is Task 4's core wiring test: a configured
// EventSink is called once per event, and what it receives matches what the
// bus received — same Seq, Timestamp, Type and Details.
func TestIngesterPersistsAfterPublish(t *testing.T) {
	_, ctrl := startFakeController(t, "events")
	fake, addr := startFakeEventStream(t)

	bus := cache.NewInProcessBus()
	msgs, unsubscribe := bus.Subscribe(liveBusTopic)
	defer unsubscribe()

	sink := &fakeSink{}
	m := newTestMetrics()
	ing := events.NewIngester(ctrl, addr, bus, m, events.WithEventSink(sink))
	ing.SetConnectGrace(preconditionGrace)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); ing.Run(ctx) }()

	waitFor(t, "the ingester to establish a stream", ing.Healthy)

	fake.send(t, topologyEvent(42))

	var busEvent events.LiveEvent
	select {
	case msg := <-msgs:
		if err := json.Unmarshal(msg.Data, &busEvent); err != nil {
			t.Fatalf("bus payload is not a LiveEvent: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the event never reached the bus")
	}

	waitFor(t, "the sink to be called once", func() bool { return sink.callCount() == 1 })

	got := sink.calls()[0]
	if got.Seq != busEvent.Seq || !got.Timestamp.Equal(busEvent.Timestamp) ||
		got.Type != busEvent.Type || string(got.Details) != string(busEvent.Details) {
		t.Errorf("sink received %+v, want it to match the bus event %+v", got, busEvent)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// TestIngesterSinkErrorDoesNotStopPublishing drives three events through a
// sink that fails on every call. All three must still reach the bus, the
// ingester must stay connected, and every failure must be counted as
// EventsPersisted{result="error"} — a database hiccup degrades history, not
// the realtime feed.
func TestIngesterSinkErrorDoesNotStopPublishing(t *testing.T) {
	_, ctrl := startFakeController(t, "events")
	fake, addr := startFakeEventStream(t)

	bus := cache.NewInProcessBus()
	msgs, unsubscribe := bus.Subscribe(liveBusTopic)
	defer unsubscribe()

	sink := &fakeSink{err: errors.New("db unavailable")}
	m := newTestMetrics()
	ing := events.NewIngester(ctrl, addr, bus, m, events.WithEventSink(sink))
	ing.SetConnectGrace(preconditionGrace)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); ing.Run(ctx) }()

	waitFor(t, "the ingester to establish a stream", ing.Healthy)

	for seq := uint64(1); seq <= 3; seq++ {
		fake.send(t, topologyEvent(seq))
		select {
		case <-msgs:
		case <-time.After(5 * time.Second):
			t.Fatalf("event seq=%d never reached the bus", seq)
		}
	}

	waitFor(t, `events_persisted_total{result="error"} == 3`, func() bool {
		return testutil.ToFloat64(m.EventsPersisted.WithLabelValues("error")) == 3
	})
	if !ing.Healthy() {
		t.Error("a sink failure must not knock the ingester unhealthy")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// TestIngesterSinkConflictIsNotAnError is the normal N-replica path: another
// replica already inserted the row, InsertEvent returns (false, nil), and that
// must be counted as EventsPersisted{result="conflict"} without a single
// error-level log line.
func TestIngesterSinkConflictIsNotAnError(t *testing.T) {
	_, ctrl := startFakeController(t, "events")
	fake, addr := startFakeEventStream(t)

	bus := cache.NewInProcessBus()
	msgs, unsubscribe := bus.Subscribe(liveBusTopic)
	defer unsubscribe()

	logs := captureLogs(t)
	sink := &fakeSink{conflict: true}
	m := newTestMetrics()
	ing := events.NewIngester(ctrl, addr, bus, m, events.WithEventSink(sink))
	ing.SetConnectGrace(preconditionGrace)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); ing.Run(ctx) }()

	waitFor(t, "the ingester to establish a stream", ing.Healthy)

	fake.send(t, topologyEvent(7))
	select {
	case <-msgs:
	case <-time.After(5 * time.Second):
		t.Fatal("the event never reached the bus")
	}

	waitFor(t, `events_persisted_total{result="conflict"} == 1`, func() bool {
		return testutil.ToFloat64(m.EventsPersisted.WithLabelValues("conflict")) == 1
	})

	logs.mu.Lock()
	for _, rec := range logs.records {
		if rec.level >= slog.LevelWarn {
			t.Errorf("logged at %v: %q — a persistence conflict is the normal N-replica path, not a warning", rec.level, rec.msg)
		}
	}
	logs.mu.Unlock()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// TestIngesterWithoutSinkIsUnchanged is the M1/M2 regression test
// (database.mode=disabled): constructed with no options at all, the ingester's
// bus delivery must be identical to before this task, and no EventsPersisted
// label may move.
func TestIngesterWithoutSinkIsUnchanged(t *testing.T) {
	_, ctrl := startFakeController(t, "events")
	fake, addr := startFakeEventStream(t)

	bus := cache.NewInProcessBus()
	msgs, unsubscribe := bus.Subscribe(liveBusTopic)
	defer unsubscribe()

	m := newTestMetrics()
	ing := events.NewIngester(ctrl, addr, bus, m) // no Option at all
	ing.SetConnectGrace(preconditionGrace)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); ing.Run(ctx) }()

	waitFor(t, "the ingester to establish a stream", ing.Healthy)

	fake.send(t, topologyEvent(9))
	select {
	case <-msgs:
	case <-time.After(5 * time.Second):
		t.Fatal("the event never reached the bus")
	}

	for _, result := range []string{"ok", "conflict", "error"} {
		if got := testutil.ToFloat64(m.EventsPersisted.WithLabelValues(result)); got != 0 {
			t.Errorf("events_persisted_total{result=%s} = %v, want 0 with no sink configured", result, got)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// TestIngesterSinkGetsIndependentTimeout proves the sink's context is its own
// short-lived timeout, not the long-lived stream context: a sink that blocks
// well past that budget must still let go, so the consume loop can pick up
// the next event rather than wedging for the life of the stream.
func TestIngesterSinkGetsIndependentTimeout(t *testing.T) {
	_, ctrl := startFakeController(t, "events")
	fake, addr := startFakeEventStream(t)

	bus := cache.NewInProcessBus()
	msgs, unsubscribe := bus.Subscribe(liveBusTopic)
	defer unsubscribe()

	sink := &fakeSink{delay: 30 * time.Second} // far past the sink's own timeout budget
	m := newTestMetrics()
	ing := events.NewIngester(ctrl, addr, bus, m, events.WithEventSink(sink))
	ing.SetConnectGrace(preconditionGrace)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); ing.Run(ctx) }()

	waitFor(t, "the ingester to establish a stream", ing.Healthy)

	fake.send(t, topologyEvent(1))
	select {
	case <-msgs:
	case <-time.After(5 * time.Second):
		t.Fatal("the first event never reached the bus")
	}

	// The sink is still blocked inside InsertEvent for event 1 (its own 5s
	// budget has not elapsed yet). Handing event 2 straight to the fake
	// stream's channel, bypassing fake.send's own 5s timeout: gRPC's stream
	// buffering means the server can accept this into its send loop almost
	// immediately regardless of whether the client has called Recv again, so
	// that handoff alone proves nothing about the consume loop.
	select {
	case fake.events <- topologyEvent(2):
	case <-time.After(8 * time.Second):
		t.Fatal("the fake server never picked up the second event")
	}

	// What DOES prove the consume loop freed itself is the second event making
	// it all the way to the bus, which requires publish(ev1) — and therefore
	// the blocked sink call — to have returned first. The deadline here is
	// comfortably longer than sinkTimeout (5s) but far short of the fake
	// sink's 30s delay, so this only passes if the sink's own context, not the
	// stream context, is what unblocked it.
	select {
	case <-msgs:
	case <-time.After(8 * time.Second):
		t.Fatal("the second event never reached the bus — the consume loop stayed wedged past the sink's own timeout budget")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// failingBus wraps a real bus and makes every Publish fail, while Subscribe
// keeps working: it models a Valkey outage on the publish path only, which is
// the shape the ingester has to survive. The wrapped bus is still there so the
// test can prove nothing else changed.
type failingBus struct {
	inner    cache.Bus
	err      error
	attempts atomic.Int64
}

func (b *failingBus) Publish(context.Context, string, cache.Message) error {
	b.attempts.Add(1)
	return b.err
}

func (b *failingBus) Subscribe(topic string) (<-chan cache.Message, func()) {
	return b.inner.Subscribe(topic)
}

// TestIngesterBusErrorStillPersists is the other half of
// TestIngesterSinkErrorDoesNotStopPublishing: the two failures are
// independent in BOTH directions. With bus.Publish failing on every call, all
// three events must still reach the sink and be counted as
// EventsPersisted{result="ok"} — a Valkey outage degrades realtime, it must
// not silently stop durable history while the database is healthy. (The
// earlier bug: publish returned early on the bus error and never reached the
// sink block at all.)
func TestIngesterBusErrorStillPersists(t *testing.T) {
	_, ctrl := startFakeController(t, "events")
	fake, addr := startFakeEventStream(t)

	bus := &failingBus{inner: cache.NewInProcessBus(), err: errors.New("valkey unreachable")}
	sink := &fakeSink{}
	m := newTestMetrics()
	ing := events.NewIngester(ctrl, addr, bus, m, events.WithEventSink(sink))
	ing.SetConnectGrace(preconditionGrace)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); ing.Run(ctx) }()

	waitFor(t, "the ingester to establish a stream", ing.Healthy)

	for seq := uint64(1); seq <= 3; seq++ {
		fake.send(t, topologyEvent(seq))
	}

	waitFor(t, "the sink to receive all three events", func() bool { return sink.callCount() == 3 })
	waitFor(t, `events_persisted_total{result="ok"} == 3`, func() bool {
		return testutil.ToFloat64(m.EventsPersisted.WithLabelValues("ok")) == 3
	})

	// Every event was still offered to the bus: the fix is "do not return
	// early", not "skip the bus".
	if got := bus.attempts.Load(); got != 3 {
		t.Errorf("bus.Publish called %d times, want 3", got)
	}
	seen := make(map[uint64]bool, 3)
	for _, ev := range sink.calls() {
		seen[ev.Seq] = true
	}
	for seq := uint64(1); seq <= 3; seq++ {
		if !seen[seq] {
			t.Errorf("event seq=%d never reached the sink", seq)
		}
	}
	if !ing.Healthy() {
		t.Error("a bus failure must not tear the stream down")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

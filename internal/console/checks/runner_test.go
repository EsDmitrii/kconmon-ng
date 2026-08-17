package checks_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/checks"
	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
	"github.com/EsDmitrii/kconmon-ng/internal/console/ws"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

func testMetrics(t *testing.T) *metrics.Metrics {
	t.Helper()
	return metrics.New("kconmon_ng_test", prometheus.NewRegistry())
}

// fakeDiagnosticsServer speaks the real POST /api/v1/diagnostics contract
// (internal/controller/diagnostics.go); per-pair behaviour (fail the check, or answer with a given
// HTTP status instead of 200) is configurable by key so one server can drive every runner_test.go
// scenario.
type fakeDiagnosticsServer struct {
	mu        sync.Mutex
	failPairs map[string]bool
	status    map[string]int
	delay     time.Duration
	nodes     []controllerclient.Node
	agents    []controllerclient.Agent

	calls     atomic.Int32
	inFlight  atomic.Int32
	highWater atomic.Int32
}

// withAgents registers one agent per node name, with no Kubernetes Node
// entries at all -- the k8s-less topology the QA stand and any agent-only
// deployment actually reports.
func (f *fakeDiagnosticsServer) withAgents(nodeNames ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.agents = nil
	for _, n := range nodeNames {
		f.agents = append(f.agents, controllerclient.Agent{ID: n + "-agent", NodeName: n, Zone: "zone-a"})
	}
}

func newFakeDiagnosticsServer() *fakeDiagnosticsServer {
	return &fakeDiagnosticsServer{failPairs: make(map[string]bool), status: make(map[string]int)}
}

func pairKey(src, dst string) string { return src + "->" + dst }

func (f *fakeDiagnosticsServer) failPair(src, dst string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failPairs[pairKey(src, dst)] = true
}

func (f *fakeDiagnosticsServer) statusFor(src, dst string, code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[pairKey(src, dst)] = code
}

func (f *fakeDiagnosticsServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/topology" {
			f.mu.Lock()
			snap := controllerclient.Topology{Nodes: f.nodes, Agents: f.agents, Timestamp: time.Now()}
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(snap)
			return
		}
		if r.URL.Path != "/api/v1/diagnostics" {
			http.NotFound(w, r)
			return
		}
		cur := f.inFlight.Add(1)
		defer f.inFlight.Add(-1)
		for {
			hw := f.highWater.Load()
			if cur <= hw || f.highWater.CompareAndSwap(hw, cur) {
				break
			}
		}
		f.calls.Add(1)

		var req struct{ Source, Destination, Type, Plane string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}

		if f.delay > 0 {
			time.Sleep(f.delay)
		}

		key := pairKey(req.Source, req.Destination)
		f.mu.Lock()
		statusOverride := f.status[key]
		fail := f.failPairs[key]
		f.mu.Unlock()

		if statusOverride != 0 {
			http.Error(w, "induced failure", statusOverride)
			return
		}

		result := model.CheckResult{
			Type: model.CheckType(req.Type), Success: !fail,
			Source: req.Source, Destination: req.Destination,
			Duration: 5 * time.Millisecond, Timestamp: time.Now(),
		}
		if fail {
			result.Error = "induced check failure"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	})
}

func startFakeDiagnosticsServer(t *testing.T) (*fakeDiagnosticsServer, *controllerclient.Client) {
	t.Helper()
	fake := newFakeDiagnosticsServer()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	return fake, controllerclient.New(srv.URL, 10*time.Second)
}

// recordingBus is a fake cache.Bus that records every Publish call in order under a mutex.
type recordingBus struct {
	mu   sync.Mutex
	msgs []recordedMsg
}

type recordedMsg struct {
	topic string
	msg   cache.Message
}

func newRecordingBus() *recordingBus { return &recordingBus{} }

// CrossReplica is true: this double stands in for the shared bus, which is the only configuration
// where forwarding a cancel to another replica means anything.
func (b *recordingBus) CrossReplica() bool { return true }

func (b *recordingBus) Publish(_ context.Context, topic string, msg cache.Message) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.msgs = append(b.msgs, recordedMsg{topic: topic, msg: msg})
	return nil
}

func (b *recordingBus) Subscribe(string) (<-chan cache.Message, func()) {
	ch := make(chan cache.Message)
	close(ch)
	return ch, func() {}
}

func (b *recordingBus) snapshot() []recordedMsg {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]recordedMsg, len(b.msgs))
	copy(out, b.msgs)
	return out
}

func (b *recordingBus) onTopic(topic string) []recordedMsg {
	var out []recordedMsg
	for _, m := range b.snapshot() {
		if m.topic == topic {
			out = append(out, m)
		}
	}
	return out
}

type frameView struct {
	State       string `json:"state"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Success     bool   `json:"success"`
	Status      string `json:"status"`
}

func decodeFrame(t *testing.T, msg cache.Message) frameView {
	t.Helper()
	var fv frameView
	if err := json.Unmarshal(msg.Data, &fv); err != nil {
		t.Fatalf("decode frame %s: %v", msg.Data, err)
	}
	return fv
}

// waitForTerminal polls the store until id reaches a terminal status
// (anything but pending/running), or fails the test after 10s.
func waitForTerminal(t *testing.T, st checks.Store, id string) store.Run {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		run, err := st.GetRun(context.Background(), id)
		if err == nil && run.Status != "pending" && run.Status != "running" {
			return run
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("run %s did not reach a terminal status within 10s", id)
	return store.Run{}
}

func testInitiator() authz.Subject {
	return authz.Subject{Kind: authz.SubjectUser, ID: "u1"}
}

// TestStartDispatchedBeforeTerminalPerPairAndFinishedBeforeClosed replaces the previous
// total-count-only "frames in order" test.
func TestStartDispatchedBeforeTerminalPerPairAndFinishedBeforeClosed(t *testing.T) {
	fake, ctrl := startFakeDiagnosticsServer(t)
	_ = fake
	bus := cache.NewInProcessBus()
	hub := ws.NewHub(bus, testMetrics(t))
	wsSrv := httptest.NewServer(http.HandlerFunc(hub.ServeWS))
	t.Cleanup(wsSrv.Close)
	mem := checks.NewMemoryStore()
	runner := checks.NewRunner(ctrl, hub, bus, mem, testMetrics(t))

	spec := checks.Spec{
		Sources: []string{"n1", "n2"}, Destinations: []string{"n3", "n4"},
		Type: "tcp", Plane: "pod", Timeout: 2 * time.Second,
	}
	id, err := runner.Start(context.Background(), spec, testInitiator())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	run := waitForTerminal(t, mem, id)
	if run.Status != "succeeded" {
		t.Fatalf("run.Status = %q, want succeeded", run.Status)
	}

	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(wsSrv.URL, "http")+"/", nil)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
			_ = resp.Body.Close()
		}
		t.Fatalf("dial: %v (http status %d)", err, status)
	}
	_ = resp.Body.Close()
	t.Cleanup(func() { _ = conn.Close() })

	if err := conn.WriteJSON(ws.ClientMessage{Action: ws.ActionSubscribe, Topic: ws.RunTopic(id), LastSeq: 0}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// The run already finished (waitForTerminal above), so this is not a race against the run's own
	// goroutine.
	dispatchedAt := map[string]int{} // pairKey -> index of that pair's dispatched frame
	terminalAt := map[string]int{}   // pairKey -> index of that pair's terminal frame
	var envelopes []ws.Envelope
	finishedIdx, closedIdx := -1, -1
	for {
		if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatalf("set read deadline: %v", err)
		}
		var env ws.Envelope
		if err := conn.ReadJSON(&env); err != nil {
			t.Fatalf("read envelope: %v", err)
		}
		envelopes = append(envelopes, env)
		idx := len(envelopes) - 1
		if env.Type == ws.TypeClosed {
			closedIdx = idx
			break
		}
		fv := decodeFrame(t, cache.Message{Data: env.Data})
		switch fv.State {
		case "dispatched":
			dispatchedAt[pairKey(fv.Source, fv.Destination)] = idx
		case "succeeded", "failed", "timeout":
			terminalAt[pairKey(fv.Source, fv.Destination)] = idx
		case "finished":
			finishedIdx = idx
		default:
			t.Errorf("unexpected frame state %q", fv.State)
		}
	}

	if len(dispatchedAt) != 4 || len(terminalAt) != 4 {
		t.Fatalf("dispatched=%d terminal=%d frames, want 4 each", len(dispatchedAt), len(terminalAt))
	}
	for key, dIdx := range dispatchedAt {
		tIdx, ok := terminalAt[key]
		if !ok {
			t.Errorf("pair %s has a dispatched frame but no terminal frame", key)
			continue
		}
		if dIdx >= tIdx {
			t.Errorf("pair %s: dispatched frame at index %d, terminal frame at %d -- want dispatched strictly before terminal", key, dIdx, tIdx)
		}
	}

	if finishedIdx == -1 {
		t.Fatal("no finished frame observed")
	}
	if envelopes[finishedIdx].Type != ws.TypeEvent {
		t.Errorf("finished frame envelope type = %q, want %q", envelopes[finishedIdx].Type, ws.TypeEvent)
	}
	if finishedIdx >= closedIdx {
		t.Errorf("finished frame at index %d, TypeClosed at %d -- want finished strictly before closed", finishedIdx, closedIdx)
	}
	if envelopes[finishedIdx].Seq >= envelopes[closedIdx].Seq {
		t.Errorf("finished frame seq %d, TypeClosed seq %d -- want finished seq strictly lower (I-2, task-22-brief.md)",
			envelopes[finishedIdx].Seq, envelopes[closedIdx].Seq)
	}
	var finalView frameView
	if err := json.Unmarshal(envelopes[finishedIdx].Data, &finalView); err != nil {
		t.Fatalf("decode finished frame: %v", err)
	}
	if finalView.Status != "succeeded" {
		t.Errorf("finished frame status = %q, want succeeded", finalView.Status)
	}
}

func TestStartConcurrencyNeverExceeds8(t *testing.T) {
	fake, ctrl := startFakeDiagnosticsServer(t)
	fake.delay = 25 * time.Millisecond
	bus := newRecordingBus()
	hub := ws.NewHub(bus, testMetrics(t))
	mem := checks.NewMemoryStore()
	runner := checks.NewRunner(ctrl, hub, bus, mem, testMetrics(t))

	dests := make([]string, 20)
	for i := range dests {
		dests[i] = fmt.Sprintf("dst-%d", i)
	}
	spec := checks.Spec{Sources: []string{"src"}, Destinations: dests, Type: "tcp", Plane: "pod", Timeout: 5 * time.Second}

	id, err := runner.Start(context.Background(), spec, testInitiator())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForTerminal(t, mem, id)

	if hw := fake.highWater.Load(); hw > 8 {
		t.Errorf("high-water mark of concurrent dispatches = %d, want <= 8", hw)
	}
	if calls := fake.calls.Load(); calls != 20 {
		t.Errorf("calls = %d, want 20", calls)
	}
}

func TestStartOneFailingPairYieldsPartial(t *testing.T) {
	fake, ctrl := startFakeDiagnosticsServer(t)
	fake.failPair("n1", "n2")
	bus := newRecordingBus()
	hub := ws.NewHub(bus, testMetrics(t))
	mem := checks.NewMemoryStore()
	runner := checks.NewRunner(ctrl, hub, bus, mem, testMetrics(t))

	spec := checks.Spec{Sources: []string{"n1"}, Destinations: []string{"n2", "n3"}, Type: "tcp", Plane: "pod", Timeout: 2 * time.Second}
	id, err := runner.Start(context.Background(), spec, testInitiator())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	run := waitForTerminal(t, mem, id)
	if run.Status != "partial" {
		t.Fatalf("run.Status = %q, want partial", run.Status)
	}
	if run.PairOK != 1 || run.PairFailed != 1 {
		t.Errorf("run pair counts = ok:%d failed:%d, want ok:1 failed:1", run.PairOK, run.PairFailed)
	}
}

// The full-mesh fallback must plan against the MEASUREMENT FLEET -- the nodes that have a
// registered agent.
func TestStartFullMeshFallbackPlansAgainstAgentsNotKubernetesNodes(t *testing.T) {
	fake, ctrl := startFakeDiagnosticsServer(t)
	// Ten agents, zero Kubernetes nodes: the QA stand's exact topology.
	fake.withAgents("n1", "n2", "n3")
	bus := newRecordingBus()
	hub := ws.NewHub(bus, testMetrics(t))
	mem := checks.NewMemoryStore()
	runner := checks.NewRunner(ctrl, hub, bus, mem, testMetrics(t))

	// Both sides empty -- the console's "all <-> all".
	spec := checks.Spec{Type: "tcp", Plane: "pod", Timeout: 2 * time.Second}
	id, err := runner.Start(context.Background(), spec, testInitiator())
	if err != nil {
		t.Fatalf("Start(all<->all with agents but no k8s nodes): %v, want a planned run", err)
	}

	run := waitForTerminal(t, mem, id)
	// 3 nodes, ordered pairs, self-pairs dropped: 3*3-3 = 6.
	if run.PairTotal != 6 {
		t.Fatalf("run.PairTotal = %d, want 6 (3 agents, full mesh, self-pairs dropped)", run.PairTotal)
	}
	if run.Status != "succeeded" {
		t.Errorf("run.Status = %q, want succeeded", run.Status)
	}
}

// The one-sided fallbacks are the same bug wearing different clothes; both orientations are pinned
// so a fix that only repairs the symmetric all<->all shape cannot pass.
func TestStartOneSidedFallbackPlansAgainstAgents(t *testing.T) {
	for _, tc := range []struct {
		name      string
		spec      checks.Spec
		wantPairs int32
	}{
		{
			name:      "explicit sources, all destinations",
			spec:      checks.Spec{Sources: []string{"n1"}, Type: "tcp", Plane: "pod", Timeout: 2 * time.Second},
			wantPairs: 2, // n1->n2, n1->n3 (self-pair dropped)
		},
		{
			name:      "all sources, explicit destinations",
			spec:      checks.Spec{Destinations: []string{"n3"}, Type: "tcp", Plane: "pod", Timeout: 2 * time.Second},
			wantPairs: 2, // n1->n3, n2->n3 (self-pair dropped)
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake, ctrl := startFakeDiagnosticsServer(t)
			fake.withAgents("n1", "n2", "n3")
			bus := newRecordingBus()
			hub := ws.NewHub(bus, testMetrics(t))
			mem := checks.NewMemoryStore()
			runner := checks.NewRunner(ctrl, hub, bus, mem, testMetrics(t))

			id, err := runner.Start(context.Background(), tc.spec, testInitiator())
			if err != nil {
				t.Fatalf("Start: %v, want a planned run", err)
			}
			run := waitForTerminal(t, mem, id)
			if run.PairTotal != tc.wantPairs {
				t.Errorf("run.PairTotal = %d, want %d", run.PairTotal, tc.wantPairs)
			}
		})
	}
}

// With no agents registered at all there is genuinely nothing to probe, and ErrNoNodes must still
// be what an operator gets.
func TestStartFullMeshFallbackStillFailsWithNoAgents(t *testing.T) {
	fake, ctrl := startFakeDiagnosticsServer(t)
	fake.mu.Lock()
	fake.nodes = []controllerclient.Node{{Name: "k8s-only", Zone: "zone-a", Ready: true}}
	fake.mu.Unlock()
	bus := newRecordingBus()
	hub := ws.NewHub(bus, testMetrics(t))
	runner := checks.NewRunner(ctrl, hub, bus, checks.NewMemoryStore(), testMetrics(t))

	_, err := runner.Start(context.Background(), checks.Spec{Type: "tcp", Plane: "pod"}, testInitiator())
	if !errors.Is(err, checks.ErrNoNodes) {
		t.Fatalf("Start with zero agents = %v, want ErrNoNodes", err)
	}
}

func TestStartAllFailingPairsYieldsFailed(t *testing.T) {
	fake, ctrl := startFakeDiagnosticsServer(t)
	fake.failPair("n1", "n2")
	fake.failPair("n1", "n3")
	bus := newRecordingBus()
	hub := ws.NewHub(bus, testMetrics(t))
	mem := checks.NewMemoryStore()
	runner := checks.NewRunner(ctrl, hub, bus, mem, testMetrics(t))

	spec := checks.Spec{Sources: []string{"n1"}, Destinations: []string{"n2", "n3"}, Type: "tcp", Plane: "pod", Timeout: 2 * time.Second}
	id, err := runner.Start(context.Background(), spec, testInitiator())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	run := waitForTerminal(t, mem, id)
	if run.Status != "failed" {
		t.Fatalf("run.Status = %q, want failed", run.Status)
	}
	if run.PairFailed != 2 {
		t.Errorf("run.PairFailed = %d, want 2", run.PairFailed)
	}
}

// A controller 504 for one pair must produce a "timeout" progress state and
// pair-result, distinct from an ordinary "failed" pair.
func TestStartControllerTimeoutMapsToTimeoutState(t *testing.T) {
	fake, ctrl := startFakeDiagnosticsServer(t)
	fake.statusFor("n1", "n2", http.StatusGatewayTimeout)
	bus := newRecordingBus()
	hub := ws.NewHub(bus, testMetrics(t))
	mem := checks.NewMemoryStore()
	runner := checks.NewRunner(ctrl, hub, bus, mem, testMetrics(t))

	spec := checks.Spec{Sources: []string{"n1"}, Destinations: []string{"n2"}, Type: "tcp", Plane: "pod", Timeout: 2 * time.Second}
	id, err := runner.Start(context.Background(), spec, testInitiator())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForTerminal(t, mem, id)

	frames := bus.onTopic(ws.RunTopic(id))
	var sawTimeout bool
	for _, rec := range frames {
		if decodeFrame(t, rec.msg).State == "timeout" {
			sawTimeout = true
		}
	}
	if !sawTimeout {
		t.Error("expected a progress frame with state=timeout for the 504 pair")
	}
}

// Cancelling the CALLER's context after Start returns must not stop a run already in flight.
func TestStartCallerContextCancelDoesNotStopRun(t *testing.T) {
	fake, ctrl := startFakeDiagnosticsServer(t)
	fake.delay = 100 * time.Millisecond
	bus := newRecordingBus()
	hub := ws.NewHub(bus, testMetrics(t))
	mem := checks.NewMemoryStore()
	runner := checks.NewRunner(ctrl, hub, bus, mem, testMetrics(t))

	callerCtx, cancel := context.WithCancel(context.Background())
	spec := checks.Spec{Sources: []string{"n1"}, Destinations: []string{"n2", "n3"}, Type: "tcp", Plane: "pod", Timeout: 5 * time.Second}
	id, err := runner.Start(callerCtx, spec, testInitiator())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancel() // simulate the browser closing the tab / the HTTP request ending

	run := waitForTerminal(t, mem, id)
	if run.Status != "succeeded" {
		t.Fatalf("run.Status = %q, want succeeded (caller ctx cancellation must not affect the run)", run.Status)
	}
	if run.PairOK != 2 {
		t.Errorf("run.PairOK = %d, want 2", run.PairOK)
	}
}

// With the memory store, Runner.Get works and the 51st run evicts the 1st.
func TestGetWithMemoryStoreRingEviction(t *testing.T) {
	mem := checks.NewMemoryStore()
	runner := checks.NewRunner(nil, nil, nil, mem, testMetrics(t))
	ctx := context.Background()

	var ids []string
	for i := 0; i < 51; i++ {
		id := fmt.Sprintf("run-%02d", i)
		if _, err := mem.CreateRun(ctx, id, "tcp", "pod", json.RawMessage(`{}`), "user", "u1", 1, time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("CreateRun(%d, time.Now().Add(time.Hour)): %v", i, err)
		}
		ids = append(ids, id)
	}

	if _, err := runner.Get(ctx, ids[0]); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get(first run) err = %v, want ErrNotFound (evicted)", err)
	}
	got, err := runner.Get(ctx, ids[50])
	if err != nil {
		t.Fatalf("Get(last run): %v", err)
	}
	if got.ID != ids[50] {
		t.Errorf("Get(last run).ID = %q, want %q", got.ID, ids[50])
	}
}

// hub.OpenTopic returning false (registry full, or the hub already shut down) must still let the
// run execute to completion.
func TestStartRunsToCompletionWhenOpenTopicReturnsFalse(t *testing.T) {
	fake, ctrl := startFakeDiagnosticsServer(t)
	_ = fake
	bus := newRecordingBus()
	hub := ws.NewHub(bus, testMetrics(t))

	hubCtx, hubCancel := context.WithCancel(context.Background())
	hubDone := make(chan struct{})
	go func() { hub.Run(hubCtx); close(hubDone) }()
	hubCancel()
	<-hubDone // hub is now closed; OpenTopic will refuse every topic from here on

	mem := checks.NewMemoryStore()
	runner := checks.NewRunner(ctrl, hub, bus, mem, testMetrics(t))

	spec := checks.Spec{Sources: []string{"n1"}, Destinations: []string{"n2"}, Type: "tcp", Plane: "pod", Timeout: 2 * time.Second}
	id, err := runner.Start(context.Background(), spec, testInitiator())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	run := waitForTerminal(t, mem, id)
	if run.Status != "succeeded" {
		t.Fatalf("run.Status = %q, want succeeded even though OpenTopic refused the topic", run.Status)
	}
}

// TestGetResultsReturnsPerPairRows is the httpapi seam GET /api/v1/runs/{id} needs.
func TestGetResultsReturnsPerPairRows(t *testing.T) {
	fake, ctrl := startFakeDiagnosticsServer(t)
	_ = fake
	bus := newRecordingBus()
	hub := ws.NewHub(bus, testMetrics(t))
	mem := checks.NewMemoryStore()
	runner := checks.NewRunner(ctrl, hub, bus, mem, testMetrics(t))

	spec := checks.Spec{Sources: []string{"n1"}, Destinations: []string{"n2", "n3"}, Type: "tcp", Plane: "pod", Timeout: 2 * time.Second}
	id, err := runner.Start(context.Background(), spec, testInitiator())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForTerminal(t, mem, id)

	results, _, err := runner.GetResults(context.Background(), id)
	if err != nil {
		t.Fatalf("GetResults: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("GetResults returned %d rows, want 2", len(results))
	}
	for _, r := range results {
		if r.RunID != id {
			t.Errorf("result.RunID = %q, want %q", r.RunID, id)
		}
		if !r.Success {
			t.Errorf("result for %s->%s: Success = false, want true", r.SourceNode, r.DestinationNode)
		}
	}
}

// TestGetResultsUnknownRunReturnsEmpty mirrors MemoryStore.GetRunResults'
// own "no rows is not itself a failure" contract (memory.go) -- an id
// naming no run must not be an error.
func TestGetResultsUnknownRunReturnsEmpty(t *testing.T) {
	mem := checks.NewMemoryStore()
	runner := checks.NewRunner(nil, nil, nil, mem, testMetrics(t))

	results, _, err := runner.GetResults(context.Background(), "does-not-exist")
	if err != nil {
		t.Fatalf("GetResults: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("GetResults(unknown) = %d rows, want 0", len(results))
	}
}

// TestWaitReturnsPromptlyWhenNoRunsInFlight covers Wait's trivial case: with
// nothing launched, it must return immediately rather than block until ctx's
// deadline.
func TestWaitReturnsPromptlyWhenNoRunsInFlight(t *testing.T) {
	runner := checks.NewRunner(nil, nil, nil, checks.NewMemoryStore(), testMetrics(t))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	runner.Wait(ctx)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Wait with no runs in flight took %s, want it to return promptly", elapsed)
	}
}

// TestWaitBlocksUntilRunFinishes is the shutdown-drain contract.
func TestWaitBlocksUntilRunFinishes(t *testing.T) {
	fake, ctrl := startFakeDiagnosticsServer(t)
	fake.delay = 200 * time.Millisecond
	bus := newRecordingBus()
	hub := ws.NewHub(bus, testMetrics(t))
	mem := checks.NewMemoryStore()
	runner := checks.NewRunner(ctrl, hub, bus, mem, testMetrics(t))

	spec := checks.Spec{Sources: []string{"n1"}, Destinations: []string{"n2"}, Type: "tcp", Plane: "pod", Timeout: 2 * time.Second}
	id, err := runner.Start(context.Background(), spec, testInitiator())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	runner.Wait(ctx)
	elapsed := time.Since(start)
	if elapsed >= 5*time.Second {
		t.Fatalf("Wait took %s, want it to return once the run finished, well before the 5s budget", elapsed)
	}

	run, err := mem.GetRun(context.Background(), id)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status == "pending" || run.Status == "running" {
		t.Errorf("run.Status = %q after Wait returned, want a terminal status", run.Status)
	}
}

// TestWaitReturnsAtBudgetWithRunStillInFlight is Wait's other half.
func TestWaitReturnsAtBudgetWithRunStillInFlight(t *testing.T) {
	fake, ctrl := startFakeDiagnosticsServer(t)
	fake.delay = 500 * time.Millisecond
	bus := newRecordingBus()
	hub := ws.NewHub(bus, testMetrics(t))
	mem := checks.NewMemoryStore()
	runner := checks.NewRunner(ctrl, hub, bus, mem, testMetrics(t))

	spec := checks.Spec{Sources: []string{"n1"}, Destinations: []string{"n2"}, Type: "tcp", Plane: "pod", Timeout: 5 * time.Second}
	if _, err := runner.Start(context.Background(), spec, testInitiator()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	runner.Wait(ctx)
	elapsed := time.Since(start)
	if elapsed > time.Second {
		t.Errorf("Wait took %s, want it to return promptly once ctx's budget (50ms) fired", elapsed)
	}
}

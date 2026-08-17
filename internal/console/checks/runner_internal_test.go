package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
	"github.com/EsDmitrii/kconmon-ng/internal/console/ws"
)

// That is deliberate, not a shortcut around runner_test.go's black-box style.

// slowFakeCtrl's Diagnose blocks for delay or until ctx is cancelled,
// whichever comes first -- the shape a slow controller/agent produces, which
// is what actually drives runCtx past its own deadline mid-dispatch below.
type slowFakeCtrl struct{ delay time.Duration }

func (f *slowFakeCtrl) Topology(context.Context) (*controllerclient.Topology, error) {
	return &controllerclient.Topology{}, nil
}

func (f *slowFakeCtrl) Diagnose(ctx context.Context, _ controllerclient.DiagnoseRequest, _ time.Duration) (json.RawMessage, error) {
	select {
	case <-time.After(f.delay):
		return json.RawMessage(`{"success":true}`), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// panicOnceFakeCtrl panics on its first Diagnose call, then answers normally.
type panicOnceFakeCtrl struct {
	panicked atomic.Bool
}

func (f *panicOnceFakeCtrl) Topology(context.Context) (*controllerclient.Topology, error) {
	return &controllerclient.Topology{}, nil
}

func (f *panicOnceFakeCtrl) Diagnose(context.Context, controllerclient.DiagnoseRequest, time.Duration) (json.RawMessage, error) {
	if f.panicked.CompareAndSwap(false, true) {
		panic("induced panic for runOneRecovered's test")
	}
	return json.RawMessage(`{"success":true}`), nil
}

// ctxObservingStore wraps a *MemoryStore and records whether ctx was already Done (i.e.
// runCtx-expired) at the moment FinishRun/UpsertRunResult were called.
type ctxObservingStore struct {
	*MemoryStore

	finishRunCalled          bool
	finishRunCtxWasLive      bool
	upsertResultCalled       bool
	upsertResultCtxWasLive   bool
	upsertSnapshotCalled     bool
	upsertSnapshotCtxWasLive bool
}

func (s *ctxObservingStore) FinishRun(ctx context.Context, id, status string, pairOK, pairFailed int32) error {
	s.finishRunCalled = true
	s.finishRunCtxWasLive = ctx.Err() == nil
	return s.MemoryStore.FinishRun(ctx, id, status, pairOK, pairFailed)
}

func (s *ctxObservingStore) UpsertRunResult(ctx context.Context, in store.RunResultInput) (store.RunResult, error) { //nolint:gocritic // hugeParam: matches store.RunStore.UpsertRunResult's own signature
	s.upsertResultCalled = true
	if ctx.Err() == nil {
		s.upsertResultCtxWasLive = true
	}
	return s.MemoryStore.UpsertRunResult(ctx, in)
}

func (s *ctxObservingStore) UpsertPathSnapshot(ctx context.Context, in store.PathSnapshotInput) (store.PathSnapshot, bool, error) { //nolint:gocritic // hugeParam: matches store.PathSnapshotStore's own signature
	s.upsertSnapshotCalled = true
	if ctx.Err() == nil {
		s.upsertSnapshotCtxWasLive = true
	}
	return s.MemoryStore.UpsertPathSnapshot(ctx, in)
}

// mtrThenCancelCtrl answers with a complete mtr trace and then cancels the run's own context before
// returning it; a pair whose result HAS arrived must still be projected.
type mtrThenCancelCtrl struct {
	cancel context.CancelFunc
	body   json.RawMessage
}

func (f *mtrThenCancelCtrl) Topology(context.Context) (*controllerclient.Topology, error) {
	return &controllerclient.Topology{}, nil
}

func (f *mtrThenCancelCtrl) Diagnose(context.Context, controllerclient.DiagnoseRequest, time.Duration) (json.RawMessage, error) {
	f.cancel()
	return f.body, nil
}

// The stuck-run reaper's cutoff MUST clear the longest duration Start accepts.
func TestMaxRunLifetimeCoversTheLongestAcceptedDuration(t *testing.T) {
	got := maxRunLifetime()
	if got <= MaxRunDuration {
		t.Fatalf("maxRunLifetime() = %s, want > MaxRunDuration (%s) -- the reaper would kill live interval runs", got, MaxRunDuration)
	}
	/* And it must clear the deadline Start actually hands the worst case, with the reap slack still
	   on top. The worst case is maxPairs pairs from ONE source: the per-source gate serialises those
	   hardest, and it is the gate runDeadline used to ignore. */
	worstPairs := make([]Pair, maxPairs)
	for i := range worstPairs {
		worstPairs[i] = Pair{Source: "one-source"}
	}
	worst := runDeadline(worstPairs, maxPerPairTimeout, MaxRunDuration)
	if got < worst+reapSlack {
		t.Errorf("maxRunLifetime() = %s, want at least %s (worst-case deadline + reapSlack)", got, worst+reapSlack)
	}
}

// The sampling schedule is a documented contract (the API states the cap), so
// its arithmetic is pinned rather than left to emerge.
func TestSampleIntervalAndPlannedRoundsRespectTheCap(t *testing.T) {
	for _, tc := range []struct {
		name         string
		duration     time.Duration
		wantInterval time.Duration
		wantRounds   int
	}{
		{"instant", 0, 0, 1},
		// Short runs sit on the MinSampleInterval floor.
		{"10s floor", 10 * time.Second, MinSampleInterval, 2},
		{"1m", time.Minute, MinSampleInterval, 12},
		{"15m", 15 * time.Minute, MinSampleInterval, 180},
		// From here the interval widens so the cap is never exceeded.
		{"1h", time.Hour, 7200 * time.Millisecond, MaxSamplesPerPair},
		{"24h", 24 * time.Hour, 172800 * time.Millisecond, MaxSamplesPerPair},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SampleInterval(tc.duration); got != tc.wantInterval {
				t.Errorf("SampleInterval(%s) = %s, want %s", tc.duration, got, tc.wantInterval)
			}
			// The base cadence governs whenever the check type is faster than it, which is every
			// type but mtr; PlannedSamplesPerPair is then the old plannedRounds exactly.
			if got := PlannedSamplesPerPair(tc.duration, SampleInterval(tc.duration)); got != tc.wantRounds {
				t.Errorf("PlannedSamplesPerPair(%s) = %d, want %d", tc.duration, got, tc.wantRounds)
			}
			if got := PlannedSamplesPerPair(tc.duration, SampleInterval(tc.duration)); got > MaxSamplesPerPair {
				t.Errorf("PlannedSamplesPerPair(%s) = %d exceeds the documented cap %d", tc.duration, got, MaxSamplesPerPair)
			}
		})
	}
}

// countingCtrl answers every Diagnose with the same successful result and
// counts the calls -- the seam for asserting that an interval run actually
// RE-PROBES rather than dispatching once and sleeping.
type countingCtrl struct {
	calls atomic.Int32
	topo  *controllerclient.Topology
}

func (c *countingCtrl) Topology(context.Context) (*controllerclient.Topology, error) {
	return c.topo, nil
}

func (c *countingCtrl) Diagnose(context.Context, controllerclient.DiagnoseRequest, time.Duration) (json.RawMessage, error) {
	c.calls.Add(1)
	return json.RawMessage(`{"type":"tcp","success":true,"duration":1000000}`), nil
}

// An interval run must probe every pair ONCE PER ROUND and keep every probe as its own sample.
func TestExecuteIntervalRunKeepsEverySampleAndReProbes(t *testing.T) {
	m := metrics.New("kconmon_ng_test_interval", prometheus.NewRegistry())
	bus := cache.NewInProcessBus()
	hub := ws.NewHub(bus, m)
	st := NewMemoryStore()
	ctrl := &countingCtrl{}
	r := NewRunner(ctrl, hub, bus, st, m)

	spec := Spec{
		Sources: []string{"n1"}, Destinations: []string{"n2", "n3"},
		// Three 50ms boundaries fit before 130ms elapses, which is how a wall-clock run says
		// "three rounds": the count is no longer a cap the loop stops on.
		Type: "tcp", Plane: "pod", Timeout: 1 * time.Second, Duration: 130 * time.Millisecond,
	}
	pairs := []Pair{
		{Source: "n1", Destination: NodeDestination("n2")},
		{Source: "n1", Destination: NodeDestination("n3")},
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	run, err := st.CreateRun(context.Background(), uuid.NewString(), spec.Type, spec.Plane, specJSON, "user", "u1", int32(len(pairs)), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	runCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	const rounds = 3
	r.execute(runCtx, cancel, run.ID, pairs, &spec, 1*time.Second, rounds, 50*time.Millisecond, false)

	// 2 pairs x 3 rounds.
	if got := ctrl.calls.Load(); got != 6 {
		t.Errorf("Diagnose calls = %d, want 6 (2 pairs x 3 rounds)", got)
	}
	results, _, err := st.GetRunResults(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("GetRunResults: %v", err)
	}
	if len(results) != 6 {
		t.Fatalf("stored results = %d, want 6 -- every sample is kept, not overwritten", len(results))
	}
	// Each pair carries seqs 0,1,2 exactly once.
	seqs := map[string]map[int32]int{}
	for i := range results {
		key := results[i].SourceNode + "->" + results[i].DestinationNode
		if seqs[key] == nil {
			seqs[key] = map[int32]int{}
		}
		seqs[key][results[i].SampleSeq]++
	}
	for key, bySeq := range seqs {
		for seq := int32(0); seq < rounds; seq++ {
			if bySeq[seq] != 1 {
				t.Errorf("pair %s sample_seq %d appeared %d times, want exactly 1", key, seq, bySeq[seq])
			}
		}
	}

	final, err := st.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if final.Status != "succeeded" {
		t.Errorf("status = %q, want succeeded", final.Status)
	}
	// pairOk/pairFailed count PAIRS, never samples: 2 pairs x 3 all-ok rounds is
	// 2/2, the same numerator the run detail page shows.
	if final.PairOK != 2 || final.PairFailed != 0 {
		t.Errorf("pair counts = ok:%d failed:%d, want ok:2 failed:0 (pairs, not samples)", final.PairOK, final.PairFailed)
	}
	if final.PairTotal != 2 {
		t.Errorf("PairTotal = %d, want 2 -- ok/total must be commensurable", final.PairTotal)
	}
}

// lastRoundFailsCtrl succeeds for every pair except one destination's FINAL
// sample -- the shape that separates "latest sample" from "any sample".
type lastRoundFailsCtrl struct {
	failDestination string
	rounds          int32

	mu    sync.Mutex
	calls map[string]int32
}

func (c *lastRoundFailsCtrl) Topology(context.Context) (*controllerclient.Topology, error) {
	return nil, nil //nolint:nilnil // never consulted by execute
}

func (c *lastRoundFailsCtrl) Diagnose(_ context.Context, req controllerclient.DiagnoseRequest, _ time.Duration) (json.RawMessage, error) {
	c.mu.Lock()
	if c.calls == nil {
		c.calls = map[string]int32{}
	}
	c.calls[req.Destination]++
	n := c.calls[req.Destination]
	c.mu.Unlock()
	if req.Destination == c.failDestination && n == c.rounds {
		return json.RawMessage(`{"type":"tcp","success":false,"error":"last sample failed"}`), nil
	}
	return json.RawMessage(`{"type":"tcp","success":true,"duration":1000000}`), nil
}

// The stored pairOk/pairFailed an interval run finishes with must apply the run
// detail page's rule -- a pair is OK when its LATEST sample succeeded -- so the
// history list and the detail page cannot disagree about the same run.
func TestExecuteIntervalRunCountsPairsByTheirLatestSample(t *testing.T) {
	m := metrics.New("kconmon_ng_test_latestsample", prometheus.NewRegistry())
	bus := cache.NewInProcessBus()
	hub := ws.NewHub(bus, m)
	st := NewMemoryStore()
	const rounds = 3
	ctrl := &lastRoundFailsCtrl{failDestination: "n3", rounds: rounds}
	r := NewRunner(ctrl, hub, bus, st, m)

	spec := Spec{
		Sources: []string{"n1"}, Destinations: []string{"n2", "n3"},
		Type: "tcp", Plane: "pod", Timeout: time.Second, Duration: 130 * time.Millisecond,
	}
	pairs := []Pair{
		{Source: "n1", Destination: NodeDestination("n2")},
		{Source: "n1", Destination: NodeDestination("n3")},
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	run, err := st.CreateRun(context.Background(), uuid.NewString(), spec.Type, spec.Plane, specJSON, "user", "u1", int32(len(pairs)), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	runCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	r.execute(runCtx, cancel, run.ID, pairs, &spec, time.Second, rounds, 50*time.Millisecond, false)

	final, err := st.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	// n3 sent 2 OK samples and then failed; counted per sample it would be 5/1.
	if final.PairOK != 1 || final.PairFailed != 1 {
		t.Errorf("pair counts = ok:%d failed:%d, want ok:1 failed:1 (n3's latest sample failed)", final.PairOK, final.PairFailed)
	}
	// The status still reads every sample: one bad sample out of six is partial.
	if final.Status != "partial" {
		t.Errorf("status = %q, want partial", final.Status)
	}
}

// An instant run is one round and must be indistinguishable.
func TestExecuteInstantRunStillWritesOneSampleZeroPerPair(t *testing.T) {
	m := metrics.New("kconmon_ng_test_instant", prometheus.NewRegistry())
	bus := cache.NewInProcessBus()
	hub := ws.NewHub(bus, m)
	st := NewMemoryStore()
	ctrl := &countingCtrl{}
	r := NewRunner(ctrl, hub, bus, st, m)

	spec := Spec{Sources: []string{"n1"}, Destinations: []string{"n2"}, Type: "tcp", Plane: "pod", Timeout: time.Second}
	pairs := []Pair{{Source: "n1", Destination: NodeDestination("n2")}}
	specJSON, _ := json.Marshal(spec) //nolint:errcheck // fixed spec always marshals
	run, err := st.CreateRun(context.Background(), uuid.NewString(), spec.Type, spec.Plane, specJSON, "user", "u1", 1, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	runCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	r.execute(runCtx, cancel, run.ID, pairs, &spec, time.Second, PlannedSamplesPerPair(0, 0), SampleInterval(0), false)

	results, _, err := st.GetRunResults(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("GetRunResults: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("stored results = %d, want 1", len(results))
	}
	if results[0].SampleSeq != 0 {
		t.Errorf("sample_seq = %d, want 0 for an instant run", results[0].SampleSeq)
	}
	if got := ctrl.calls.Load(); got != 1 {
		t.Errorf("Diagnose calls = %d, want 1", got)
	}
	// And the spec snapshot must not have grown a duration key.
	if strings.Contains(string(specJSON), "Duration") {
		t.Errorf("instant spec snapshot = %s, want no Duration key (omitempty)", specJSON)
	}
}

// Cancelling an interval run mid-flight stops the ROUND LOOP, not just the round in progress.
func TestExecuteIntervalRunCancelStopsTheRoundLoop(t *testing.T) {
	m := metrics.New("kconmon_ng_test_intervalcancel", prometheus.NewRegistry())
	bus := cache.NewInProcessBus()
	hub := ws.NewHub(bus, m)
	st := NewMemoryStore()
	ctrl := &countingCtrl{}
	r := NewRunner(ctrl, hub, bus, st, m)

	spec := Spec{
		Sources: []string{"n1"}, Destinations: []string{"n2"},
		Type: "tcp", Plane: "pod", Timeout: time.Second, Duration: time.Hour,
	}
	pairs := []Pair{{Source: "n1", Destination: NodeDestination("n2")}}
	specJSON, _ := json.Marshal(spec) //nolint:errcheck // fixed spec always marshals
	run, err := st.CreateRun(context.Background(), uuid.NewString(), spec.Type, spec.Plane, specJSON, "user", "u1", 1, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	runCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	// Register the control the way Start does, so the cancelled flag is what
	// decides the terminal status.
	ctl := &runControl{cancel: cancel}
	r.runControls.Store(run.ID, ctl)

	go func() {
		time.Sleep(60 * time.Millisecond)
		ctl.cancelled.Store(true)
		ctl.cancel()
	}()

	done := make(chan struct{})
	go func() {
		// 500 rounds, 20ms apart, would be 10 seconds if the cancel were ignored.
		r.execute(runCtx, cancel, run.ID, pairs, &spec, time.Second, 500, 20*time.Millisecond, false)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("execute did not return after cancel -- the round loop ignored runCtx")
	}

	final, err := st.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if final.Status != statusCancelled {
		t.Errorf("status = %q, want %q", final.Status, statusCancelled)
	}
	if got := ctrl.calls.Load(); got >= 500 {
		t.Errorf("Diagnose calls = %d, want far fewer than the 500 planned rounds", got)
	}
}

// A trace that arrived must reach path history even though the run's own context was cancelled
// between the dispatch and the write.
func TestExecuteProjectsMTRSnapshotOnAContextOutlivingTheRun(t *testing.T) {
	m := metrics.New("kconmon_ng_test_m5", prometheus.NewRegistry())
	bus := cache.NewInProcessBus()
	hub := ws.NewHub(bus, m)
	st := &ctxObservingStore{MemoryStore: NewMemoryStore()}

	runCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	ctrl := &mtrThenCancelCtrl{cancel: cancel, body: json.RawMessage(
		`{"type":"mtr","success":true,"details":{"target":"10.0.0.2","hops":[` +
			`{"number":1,"ip":"10.0.0.254","rtt":500000,"lossRatio":0},` +
			`{"number":2,"ip":"10.0.0.2","rtt":2000000,"lossRatio":0}]}}`)}
	r := NewRunner(ctrl, hub, bus, st, m)

	spec := Spec{Sources: []string{"n1"}, Destinations: []string{"n2"}, Type: "mtr", Plane: "pod", Timeout: 1 * time.Second}
	pairs := []Pair{{Source: "n1", Destination: NodeDestination("n2")}}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	// Start mints one with uuid.NewString, so a placeholder here would test a shape production never
	// produces.
	run, err := st.CreateRun(context.Background(), uuid.NewString(), spec.Type, spec.Plane, specJSON, "user", "u1", int32(len(pairs)), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// The topic is deliberately left unopened: this test is about the store hook.
	r.execute(runCtx, cancel, run.ID, pairs, &spec, 1*time.Second, 1, 0, false)

	if !st.upsertSnapshotCalled {
		t.Fatal("UpsertPathSnapshot was never called for a successful mtr pair")
	}
	if !st.upsertSnapshotCtxWasLive {
		t.Error("UpsertPathSnapshot's ctx was already Done -- it must run on a context derived from context.WithoutCancel(runCtx), not runCtx itself")
	}
	if got := testutil.ToFloat64(m.MTRSnapshots.WithLabelValues("new-path")); got != 1 {
		t.Errorf("MTRSnapshots(new-path) = %v, want 1", got)
	}
}

// A run whose runCtx deadline fires WHILE a pair is still dispatching (a slow controller/agent --
// exactly the case runDeadline's own slack cannot always absorb) must still reach FinishRun and end
// up in a terminal status.
func TestExecuteFinishesRunAfterRunCtxDeadlineFiresMidDispatch(t *testing.T) {
	m := metrics.New("kconmon_ng_test_i1", prometheus.NewRegistry())
	bus := cache.NewInProcessBus()
	hub := ws.NewHub(bus, m)
	st := &ctxObservingStore{MemoryStore: NewMemoryStore()}
	ctrl := &slowFakeCtrl{delay: 300 * time.Millisecond}
	r := NewRunner(ctrl, hub, bus, st, m)

	spec := Spec{Sources: []string{"n1"}, Destinations: []string{"n2"}, Type: "tcp", Plane: "pod", Timeout: 1 * time.Second}
	pairs := []Pair{{Source: "n1", Destination: NodeDestination("n2")}}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	run, err := st.CreateRun(context.Background(), "run-i1", spec.Type, spec.Plane, specJSON, "user", "u1", int32(len(pairs)), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// runCtx's deadline (30ms) fires well before the fake controller's 300ms
	// dispatch delay does -- exactly the "runCtx expires mid-flight" case.
	runCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	topic := ws.RunTopic(run.ID)
	topicOpen := hub.OpenTopic(runCtx, topic)

	r.execute(runCtx, cancel, run.ID, pairs, &spec, 1*time.Second, 1, 0, topicOpen)

	if !st.finishRunCalled {
		t.Fatal("FinishRun was never called")
	}
	if !st.finishRunCtxWasLive {
		t.Error("FinishRun's ctx was already Done -- it must run on a context derived from context.WithoutCancel(runCtx), not runCtx itself")
	}
	if !st.upsertResultCalled {
		t.Fatal("UpsertRunResult was never called")
	}
	if !st.upsertResultCtxWasLive {
		t.Error("UpsertRunResult's ctx was already Done -- it must run on a context derived from context.WithoutCancel(runCtx), not runCtx itself")
	}

	got, err := st.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Status == "pending" || got.Status == "running" {
		t.Errorf("run.Status = %q, want a terminal status even though runCtx expired mid-flight", got.Status)
	}
}

// A panic inside one pair's dispatch must not crash the process or hang wg.Wait forever waiting on
// a goroutine that never reached wg.Done.
func TestRunOneRecoveredSurvivesAPanicAndRecordsTheirPairFailed(t *testing.T) {
	m := metrics.New("kconmon_ng_test_c", prometheus.NewRegistry())
	bus := cache.NewInProcessBus()
	hub := ws.NewHub(bus, m)
	st := NewMemoryStore()
	ctrl := &panicOnceFakeCtrl{}
	r := NewRunner(ctrl, hub, bus, st, m)

	spec := Spec{Sources: []string{"n1"}, Destinations: []string{"n2", "n3"}, Type: "tcp", Plane: "pod", Timeout: 2 * time.Second}
	pairs := []Pair{{Source: "n1", Destination: NodeDestination("n2")}, {Source: "n1", Destination: NodeDestination("n3")}}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	run, err := st.CreateRun(context.Background(), "run-c", spec.Type, spec.Plane, specJSON, "user", "u1", int32(len(pairs)), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	runCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	topic := ws.RunTopic(run.ID)
	topicOpen := hub.OpenTopic(runCtx, topic)

	// If runOneRecovered did not recover the panic.
	r.execute(runCtx, cancel, run.ID, pairs, &spec, 2*time.Second, 1, 0, topicOpen)

	got, err := st.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Status != "partial" {
		t.Fatalf("run.Status = %q, want partial (one panicked pair failed, one succeeded)", got.Status)
	}
	if got.PairOK != 1 || got.PairFailed != 1 {
		t.Errorf("run pair counts = ok:%d failed:%d, want ok:1 failed:1", got.PairOK, got.PairFailed)
	}
}

/* ── the deadline has to cover the gate that actually paces the run ──────── */

/*
 * dispatchRound gates every pair twice: the run-wide window (maxConcurrency) and the per-source one
 * (maxPerSourceConcurrency, four times narrower). runDeadline counted only the first, so a run whose
 * pairs all leave the SAME node was given a context a quarter as long as the work needs: dispatch
 * stopped mid-flight at the deadline, the reaper later marked the run cancelled, and the pairs that
 * never ran left no result at all — a diagnostics run that answered less than it was asked.
 */
func TestRunDeadlineCoversASingleSourceFanOut(t *testing.T) {
	const perPair = 90 * time.Second
	pairs := make([]Pair, 12)
	for i := range pairs {
		pairs[i] = Pair{Source: "node-a", Destination: NodeDestination(fmt.Sprintf("node-%d", i))}
	}

	got := runDeadline(pairs, perPair, 0)
	// Twelve pairs, one source, two at a time: six sequential batches.
	want := 6*perPair + runDeadlineSlack
	if got != want {
		t.Errorf("runDeadline for 12 pairs from one source = %s, want %s (the per-source gate paces it)", got, want)
	}

	// Spread across sources the run-wide window governs instead, and the deadline is shorter.
	spread := make([]Pair, 12)
	for i := range spread {
		spread[i] = Pair{Source: fmt.Sprintf("node-%d", i), Destination: NodeDestination("node-x")}
	}
	if got := runDeadline(spread, perPair, 0); got >= want {
		t.Errorf("runDeadline for 12 pairs from 12 sources = %s, want less than the single-source %s", got, want)
	}
}

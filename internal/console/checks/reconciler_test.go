package checks_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/EsDmitrii/kconmon-ng/internal/console/checks"
	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// reconcileLockKey stands in for scheduler.LockKey, which this package cannot
// import (internal/console/scheduler imports checks). The reconciler takes the
// key as a dependency for exactly that reason, so any value drives the tests.
const reconcileLockKey int64 = 2111970501

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// fakeReconcileLocker is the advisory-lock seam. leader == false reproduces
// the steady state on every replica but one: WithAdvisoryLock reports
// (false, nil) and never runs the tick's work.
type fakeReconcileLocker struct {
	leader bool
	err    error
	keys   []int64
}

func (f *fakeReconcileLocker) WithAdvisoryLock(ctx context.Context, key int64, fn func(context.Context) error) (bool, error) {
	f.keys = append(f.keys, key)
	if f.err != nil {
		return false, f.err
	}
	if !f.leader {
		return false, nil
	}
	return true, fn(ctx)
}

// fakeReconcileStore is an in-memory check_schedules / check_definitions /
// targets. It counts every read so the not-leader test can assert that a
// non-holder issues none at all.
type fakeReconcileStore struct {
	schedules   []store.Schedule
	definitions map[string]store.Definition
	targets     map[string]store.Target

	listErr error
	reads   int
}

func newFakeReconcileStore() *fakeReconcileStore {
	return &fakeReconcileStore{
		definitions: map[string]store.Definition{},
		targets:     map[string]store.Target{},
	}
}

func (f *fakeReconcileStore) ListSchedules(_ context.Context, _ store.ScheduleFilter) (store.SchedulePage, error) {
	f.reads++
	if f.listErr != nil {
		return store.SchedulePage{}, f.listErr
	}
	return store.SchedulePage{Schedules: f.schedules}, nil
}

func (f *fakeReconcileStore) GetDefinition(_ context.Context, id string) (store.Definition, error) {
	f.reads++
	d, ok := f.definitions[id]
	if !ok {
		return store.Definition{}, store.ErrNotFound
	}
	return d, nil
}

func (f *fakeReconcileStore) GetTarget(_ context.Context, id string) (store.Target, error) {
	f.reads++
	t, ok := f.targets[id]
	if !ok {
		return store.Target{}, store.ErrNotFound
	}
	return t, nil
}

// fakeTopology serves a snapshot the test mutates between ticks.
type fakeTopology struct {
	snap  *controllerclient.Topology
	err   error
	calls int
}

func (f *fakeTopology) Topology(context.Context) (*controllerclient.Topology, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.snap, nil
}

// fakeExternalChecksAPI records every PUT body and can fail persistently --
// the behaviour a real controller cannot be asked for on demand.
type fakeExternalChecksAPI struct {
	puts []map[string][]controllerclient.ExternalCheckSpec
	err  error
}

func (f *fakeExternalChecksAPI) PutExternalChecks(
	_ context.Context, agents map[string][]controllerclient.ExternalCheckSpec,
) (*controllerclient.ExternalChecksResult, error) {
	// Copied, not aliased: the reconciler is free to reuse or mutate the map
	// it handed over, and a test that asserted against a live reference would
	// pass or fail on that implementation detail.
	snapshot := make(map[string][]controllerclient.ExternalCheckSpec, len(agents))
	for id, specs := range agents {
		snapshot[id] = append([]controllerclient.ExternalCheckSpec(nil), specs...)
	}
	f.puts = append(f.puts, snapshot)
	if f.err != nil {
		return nil, f.err
	}
	return &controllerclient.ExternalChecksResult{Agents: len(agents), Changed: len(agents)}, nil
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

type reconcileHarness struct {
	lock  *fakeReconcileLocker
	st    *fakeReconcileStore
	topo  *fakeTopology
	ctrl  *fakeExternalChecksAPI
	m     *metrics.Metrics
	reg   *prometheus.Registry
	recon *checks.Reconciler
}

func newReconcileHarness(t *testing.T) *reconcileHarness {
	t.Helper()
	reg := prometheus.NewRegistry()
	h := &reconcileHarness{
		lock: &fakeReconcileLocker{leader: true},
		st:   newFakeReconcileStore(),
		topo: &fakeTopology{snap: topologyOf(agentAt("node-a", "zone-a"))},
		ctrl: &fakeExternalChecksAPI{},
		m:    metrics.New("kconmon_ng", reg),
		reg:  reg,
	}
	h.recon = checks.NewReconciler(checks.ReconcilerDeps{
		Lock: h.lock, Store: h.st, Topology: h.topo, Controller: h.ctrl,
		Metrics: h.m, Interval: time.Second, LockKey: reconcileLockKey,
	})
	return h
}

// addContinuous registers an enabled continuous schedule and its definition.
func (h *reconcileHarness) addContinuous(id, checkType, selection string) {
	h.st.schedules = append(h.st.schedules, store.Schedule{
		ID: "sched-" + id, DefinitionID: id, Kind: "continuous", Enabled: true,
	})
	h.st.definitions[id] = store.Definition{
		ID: id, Name: "def-" + id, SourceSelection: selection,
		DestinationKind: "adhoc", DestinationAddress: "api.example.com:443",
		CheckType: checkType, Plane: "pod", Params: json.RawMessage(`{}`), Enabled: true,
	}
}

func agentAt(node, zone string) controllerclient.Agent {
	return controllerclient.Agent{ID: node + "-pod", NodeName: node, Zone: zone}
}

func topologyOf(agents ...controllerclient.Agent) *controllerclient.Topology {
	return &controllerclient.Topology{Agents: agents}
}

func (h *reconcileHarness) reconcileCount(result string) float64 {
	return testutil.ToFloat64(h.m.ExternalReconciles.WithLabelValues(result))
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestReconcilerIdenticalTopologyPutsOnce is change detection's core claim:
// the steady state is one PUT, not one per tick. The second tick still
// computes the whole desired state -- it just recognizes it.
func TestReconcilerIdenticalTopologyPutsOnce(t *testing.T) {
	h := newReconcileHarness(t)
	h.addContinuous("def-1", "tcp", "one-per-zone")

	h.recon.Tick(context.Background())
	h.recon.Tick(context.Background())

	if len(h.ctrl.puts) != 1 {
		t.Fatalf("PUTs = %d, want exactly 1 for an unchanged desired state", len(h.ctrl.puts))
	}
	if got := h.reconcileCount("pushed"); got != 1 {
		t.Errorf("ExternalReconciles{pushed} = %v, want 1", got)
	}
	if got := h.reconcileCount("unchanged"); got != 1 {
		t.Errorf("ExternalReconciles{unchanged} = %v, want 1", got)
	}
	if got := h.lock.keys; len(got) != 2 || got[0] != reconcileLockKey || got[1] != reconcileLockKey {
		t.Errorf("lock keys = %v, want two attempts on %d", got, reconcileLockKey)
	}
}

// TestReconcilerNewZoneTriggersPut and its sibling below are the determinism observed end to end.
func TestReconcilerNewZoneTriggersPut(t *testing.T) {
	h := newReconcileHarness(t)
	h.addContinuous("def-1", "tcp", "one-per-zone")

	h.recon.Tick(context.Background())
	h.topo.snap = topologyOf(agentAt("node-a", "zone-a"), agentAt("node-b", "zone-b"))
	h.recon.Tick(context.Background())

	if len(h.ctrl.puts) != 2 {
		t.Fatalf("PUTs = %d, want 2 (a new zone needs its own representative)", len(h.ctrl.puts))
	}
	last := h.ctrl.puts[1]
	if len(last) != 2 {
		t.Fatalf("assigned agents = %d, want 2 (one per zone)", len(last))
	}
	for _, agentID := range []string{"node-a-pod", "node-b-pod"} {
		if _, ok := last[agentID]; !ok {
			t.Errorf("agent %q missing from the assignment; got keys %v", agentID, keysOf(last))
		}
	}
}

func TestReconcilerRepresentedZoneDoesNotTriggerPut(t *testing.T) {
	h := newReconcileHarness(t)
	h.addContinuous("def-1", "tcp", "one-per-zone")

	h.recon.Tick(context.Background())
	// node-b joins zone-a, which node-a already represents. one-per-zone picks
	// the sorted-first node name, so node-a keeps the probe and the desired
	// state is byte-identical.
	h.topo.snap = topologyOf(agentAt("node-a", "zone-a"), agentAt("node-b", "zone-a"))
	h.recon.Tick(context.Background())

	if len(h.ctrl.puts) != 1 {
		t.Fatalf("PUTs = %d, want 1 (a node in a represented zone changes nothing)", len(h.ctrl.puts))
	}
	if got := h.reconcileCount("unchanged"); got != 1 {
		t.Errorf("ExternalReconciles{unchanged} = %v, want 1", got)
	}
}

// TestReconcilerDisabledDefinitionDropsItsSpecs: the operator switches a
// definition off and the next PUT no longer carries it -- without any delta
// tracking, because the whole desired state is recomputed.
func TestReconcilerDisabledDefinitionDropsItsSpecs(t *testing.T) {
	h := newReconcileHarness(t)
	h.addContinuous("def-1", "tcp", "all")
	h.addContinuous("def-2", "icmp", "all")

	h.recon.Tick(context.Background())
	if got := len(h.ctrl.puts[0]["node-a-pod"]); got != 2 {
		t.Fatalf("first PUT carried %d specs for node-a-pod, want 2", got)
	}

	def := h.st.definitions["def-1"]
	def.Enabled = false
	h.st.definitions["def-1"] = def
	h.recon.Tick(context.Background())

	if len(h.ctrl.puts) != 2 {
		t.Fatalf("PUTs = %d, want 2 (disabling a definition is a change)", len(h.ctrl.puts))
	}
	specs := h.ctrl.puts[1]["node-a-pod"]
	if len(specs) != 1 || specs[0].DefinitionID != "def-2" {
		t.Errorf("second PUT specs = %+v, want only def-2", specs)
	}
}

// TestReconcilerPersistentFailureRetriesNextTick: a controller that keeps failing is counted and
// logged, the loop stays alive.
func TestReconcilerPersistentFailureRetriesNextTick(t *testing.T) {
	h := newReconcileHarness(t)
	h.addContinuous("def-1", "tcp", "all")
	h.ctrl.err = controllerclient.ErrUnavailable

	h.recon.Tick(context.Background())
	h.recon.Tick(context.Background())

	if len(h.ctrl.puts) != 2 {
		t.Fatalf("PUT attempts = %d, want 2 (a failed PUT must be retried next tick)", len(h.ctrl.puts))
	}
	if got := h.reconcileCount("error"); got != 2 {
		t.Errorf("ExternalReconciles{error} = %v, want 2", got)
	}
	if got := h.reconcileCount("unchanged"); got != 0 {
		t.Errorf("ExternalReconciles{unchanged} = %v, want 0 (a failure must not mark the state clean)", got)
	}

	// The ticker is alive and self-healing: the moment the controller recovers
	// the same tick loop pushes successfully.
	h.ctrl.err = nil
	h.recon.Tick(context.Background())
	if got := h.reconcileCount("pushed"); got != 1 {
		t.Errorf("ExternalReconciles{pushed} = %v, want 1 after recovery", got)
	}
}

// TestReconcilerNotLeaderTouchesNothing: a replica that did not get the lock
// issues no store read, no topology request and no PUT -- one counter
// increment and nothing else.
func TestReconcilerNotLeaderTouchesNothing(t *testing.T) {
	h := newReconcileHarness(t)
	h.addContinuous("def-1", "tcp", "all")
	h.lock.leader = false

	h.recon.Tick(context.Background())

	if h.st.reads != 0 {
		t.Errorf("store reads = %d, want 0 for a non-holder", h.st.reads)
	}
	if h.topo.calls != 0 {
		t.Errorf("topology reads = %d, want 0 for a non-holder", h.topo.calls)
	}
	if len(h.ctrl.puts) != 0 {
		t.Errorf("PUTs = %d, want 0 for a non-holder", len(h.ctrl.puts))
	}
	if got := h.reconcileCount("not-leader"); got != 1 {
		t.Errorf("ExternalReconciles{not-leader} = %v, want 1", got)
	}
}

// TestReconcilerSkipsIneligibleCheckTypes: mtr and udp are never PUT. The
// controller answers 400 for the whole body on either, so one such definition
// would otherwise cost every other agent its entire assignment.
func TestReconcilerSkipsIneligibleCheckTypes(t *testing.T) {
	for _, checkType := range []string{"mtr", "udp"} {
		t.Run(checkType, func(t *testing.T) {
			h := newReconcileHarness(t)
			h.addContinuous("def-bad", checkType, "all")
			h.addContinuous("def-ok", "http", "all")

			h.recon.Tick(context.Background())
			h.recon.Tick(context.Background())

			if len(h.ctrl.puts) != 1 {
				t.Fatalf("PUTs = %d, want 1", len(h.ctrl.puts))
			}
			specs := h.ctrl.puts[0]["node-a-pod"]
			if len(specs) != 1 || specs[0].DefinitionID != "def-ok" {
				t.Fatalf("PUT specs = %+v, want only def-ok", specs)
			}
			// Counted on EVERY tick (a steady rate is what makes it
			// alertable), even though it is only logged once.
			if got := testutil.ToFloat64(h.m.ExternalSpecsSkipped.WithLabelValues("check-type")); got != 2 {
				t.Errorf("ExternalSpecsSkipped{check-type} = %v, want 2 (once per tick)", got)
			}
		})
	}
}

// TestReconcilerWarnsOnceForASkippedDefinition pins the log cadence the metric
// deliberately does not follow: one line per definition per process, however
// many ticks run.
func TestReconcilerWarnsOnceForASkippedDefinition(t *testing.T) {
	h := newReconcileHarness(t)
	h.addContinuous("def-bad", "mtr", "all")
	h.addContinuous("def-ok", "tcp", "all")

	logs := captureReconcileLogs(t)
	for range 3 {
		h.recon.Tick(context.Background())
	}

	if got := strings.Count(logs.String(), "def-bad"); got != 1 {
		t.Errorf("log lines naming def-bad = %d, want exactly 1 over 3 ticks:\n%s", got, logs.String())
	}
}

// TestReconcilerGaugeTracksProjectedSeries: the gauge is AssignAgents' own
// projection, refreshed on a push and on an unchanged tick alike, because the
// number describes the assignment currently in force either way.
func TestReconcilerGaugeTracksProjectedSeries(t *testing.T) {
	h := newReconcileHarness(t)
	h.addContinuous("def-1", "tcp", "all")
	h.topo.snap = topologyOf(agentAt("node-a", "zone-a"), agentAt("node-b", "zone-b"))

	h.recon.Tick(context.Background())
	if got := testutil.ToFloat64(h.m.ExternalSeriesProjected.WithLabelValues()); got != 2 {
		t.Fatalf("ExternalSeriesProjected = %v, want 2 (one definition x two agents)", got)
	}

	// An unchanged tick keeps it current rather than letting it go stale.
	h.recon.Tick(context.Background())
	if got := testutil.ToFloat64(h.m.ExternalSeriesProjected.WithLabelValues()); got != 2 {
		t.Errorf("ExternalSeriesProjected after an unchanged tick = %v, want 2", got)
	}

	// A third agent under "all" widens the projection.
	h.topo.snap = topologyOf(agentAt("node-a", "zone-a"), agentAt("node-b", "zone-b"), agentAt("node-c", "zone-c"))
	h.recon.Tick(context.Background())
	if got := testutil.ToFloat64(h.m.ExternalSeriesProjected.WithLabelValues()); got != 3 {
		t.Errorf("ExternalSeriesProjected = %v, want 3", got)
	}
}

// TestReconcilerPutBodyShape pins the wire contract the handler decodes.
func TestReconcilerPutBodyShape(t *testing.T) {
	h := newReconcileHarness(t)
	h.st.schedules = append(h.st.schedules, store.Schedule{
		ID: "sched-1", DefinitionID: "def-1", Kind: "continuous", Enabled: true,
	})
	h.st.definitions["def-1"] = store.Definition{
		ID: "def-1", Name: "dns-root", SourceSelection: "all",
		DestinationKind: "target", DestinationTargetID: "target-1",
		CheckType: "dns", Plane: "pod", Params: json.RawMessage(`{"query":"example.com"}`), Enabled: true,
	}
	h.st.targets["target-1"] = store.Target{ID: "target-1", Name: "dns-root", Kind: "host", Address: "8.8.8.8:53"}

	h.recon.Tick(context.Background())

	specs, ok := h.ctrl.puts[0]["node-a-pod"]
	if !ok {
		t.Fatalf("assignment is not keyed by the controller's agent ID; got keys %v", keysOf(h.ctrl.puts[0]))
	}
	if len(specs) != 1 {
		t.Fatalf("specs = %d, want 1", len(specs))
	}
	got := specs[0]
	want := controllerclient.ExternalCheckSpec{
		DefinitionID: "def-1",
		Target:       controllerclient.ExternalTarget{Name: "dns-root", Kind: "host", Address: "8.8.8.8", Port: 53},
		CheckType:    "dns",
		IntervalNs:   (30 * time.Second).Nanoseconds(),
		TimeoutNs:    (5 * time.Second).Nanoseconds(),
		Params:       json.RawMessage(`{"query":"example.com"}`),
	}
	if got.DefinitionID != want.DefinitionID || got.Target != want.Target || got.CheckType != want.CheckType ||
		got.IntervalNs != want.IntervalNs || got.TimeoutNs != want.TimeoutNs || string(got.Params) != string(want.Params) {
		t.Errorf("spec = %+v, want %+v", got, want)
	}
}

// TestReconcilerNonContinuousSchedulesAreIgnored: interval and once schedules
// belong to the scheduler, and a definition reachable only through one of them
// must never end up in an agent's continuous assignment.
func TestReconcilerNonContinuousSchedulesAreIgnored(t *testing.T) {
	h := newReconcileHarness(t)
	h.addContinuous("def-1", "tcp", "all")
	h.st.schedules = append(h.st.schedules,
		store.Schedule{ID: "s-int", DefinitionID: "def-interval", Kind: "interval", IntervalNs: 1, Enabled: true},
		// A DISABLED continuous schedule is equally out of scope: the operator
		// paused it, and a paused check must stop probing.
		store.Schedule{ID: "s-off", DefinitionID: "def-off", Kind: "continuous", Enabled: false},
	)
	h.st.definitions["def-interval"] = store.Definition{
		ID: "def-interval", Name: "interval-def", SourceSelection: "all",
		DestinationKind: "adhoc", DestinationAddress: "a.example.com", CheckType: "tcp", Enabled: true,
	}
	h.st.definitions["def-off"] = store.Definition{
		ID: "def-off", Name: "paused-def", SourceSelection: "all",
		DestinationKind: "adhoc", DestinationAddress: "b.example.com", CheckType: "tcp", Enabled: true,
	}

	h.recon.Tick(context.Background())

	specs := h.ctrl.puts[0]["node-a-pod"]
	if len(specs) != 1 || specs[0].DefinitionID != "def-1" {
		t.Errorf("specs = %+v, want only def-1", specs)
	}
}

// TestReconcilerStoreFailureIsCountedAndRetried: a read failure is the same
// class of outcome as a PUT failure -- counted, logged, state left dirty.
func TestReconcilerStoreFailureIsCountedAndRetried(t *testing.T) {
	h := newReconcileHarness(t)
	h.addContinuous("def-1", "tcp", "all")
	h.st.listErr = errors.New("boom")

	h.recon.Tick(context.Background())
	if len(h.ctrl.puts) != 0 {
		t.Errorf("PUTs = %d, want 0 when the store could not be read", len(h.ctrl.puts))
	}
	if got := h.reconcileCount("error"); got != 1 {
		t.Errorf("ExternalReconciles{error} = %v, want 1", got)
	}

	h.st.listErr = nil
	h.recon.Tick(context.Background())
	if len(h.ctrl.puts) != 1 {
		t.Errorf("PUTs = %d, want 1 once the store recovered", len(h.ctrl.puts))
	}
}

// TestReconcilerRunStopsOnContextCancel: the loop is a background component
// and must return promptly when cmd/console cancels bgCtx.
func TestReconcilerRunStopsOnContextCancel(t *testing.T) {
	h := newReconcileHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.recon.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// captureReconcileLogs swaps in a capturing default logger for the duration of one test; the buffer
// is mutex-guarded because other tests in this package run goroutines that log (Runner's fan-out).
func captureReconcileLogs(t *testing.T) *reconcileLogBuffer {
	t.Helper()
	buf := &reconcileLogBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return buf
}

type reconcileLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *reconcileLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *reconcileLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func keysOf(m map[string][]controllerclient.ExternalCheckSpec) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

/* ── the memo is about a REMOTE state, so it expires ─────────────────────── */

/*
 * The controller keeps its assignments in memory. A restart (rolling update, OOM kill, node drain —
 * the chart runs one replica by default) or a leadership move leaves the new process with none:
 * every agent re-subscribes, is handed an EMPTY assignment, and stops probing. With an
 * equality-only skip this reconciler said "unchanged" forever after, so continuous external checks
 * stayed dead until something else happened to alter the desired state. A periodic full resync is
 * what repairs it, and the PUT is a whole-state replace, so re-sending costs nothing but a request.
 */
func TestReconcilerResendsTheDesiredStatePeriodically(t *testing.T) {
	h := newReconcileHarness(t)
	h.addContinuous("def-1", "tcp", "one-per-zone")

	h.recon.Tick(context.Background())
	h.recon.Tick(context.Background())
	if len(h.ctrl.puts) != 1 {
		t.Fatalf("PUTs = %d, want 1 while the memo is fresh", len(h.ctrl.puts))
	}

	// The controller has restarted and lost everything; nothing about the DESIRED state changed, so
	// only the age of the last push can trigger the repair.
	h.recon.SetLastPushedAt(time.Now().Add(-checks.ExternalResyncInterval - time.Second))
	h.recon.Tick(context.Background())

	if len(h.ctrl.puts) != 2 {
		t.Fatalf("PUTs = %d, want a second one once the resync interval has passed", len(h.ctrl.puts))
	}
	// And it is the same desired state, re-asserted rather than recomputed into something else.
	if len(h.ctrl.puts[1]) != len(h.ctrl.puts[0]) {
		t.Errorf("resync PUT covered %d agents, want the same %d as the first", len(h.ctrl.puts[1]), len(h.ctrl.puts[0]))
	}
}

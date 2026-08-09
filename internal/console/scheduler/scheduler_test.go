package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/checks"
	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// fixedNow is the clock every test below pins, so next-fire arithmetic is
// asserted as an exact equality rather than a tolerance window.
var fixedNow = time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// fakeLocker is the advisory-lock seam. leader == false reproduces the
// steady state on every replica but one: WithAdvisoryLock reports (false,
// nil) and never runs the tick's work.
type fakeLocker struct {
	leader bool
	err    error
	keys   []int64
}

func (f *fakeLocker) WithAdvisoryLock(ctx context.Context, key int64, fn func(context.Context) error) (bool, error) {
	f.keys = append(f.keys, key)
	if f.err != nil {
		return false, f.err
	}
	if !f.leader {
		return false, nil
	}
	return true, fn(ctx)
}

// fakeStore is an in-memory check_schedules / check_definitions / targets.
type fakeStore struct {
	schedules   map[string]store.Schedule
	definitions map[string]store.Definition
	targets     map[string]store.Target
	listErr     error
	// marks records every MarkScheduleFired call in order.
	marks []markCall
}

type markCall struct {
	id      string
	firedAt time.Time
	next    *time.Time
	// lastErr is the text this fire recorded on the row, "" when it went
	// through (QA round 5, finding #5).
	lastErr string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		schedules:   map[string]store.Schedule{},
		definitions: map[string]store.Definition{},
		targets:     map[string]store.Target{},
	}
}

// ListDueSchedules mirrors the real query's predicate (enabled AND
// next_fire_at <= due, soonest first) closely enough that a scheduler bug
// cannot hide behind a lenient fake.
func (f *fakeStore) ListDueSchedules(_ context.Context, due time.Time, limit int) ([]store.Schedule, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []store.Schedule
	for _, s := range f.schedules {
		if !s.Enabled || s.NextFireAt == nil || s.NextFireAt.After(due) {
			continue
		}
		out = append(out, s)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (f *fakeStore) MarkScheduleFired(
	_ context.Context, id string, firedAt time.Time, nextFireAt *time.Time, lastError string,
) error {
	s, ok := f.schedules[id]
	if !ok {
		return store.ErrNotFound
	}
	f.marks = append(f.marks, markCall{id: id, firedAt: firedAt, next: nextFireAt, lastErr: lastError})
	s.LastFiredAt = &firedAt
	s.NextFireAt = nextFireAt
	// The real UPDATE derives the stamp from the text; the fake does too, so a
	// test cannot observe a pair the database would never produce.
	s.LastError = lastError
	if lastError == "" {
		s.LastErrorAt = nil
	} else {
		s.LastErrorAt = &firedAt
	}
	f.schedules[id] = s
	return nil
}

func (f *fakeStore) GetDefinition(_ context.Context, id string) (store.Definition, error) {
	d, ok := f.definitions[id]
	if !ok {
		return store.Definition{}, store.ErrNotFound
	}
	return d, nil
}

func (f *fakeStore) GetTarget(_ context.Context, id string) (store.Target, error) {
	tg, ok := f.targets[id]
	if !ok {
		return store.Target{}, store.ErrNotFound
	}
	return tg, nil
}

// fakeRunner records what was started and lets a test pin the status a
// previously started run reports back -- the seam overrun protection reads.
type fakeRunner struct {
	started   []startCall
	startErr  error
	runs      map[string]checks.Run
	nextID    int
	reaped    int64
	reapErr   error
	reapCalls int
}

type startCall struct {
	spec      checks.Spec
	initiator authz.Subject
}

func newFakeRunner() *fakeRunner { return &fakeRunner{runs: map[string]checks.Run{}} }

func (f *fakeRunner) Start(_ context.Context, spec checks.Spec, initiator authz.Subject) (string, error) { //nolint:gocritic // Subject is a value type by design
	if f.startErr != nil {
		return "", f.startErr
	}
	f.started = append(f.started, startCall{spec: spec, initiator: initiator})
	f.nextID++
	id := "00000000-0000-4000-8000-00000000000" + string(rune('0'+f.nextID))
	f.runs[id] = checks.Run{ID: id, Status: "running"}
	return id, nil
}

func (f *fakeRunner) Get(_ context.Context, runID string) (checks.Run, error) {
	run, ok := f.runs[runID]
	if !ok {
		return checks.Run{}, store.ErrNotFound
	}
	return run, nil
}

func (f *fakeRunner) ReapStuckRuns(_ context.Context, _ int32) (int64, error) {
	f.reapCalls++
	return f.reaped, f.reapErr
}

// fakeTopology is a fixed snapshot: no controller, no HTTP server.
type fakeTopology struct {
	topo *controllerclient.Topology
	err  error
	hits int
}

func (f *fakeTopology) Topology(context.Context) (*controllerclient.Topology, error) {
	f.hits++
	return f.topo, f.err
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const (
	defID      = "11111111-1111-4111-8111-111111111111"
	schedID    = "22222222-2222-4222-8222-222222222222"
	targetID   = "33333333-3333-4333-8333-333333333333"
	oneMinute  = time.Minute
	fiveMinute = 5 * time.Minute
)

// harness bundles a Scheduler with every double it was built from, so a test
// asserts against the same objects it seeded.
type harness struct {
	s      *Scheduler
	lock   *fakeLocker
	store  *fakeStore
	runner *fakeRunner
	topo   *fakeTopology
	m      *metrics.Metrics
}

func newHarness(t *testing.T, leader bool) *harness {
	t.Helper()
	h := &harness{
		lock:   &fakeLocker{leader: leader},
		store:  newFakeStore(),
		runner: newFakeRunner(),
		topo:   &fakeTopology{topo: &controllerclient.Topology{}},
		m:      metrics.New("kconmon_ng_sched_test", prometheus.NewRegistry()),
	}
	h.s = New(Deps{
		Lock: h.lock, Store: h.store, Runner: h.runner, Topology: h.topo,
		Metrics: h.m, Interval: time.Second,
	})
	h.s.now = func() time.Time { return fixedNow }
	return h
}

// seedDefinition installs an enabled, adhoc-destination definition -- the
// simplest shape that produces a valid spec with no topology round trip.
func (h *harness) seedDefinition() {
	h.store.definitions[defID] = store.Definition{
		ID: defID, Name: "edge-tcp", SourceSelection: "all",
		DestinationKind: "adhoc", DestinationAddress: "10.0.0.1:53",
		CheckType: "tcp", Plane: "pod", Enabled: true,
	}
}

// seedSchedule installs a due schedule of kind with intervalNs, already past
// its next_fire_at.
func (h *harness) seedSchedule(kind string, intervalNs int64) {
	past := fixedNow.Add(-time.Second)
	h.store.schedules[schedID] = store.Schedule{
		ID: schedID, DefinitionID: defID, Kind: kind,
		IntervalNs: intervalNs, Enabled: true, NextFireAt: &past,
	}
}

func (h *harness) counter(vec *prometheus.CounterVec, labels ...string) float64 {
	return testutil.ToFloat64(vec.WithLabelValues(labels...))
}

// ---------------------------------------------------------------------------
// Locking
// ---------------------------------------------------------------------------

// TestTickWithoutTheLockIsASilentSkip is the contract every replica but one
// lives in: no work, no error, no log-worthy outcome -- just the not-leader
// counter, which is what makes "the loop is alive but not leading" visible
// without a line per replica per tick.
func TestTickWithoutTheLockIsASilentSkip(t *testing.T) {
	h := newHarness(t, false)
	h.seedDefinition()
	h.seedSchedule(kindInterval, int64(oneMinute))

	h.s.Tick(context.Background())

	if got := h.counter(h.m.SchedulerTicks, tickNotLeader); got != 1 {
		t.Errorf("SchedulerTicks{not-leader} = %v, want 1", got)
	}
	if got := h.counter(h.m.SchedulerTicks, tickOK); got != 0 {
		t.Errorf("SchedulerTicks{ok} = %v, want 0", got)
	}
	if len(h.runner.started) != 0 {
		t.Errorf("started %d runs without the lock, want 0", len(h.runner.started))
	}
	if h.runner.reapCalls != 0 {
		t.Errorf("reaper ran %d times without the lock, want 0", h.runner.reapCalls)
	}
}

// TestTickUsesItsOwnLockKey pins the key AND its distinctness from the two
// other advisory locks this module takes. A collision would make a migration
// run or a retention sweep look exactly like "another replica is scheduling",
// and nothing would fire for as long as it ran.
func TestTickUsesItsOwnLockKey(t *testing.T) {
	h := newHarness(t, true)
	h.s.Tick(context.Background())

	if len(h.lock.keys) != 1 || h.lock.keys[0] != LockKey {
		t.Fatalf("lock keys = %v, want [%d]", h.lock.keys, LockKey)
	}
	const gooseDefaultLockID int64 = 4097083626 // goose lock.DefaultLockID
	const pruneLockKey int64 = 3698486424       // store's retention sweep
	if LockKey == gooseDefaultLockID || LockKey == pruneLockKey {
		t.Fatalf("LockKey %d collides with goose's or the pruner's key", LockKey)
	}
}

// TestTickReportsAWorkFailureAsError keeps the three tick outcomes honest:
// a failing tick must not be counted as a successful one.
func TestTickReportsAWorkFailureAsError(t *testing.T) {
	h := newHarness(t, true)
	h.store.listErr = errors.New("connection refused")

	h.s.Tick(context.Background())

	if got := h.counter(h.m.SchedulerTicks, tickError); got != 1 {
		t.Errorf("SchedulerTicks{error} = %v, want 1", got)
	}
	if got := h.counter(h.m.SchedulerTicks, tickOK); got != 0 {
		t.Errorf("SchedulerTicks{ok} = %v, want 0", got)
	}
	// The reaper still ran: one half failing must not cost the fleet the
	// other half.
	if h.runner.reapCalls != 1 {
		t.Errorf("reaper ran %d times, want 1 even when the schedule pass failed", h.runner.reapCalls)
	}
}

// ---------------------------------------------------------------------------
// Due selection and next-fire arithmetic
// ---------------------------------------------------------------------------

// TestIntervalScheduleFiresAndAdvancesFromNow pins the drift choice: the next
// fire is anchored on the observed fire (now + interval), never on the
// previous one, so an outage can never produce a burst of back-dated runs.
func TestIntervalScheduleFiresAndAdvancesFromNow(t *testing.T) {
	h := newHarness(t, true)
	h.seedDefinition()
	h.seedSchedule(kindInterval, int64(fiveMinute))

	h.s.Tick(context.Background())

	if len(h.runner.started) != 1 {
		t.Fatalf("started %d runs, want 1", len(h.runner.started))
	}
	if got := h.counter(h.m.SchedulerFired, kindInterval); got != 1 {
		t.Errorf("SchedulerFired{interval} = %v, want 1", got)
	}
	if len(h.store.marks) != 1 {
		t.Fatalf("MarkScheduleFired called %d times, want 1", len(h.store.marks))
	}
	mark := h.store.marks[0]
	if !mark.firedAt.Equal(fixedNow) {
		t.Errorf("firedAt = %v, want %v", mark.firedAt, fixedNow)
	}
	want := fixedNow.Add(fiveMinute)
	if mark.next == nil || !mark.next.Equal(want) {
		t.Errorf("nextFireAt = %v, want %v (now + interval, not previous + interval)", mark.next, want)
	}
}

// TestNotYetDueScheduleIsNotFired guards the due predicate itself: a schedule
// whose next_fire_at is in the future must never be handed to the runner.
func TestNotYetDueScheduleIsNotFired(t *testing.T) {
	h := newHarness(t, true)
	h.seedDefinition()
	future := fixedNow.Add(time.Hour)
	h.store.schedules[schedID] = store.Schedule{
		ID: schedID, DefinitionID: defID, Kind: kindInterval,
		IntervalNs: int64(oneMinute), Enabled: true, NextFireAt: &future,
	}

	h.s.Tick(context.Background())

	if len(h.runner.started) != 0 {
		t.Errorf("started %d runs for a schedule that is not due, want 0", len(h.runner.started))
	}
	if got := h.counter(h.m.SchedulerTicks, tickOK); got != 1 {
		t.Errorf("SchedulerTicks{ok} = %v, want 1 (an empty tick is a successful tick)", got)
	}
}

// TestOnceScheduleFiresExactlyOnceAndRetiresItself is the once-kind contract
// end to end: it fires, its next_fire_at becomes NULL -- the terminal state
// store's MarkScheduleFired query names -- and a second tick finds nothing.
func TestOnceScheduleFiresExactlyOnceAndRetiresItself(t *testing.T) {
	h := newHarness(t, true)
	h.seedDefinition()
	runAt := fixedNow.Add(-time.Minute)
	h.store.schedules[schedID] = store.Schedule{
		ID: schedID, DefinitionID: defID, Kind: kindOnce,
		RunAt: &runAt, Enabled: true, NextFireAt: &runAt,
	}

	h.s.Tick(context.Background())
	h.s.Tick(context.Background())

	if len(h.runner.started) != 1 {
		t.Fatalf("started %d runs over two ticks, want exactly 1", len(h.runner.started))
	}
	if got := h.counter(h.m.SchedulerFired, kindOnce); got != 1 {
		t.Errorf("SchedulerFired{once} = %v, want 1", got)
	}
	if len(h.store.marks) != 1 || h.store.marks[0].next != nil {
		t.Fatalf("marks = %+v, want exactly one with a nil nextFireAt", h.store.marks)
	}
	if got := h.store.schedules[schedID]; got.NextFireAt != nil {
		t.Errorf("nextFireAt = %v, want nil (retired from the due index)", got.NextFireAt)
	}
	// Retired, NOT disabled: the operator's own flag is left alone.
	if !h.store.schedules[schedID].Enabled {
		t.Error("enabled = false, want the operator's flag untouched (NULL next_fire_at is what retires it)")
	}
}

// TestContinuousScheduleIsNeverFired covers the kind this loop deliberately
// does not own. It must not fire, and it must not have its cadence stamped
// either -- there is no cadence to advance.
func TestContinuousScheduleIsNeverFired(t *testing.T) {
	h := newHarness(t, true)
	h.seedDefinition()
	h.seedSchedule(kindContinuous, 0)

	h.s.Tick(context.Background())

	if len(h.runner.started) != 0 {
		t.Errorf("started %d runs for a continuous schedule, want 0 (they are agent-side)", len(h.runner.started))
	}
	if len(h.store.marks) != 0 {
		t.Errorf("MarkScheduleFired called %+v for a continuous schedule, want never", h.store.marks)
	}
	// And it is NOT counted as a skip: 'continuous' is a whole kind this loop
	// does not own, not an anomaly, so counting it would make skipped_total
	// climb forever on a perfectly healthy fleet.
	for _, reason := range []string{skipOverrun, skipDisabled} {
		if got := h.counter(h.m.SchedulerSkipped, reason); got != 0 {
			t.Errorf("SchedulerSkipped{%s} = %v, want 0", reason, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Skips
// ---------------------------------------------------------------------------

// TestOverrunSkipsAndAdvancesWithoutQueueing is the backlog guard: a
// 1-minute schedule sitting over a 5-minute run must skip the occurrence AND
// move its cadence on, so the missed fires are dropped rather than owed.
func TestOverrunSkipsAndAdvancesWithoutQueueing(t *testing.T) {
	h := newHarness(t, true)
	h.seedDefinition()
	h.seedSchedule(kindInterval, int64(oneMinute))

	// Tick 1 fires and records the run (status "running" in the fake).
	h.s.Tick(context.Background())
	if len(h.runner.started) != 1 {
		t.Fatalf("first tick started %d runs, want 1", len(h.runner.started))
	}

	// Make it due again and tick twice more: the run is still in flight.
	for range 2 {
		past := fixedNow.Add(-time.Second)
		sched := h.store.schedules[schedID]
		sched.NextFireAt = &past
		h.store.schedules[schedID] = sched
		h.s.Tick(context.Background())
	}

	if len(h.runner.started) != 1 {
		t.Errorf("started %d runs, want 1: an in-flight run must suppress every further fire", len(h.runner.started))
	}
	if got := h.counter(h.m.SchedulerSkipped, skipOverrun); got != 2 {
		t.Errorf("SchedulerSkipped{overrun} = %v, want 2", got)
	}
	// Each skipped occurrence still advanced the cadence -- that is what
	// stops a backlog accumulating.
	if len(h.store.marks) != 3 {
		t.Fatalf("MarkScheduleFired called %d times, want 3 (one per occurrence, fired or skipped)", len(h.store.marks))
	}
	for i, mark := range h.store.marks[1:] {
		want := fixedNow.Add(oneMinute)
		if mark.next == nil || !mark.next.Equal(want) {
			t.Errorf("skipped occurrence %d nextFireAt = %v, want %v", i, mark.next, want)
		}
	}
}

// TestOverrunClearsOnceThePreviousRunIsTerminal proves the guard releases:
// it holds a schedule back only while its run is genuinely in flight.
func TestOverrunClearsOnceThePreviousRunIsTerminal(t *testing.T) {
	h := newHarness(t, true)
	h.seedDefinition()
	h.seedSchedule(kindInterval, int64(oneMinute))

	h.s.Tick(context.Background())
	for id, run := range h.runner.runs {
		run.Status = "succeeded"
		h.runner.runs[id] = run
	}

	past := fixedNow.Add(-time.Second)
	sched := h.store.schedules[schedID]
	sched.NextFireAt = &past
	h.store.schedules[schedID] = sched
	h.s.Tick(context.Background())

	if len(h.runner.started) != 2 {
		t.Errorf("started %d runs, want 2 once the previous one finished", len(h.runner.started))
	}
	if got := h.counter(h.m.SchedulerSkipped, skipOverrun); got != 0 {
		t.Errorf("SchedulerSkipped{overrun} = %v, want 0", got)
	}
}

// TestDisabledDefinitionSkipsButKeepsTheCadence: a paused check is not a
// broken one. It must not dispatch, it must be counted, and its cadence must
// keep ticking so it resumes the moment the definition is switched back on.
func TestDisabledDefinitionSkipsButKeepsTheCadence(t *testing.T) {
	h := newHarness(t, true)
	h.seedDefinition()
	def := h.store.definitions[defID]
	def.Enabled = false
	h.store.definitions[defID] = def
	h.seedSchedule(kindInterval, int64(oneMinute))

	h.s.Tick(context.Background())

	if len(h.runner.started) != 0 {
		t.Errorf("started %d runs for a disabled definition, want 0", len(h.runner.started))
	}
	if got := h.counter(h.m.SchedulerSkipped, skipDisabled); got != 1 {
		t.Errorf("SchedulerSkipped{disabled} = %v, want 1", got)
	}
	if len(h.store.marks) != 1 || h.store.marks[0].next == nil {
		t.Fatalf("marks = %+v, want one advancing the cadence", h.store.marks)
	}
}

// TestFailedStartStillAdvancesTheCadence is the hot-loop guard: a definition
// the runner refuses must not be re-selected every tick, seconds apart,
// forever.
func TestFailedStartStillAdvancesTheCadence(t *testing.T) {
	h := newHarness(t, true)
	h.seedDefinition()
	h.seedSchedule(kindInterval, int64(oneMinute))
	h.runner.startErr = errors.New("controller unavailable")

	h.s.Tick(context.Background())

	if got := h.counter(h.m.SchedulerFired, kindInterval); got != 0 {
		t.Errorf("SchedulerFired{interval} = %v, want 0: nothing started", got)
	}
	if len(h.store.marks) != 1 {
		t.Fatalf("MarkScheduleFired called %d times, want 1 even though the start failed", len(h.store.marks))
	}
	if got := h.counter(h.m.SchedulerTicks, tickError); got != 1 {
		t.Errorf("SchedulerTicks{error} = %v, want 1: the failure is still reported", got)
	}
}

// ---------------------------------------------------------------------------
// Spec projection
// ---------------------------------------------------------------------------

// TestScheduledRunIsAnOrdinaryRun pins the spec a fired schedule produces,
// including the initiator: a scheduled run is traceable back to the schedule
// row that caused it, which is the whole point of recording the id.
func TestScheduledRunIsAnOrdinaryRun(t *testing.T) {
	h := newHarness(t, true)
	h.seedDefinition()
	h.seedSchedule(kindInterval, int64(oneMinute))

	h.s.Tick(context.Background())

	call := h.runner.started[0]
	if call.spec.Type != "tcp" || call.spec.Plane != "pod" {
		t.Errorf("spec = %+v, want the definition's type and plane", call.spec)
	}
	if len(call.spec.Sources) != 0 {
		t.Errorf("sources = %v, want empty for selection \"all\" (Plan expands it against live topology)", call.spec.Sources)
	}
	if len(call.spec.TypedDestinations) != 1 {
		t.Fatalf("typed destinations = %+v, want exactly one", call.spec.TypedDestinations)
	}
	dest := call.spec.TypedDestinations[0]
	if dest.Kind != checks.DestKindAdhoc || dest.Address != "10.0.0.1:53" {
		t.Errorf("destination = %+v, want the adhoc address", dest)
	}
	if dest.Name != "edge-tcp" {
		t.Errorf("destination name = %q, want the definition name (never the raw address: it becomes a label)", dest.Name)
	}
	if string(call.initiator.Kind) != initiatorKind || call.initiator.ID != schedID {
		t.Errorf("initiator = %+v, want kind %q and the schedule id", call.initiator, initiatorKind)
	}
	if h.topo.hits != 0 {
		t.Errorf("topology read %d times, want 0: selection \"all\" needs no snapshot", h.topo.hits)
	}
}

// TestTargetDestinationResolvesThroughTheTargetsTable covers the one
// destination kind that needs a second read.
func TestTargetDestinationResolvesThroughTheTargetsTable(t *testing.T) {
	h := newHarness(t, true)
	h.store.definitions[defID] = store.Definition{
		ID: defID, Name: "probe-dns", SourceSelection: "all",
		DestinationKind: "target", DestinationTargetID: targetID,
		CheckType: "dns", Plane: "pod", Enabled: true,
	}
	h.store.targets[targetID] = store.Target{ID: targetID, Name: "corp-dns", Kind: "host", Address: "10.9.9.9:53"}
	h.seedSchedule(kindInterval, int64(oneMinute))

	h.s.Tick(context.Background())

	if len(h.runner.started) != 1 {
		t.Fatalf("started %d runs, want 1", len(h.runner.started))
	}
	dest := h.runner.started[0].spec.TypedDestinations[0]
	if dest.Kind != checks.DestKindTarget || dest.Name != "corp-dns" || dest.Address != "10.9.9.9:53" {
		t.Errorf("destination = %+v, want the resolved target", dest)
	}
}

// TestOnePerZoneNarrowsSourcesDeterministically pins the one selection that
// actually shrinks the source set, and pins the tiebreak (first node by
// sorted name per zone) that makes two ticks over an unchanged topology
// produce byte-identical specs.
func TestOnePerZoneNarrowsSourcesDeterministically(t *testing.T) {
	h := newHarness(t, true)
	h.seedDefinition()
	def := h.store.definitions[defID]
	def.SourceSelection = "one-per-zone"
	h.store.definitions[defID] = def
	h.seedSchedule(kindInterval, int64(oneMinute))
	h.topo.topo = &controllerclient.Topology{Agents: []controllerclient.Agent{
		{NodeName: "node-b", Zone: "eu-1"},
		{NodeName: "node-a", Zone: "eu-1"},
		{NodeName: "node-z", Zone: "eu-2"},
		{NodeName: "node-y", Zone: ""},
	}}

	h.s.Tick(context.Background())

	if len(h.runner.started) != 1 {
		t.Fatalf("started %d runs, want 1", len(h.runner.started))
	}
	got := h.runner.started[0].spec.Sources
	want := []string{"node-a", "node-y", "node-z"}
	if len(got) != len(want) {
		t.Fatalf("sources = %v, want %v (one per zone, zoneless agents forming one bucket)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sources = %v, want %v", got, want)
		}
	}
}

// TestOnePerZoneRefusesWhenTopologyIsUnreadable is the deliberate
// fail-CLOSED: unlike httpapi's projection guard, which fails open so a
// controller outage cannot become a config-write outage, failing open here
// would DISPATCH probe traffic from every node when the operator asked for
// one per zone.
func TestOnePerZoneRefusesWhenTopologyIsUnreadable(t *testing.T) {
	h := newHarness(t, true)
	h.seedDefinition()
	def := h.store.definitions[defID]
	def.SourceSelection = "one-per-zone"
	h.store.definitions[defID] = def
	h.seedSchedule(kindInterval, int64(oneMinute))
	h.topo.err = errors.New("controller unavailable")

	h.s.Tick(context.Background())

	if len(h.runner.started) != 0 {
		t.Errorf("started %d runs with an unreadable topology, want 0", len(h.runner.started))
	}
	if got := h.counter(h.m.SchedulerTicks, tickError); got != 1 {
		t.Errorf("SchedulerTicks{error} = %v, want 1", got)
	}
	// The cadence still advanced, so one controller blip does not turn into a
	// per-tick retry storm.
	if len(h.store.marks) != 1 {
		t.Errorf("MarkScheduleFired called %d times, want 1", len(h.store.marks))
	}
}

// TestTopologyIsReadAtMostOncePerTick keeps a due backlog from turning one
// controller outage into one round trip per schedule.
func TestTopologyIsReadAtMostOncePerTick(t *testing.T) {
	h := newHarness(t, true)
	h.seedDefinition()
	def := h.store.definitions[defID]
	def.SourceSelection = "one-per-zone"
	h.store.definitions[defID] = def
	h.topo.topo = &controllerclient.Topology{Agents: []controllerclient.Agent{{NodeName: "n1", Zone: "z"}}}

	past := fixedNow.Add(-time.Second)
	for _, id := range []string{schedID, "44444444-4444-4444-8444-444444444444"} {
		h.store.schedules[id] = store.Schedule{
			ID: id, DefinitionID: defID, Kind: kindInterval,
			IntervalNs: int64(oneMinute), Enabled: true, NextFireAt: &past,
		}
	}

	h.s.Tick(context.Background())

	if len(h.runner.started) != 2 {
		t.Fatalf("started %d runs, want 2", len(h.runner.started))
	}
	if h.topo.hits != 1 {
		t.Errorf("topology read %d times in one tick, want 1", h.topo.hits)
	}
}

// ---------------------------------------------------------------------------
// Reaper
// ---------------------------------------------------------------------------

// TestReaperRunsUnderTheLockAndCounts pins Task 12's sweep to this loop: it
// runs on the leader, every tick, and what it moved is counted.
func TestReaperRunsUnderTheLockAndCounts(t *testing.T) {
	h := newHarness(t, true)
	h.runner.reaped = 3

	h.s.Tick(context.Background())

	if h.runner.reapCalls != 1 {
		t.Fatalf("reaper ran %d times, want 1", h.runner.reapCalls)
	}
	if got := testutil.ToFloat64(h.m.RunsReaped.WithLabelValues()); got != 3 {
		t.Errorf("RunsReaped = %v, want 3", got)
	}
}

// TestReaperFailureIsReportedButDoesNotStopTheLoop.
func TestReaperFailureIsReportedButDoesNotStopTheLoop(t *testing.T) {
	h := newHarness(t, true)
	h.seedDefinition()
	h.seedSchedule(kindInterval, int64(oneMinute))
	h.runner.reapErr = errors.New("statement timeout")

	h.s.Tick(context.Background())

	if len(h.runner.started) != 1 {
		t.Errorf("started %d runs, want 1: a reaper failure must not cost the schedule pass", len(h.runner.started))
	}
	if got := h.counter(h.m.SchedulerTicks, tickError); got != 1 {
		t.Errorf("SchedulerTicks{error} = %v, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// nextFireAt, in isolation
// ---------------------------------------------------------------------------

func TestNextFireAtArithmetic(t *testing.T) {
	cases := []struct {
		name  string
		sched store.Schedule
		want  *time.Time
	}{
		{
			name:  "once retires",
			sched: store.Schedule{ID: schedID, Kind: kindOnce},
			want:  nil,
		},
		{
			name:  "interval re-anchors on now",
			sched: store.Schedule{ID: schedID, Kind: kindInterval, IntervalNs: int64(fiveMinute)},
			want:  ptr(fixedNow.Add(fiveMinute)),
		},
		{
			name:  "interval with no interval retires rather than dividing by it",
			sched: store.Schedule{ID: schedID, Kind: kindInterval},
			want:  nil,
		},
		{
			name:  "an unrecognized kind retires instead of re-firing forever",
			sched: store.Schedule{ID: schedID, Kind: "cron"},
			want:  nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nextFireAt(&tc.sched, fixedNow) //nolint:gosec // G601 pre-1.22 alias, loop var is per-iteration
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("nextFireAt = %v, want nil", got)
			case tc.want != nil && (got == nil || !got.Equal(*tc.want)):
				t.Errorf("nextFireAt = %v, want %v", got, tc.want)
			}
		})
	}
}

func ptr(t time.Time) *time.Time { return &t }

// ---------------------------------------------------------------------------
// The failing-schedule record (QA round 5, finding #5)
// ---------------------------------------------------------------------------

// TestFireRecordsWhyItProducedNoRun is the whole point of last_error. A
// schedule whose definition is gone advances its cadence exactly like a
// healthy one -- fireOne must, or the row stays due and becomes a hot loop --
// so before this the two were INDISTINGUISHABLE in the console: enabled, a
// fresh "last", a "next" a minute out.
func TestFireRecordsWhyItProducedNoRun(t *testing.T) {
	h := newHarness(t, true)
	// No seedDefinition: the schedule points at a definition that is not there.
	h.seedSchedule(kindInterval, int64(oneMinute))

	h.s.Tick(context.Background())

	if len(h.store.marks) != 1 {
		t.Fatalf("MarkScheduleFired called %d times, want 1", len(h.store.marks))
	}
	got := h.store.marks[0].lastErr
	if got == "" {
		t.Fatal("lastError is empty for a fire that produced no run")
	}
	// The store's own vocabulary survives -- an operator reading the row gets
	// the actionable half, not a paraphrase.
	if !strings.Contains(got, "get definition") {
		t.Errorf("lastError = %q, want it to name the failure that happened", got)
	}
	// …and the schedule's own id is NOT repeated: the row IS that schedule.
	if strings.Contains(got, schedID) {
		t.Errorf("lastError = %q, want the redundant schedule id stripped", got)
	}
	if h.store.schedules[schedID].LastErrorAt == nil {
		t.Error("LastErrorAt is nil beside a non-empty LastError")
	}
}

// A tick that goes through CLEARS the previous failure. The column describes
// the LAST attempt, not the last bad one -- otherwise one bad minute leaves a
// schedule red for the rest of its life.
func TestASuccessfulFireClearsTheRecordedError(t *testing.T) {
	h := newHarness(t, true)
	h.seedSchedule(kindInterval, int64(oneMinute))

	h.s.Tick(context.Background()) // fails: no definition
	if h.store.schedules[schedID].LastError == "" {
		t.Fatal("the first tick recorded no error, so there is nothing to clear")
	}

	// The definition appears and the schedule comes due again.
	h.seedDefinition()
	past := fixedNow.Add(-time.Second)
	s := h.store.schedules[schedID]
	s.NextFireAt = &past
	h.store.schedules[schedID] = s

	h.s.Tick(context.Background())

	if len(h.store.marks) != 2 {
		t.Fatalf("MarkScheduleFired called %d times, want 2", len(h.store.marks))
	}
	if h.store.marks[1].lastErr != "" {
		t.Errorf("second mark lastError = %q, want cleared", h.store.marks[1].lastErr)
	}
	if got := h.store.schedules[schedID]; got.LastError != "" || got.LastErrorAt != nil {
		t.Errorf("row after a good fire = %q/%v, want both cleared", got.LastError, got.LastErrorAt)
	}
}

// A deliberate SKIP is not a failure. An enabled schedule on a DISABLED
// definition is a paused check (startFor says so), and marking it red would
// have the console contradict its own explanation.
func TestASkipRecordsNoError(t *testing.T) {
	h := newHarness(t, true)
	h.seedDefinition()
	def := h.store.definitions[defID]
	def.Enabled = false
	h.store.definitions[defID] = def
	h.seedSchedule(kindInterval, int64(oneMinute))

	h.s.Tick(context.Background())

	if len(h.store.marks) != 1 {
		t.Fatalf("MarkScheduleFired called %d times, want 1", len(h.store.marks))
	}
	if h.store.marks[0].lastErr != "" {
		t.Errorf("lastError = %q for a skipped (not failed) fire, want empty", h.store.marks[0].lastErr)
	}
}

// A run the runner refuses to START is a failure of this fire, same as a
// missing definition: the schedule produced no run, and the row must say so.
func TestAFailedStartIsRecordedOnTheRow(t *testing.T) {
	h := newHarness(t, true)
	h.seedDefinition()
	h.seedSchedule(kindInterval, int64(oneMinute))
	h.runner.startErr = errors.New("runner is shutting down")

	h.s.Tick(context.Background())

	if len(h.store.marks) != 1 {
		t.Fatalf("MarkScheduleFired called %d times, want 1", len(h.store.marks))
	}
	if !strings.Contains(h.store.marks[0].lastErr, "runner is shutting down") {
		t.Errorf("lastError = %q, want the runner's own message", h.store.marks[0].lastErr)
	}
}

// scheduleErrorText strips ONE prefix and only when it is actually there --
// an error from another layer must not be silently trimmed into nonsense.
func TestScheduleErrorText(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			"strips the schedule's own prefix",
			"scheduler: schedule " + schedID + ": get definition " + defID + ": store: not found",
			"get definition " + defID + ": store: not found",
		},
		{
			"leaves an error that does not carry it",
			"store: mark schedule fired: connection refused",
			"store: mark schedule fired: connection refused",
		},
		{
			"leaves an error naming a DIFFERENT schedule",
			"scheduler: schedule " + defID + ": boom",
			"scheduler: schedule " + defID + ": boom",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scheduleErrorText(schedID, errors.New(tc.in)); got != tc.want {
				t.Errorf("scheduleErrorText = %q, want %q", got, tc.want)
			}
		})
	}
}

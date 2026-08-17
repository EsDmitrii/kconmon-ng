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

	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/ws"
)

// TestInterleaveBySourceSpreadsTheDispatchWindow is the pacing half of the mass-MTR fix: Plan emits
// pairs source-major, so a bounded window of 8 used to land entirely on one agent, whose on-demand
// semaphore then refused the overflow.
func TestInterleaveBySourceSpreadsTheDispatchWindow(t *testing.T) {
	pairs := make([]Pair, 0, 9)
	for _, src := range []string{"node-1", "node-2", "node-3"} {
		for _, dst := range []string{"a", "b", "c"} {
			pairs = append(pairs, Pair{Source: src, Destination: NodeDestination(dst)})
		}
	}

	order := interleaveBySource(pairs)
	if len(order) != len(pairs) {
		t.Fatalf("interleave dropped pairs: %d of %d", len(order), len(pairs))
	}

	seen := make(map[int]bool, len(order))
	for _, i := range order {
		if seen[i] {
			t.Fatalf("index %d emitted twice", i)
		}
		seen[i] = true
	}

	// The first window of one-per-source must touch every source before repeating any of them.
	first := map[string]int{}
	for _, i := range order[:3] {
		first[pairs[i].Source]++
	}
	if len(first) != 3 {
		t.Errorf("the first three dispatches covered %d sources, want 3: %v", len(first), first)
	}
}

// TestClampTimeoutForGivesMTRItsTraceBudget pins the deadline half of the fix: an operator-supplied
// timeout below the trace budget turned work still in flight into a dispatch timeout.
func TestClampTimeoutForGivesMTRItsTraceBudget(t *testing.T) {
	if got := clampTimeoutFor("mtr", 5*time.Second); got != mtrMinPerPairTimeout {
		t.Errorf("mtr per-pair timeout = %s, want the %s floor", got, mtrMinPerPairTimeout)
	}
	if got := clampTimeoutFor("mtr", 110*time.Second); got != 110*time.Second {
		t.Errorf("mtr per-pair timeout = %s, want the operator's own 110s", got)
	}
	if got := clampTimeoutFor("tcp", 5*time.Second); got != 5*time.Second {
		t.Errorf("tcp per-pair timeout = %s, want 5s unchanged", got)
	}
}

// allToAllPairs builds the n-node all<->all plan the stand runs.
func allToAllPairs(n int) []Pair {
	nodes := make([]string, 0, n)
	for i := range n {
		nodes = append(nodes, fmt.Sprintf("node-%d", i+1))
	}
	var pairs []Pair
	for _, src := range nodes {
		for _, dst := range nodes {
			if src == dst {
				continue
			}
			pairs = append(pairs, Pair{Source: src, Destination: NodeDestination(dst)})
		}
	}
	return pairs
}

// TestDispatchRoundNeverExceedsOneAgentsCapacity is the live-run regression measured at the seam
// that actually broke: with a source-major plan and a window of 8, four of every eight dispatches
// hit an agent already running maxConcurrentTasks and were refused outright.
func TestDispatchRoundNeverExceedsOneAgentsCapacity(t *testing.T) {
	pairs := allToAllPairs(10)

	var mu sync.Mutex
	inFlight := map[string]int{}
	peak := 0

	gate := newSourceGate()
	var wg sync.WaitGroup
	ctx := context.Background()
	sem := make(chan struct{}, maxConcurrency)

	for _, i := range interleaveBySource(pairs) {
		sem <- struct{}{}
		if !gate.acquire(ctx, pairs[i].Source) {
			<-sem
			break
		}
		wg.Add(1)
		go func(source string) {
			defer wg.Done()
			defer func() { gate.release(source); <-sem }()

			mu.Lock()
			inFlight[source]++
			if inFlight[source] > peak {
				peak = inFlight[source]
			}
			mu.Unlock()

			time.Sleep(time.Millisecond)

			mu.Lock()
			inFlight[source]--
			mu.Unlock()
		}(pairs[i].Source)
	}
	wg.Wait()

	if peak > maxPerSourceConcurrency {
		t.Errorf("peak in-flight dispatches against one agent = %d, want at most %d", peak, maxPerSourceConcurrency)
	}
}

// TestEffectiveSampleIntervalStretchesForSlowTypes is the owner's case: a 15m MTR run over 4 pairs
// used to be refused because the base 5s cadence cannot hold a 90s trace — but the base cadence is
// derived from the duration and an operator cannot dial it, so refusing was advice nobody could
// act on. The cadence is re-planned instead.
func TestEffectiveSampleIntervalStretchesForSlowTypes(t *testing.T) {
	pairs := allToAllPairs(2) // 2 pairs, one per source
	mtrSpec := &Spec{Type: "mtr", Duration: 15 * time.Minute}

	got := EffectiveSampleInterval(mtrSpec, pairs, mtrMinPerPairTimeout)
	if got != mtrMinPerPairTimeout {
		t.Errorf("effective interval = %s, want the %s trace budget", got, mtrMinPerPairTimeout)
	}
	if samples := PlannedSamplesPerPair(mtrSpec.Duration, got); samples != 10 {
		t.Errorf("planned samples = %d, want 10 (15m at 90s)", samples)
	}
}

// TestEffectiveSampleIntervalLeavesFastTypesAlone pins that nothing changed for the types whose
// probes finish well inside the base cadence.
func TestEffectiveSampleIntervalLeavesFastTypesAlone(t *testing.T) {
	spec := &Spec{Type: "tcp", Duration: 15 * time.Minute}
	base := SampleInterval(spec.Duration)

	if got := EffectiveSampleInterval(spec, allToAllPairs(4), clampTimeoutFor("tcp", 5*time.Second)); got != base {
		t.Errorf("effective interval = %s, want the base cadence %s untouched", got, base)
	}
	if got := PlannedSamplesPerPair(spec.Duration, base); got != 180 {
		t.Errorf("planned samples = %d, want 180 (15m at 5s)", got)
	}
}

// TestEffectiveSampleIntervalCountsTheWholeRound covers the big fan-out: the cadence a run can keep
// is bounded by one ROUND, not by one pair, so 90 mtr pairs stretch far past a single trace budget.
func TestEffectiveSampleIntervalCountsTheWholeRound(t *testing.T) {
	spec := &Spec{Type: "mtr", Duration: 15 * time.Minute}
	pairs := allToAllPairs(10)

	got := EffectiveSampleInterval(spec, pairs, mtrMinPerPairTimeout)
	want := roundFloor(pairs, mtrMinPerPairTimeout) // 12 batches x 90s
	if got != want {
		t.Errorf("effective interval = %s, want the round floor %s", got, want)
	}
	// One round outlasts the whole duration, which is one honest pass rather than a refusal.
	if samples := PlannedSamplesPerPair(spec.Duration, got); samples != 1 {
		t.Errorf("planned samples = %d, want 1", samples)
	}
}

// TestEffectiveSampleIntervalIgnoresInstantRuns keeps an instant run at zero cadence: it has one
// round by definition.
func TestEffectiveSampleIntervalIgnoresInstantRuns(t *testing.T) {
	if got := EffectiveSampleInterval(&Spec{Type: "mtr"}, allToAllPairs(4), mtrMinPerPairTimeout); got != 0 {
		t.Errorf("effective interval = %s, want 0 for an instant run", got)
	}
	if got := PlannedSamplesPerPair(0, 0); got != 1 {
		t.Errorf("planned samples = %d, want 1 for an instant run", got)
	}
}

// TestNoDurationRunIsImpossible is the replacement for the removed reject path: whatever the shape,
// the plan is at least one sample per pair, so there is nothing left for a 422 to refuse.
func TestNoDurationRunIsImpossible(t *testing.T) {
	for _, checkType := range []string{"tcp", "udp", "icmp", "dns", "http", "mtr"} {
		for _, duration := range []time.Duration{MinRunDuration, time.Minute, 15 * time.Minute, MaxRunDuration} {
			for _, nodes := range []int{2, 10, 20} {
				spec := &Spec{Type: checkType, Duration: duration}
				pairs := allToAllPairs(nodes)
				interval := EffectiveSampleInterval(spec, pairs, clampTimeoutFor(checkType, 0))
				if interval <= 0 {
					t.Fatalf("%s/%s/%d nodes: interval = %s, want a positive cadence", checkType, duration, nodes, interval)
				}
				if got := PlannedSamplesPerPair(duration, interval); got < 1 {
					t.Fatalf("%s/%s/%d nodes: planned samples = %d, want at least 1", checkType, duration, nodes, got)
				}
			}
		}
	}
}

// pacedCtrl answers every Diagnose after a fixed delay and counts the calls, so a test can drive a
// run whose rounds take a known amount of wall clock.
type pacedCtrl struct {
	delay time.Duration
	calls atomic.Int32
}

func (c *pacedCtrl) Topology(context.Context) (*controllerclient.Topology, error) {
	return &controllerclient.Topology{}, nil
}

func (c *pacedCtrl) Diagnose(
	ctx context.Context, _ controllerclient.DiagnoseRequest, _ time.Duration,
) (json.RawMessage, error) {
	c.calls.Add(1)
	select {
	case <-time.After(c.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return json.RawMessage(`{"success":true,"type":"mtr"}`), nil
}

// runPaced executes one duration run against a pacedCtrl and returns the store and the run id.
func runPaced(t *testing.T, ctrl *pacedCtrl, spec *Spec, pairs []Pair, pace, budget time.Duration) (*MemoryStore, string) {
	t.Helper()

	m := metrics.New("kconmon_ng_test_"+strings.ReplaceAll(t.Name(), "/", "_"), prometheus.NewRegistry())
	bus := cache.NewInProcessBus()
	hub := ws.NewHub(bus, m)
	st := NewMemoryStore()
	r := NewRunner(ctrl, hub, bus, st, m)

	specJSON, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	run, err := st.CreateRun(context.Background(), uuid.NewString(), spec.Type, spec.Plane, specJSON,
		"user", "u1", int32(len(pairs)), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	runCtx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	rounds := PlannedSamplesPerPair(spec.Duration, EffectiveSampleInterval(spec, pairs, mtrMinPerPairTimeout))
	r.execute(runCtx, cancel, run.ID, pairs, spec, time.Second, rounds, pace, false)
	return st, run.ID
}

// TestDurationRunKeepsGoingWhileFastRoundsFinishEarly is the owner's bug: a 5m/90-pair MTR planned
// ONE round off the worst-case interval and exited after ~30s of real work, reporting succeeded.
// The duration is wall clock, so fast rounds must repeat until it elapses.
func TestDurationRunKeepsGoingWhileFastRoundsFinishEarly(t *testing.T) {
	pairs := allToAllPairs(4) // 12 pairs, 3 per source
	spec := &Spec{Type: "mtr", Plane: "pod", Duration: 600 * time.Millisecond, Timeout: time.Second}

	// Worst-case planning says one round; the round actually takes a few ms per pair.
	planned := PlannedSamplesPerPair(spec.Duration, EffectiveSampleInterval(spec, pairs, mtrMinPerPairTimeout))
	if planned != 1 {
		t.Fatalf("setup: planned samples = %d, want the worst-case floor of 1", planned)
	}

	ctrl := &pacedCtrl{delay: 2 * time.Millisecond}
	st, id := runPaced(t, ctrl, spec, pairs, 20*time.Millisecond, 20*time.Second)

	results, _, err := st.GetRunResults(context.Background(), id)
	if err != nil {
		t.Fatalf("GetRunResults: %v", err)
	}
	rounds := len(results) / len(pairs)
	if rounds < 3 {
		t.Errorf("the run did %d rounds over its %s duration, want several; it exited on the planned count",
			rounds, spec.Duration)
	}
}

// TestDurationRunEndsAfterAnOverrunningRound is the other side: when one round is slower than the
// whole duration, the in-flight round finishes and the run ends there.
func TestDurationRunEndsAfterAnOverrunningRound(t *testing.T) {
	pairs := allToAllPairs(2) // 2 pairs
	spec := &Spec{Type: "mtr", Plane: "pod", Duration: 100 * time.Millisecond, Timeout: time.Second}

	ctrl := &pacedCtrl{delay: 250 * time.Millisecond}
	st, id := runPaced(t, ctrl, spec, pairs, 20*time.Millisecond, 20*time.Second)

	results, _, err := st.GetRunResults(context.Background(), id)
	if err != nil {
		t.Fatalf("GetRunResults: %v", err)
	}
	if len(results) != len(pairs) {
		t.Errorf("results = %d, want exactly one round of %d (the round outlasted the duration)",
			len(results), len(pairs))
	}

	run, err := st.GetRun(context.Background(), id)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != "succeeded" {
		t.Errorf("status = %q, want succeeded: one honest round is not a failure", run.Status)
	}
}

// TestDurationRunStopsAtTheSampleCap keeps MaxSamplesPerPair the one true upper bound now that the
// planned count no longer ends the run.
func TestDurationRunStopsAtTheSampleCap(t *testing.T) {
	pairs := []Pair{{Source: "n1", Destination: NodeDestination("n2")}}
	// A long duration with instant rounds and a zero-length cadence would otherwise spin forever.
	spec := &Spec{Type: "mtr", Plane: "pod", Duration: MaxRunDuration, Timeout: time.Second}

	ctrl := &pacedCtrl{}
	m := metrics.New("kconmon_ng_test_cap", prometheus.NewRegistry())
	bus := cache.NewInProcessBus()
	hub := ws.NewHub(bus, m)
	st := NewMemoryStore()
	r := NewRunner(ctrl, hub, bus, st, m)

	specJSON, _ := json.Marshal(spec) //nolint:errcheck // fixed spec always marshals
	run, err := st.CreateRun(context.Background(), uuid.NewString(), spec.Type, spec.Plane, specJSON, "user", "u1", 1, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	runCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// A zero pacing interval makes every boundary already past, so only the cap can stop this.
	r.execute(runCtx, cancel, run.ID, pairs, spec, time.Second, 1, 0, false)

	results, _, err := st.GetRunResults(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("GetRunResults: %v", err)
	}
	if len(results) != MaxSamplesPerPair {
		t.Errorf("results = %d, want exactly the %d-sample cap", len(results), MaxSamplesPerPair)
	}
}

package checks_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/checks"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

/*
The reaper is judged PER ROW now: each run gets its own declared duration plus one worst round

	over its own fan-out plus the flat slack. These two budgets are the extremes — one that allows a
	run nothing, and one that allows it an hour — which is how a test names the intent it wants
	without reconstructing the arithmetic.
*/
var (
	reapEverything = store.ReapBudget{PerSourceConcurrency: 1, PerPairTimeout: 0, Slack: 0}
)

// mustCreateRunning creates a run in the MemoryStore and advances it to
// "running", returning the created row (whose CreatedAt the reaper cutoffs
// below are expressed relative to).
func mustCreateRunning(t *testing.T, m *checks.MemoryStore, id string) store.Run {
	t.Helper()
	ctx := context.Background()
	run, err := m.CreateRun(ctx, id, "tcp", "pod", json.RawMessage(`{}`), "user", "u1", 1, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("CreateRun(%s, time.Now().Add(time.Hour)): %v", id, err)
	}
	if err := m.MarkRunStarted(ctx, id); err != nil {
		t.Fatalf("MarkRunStarted(%s): %v", id, err)
	}
	return run
}

func TestMemoryStoreReapStuckRunsFinishesOnlyOldRunningRuns(t *testing.T) {
	m := checks.NewMemoryStore()
	ctx := context.Background()

	stuck := mustCreateRunning(t, m, "stuck")
	time.Sleep(2 * time.Millisecond)
	healthy := mustCreateRunning(t, m, "healthy")

	/* A budget that allows a run just over the age of the younger one: the older is past its
	   allowance, the younger is not. */
	n, err := m.ReapStuckRuns(ctx, store.ReapBudget{
		PerSourceConcurrency: 1,
		Slack:                time.Since(healthy.CreatedAt) + time.Millisecond,
	}, 100)
	if err != nil {
		t.Fatalf("ReapStuckRuns: %v", err)
	}
	if n != 1 {
		t.Fatalf("reaped %d runs, want 1", n)
	}

	got, err := m.GetRun(ctx, stuck.ID)
	if err != nil {
		t.Fatalf("GetRun(stuck): %v", err)
	}
	if got.Status != "cancelled" {
		t.Errorf("stuck run status = %q, want cancelled", got.Status)
	}
	if got.FinishedAt == nil {
		t.Error("stuck run FinishedAt is nil, want it stamped by the reaper")
	}

	still, err := m.GetRun(ctx, healthy.ID)
	if err != nil {
		t.Fatalf("GetRun(healthy): %v", err)
	}
	if still.Status != "running" {
		t.Errorf("healthy run status = %q, want running (untouched)", still.Status)
	}
	if still.FinishedAt != nil {
		t.Error("healthy run FinishedAt was stamped, want nil")
	}
}

/*
The reaper touches runs that are MID-FLIGHT: 'running' and 'pending' both, since a replica that

	died between CreateRun and MarkRunStarted leaves the latter behind and nothing else ever finishes
	it. A run that already reached a terminal status is left alone however old it is.
*/
func TestMemoryStoreReapStuckRunsTakesPendingAndLeavesTerminalRuns(t *testing.T) {
	m := checks.NewMemoryStore()
	ctx := context.Background()

	if _, err := m.CreateRun(ctx, "pending", "tcp", "pod", json.RawMessage(`{}`), "user", "u1", 1, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("CreateRun(pending, time.Now().Add(time.Hour)): %v", err)
	}
	mustCreateRunning(t, m, "done")
	if err := m.FinishRun(ctx, "done", "succeeded", 1, 0); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	n, err := m.ReapStuckRuns(ctx, reapEverything, 100)
	if err != nil {
		t.Fatalf("ReapStuckRuns: %v", err)
	}
	if n != 1 {
		t.Fatalf("reaped %d runs, want 1 (the orphaned pending run)", n)
	}
	for _, tc := range []struct{ id, want string }{{"pending", "cancelled"}, {"done", "succeeded"}} {
		run, err := m.GetRun(ctx, tc.id)
		if err != nil {
			t.Fatalf("GetRun(%s): %v", tc.id, err)
		}
		if run.Status != tc.want {
			t.Errorf("run %s status = %q, want %q", tc.id, run.Status, tc.want)
		}
	}
}

func TestMemoryStoreReapStuckRunsHonoursLimit(t *testing.T) {
	m := checks.NewMemoryStore()
	for i := 0; i < 5; i++ {
		mustCreateRunning(t, m, string(rune('a'+i)))
	}
	n, err := m.ReapStuckRuns(context.Background(), reapEverything, 2)
	if err != nil {
		t.Fatalf("ReapStuckRuns: %v", err)
	}
	if n != 2 {
		t.Fatalf("reaped %d runs, want 2 (the limit)", n)
	}
}

// cutoffRecordingStore captures the cutoff Runner.ReapStuckRuns computes, so
// the checks-level wrapper's own arithmetic (the longest a run can legitimately
// stay "running", plus slack) is asserted without waiting hours for a real one.
type cutoffRecordingStore struct {
	*checks.MemoryStore
	gotBudget store.ReapBudget
	gotLimit  int32
	reaped    int64
}

func (s *cutoffRecordingStore) ReapStuckRuns(_ context.Context, budget store.ReapBudget, limit int32) (int64, error) { //nolint:gocritic // hugeParam: mirrors the seam
	s.gotBudget = budget
	s.gotLimit = limit
	return s.reaped, nil
}

// The scheduler calls Runner.ReapStuckRuns with no cutoff of its own: the cutoff is this package's
// business.
func TestRunnerReapStuckRunsUsesAConservativePastCutoff(t *testing.T) {
	st := &cutoffRecordingStore{MemoryStore: checks.NewMemoryStore(), reaped: 3}
	runner := checks.NewRunner(nil, nil, nil, st, testMetrics(t))

	before := time.Now().UTC()
	n, err := runner.ReapStuckRuns(context.Background(), 0)
	if err != nil {
		t.Fatalf("ReapStuckRuns: %v", err)
	}
	if n != 3 {
		t.Errorf("ReapStuckRuns returned %d, want the store's own count (3) passed through", n)
	}

	/* The budget must cover what a live run is actually given: one worst batch per pair at the
	   per-pair ceiling, over the per-source gate that paces it. A budget narrower than that would
	   reap runs that are still working. */
	if st.gotBudget.PerSourceConcurrency <= 0 || st.gotBudget.PerPairTimeout <= 0 || st.gotBudget.Slack <= 0 {
		t.Errorf("budget = %+v, want every term positive", st.gotBudget)
	}
	_ = before
	if st.gotLimit <= 0 {
		t.Errorf("limit = %d, want a positive default when the caller passes 0", st.gotLimit)
	}
}

// A caller-supplied positive limit is passed through untouched -- the default
// only fills in for 0.
func TestRunnerReapStuckRunsPassesCallerLimitThrough(t *testing.T) {
	st := &cutoffRecordingStore{MemoryStore: checks.NewMemoryStore()}
	runner := checks.NewRunner(nil, nil, nil, st, testMetrics(t))

	if _, err := runner.ReapStuckRuns(context.Background(), 7); err != nil {
		t.Fatalf("ReapStuckRuns: %v", err)
	}
	if st.gotLimit != 7 {
		t.Errorf("limit = %d, want 7", st.gotLimit)
	}
}

// A live run must never be reaped: the wrapper's cutoff sits far enough in the
// past that a run created right now is not selected, even against the real
// MemoryStore.
func TestRunnerReapStuckRunsLeavesAFreshRunningRunAlone(t *testing.T) {
	m := checks.NewMemoryStore()
	runner := checks.NewRunner(nil, nil, nil, m, testMetrics(t))
	ctx := context.Background()

	mustCreateRunning(t, m, "fresh")
	n, err := runner.ReapStuckRuns(ctx, 10)
	if err != nil {
		t.Fatalf("ReapStuckRuns: %v", err)
	}
	if n != 0 {
		t.Fatalf("reaped %d runs, want 0", n)
	}
	got, err := m.GetRun(ctx, "fresh")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Status != "running" {
		t.Errorf("status = %q, want running (a live run must survive the reaper)", got.Status)
	}
}

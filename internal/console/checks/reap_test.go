package checks_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/checks"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// mustCreateRunning creates a run in the MemoryStore and advances it to
// "running", returning the created row (whose CreatedAt the reaper cutoffs
// below are expressed relative to).
func mustCreateRunning(t *testing.T, m *checks.MemoryStore, id string) store.Run {
	t.Helper()
	ctx := context.Background()
	run, err := m.CreateRun(ctx, id, "tcp", "pod", json.RawMessage(`{}`), "user", "u1", 1)
	if err != nil {
		t.Fatalf("CreateRun(%s): %v", id, err)
	}
	if err := m.MarkRunStarted(ctx, id); err != nil {
		t.Fatalf("MarkRunStarted(%s): %v", id, err)
	}
	return run
}

// TestMemoryStoreReapStuckRunsFinishesOnlyOldRunningRuns is the
// database-disabled half of follow-up #6: a run left "running" past the cutoff
// is force-finished as "cancelled"; a healthy (younger) running run is left
// exactly as it was.
func TestMemoryStoreReapStuckRunsFinishesOnlyOldRunningRuns(t *testing.T) {
	m := checks.NewMemoryStore()
	ctx := context.Background()

	stuck := mustCreateRunning(t, m, "stuck")
	time.Sleep(2 * time.Millisecond)
	healthy := mustCreateRunning(t, m, "healthy")

	// Cutoff strictly between the two creations: created_at < cutoff selects
	// the older run only.
	n, err := m.ReapStuckRuns(ctx, healthy.CreatedAt, 100)
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

// The reaper touches "running" rows ONLY: a pending run that never started,
// and a run that already reached a terminal status, are both left alone no
// matter how old they are.
func TestMemoryStoreReapStuckRunsIgnoresPendingAndTerminalRuns(t *testing.T) {
	m := checks.NewMemoryStore()
	ctx := context.Background()

	if _, err := m.CreateRun(ctx, "pending", "tcp", "pod", json.RawMessage(`{}`), "user", "u1", 1); err != nil {
		t.Fatalf("CreateRun(pending): %v", err)
	}
	mustCreateRunning(t, m, "done")
	if err := m.FinishRun(ctx, "done", "succeeded", 1, 0); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	n, err := m.ReapStuckRuns(ctx, time.Now().UTC().Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("ReapStuckRuns: %v", err)
	}
	if n != 0 {
		t.Fatalf("reaped %d runs, want 0", n)
	}
	for _, tc := range []struct{ id, want string }{{"pending", "pending"}, {"done", "succeeded"}} {
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
	n, err := m.ReapStuckRuns(context.Background(), time.Now().UTC().Add(time.Hour), 2)
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
	gotBefore time.Time
	gotLimit  int32
	reaped    int64
}

func (s *cutoffRecordingStore) ReapStuckRuns(_ context.Context, before time.Time, limit int32) (int64, error) {
	s.gotBefore = before
	s.gotLimit = limit
	return s.reaped, nil
}

// The scheduler (Task 13) calls Runner.ReapStuckRuns with no cutoff of its
// own: the cutoff is this package's business, since only this package knows
// the deadline Start can hand a run (runDeadline over maxPairs at the maximum
// per-pair timeout). The cutoff must be safely IN THE PAST by at least that
// ceiling -- reaping anything younger would kill live runs.
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

	// A run started right now must not be reapable: the cutoff has to sit at
	// least the maximum possible run deadline in the past. That ceiling is
	// ceil(400/8) = 50 batches x 120s + 30s slack = 6030s.
	const maxRunDeadline = 6030 * time.Second
	if age := before.Sub(st.gotBefore); age < maxRunDeadline {
		t.Errorf("cutoff is only %s in the past, want at least %s (the longest a run can legitimately run)", age, maxRunDeadline)
	}
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

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
)

// testPruneMetrics returns Metrics on a fresh registry: metrics.New(nil)
// targets prometheus.DefaultRegisterer, which every test function in this
// binary would share and collide on ("duplicate metrics collector
// registration") the second time any test called it.
func testPruneMetrics() *metrics.Metrics {
	return metrics.New("kconmon_ng", prometheus.NewRegistry())
}

// runPruner runs p.Run(ctx) on a background goroutine and reports whether it
// returned within 1s. A panic inside Run (e.g. a nil-pool dereference the
// caller did not expect) is converted into a test failure instead of a
// crashed test binary, since the recover happens before the outer select
// observes done and the enclosing test function returns.
func runPruner(ctx context.Context, t *testing.T, p *Pruner) bool {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("Run panicked: %v", r)
			}
		}()
		p.Run(ctx)
	}()

	select {
	case <-done:
		return true
	case <-time.After(time.Second):
		return false
	}
}

// TestRunZeroRetentionIsNoOp asserts the "keep everything" contract:
// database.retentionDays=0 (internal/console/config/config.go) must make Run
// return immediately without ever touching db's pool. p.db is nil here, so
// any code path that dereferences it would panic -- caught by runPruner --
// rather than merely happening to run fast.
func TestRunZeroRetentionIsNoOp(t *testing.T) {
	p := NewPruner(nil, 0, testPruneMetrics())

	if !runPruner(context.Background(), t, p) {
		t.Fatal("Run(retention=0) did not return promptly")
	}
}

// TestRunNegativeRetentionIsNoOp asserts the same no-op contract holds for a
// negative retention, not just exactly zero.
func TestRunNegativeRetentionIsNoOp(t *testing.T) {
	p := NewPruner(nil, -time.Hour, testPruneMetrics())

	if !runPruner(context.Background(), t, p) {
		t.Fatal("Run(retention<0) did not return promptly")
	}
}

// TestRunHonoursContextCancellation asserts an already-cancelled ctx makes
// Run return immediately: it must never reach the jitter sleep, the sweep,
// or (again, p.db is nil) the pool.
func TestRunHonoursContextCancellation(t *testing.T) {
	p := NewPruner(nil, 24*time.Hour, testPruneMetrics())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if !runPruner(ctx, t, p) {
		t.Fatal("Run did not return promptly for an already-cancelled context")
	}
}

// TestPruneJitterIsBounded asserts pruneJitter's documented window, [0,
// pruneJitterMax). Sampled many times since the value is random.
func TestPruneJitterIsBounded(t *testing.T) {
	for i := 0; i < 1000; i++ {
		d := pruneJitter()
		if d < 0 || d >= pruneJitterMax {
			t.Fatalf("pruneJitter() = %v, want [0, %v)", d, pruneJitterMax)
		}
	}
}

// TestPruneSleepReturnsFalseOnCancellation asserts pruneSleep reports "ctx
// won" rather than "the sleep elapsed" once ctx is done, both for a
// currently-running sleep and for an already-cancelled ctx (the d<=0 path
// Run's zero-retention case would otherwise never exercise here).
func TestPruneSleepReturnsFalseOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if pruneSleep(ctx, time.Hour) {
		t.Error("pruneSleep(cancelled ctx, 1h) = true, want false")
	}
	if pruneSleep(ctx, 0) {
		t.Error("pruneSleep(cancelled ctx, 0) = true, want false")
	}
}

// TestPruneSleepReturnsTrueWhenDurationElapses is pruneSleep's positive case:
// an unbounded ctx and a short duration report the duration won.
func TestPruneSleepReturnsTrueWhenDurationElapses(t *testing.T) {
	if !pruneSleep(context.Background(), time.Millisecond) {
		t.Error("pruneSleep(background, 1ms) = false, want true")
	}
	if !pruneSleep(context.Background(), 0) {
		t.Error("pruneSleep(background, 0) = false, want true")
	}
}

// TestRunSweepsRunsEverySweepEvenWhenAnEarlierOneFails is the unit-level
// counterpart of PruneOnce's two-sweep contract, exercised through
// runSweeps's del-closure injection seam (the same shape deleteBatches
// itself takes) instead of a real database: a failing topology_events sweep
// must not prevent the unrelated audit_log sweep from running on the same
// call, both sweeps' partial/complete row counts must still be credited to
// their own RetentionDeleted metric and map entry, and the failure must
// still surface in the returned error.
func TestRunSweepsRunsEverySweepEvenWhenAnEarlierOneFails(t *testing.T) {
	m := testPruneMetrics()
	wantErr := errors.New("boom: statement timeout")
	secondRan := false

	// The topology_events del closure models deleteBatches' own documented
	// partial-total behavior: a first full batch (n == pruneBatchSize) commits
	// and the loop asks for a second batch, which then fails outright. Only
	// the committed first batch's 5000 rows should end up credited -- a
	// failing call's own return value is never itself added to the total
	// (deleteBatches only accumulates n after confirming del returned a nil
	// error), matching what a real failed DELETE would actually commit: none.
	calls := 0
	deleted, err := runSweeps(context.Background(), m, []sweep{
		{
			table: tableTopologyEvents,
			del: func(context.Context, int32) (int64, error) {
				calls++
				if calls == 1 {
					return pruneBatchSize, nil
				}
				return 0, wantErr
			},
		},
		{
			table: tableAuditLog,
			del: func(context.Context, int32) (int64, error) {
				secondRan = true
				return 7, nil
			},
		},
	})

	if !secondRan {
		t.Fatal("runSweeps: audit_log sweep did not run after topology_events failed")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("runSweeps: err = %v, want it to wrap %v", err, wantErr)
	}
	if got := deleted[tableTopologyEvents]; got != pruneBatchSize {
		t.Errorf("deleted[%s] = %d, want %d (first committed batch still credited)", tableTopologyEvents, got, pruneBatchSize)
	}
	if got := deleted[tableAuditLog]; got != 7 {
		t.Errorf("deleted[%s] = %d, want 7", tableAuditLog, got)
	}

	if got := testutil.ToFloat64(m.RetentionDeleted.WithLabelValues(tableTopologyEvents)); got != pruneBatchSize {
		t.Errorf("RetentionDeleted(%s) = %v, want %d", tableTopologyEvents, got, pruneBatchSize)
	}
	if got := testutil.ToFloat64(m.RetentionDeleted.WithLabelValues(tableAuditLog)); got != 7 {
		t.Errorf("RetentionDeleted(%s) = %v, want 7", tableAuditLog, got)
	}
}

// TestRunSweepsNilErrorWhenEverySweepSucceeds asserts the boring path: with
// no failures, runSweeps' errors.Join has nothing to join and returns nil,
// not a non-nil "joined zero errors" wrapper.
func TestRunSweepsNilErrorWhenEverySweepSucceeds(t *testing.T) {
	m := testPruneMetrics()

	deleted, err := runSweeps(context.Background(), m, []sweep{
		{table: tableTopologyEvents, del: func(context.Context, int32) (int64, error) { return 1, nil }},
		{table: tableAuditLog, del: func(context.Context, int32) (int64, error) { return 2, nil }},
	})
	if err != nil {
		t.Fatalf("runSweeps: err = %v, want nil", err)
	}
	if deleted[tableTopologyEvents] != 1 || deleted[tableAuditLog] != 2 {
		t.Errorf("runSweeps: deleted = %+v, want {topology_events:1 audit_log:2}", deleted)
	}
}

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

// testPruneMetrics returns Metrics on a fresh registry: metrics.New(nil) targets
// prometheus.DefaultRegisterer.
func testPruneMetrics() *metrics.Metrics {
	return metrics.New("kconmon_ng", prometheus.NewRegistry())
}

// runPruner runs p.Run(ctx) on a background goroutine and reports whether it returned within 1s; a
// panic inside Run (e.g. a nil-pool dereference the caller did not expect) is converted into a test
// failure instead of a crashed test binary.
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

// TestRunZeroRetentionIsNoOp asserts the "keep everything" contract.
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

// TestPruneSleepReturnsFalseOnCancellation asserts pruneSleep reports "ctx won" rather than "the
// sleep elapsed" once ctx is done.
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

// TestRunSweepsRunsEverySweepEvenWhenAnEarlierOneFails is the unit-level counterpart of PruneOnce's
// two-sweep contract.
func TestRunSweepsRunsEverySweepEvenWhenAnEarlierOneFails(t *testing.T) {
	m := testPruneMetrics()
	wantErr := errors.New("boom: statement timeout")
	secondRan := false

	// Only the committed first batch's 5000 rows should end up credited.
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

// retentionTables is the closed RetentionDeleted{table} label set, spelled out
// once so the tests below and any future reader see the whole of it in one
// place. Order matches PruneOnce's own sweep order.
var retentionTables = []string{
	tableTopologyEvents,
	tableAuditLog,
	tableCheckRuns,
	tableMTRSnapshots,
	tableMTREnrichment,
	tableAnnotations,
	tableK8sEvents,
	tableIncidents,
	tableMaintenance,
}

// TestRetentionTableLabelsAreTheClosedSet pins the label VALUES, not just their count.
func TestRetentionTableLabelsAreTheClosedSet(t *testing.T) {
	want := []string{
		"topology_events",
		"audit_log",
		"check_runs",
		"mtr_path_snapshots",
		"mtr_hop_enrichment",
		"annotations",
		"k8s_events",
		"incidents",
		"maintenance_windows",
	}
	if len(retentionTables) != len(want) {
		t.Fatalf("retentionTables has %d entries, want %d", len(retentionTables), len(want))
	}
	for i := range want {
		if retentionTables[i] != want[i] {
			t.Errorf("retentionTables[%d] = %q, want %q", i, retentionTables[i], want[i])
		}
	}

	seen := make(map[string]bool, len(retentionTables))
	for _, table := range retentionTables {
		if seen[table] {
			t.Errorf("retention table label %q appears twice: two sweeps would share one series", table)
		}
		seen[table] = true
	}
}

// TestWebhooksAreNotARetentionTable is the DELIBERATE ABSENCE, pinned so it cannot be "fixed" by a
// later reader who notices.
func TestWebhooksAreNotARetentionTable(t *testing.T) {
	for _, table := range retentionTables {
		if table == "webhooks" {
			t.Fatal("webhooks has a retention sweep; see prune.go's table-label comment for why it must not")
		}
	}
}

// TestAlertRulesAreNotARetentionTable is the same DELIBERATE ABSENCE for the one new table; a rule
// nobody has touched for a year, that Prometheus has been evaluating the whole time.
func TestAlertRulesAreNotARetentionTable(t *testing.T) {
	for _, table := range retentionTables {
		if table == "alert_rules" {
			t.Fatal("alert_rules has a retention sweep; see prune.go's table-label comment for why it must not")
		}
	}
}

// TestRunSweepsCreditsEveryTableIndependently runs one sweep per closed label value with a distinct
// row count and asserts each landed on its own series.
func TestRunSweepsCreditsEveryTableIndependently(t *testing.T) {
	m := testPruneMetrics()

	sweeps := make([]sweep, 0, len(retentionTables))
	for i, table := range retentionTables {
		n := int64(i + 1)
		sweeps = append(sweeps, sweep{
			table: table,
			del:   func(context.Context, int32) (int64, error) { return n, nil },
		})
	}

	deleted, err := runSweeps(context.Background(), m, sweeps)
	if err != nil {
		t.Fatalf("runSweeps: err = %v, want nil", err)
	}
	if len(deleted) != len(retentionTables) {
		t.Fatalf("runSweeps returned %d table entries, want %d", len(deleted), len(retentionTables))
	}
	for i, table := range retentionTables {
		want := float64(i + 1)
		if got := deleted[table]; got != int64(want) {
			t.Errorf("deleted[%s] = %d, want %v", table, got, want)
		}
		if got := testutil.ToFloat64(m.RetentionDeleted.WithLabelValues(table)); got != want {
			t.Errorf("RetentionDeleted(%s) = %v, want %v", table, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// PoolStatsPoller
// ---------------------------------------------------------------------------

// newFakePoller returns a poller reading from a caller-controlled poolStats; the fake is a plain
// func rather than a *pgxpool.Stat because pgxpool.Stat cannot be constructed outside its own
// package.
func newFakePoller(m *metrics.Metrics, stats func() poolStats, interval time.Duration) *PoolStatsPoller {
	return &PoolStatsPoller{stats: stats, m: m, interval: interval}
}

// TestPoolStatsPollerObserveWritesEveryState asserts one sample lands on all
// three StorePoolConns series -- the whole point of the ticket: the gauge was
// registered but never written, so it read as a permanently empty pool.
func TestPoolStatsPollerObserveWritesEveryState(t *testing.T) {
	m := testPruneMetrics()
	p := newFakePoller(m, func() poolStats {
		return poolStats{acquired: 3, idle: 5, total: 8}
	}, time.Hour)

	p.observe()

	for state, want := range map[string]float64{
		poolStateAcquired: 3,
		poolStateIdle:     5,
		poolStateTotal:    8,
	} {
		if got := testutil.ToFloat64(m.StorePoolConns.WithLabelValues(state)); got != want {
			t.Errorf("StorePoolConns(%s) = %v, want %v", state, got, want)
		}
	}
}

// TestPoolStatsPollerObserveOverwritesPreviousSample asserts the gauge tracks
// the pool rather than accumulating: a Set, not an Add. A second, smaller
// sample must lower the series.
func TestPoolStatsPollerObserveOverwritesPreviousSample(t *testing.T) {
	m := testPruneMetrics()
	stats := poolStats{acquired: 9, idle: 1, total: 10}
	p := newFakePoller(m, func() poolStats { return stats }, time.Hour)

	p.observe()
	stats = poolStats{acquired: 0, idle: 2, total: 2}
	p.observe()

	if got := testutil.ToFloat64(m.StorePoolConns.WithLabelValues(poolStateAcquired)); got != 0 {
		t.Errorf("StorePoolConns(acquired) = %v after a 9 -> 0 sample, want 0", got)
	}
	if got := testutil.ToFloat64(m.StorePoolConns.WithLabelValues(poolStateTotal)); got != 2 {
		t.Errorf("StorePoolConns(total) = %v after a 10 -> 2 sample, want 2", got)
	}
}

// TestPoolStatsPollerRunSamplesBeforeFirstTick asserts Run writes the gauge immediately, without
// waiting out one interval.
func TestPoolStatsPollerRunSamplesBeforeFirstTick(t *testing.T) {
	m := testPruneMetrics()
	sampled := make(chan struct{}, 1)
	p := newFakePoller(m, func() poolStats {
		select {
		case sampled <- struct{}{}:
		default:
		}
		return poolStats{acquired: 1, idle: 2, total: 3}
	}, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(ctx)
	}()

	select {
	case <-sampled:
	case <-time.After(2 * time.Second):
		cancel()
		<-done
		t.Fatal("Run did not sample the pool before its first tick")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancellation")
	}

	if got := testutil.ToFloat64(m.StorePoolConns.WithLabelValues(poolStateTotal)); got != 3 {
		t.Errorf("StorePoolConns(total) = %v, want 3", got)
	}
}

// TestPoolStatsPollerRunKeepsSampling asserts the ticker loop keeps writing
// after the initial sample, not just once.
func TestPoolStatsPollerRunKeepsSampling(t *testing.T) {
	m := testPruneMetrics()
	samples := make(chan struct{}, 8)
	p := newFakePoller(m, func() poolStats {
		select {
		case samples <- struct{}{}:
		default:
		}
		return poolStats{acquired: 1, idle: 1, total: 2}
	}, time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	deadline := time.After(2 * time.Second)
	for i := 0; i < 3; i++ {
		select {
		case <-samples:
		case <-deadline:
			t.Fatalf("only %d samples arrived, want at least 3", i)
		}
	}
}

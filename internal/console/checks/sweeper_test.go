package checks_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/EsDmitrii/kconmon-ng/internal/console/checks"
	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/promql"
)

// sweepInterval is the cadence every sweeper test runs at; rotation is derived from now/interval,
// so tests move the clock instead of sleeping.
const sweepInterval = time.Minute

func newTestSweeper(t *testing.T, ctrl *controllerclient.Client, intended checks.IntendedPairsSource) (*checks.Sweeper, *metrics.Metrics) {
	t.Helper()
	m := testMetrics(t)
	s := checks.NewSweeper(checks.SweeperDeps{
		Topology:   ctrl,
		Controller: ctrl,
		Metrics:    m,
		Interval:   sweepInterval,
		CheckType:  "tcp",
		Timeout:    2 * time.Second,
		Intended:   intended,
	})
	return s, m
}

// tickAt drives one sweep with the clock pinned to the given instant.
func tickAt(s *checks.Sweeper, at time.Time) {
	s.SetNow(func() time.Time { return at })
	s.Tick(context.Background())
}

// epoch is an instant that is an exact multiple of sweepInterval, so the rotation index arithmetic
// in the assertions below is readable.
var epoch = time.Unix(0, 0)

func TestSweeperRotatesThroughTheCensusAndRecordsOnlyZoneResults(t *testing.T) {
	fake, ctrl := startFakeDiagnosticsServer(t)
	zonedAgents(fake, map[string][]string{"zone-a": {"a1"}, "zone-b": {"b1"}})
	// The census in sorted order: a1->b1, then b1->a1. Failing the first pair pins WHICH pair each
	// tick probed without the fake having to log requests.
	fake.failPair("a1", "b1")
	s, m := newTestSweeper(t, ctrl, nil)

	tickAt(s, epoch)
	if got := testutil.ToFloat64(m.SweepResults.WithLabelValues("zone-a", "zone-b", "failed")); got != 1 {
		t.Errorf("after tick 1: SweepResults(zone-a,zone-b,failed) = %v, want 1", got)
	}

	tickAt(s, epoch.Add(sweepInterval))
	if got := testutil.ToFloat64(m.SweepResults.WithLabelValues("zone-b", "zone-a", "ok")); got != 1 {
		t.Errorf("after tick 2: SweepResults(zone-b,zone-a,ok) = %v, want 1", got)
	}

	// Wrap-around: the third interval lands back on the first pair.
	tickAt(s, epoch.Add(2*sweepInterval))
	if got := testutil.ToFloat64(m.SweepResults.WithLabelValues("zone-a", "zone-b", "failed")); got != 2 {
		t.Errorf("after tick 3: SweepResults(zone-a,zone-b,failed) = %v, want 2", got)
	}

	if calls := fake.calls.Load(); calls != 3 {
		t.Errorf("diagnose calls = %d, want exactly 3 (one pair per tick)", calls)
	}
}

// fakeIntended is an IntendedPairsSource test double.
type fakeIntended struct {
	pairs map[checks.PairKey]struct{}
	err   error
}

func (f *fakeIntended) IntendedPairs(context.Context) (map[checks.PairKey]struct{}, error) {
	return f.pairs, f.err
}

func TestSweeperSkipsPairsTheTopologyPlanAlreadyProbes(t *testing.T) {
	fake, ctrl := startFakeDiagnosticsServer(t)
	zonedAgents(fake, map[string][]string{"zone-a": {"a1"}, "zone-b": {"b1"}})
	fake.failPair("a1", "b1") // if the planned pair is probed anyway, a "failed" sample betrays it
	s, m := newTestSweeper(t, ctrl, &fakeIntended{pairs: map[checks.PairKey]struct{}{
		{Source: "a1", Destination: "b1"}: {},
	}})

	// Two ticks; the census holds ONE unplanned pair, so both land on b1->a1.
	tickAt(s, epoch)
	tickAt(s, epoch.Add(sweepInterval))

	if got := testutil.ToFloat64(m.SweepResults.WithLabelValues("zone-b", "zone-a", "ok")); got != 2 {
		t.Errorf("SweepResults(zone-b,zone-a,ok) = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.SweepResults.WithLabelValues("zone-a", "zone-b", "failed")); got != 0 {
		t.Errorf("SweepResults(zone-a,zone-b,failed) = %v, want 0 — the planned pair must not be swept", got)
	}
}

func TestSweeperSweepsTheFullCensusWhenThePlanCannotBeRead(t *testing.T) {
	fake, ctrl := startFakeDiagnosticsServer(t)
	zonedAgents(fake, map[string][]string{"zone-a": {"a1"}, "zone-b": {"b1"}})
	s, m := newTestSweeper(t, ctrl, &fakeIntended{err: context.DeadlineExceeded})

	tickAt(s, epoch)

	// The plan being unreadable degrades to the superset census, never to a skipped tick.
	if got := testutil.ToFloat64(m.SweepResults.WithLabelValues("zone-a", "zone-b", "ok")); got != 1 {
		t.Errorf("SweepResults(zone-a,zone-b,ok) = %v, want 1", got)
	}
}

func TestSweeperDoesNothingWithFewerThanTwoNodes(t *testing.T) {
	fake, ctrl := startFakeDiagnosticsServer(t)
	zonedAgents(fake, map[string][]string{"zone-a": {"a1"}})
	s, _ := newTestSweeper(t, ctrl, nil)

	tickAt(s, epoch)

	if calls := fake.calls.Load(); calls != 0 {
		t.Errorf("diagnose calls = %d, want 0", calls)
	}
}

func TestPromIntendedPairsParsesTheIntendedVector(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"source_node":"a1","destination_node":"b1"},"value":[1,"1"]},
			{"metric":{"source_node":"b1","destination_node":"a1"},"value":[1,"1"]},
			{"metric":{"source_node":"","destination_node":"x"},"value":[1,"1"]}
		]}}`))
	}))
	t.Cleanup(srv.Close)

	src := checks.NewPromIntendedPairs(promql.New(srv.URL, promql.Guards{
		QueryTimeout: 5 * time.Second, MaxRange: time.Hour, MaxResponseBytes: 1 << 20,
	}))
	pairs, err := src.IntendedPairs(context.Background())
	if err != nil {
		t.Fatalf("IntendedPairs: %v", err)
	}
	want := map[checks.PairKey]struct{}{
		{Source: "a1", Destination: "b1"}: {},
		{Source: "b1", Destination: "a1"}: {},
	}
	if len(pairs) != len(want) {
		t.Fatalf("pairs = %v, want %v", pairs, want)
	}
	for k := range want {
		if _, ok := pairs[k]; !ok {
			t.Errorf("missing pair %v", k)
		}
	}
}

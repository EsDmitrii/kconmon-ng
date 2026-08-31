package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/checker"
	"github.com/EsDmitrii/kconmon-ng/internal/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// M9-2: a round is timed, and one that exceeds its own interval counts as an
// overrun — the operator-visible form of the cadence collapse M9-1 bounds.
func TestProbeCycleDurationAndOverruns(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.NewPrometheusMetrics("test_m92", reg)

	s := NewScheduler(checker.Target{AgentID: "self", NodeName: "self-node"}, func(model.CheckResult) {})
	s.SetSelfMetrics(m)
	sc := &slowChecker{name: model.CheckTCP, delay: 40 * time.Millisecond}
	s.AddChecker(sc, SchedulerConfig{Interval: 10 * time.Millisecond})
	s.UpdatePeers(makePeers(1))

	s.runCheckerOnce(context.Background(), sc)

	if got := testutil.CollectAndCount(m.AgentProbeCycleDuration); got != 1 {
		t.Fatalf("expected 1 probe cycle duration series, got %d", got)
	}
	if got := testutil.ToFloat64(m.AgentProbeCycleOverruns.WithLabelValues("tcp")); got != 1 {
		t.Fatalf("a 40ms round on a 10ms interval must count one overrun, got %v", got)
	}

	// A round inside its interval is not an overrun.
	fast := &slowChecker{name: model.CheckICMP, delay: time.Millisecond}
	s.AddChecker(fast, SchedulerConfig{Interval: time.Second})
	s.runCheckerOnce(context.Background(), fast)
	if got := testutil.ToFloat64(m.AgentProbeCycleOverruns.WithLabelValues("icmp")); got != 0 {
		t.Fatalf("a fast round must not count as an overrun, got %v", got)
	}
}

// M9-2: a round truncated by shutdown records nothing — it would measure the
// cancellation, not the network.
func TestProbeCycleNotObservedOnCancel(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.NewPrometheusMetrics("test_m92c", reg)

	s := NewScheduler(checker.Target{AgentID: "self", NodeName: "self-node"}, func(model.CheckResult) {})
	s.SetSelfMetrics(m)
	sc := &slowChecker{name: model.CheckTCP, delay: 5 * time.Second}
	s.AddChecker(sc, SchedulerConfig{Interval: 10 * time.Millisecond})
	s.UpdatePeers(makePeers(1))

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	s.runCheckerOnce(ctx, sc)

	if got := testutil.CollectAndCount(m.AgentProbeCycleDuration); got != 0 {
		t.Fatalf("a cancelled round must not be observed, got %d series", got)
	}
	if got := testutil.ToFloat64(m.AgentProbeCycleOverruns.WithLabelValues("tcp")); got != 0 {
		t.Fatalf("a cancelled round must not count as an overrun, got %v", got)
	}
}

// M9-2: agent New() preinits the self-metrics at zero and arms the peer-list
// age gauge, so alert expressions see series from the first scrape.
func TestAgentSelfMetricsPreinit(t *testing.T) {
	t.Setenv("KCONMON_NG_POD_NAME", "self-pod")
	t.Setenv("KCONMON_NG_POD_IP", "10.0.0.1")
	cfg := testRunConfig(t, "127.0.0.1:1")
	cfg.Agent.NodeName = "self-node"
	cfg.Checkers.TCP.Enabled = true

	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	families, err := a.promReg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	found := map[string]bool{}
	for _, f := range families {
		found[f.GetName()] = true
	}
	for _, name := range []string{
		"kconmon_ng_agent_probe_cycle_duration_seconds",
		"kconmon_ng_agent_probe_cycle_overruns_total",
		"kconmon_ng_agent_controller_reconnects_total",
		"kconmon_ng_agent_peer_list_age_seconds",
		"kconmon_ng_agent_mtr_reactive_inflight",
		"kconmon_ng_agent_mtr_reactive_coalesced_total",
	} {
		if !found[name] {
			var have []string
			for n := range found {
				if strings.Contains(n, "_agent_") {
					have = append(have, n)
				}
			}
			t.Errorf("self-metric %s not exported after New(); agent families present: %v", name, have)
		}
	}

	// The age gauge reads the scheduler's peer-list stamp.
	a.scheduler.UpdatePeers([]checker.Target{{AgentID: "p1", NodeName: "n1", PodIP: "10.0.0.2"}})
	age := gaugeValue(t, a.promReg, "kconmon_ng_agent_peer_list_age_seconds")
	if age < 0 || age > 5 {
		t.Errorf("peer list age just after an update = %v, want a small non-negative number", age)
	}
}

// gaugeValue reads one unlabelled gauge from a registry by family name.
func gaugeValue(t *testing.T, g prometheus.Gatherer, name string) float64 {
	t.Helper()
	families, err := g.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		ms := f.GetMetric()
		if len(ms) != 1 {
			t.Fatalf("family %s has %d series, want 1", name, len(ms))
		}
		return ms[0].GetGauge().GetValue()
	}
	t.Fatalf("family %s not found", name)
	return 0
}

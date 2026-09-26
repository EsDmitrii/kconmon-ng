package agent

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/checker"
	"github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/EsDmitrii/kconmon-ng/internal/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// gatedChecker holds every probe until release is closed, standing in for a probe that is still
// waiting out its timeout when the peer list changes.
type gatedChecker struct {
	name    model.CheckType
	started chan struct{}
	release chan struct{}
	once    sync.Once
	result  func(target checker.Target) model.CheckResult
}

func (g *gatedChecker) Name() model.CheckType { return g.name }

func (g *gatedChecker) Check(_ context.Context, target checker.Target) model.CheckResult {
	g.once.Do(func() { close(g.started) })
	<-g.release
	return g.result(target)
}

func lostUDP(checker.Target) model.CheckResult {
	return model.CheckResult{
		Type: model.CheckUDP, Error: "UDP loss: 100% (5/5)",
		Details: &model.UDPDetails{PacketsSent: 5, LossRatio: 1},
	}
}

func newDepartureAgent(t *testing.T) (*Agent, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	m := metrics.NewPrometheusMetrics("kconmon_ng", reg)
	src := checker.Target{AgentID: "id-a", NodeName: "node-a", Zone: "zone-a"}
	a := &Agent{
		metrics:   m,
		scheduler: NewScheduler(src, NewResultHandler(m, src)),
		info:      model.AgentInfo{ID: "id-a", NodeName: "node-a", Zone: "zone-a"},
		checkers:  map[model.CheckType]checker.Checker{model.CheckUDP: nil, model.CheckTCP: nil},
	}
	return a, reg
}

func seriesFor(t *testing.T, reg *prometheus.Registry, family, destination string) int {
	t.Helper()
	n := 0
	for _, v := range labelValues(t, reg, family, "destination_node") {
		if v == destination {
			n++
		}
	}
	return n
}

// A probe still in flight when its peer left must not write the peer back: a loss gauge of 1 for a
// node no update mentions again keeps UDPLossHigh firing until the agent restarts.
func TestInFlightProbeOfADepartedPeerWritesNothing(t *testing.T) {
	a, reg := newDepartureAgent(t)
	dead := checker.Target{AgentID: "id-dead", NodeName: "node-dead", Zone: "zone-b"}
	a.applyPeers([]checker.Target{dead})

	g := &gatedChecker{name: model.CheckUDP, started: make(chan struct{}), release: make(chan struct{}), result: lostUDP}
	a.scheduler.AddChecker(g, SchedulerConfig{Interval: time.Minute})
	done := make(chan struct{})
	go func() {
		a.scheduler.runCheckerOnce(context.Background(), g)
		close(done)
	}()
	<-g.started

	a.applyPeers(nil)
	close(g.release)
	<-done

	if n := seriesFor(t, reg, "kconmon_ng_udp_packet_loss_ratio", "node-dead"); n != 0 {
		t.Errorf("udp_packet_loss_ratio has %d series for a peer that left while its probe was in flight", n)
	}
	if got := testutil.ToFloat64(a.metrics.UDPResults.WithLabelValues("node-a", "node-dead", "zone-a", "zone-b", "fail")); got != 0 {
		t.Errorf("udp fail counter = %v for a departed peer, want the 0 it was pre-created at", got)
	}
}

// A reactive trace runs detached for up to 90s and must not write the departed peer back either.
func TestLateTraceOfADepartedPeerWritesNothing(t *testing.T) {
	a, reg := newDepartureAgent(t)
	dead := checker.Target{AgentID: "id-dead", NodeName: "node-dead", Zone: "zone-b"}
	a.applyPeers([]checker.Target{dead})

	s := a.scheduler
	s.SetMTRChecker(checker.NewMTRChecker(5, time.Second, time.Minute))
	s.SetSelfMetrics(a.metrics)
	bt := &blockingTrace{release: make(chan struct{})}
	s.mu.Lock()
	s.traceFn = func(ctx context.Context, target checker.Target) model.CheckResult {
		r := bt.run(ctx, target)
		r.Details = &model.MTRDetails{Reached: true, Hops: []model.MTRHop{{Number: 1, IP: "10.0.0.1", RTT: time.Millisecond}}}
		return r
	}
	s.mu.Unlock()

	failed := model.CheckResult{Type: model.CheckTCP, Error: "tcp failed"}
	s.triggerMTR(context.Background(), dead, &failed)
	waitCond(t, "the trace to start", func() bool { return bt.started.Load() == 1 })

	a.applyPeers(nil)
	close(bt.release)
	waitCond(t, "the trace to finish", func() bool {
		return testutil.ToFloat64(a.metrics.AgentMTRReactiveInflight.WithLabelValues()) == 0
	})

	for _, family := range []string{"kconmon_ng_mtr_hop_rtt_seconds", "kconmon_ng_mtr_triggered_total", "kconmon_ng_mtr_hops"} {
		if n := seriesFor(t, reg, family, "node-dead"); n != 0 {
			t.Errorf("%s has %d series for a peer that left while its trace ran", family, n)
		}
	}
}

func deliverUDP(a *Agent, dst checker.Target, loss float64) {
	a.scheduler.handler(model.CheckResult{
		Type: model.CheckUDP, Success: loss == 0,
		Source: "node-a", SourceZone: a.scheduler.sourceZone(), Destination: dst.NodeName, DestZone: dst.Zone,
		Details: &model.UDPDetails{PacketsSent: 5, PacketsRecv: 5, LossRatio: loss, MeanRTT: time.Millisecond},
	})
}

// A peer out of the plan past the grace period loses its counters and histograms; before that they
// stay, so a controller failover does not reset every pair.
func TestDepartedPeerCountersRetireAfterTheGracePeriod(t *testing.T) {
	a, reg := newDepartureAgent(t)
	clock := time.Unix(1_800_000_000, 0)
	a.now = func() time.Time { return clock }

	live := checker.Target{AgentID: "id-b", NodeName: "node-b", Zone: "zone-b"}
	gone := checker.Target{AgentID: "id-gone", NodeName: "node-gone", Zone: "zone-b"}
	a.applyPeers([]checker.Target{live, gone})
	deliverUDP(a, live, 0)
	deliverUDP(a, gone, 0)

	a.applyPeers([]checker.Target{live})
	if n := seriesFor(t, reg, "kconmon_ng_udp_rtt_seconds", "node-gone"); n != 1 {
		t.Fatalf("udp_rtt has %d series for node-gone right after it left, want 1: the grace period keeps it", n)
	}

	clock = clock.Add(departedPeerGrace - time.Second)
	a.sweepDepartedPeers()
	if n := seriesFor(t, reg, "kconmon_ng_udp_results_total", "node-gone"); n == 0 {
		t.Fatal("node-gone's counters went before the grace period was over")
	}

	clock = clock.Add(2 * time.Second)
	a.sweepDepartedPeers()
	for _, family := range []string{"kconmon_ng_udp_results_total", "kconmon_ng_udp_rtt_seconds", "kconmon_ng_tcp_results_total"} {
		if n := seriesFor(t, reg, family, "node-gone"); n != 0 {
			t.Errorf("%s still has %d series for a peer gone past the grace period", family, n)
		}
		if n := seriesFor(t, reg, family, "node-b"); n == 0 {
			t.Errorf("%s lost the live peer's series", family)
		}
	}
}

func TestPeerBackInsideTheGracePeriodKeepsItsCounters(t *testing.T) {
	a, _ := newDepartureAgent(t)
	clock := time.Unix(1_800_000_000, 0)
	a.now = func() time.Time { return clock }

	peer := checker.Target{AgentID: "id-b", NodeName: "node-b", Zone: "zone-b"}
	a.applyPeers([]checker.Target{peer})
	deliverUDP(a, peer, 0)

	a.applyPeers(nil)
	clock = clock.Add(30 * time.Second)
	a.applyPeers([]checker.Target{peer})
	clock = clock.Add(2 * departedPeerGrace)
	a.sweepDepartedPeers()

	if got := testutil.ToFloat64(a.metrics.UDPResults.WithLabelValues("node-a", "node-b", "zone-a", "zone-b", "success")); got != 1 {
		t.Errorf("udp success counter = %v after the peer came back inside the grace period, want the 1 it had", got)
	}
}

// A zone change, the peer's or this agent's, retires the gauges under the old zone.
func TestZoneChangeRetiresTheOldZoneGauges(t *testing.T) {
	a, reg := newDepartureAgent(t)
	before := checker.Target{AgentID: "id-b", NodeName: "node-b", Zone: "zone-1"}
	a.applyPeers([]checker.Target{before})
	deliverUDP(a, before, 0.8)

	after := before
	after.Zone = "zone-2"
	a.applyPeers([]checker.Target{after})
	if got := labelValues(t, reg, "kconmon_ng_udp_packet_loss_ratio", "destination_zone"); len(got) != 0 {
		t.Errorf("udp loss still carries destination zones %v after the peer moved to zone-2", got)
	}

	deliverUDP(a, after, 0.8)
	a.adoptZone("zone-a2")
	if got := labelValues(t, reg, "kconmon_ng_udp_packet_loss_ratio", "source_zone"); len(got) != 0 {
		t.Errorf("udp loss still carries source zones %v after this agent moved to zone-a2", got)
	}
}

// A probe that started before the peer's zone changed must not write the old-zone series back.
func TestInFlightProbeUnderAnOldZoneWritesNothing(t *testing.T) {
	a, reg := newDepartureAgent(t)
	peer := checker.Target{AgentID: "id-b", NodeName: "node-b", Zone: "zone-1"}
	a.applyPeers([]checker.Target{peer})

	g := &gatedChecker{name: model.CheckUDP, started: make(chan struct{}), release: make(chan struct{}), result: lostUDP}
	a.scheduler.AddChecker(g, SchedulerConfig{Interval: time.Minute})
	done := make(chan struct{})
	go func() {
		a.scheduler.runCheckerOnce(context.Background(), g)
		close(done)
	}()
	<-g.started

	moved := peer
	moved.Zone = "zone-2"
	a.applyPeers([]checker.Target{moved})
	close(g.release)
	<-done

	if got := labelValues(t, reg, "kconmon_ng_udp_packet_loss_ratio", "destination_zone"); len(got) != 0 {
		t.Errorf("an in-flight probe wrote udp loss under %v after the peer moved zones", got)
	}
}

// The peer watch and the heartbeat loop both re-register and apply peer lists; run under -race.
func TestConcurrentRegistrationUpdatesAreSerialized(t *testing.T) {
	a, _ := newDepartureAgent(t)
	peers := []checker.Target{{AgentID: "id-b", NodeName: "node-b", Zone: "zone-b"}}
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := range 50 {
				switch (i + j) % 4 {
				case 0:
					a.adoptZone([]string{"zone-a", "zone-x"}[j%2])
				case 1:
					a.applyPeers(peers)
				case 2:
					_ = a.registrationInfo()
				default:
					a.applyPeers(nil)
				}
			}
		}(i)
	}
	wg.Wait()
}

// An on-demand result carries the zone the agent has now, not the one it started with.
func TestOnDemandResultCarriesTheAdoptedZone(t *testing.T) {
	a, _ := newDepartureAgent(t)
	fc := &fakeChecker{name: model.CheckTCP, result: model.CheckResult{Success: true}}
	a.cfg = &config.Config{}
	a.checkers = map[model.CheckType]checker.Checker{model.CheckTCP: fc}
	ex := a.newTaskExecutor(newFakeReporter())

	a.adoptZone("zone-new")
	res := ex.executeOne(context.Background(), &pb.TaskRequest{
		TaskId: "t1", CheckType: "tcp", Plane: "pod",
		Target: &pb.AgentMeta{NodeName: "node-b", PodIp: "10.0.0.2", Zone: "zone-b"},
	})
	if !strings.Contains(string(res.GetDetailsJson()), `"sourceZone":"zone-new"`) {
		t.Errorf("on-demand result details %s, want sourceZone zone-new", res.GetDetailsJson())
	}
}

// A stream that outlived the backoff ceiling resubscribes after 1s, not after the last outage's 15s.
func TestResubscribeBackoffRestartsAfterAHealthyStream(t *testing.T) {
	if got := resubscribeWait(15*time.Second, 10*time.Minute); got != time.Second {
		t.Errorf("wait after a 10m stream = %v, want 1s", got)
	}
	if got := resubscribeWait(8*time.Second, 200*time.Millisecond); got != 8*time.Second {
		t.Errorf("wait after a stream that failed at once = %v, want the 8s the outage reached", got)
	}
}

// A pmtu probe that cannot size itself fails the same way for every peer and round; it warns once.
func TestPMTUProbeThatNeverSentWarnsOnce(t *testing.T) {
	logs := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	s := NewScheduler(checker.Target{AgentID: "self", NodeName: "self-node"}, nil)
	s.UpdatePeers(makePeers(5))
	unsized := &fakeChecker{name: model.CheckPMTU, result: model.CheckResult{
		Type: model.CheckPMTU, Error: "pmtu: cannot read the MTU of the interface owning 10.0.0.9: no such interface",
	}}
	s.AddChecker(unsized, SchedulerConfig{Interval: time.Minute})
	s.runCheckerOnce(context.Background(), unsized)
	s.runCheckerOnce(context.Background(), unsized)

	if n := strings.Count(logs.String(), `level=WARN msg="check failed" type=pmtu`); n != 1 {
		t.Errorf("%d check-failed warnings for one agent-local pmtu sizing error over 5 peers x 2 rounds, want 1", n)
	}

	// A black hole is a verdict about one pair and is still reported every time.
	logs.buf.Reset()
	hole := &fakeChecker{name: model.CheckPMTU, result: model.CheckResult{
		Type: model.CheckPMTU, Error: "path MTU black hole", Details: &model.PMTUDetails{Verdict: model.PMTUVerdictBlackhole},
	}}
	s.runCheckerOnce(context.Background(), hole)
	if n := strings.Count(logs.String(), `level=WARN msg="check failed" type=pmtu`); n != 5 {
		t.Errorf("%d warnings for 5 black-holed pairs, want 5", n)
	}
}

// A late result for an external check that is no longer assigned must not write its series back.
func TestExternalResultForARetiredCheckWritesNothing(t *testing.T) {
	cfg := testNewConfig()
	cfg.Checkers.External.Enabled = true
	cfg.Checkers.External.AllowedCIDRs = []string{"10.0.0.0/8"}
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	assign := &pb.ExternalCheckAssignment{Specs: []*pb.ExternalCheckSpec{{
		DefinitionId: "d1",
		Target:       &pb.ExternalTarget{Name: "vendor", Kind: "host", Address: "10.1.2.3"},
		CheckType:    "icmp",
		IntervalNs:   int64(30 * time.Second),
		TimeoutNs:    int64(time.Second),
	}}}
	late := model.CheckResult{Type: model.CheckExternal, Source: "node-a", Details: []model.ExternalDetails{{
		DefinitionID: "d1", Name: "vendor", CheckType: model.CheckICMP, Error: "timeout",
	}}}

	a.applyExternalAssignment(assign)
	a.scheduler.handler(late)
	if got := testutil.CollectAndCount(a.metrics.ExternalPacketLoss); got != 1 {
		t.Fatalf("setup: an assigned check wrote %d loss series, want 1", got)
	}

	a.applyExternalAssignment(&pb.ExternalCheckAssignment{})
	a.scheduler.handler(late)
	if got := testutil.CollectAndCount(a.metrics.ExternalPacketLoss); got != 0 {
		t.Errorf("a result that landed after the check was removed wrote %d loss series back", got)
	}
	if got := testutil.CollectAndCount(a.metrics.ExternalResults); got != 0 {
		t.Errorf("external_results_total has %d series for a removed check", got)
	}
}

// forgettingChecker records what the agent asks it to forget, like the pmtu checker's clamp warnings.
type forgettingChecker struct {
	fakeChecker
	forgot []string
}

func (f *forgettingChecker) ForgetPeer(nodeName string) { f.forgot = append(f.forgot, nodeName) }

// A departed peer is forgotten by the pmtu checker too, or node churn grows its per-peer record for
// the life of the agent.
func TestDepartedPeerIsForgottenByThePMTUChecker(t *testing.T) {
	a, _ := newDepartureAgent(t)
	pm := &forgettingChecker{fakeChecker: fakeChecker{name: model.CheckPMTU}}
	a.checkers = map[model.CheckType]checker.Checker{model.CheckPMTU: pm}

	a.applyPeers([]checker.Target{{AgentID: "id-b", NodeName: "node-b"}, {AgentID: "id-c", NodeName: "node-c"}})
	a.applyPeers([]checker.Target{{AgentID: "id-b", NodeName: "node-b"}})

	if len(pm.forgot) != 1 || pm.forgot[0] != "node-c" {
		t.Fatalf("pmtu checker forgot %v, want exactly the departed node-c", pm.forgot)
	}
}

/*
A zone change, this agent's or a peer's, leaves the pair's counters and histograms under the old
zone flat until the agent restarts: nothing writes them again, and RetirePeer only runs once the
peer leaves the plan. Every node relabel doubled the agent's per-pair cardinality.
*/
func TestZoneChangeRetiresThePairSeriesUnderTheOldZone(t *testing.T) {
	a, reg := newDepartureAgent(t)
	peer := checker.Target{AgentID: "id-b", NodeName: "node-b", Zone: "zone-1"}
	a.applyPeers([]checker.Target{peer})
	deliverUDP(a, peer, 0)

	a.adoptZone("zone-a2")
	deliverUDP(a, peer, 0)
	peer.Zone = "zone-2"
	a.applyPeers([]checker.Target{peer})
	deliverUDP(a, peer, 0)

	for _, family := range []string{"kconmon_ng_udp_results_total", "kconmon_ng_udp_rtt_seconds", "kconmon_ng_tcp_results_total"} {
		for label, want := range map[string]string{"source_zone": "zone-a2", "destination_zone": "zone-2"} {
			values := labelValues(t, reg, family, label)
			if len(values) == 0 {
				t.Errorf("%s has no series at all", family)
			}
			for _, v := range values {
				if v != want {
					t.Errorf("%s still exports %s=%s after the zone changed to %s", family, label, v, want)
				}
			}
		}
	}
}

/*
A peer that leaves and comes back inside the grace period under another zone (a node recreated under
the same name, a rollout after an agent.zone change) leaves its old-zone counters behind: the zone
change is not seen against the current list, and the departed entry that would retire them is gone.
*/
func TestPeerBackUnderANewZoneInsideTheGraceRetiresTheOldZone(t *testing.T) {
	a, reg := newDepartureAgent(t)
	clock := time.Unix(1_800_000_000, 0)
	a.now = func() time.Time { return clock }

	peer := checker.Target{AgentID: "id-b", NodeName: "node-b", Zone: "zone-1"}
	a.applyPeers([]checker.Target{peer})
	deliverUDP(a, peer, 0)

	a.applyPeers(nil)
	clock = clock.Add(30 * time.Second)
	peer.Zone = "zone-2"
	a.applyPeers([]checker.Target{peer})
	deliverUDP(a, peer, 0)
	clock = clock.Add(24 * time.Hour)
	a.sweepDepartedPeers()

	for _, family := range []string{"kconmon_ng_udp_results_total", "kconmon_ng_udp_rtt_seconds", "kconmon_ng_tcp_results_total"} {
		values := labelValues(t, reg, family, "destination_zone")
		if len(values) == 0 {
			t.Errorf("%s has no series at all", family)
		}
		for _, v := range values {
			if v != "zone-2" {
				t.Errorf("%s still exports destination_zone=%s a day after node-b came back in zone-2", family, v)
			}
		}
	}
}

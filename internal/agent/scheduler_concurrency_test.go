package agent

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/checker"
	"github.com/EsDmitrii/kconmon-ng/internal/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// slowChecker stands in for a probe waiting out its timeout against a dead
// peer: every Check blocks for delay before returning. It also tracks how many
// Checks run at once, which is what the concurrency bound is about.
type slowChecker struct {
	name  model.CheckType
	delay time.Duration
	fail  bool

	mu          sync.Mutex
	inflight    int
	maxInflight int
	calls       int
}

func (c *slowChecker) Name() model.CheckType { return c.name }

func (c *slowChecker) Check(ctx context.Context, target checker.Target) model.CheckResult {
	c.mu.Lock()
	c.calls++
	c.inflight++
	if c.inflight > c.maxInflight {
		c.maxInflight = c.inflight
	}
	c.mu.Unlock()

	select {
	case <-time.After(c.delay):
	case <-ctx.Done():
	}

	c.mu.Lock()
	c.inflight--
	c.mu.Unlock()

	return model.CheckResult{
		Type:        c.name,
		Success:     !c.fail,
		Destination: target.NodeName,
		Timestamp:   time.Now(),
	}
}

func (c *slowChecker) stats() (calls, maxInflight int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls, c.maxInflight
}

func makePeers(n int) []checker.Target {
	peers := make([]checker.Target, n)
	for i := range peers {
		peers[i] = checker.Target{
			AgentID:  fmt.Sprintf("peer-%d", i),
			NodeName: fmt.Sprintf("node-%d", i),
			PodIP:    fmt.Sprintf("10.0.%d.%d", i/250, i%250+2),
			Port:     8080,
		}
	}
	return peers
}

/*
M9-1 reproduction: a round must not pay each dead peer's timeout SERIALLY.

10 peers that each take 200ms (a scaled-down stand-in for a 1s probe timeout)
cost 2s when probed one after another — with the default 5s interval, a real
100-node partition costs 100s per round and the cadence collapses exactly when
the fleet must keep measuring. Concurrent probing keeps the round near one
probe's duration.
*/
func TestPeerRoundIsNotSerial(t *testing.T) {
	const peerCount = 10
	const probeDelay = 200 * time.Millisecond

	var results atomic.Int64
	s := NewScheduler(checker.Target{AgentID: "self", NodeName: "self-node"}, func(model.CheckResult) {
		results.Add(1)
	})
	sc := &slowChecker{name: model.CheckTCP, delay: probeDelay}
	s.AddChecker(sc, SchedulerConfig{Interval: 5 * time.Second})
	s.UpdatePeers(makePeers(peerCount))

	start := time.Now()
	s.runCheckerOnce(context.Background(), sc)
	elapsed := time.Since(start)

	if got := results.Load(); got != peerCount {
		t.Fatalf("expected %d results from one round, got %d", peerCount, got)
	}
	// Half of the serial wall clock is a generous bound: the concurrent round
	// takes ~one probeDelay, the serial one takes peerCount of them.
	serial := time.Duration(peerCount) * probeDelay
	if elapsed >= serial/2 {
		t.Fatalf("round over %d slow peers took %v — serial probing (serial would be %v); a dead-peer partition collapses the cadence", peerCount, elapsed, serial)
	}
}

// M9-1: the fan-out is bounded, not one goroutine per peer — a 100-node round
// must hold at most peerProbeConcurrency probes in flight.
func TestPeerRoundConcurrencyIsBounded(t *testing.T) {
	const peerCount = 100

	s := NewScheduler(checker.Target{AgentID: "self", NodeName: "self-node"}, func(model.CheckResult) {})
	sc := &slowChecker{name: model.CheckTCP, delay: 50 * time.Millisecond}
	s.AddChecker(sc, SchedulerConfig{Interval: 5 * time.Second})
	s.UpdatePeers(makePeers(peerCount))

	s.runCheckerOnce(context.Background(), sc)

	calls, maxInflight := sc.stats()
	if calls != peerCount {
		t.Fatalf("expected %d probes, got %d", peerCount, calls)
	}
	if maxInflight > peerProbeConcurrency {
		t.Fatalf("probe fan-out exceeded the bound: %d in flight, limit %d", maxInflight, peerProbeConcurrency)
	}
	if maxInflight < 2 {
		t.Fatalf("probes never overlapped (max in flight %d): the round is still serial", maxInflight)
	}
}

// blockingTrace is a traceFn seam that counts starts and holds every trace
// until release is closed.
type blockingTrace struct {
	started atomic.Int64
	release chan struct{}
}

func (b *blockingTrace) run(_ context.Context, target checker.Target) model.CheckResult {
	b.started.Add(1)
	<-b.release
	return model.CheckResult{Type: model.CheckMTR, Success: true, Destination: target.NodeName, Timestamp: time.Now()}
}

func waitCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

/*
M8-9 reproduction: a partition fails MANY pairs at once, and every failure used
to launch its own trace goroutine — the per-pair cooldown never bounded traces
to DISTINCT destinations, so 20 broken pairs meant 20 concurrent traceroutes on
a 200m-CPU agent. The global semaphore caps them at maxConcurrentReactiveTraces
and lets the rest retry on their next failed probe.
*/
func TestReactiveMTRIsGloballyBounded(t *testing.T) {
	const failedPeers = 20

	var mu sync.Mutex
	var traced []string
	s := NewScheduler(checker.Target{AgentID: "self", NodeName: "self-node"}, func(r model.CheckResult) {
		if r.Type == model.CheckMTR {
			mu.Lock()
			traced = append(traced, r.Destination)
			mu.Unlock()
		}
	})
	s.SetMTRChecker(checker.NewMTRChecker(5, time.Second, time.Minute))
	bt := &blockingTrace{release: make(chan struct{})}
	s.mu.Lock()
	s.traceFn = bt.run
	s.mu.Unlock()

	reg := prometheus.NewRegistry()
	m := metrics.NewPrometheusMetrics("test_m89", reg)
	s.SetSelfMetrics(m)

	failed := model.CheckResult{Type: model.CheckTCP, Success: false, Error: "tcp failed"}
	for i := range failedPeers {
		peer := checker.Target{NodeName: fmt.Sprintf("dest-%d", i)}
		s.triggerMTR(context.Background(), peer, &failed)
	}

	// Slot acquisition is synchronous in triggerMTR, so after the loop exactly
	// the bounded number of traces may run; give their goroutines a moment to
	// actually reach the trace function.
	waitCond(t, "the bounded traces to start", func() bool {
		return bt.started.Load() >= maxConcurrentReactiveTraces
	})
	time.Sleep(50 * time.Millisecond)
	if got := bt.started.Load(); got != maxConcurrentReactiveTraces {
		t.Fatalf("%d concurrent reactive traces started for %d failed peers, want at most %d", got, failedPeers, maxConcurrentReactiveTraces)
	}
	if got := testutil.ToFloat64(m.AgentMTRReactiveInflight.WithLabelValues()); got != maxConcurrentReactiveTraces {
		t.Fatalf("inflight gauge = %v while %d traces are held, want %d", got, maxConcurrentReactiveTraces, maxConcurrentReactiveTraces)
	}
	if got := testutil.ToFloat64(m.AgentMTRReactiveCoalesced.WithLabelValues("saturated")); got != failedPeers-maxConcurrentReactiveTraces {
		t.Fatalf("saturated counter = %v, want %d", got, failedPeers-maxConcurrentReactiveTraces)
	}

	// Slots free when traces finish, and a later failure gets one again.
	close(bt.release)
	waitCond(t, "the held traces to report", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(traced) == maxConcurrentReactiveTraces
	})
	waitCond(t, "the inflight gauge to drain", func() bool {
		return testutil.ToFloat64(m.AgentMTRReactiveInflight.WithLabelValues()) == 0
	})
	s.triggerMTR(context.Background(), checker.Target{NodeName: "dest-late"}, &failed)
	waitCond(t, "a trace to start after slots freed", func() bool {
		return bt.started.Load() == maxConcurrentReactiveTraces+1
	})
}

/*
M8-9: repeated failures toward the SAME destination inside the cooldown window
coalesce into the one trace already taken (or in flight) — a partition where
tcp, udp and icmp all fail toward one node runs one traceroute, not three —
and the coalesced failures are counted so an operator can see the suppression.
*/
func TestReactiveMTRCoalescesSameDestination(t *testing.T) {
	s := NewScheduler(checker.Target{AgentID: "self", NodeName: "self-node"}, func(model.CheckResult) {})
	s.SetMTRChecker(checker.NewMTRChecker(5, time.Second, time.Minute))
	bt := &blockingTrace{release: make(chan struct{})}
	close(bt.release) // traces return immediately; the cooldown is what coalesces
	s.mu.Lock()
	s.traceFn = bt.run
	s.mu.Unlock()

	reg := prometheus.NewRegistry()
	m := metrics.NewPrometheusMetrics("test_m89c", reg)
	s.SetSelfMetrics(m)

	peer := checker.Target{NodeName: "same-dest"}
	tcpFail := model.CheckResult{Type: model.CheckTCP, Success: false, Error: "tcp failed"}
	icmpFail := model.CheckResult{Type: model.CheckICMP, Success: false, Error: "icmp failed"}
	udpFail := model.CheckResult{Type: model.CheckUDP, Success: false, Error: "udp failed"}

	s.triggerMTR(context.Background(), peer, &tcpFail)
	s.triggerMTR(context.Background(), peer, &icmpFail)
	s.triggerMTR(context.Background(), peer, &udpFail)

	waitCond(t, "the single coalesced trace to start", func() bool {
		return bt.started.Load() == 1
	})
	time.Sleep(50 * time.Millisecond)
	if got := bt.started.Load(); got != 1 {
		t.Fatalf("%d traces started for 3 failures toward one destination inside the cooldown, want 1", got)
	}
	if got := testutil.ToFloat64(m.AgentMTRReactiveCoalesced.WithLabelValues("cooldown")); got != 2 {
		t.Fatalf("cooldown-coalesced counter = %v, want 2", got)
	}
}

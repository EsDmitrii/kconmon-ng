package agent

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/checker"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

type mockChecker struct {
	name  model.CheckType
	mu    sync.Mutex
	calls int
}

func (m *mockChecker) Name() model.CheckType { return m.name }

func (m *mockChecker) Check(_ context.Context, target checker.Target) model.CheckResult {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()

	return model.CheckResult{
		Type:        m.name,
		Success:     true,
		Destination: target.NodeName,
		Duration:    1 * time.Millisecond,
		Timestamp:   time.Now(),
	}
}

func (m *mockChecker) CallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func TestSchedulerRunsCheckers(t *testing.T) {
	source := checker.Target{
		AgentID:  "test-agent",
		NodeName: "test-node",
		PodIP:    "10.0.0.1",
		Zone:     "zone-a",
	}

	var results []model.CheckResult
	var mu sync.Mutex

	handler := func(r model.CheckResult) {
		mu.Lock()
		results = append(results, r)
		mu.Unlock()
	}

	s := NewScheduler(source, handler)

	mc := &mockChecker{name: model.CheckTCP}
	s.AddChecker(mc, SchedulerConfig{
		Interval: 50 * time.Millisecond,
		Jitter:   5 * time.Millisecond,
	})

	s.UpdatePeers([]checker.Target{
		{AgentID: "peer-1", NodeName: "node-1", PodIP: "10.0.0.2", Zone: "zone-b", Port: 8080},
		{AgentID: "peer-2", NodeName: "node-2", PodIP: "10.0.0.3", Zone: "zone-a", Port: 8080},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	s.Run(ctx)

	mu.Lock()
	count := len(results)
	mu.Unlock()

	if count < 2 {
		t.Errorf("expected at least 2 results (one per peer), got %d", count)
	}

	if mc.CallCount() < 2 {
		t.Errorf("expected at least 2 checker calls, got %d", mc.CallCount())
	}
}

func TestSchedulerSetSourceZone(t *testing.T) {
	source := checker.Target{AgentID: "a", NodeName: "test-node", PodIP: "10.0.0.1"}

	var results []model.CheckResult
	var mu sync.Mutex
	handler := func(r model.CheckResult) {
		mu.Lock()
		results = append(results, r)
		mu.Unlock()
	}

	s := NewScheduler(source, handler)
	s.SetSourceZone("zone-a")
	s.AddChecker(&mockChecker{name: model.CheckTCP}, SchedulerConfig{Interval: 50 * time.Millisecond})
	s.UpdatePeers([]checker.Target{{AgentID: "peer-1", NodeName: "node-1", PodIP: "10.0.0.2", Port: 8080}})

	/* The window is CI-sized on purpose: the first round only needs the goroutine scheduled once,
	   but a hosted runner can stall that for tens of milliseconds (GC, CFS throttling), and a
	   120ms window left under 120ms of slack for it. */
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	s.Run(ctx)

	mu.Lock()
	defer mu.Unlock()
	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}
	for _, r := range results {
		if r.SourceZone != "zone-a" {
			t.Fatalf("expected SourceZone zone-a after SetSourceZone, got %q", r.SourceZone)
		}
	}
}

func TestResolveZone(t *testing.T) {
	cases := []struct {
		name     string
		env      string
		resolved string
		want     string
	}{
		{"env wins over resolved", "zone-env", "zone-ctrl", "zone-env"},
		{"adopt resolved when env empty", "", "zone-ctrl", "zone-ctrl"},
		{"keep empty when both empty", "", "", ""},
		{"keep env when resolved empty", "zone-env", "", "zone-env"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveZone(tc.env, tc.resolved); got != tc.want {
				t.Errorf("resolveZone(%q, %q) = %q, want %q", tc.env, tc.resolved, got, tc.want)
			}
		})
	}
}

func TestSchedulerUpdatePeersFiltersSelf(t *testing.T) {
	source := checker.Target{
		AgentID:  "self-agent",
		NodeName: "self-node",
		PodIP:    "10.0.0.1",
		Zone:     "zone-a",
	}
	s := NewScheduler(source, func(_ model.CheckResult) {})

	s.UpdatePeers([]checker.Target{
		{AgentID: "self-agent", NodeName: "self-node", PodIP: "10.0.0.1"}, // same ID + IP
		{AgentID: "other-1", NodeName: "n1", PodIP: "10.0.0.2"},
		{AgentID: "other-2", NodeName: "n2", PodIP: "10.0.0.3"},
		{AgentID: "sneaky", NodeName: "n3", PodIP: "10.0.0.1"}, // different ID, same IP
	})

	s.mu.RLock()
	peers := make([]checker.Target, len(s.peers))
	copy(peers, s.peers)
	s.mu.RUnlock()

	if len(peers) != 2 {
		t.Fatalf("expected 2 peers after self-filter, got %d", len(peers))
	}
	for _, p := range peers {
		if p.AgentID == source.AgentID || p.PodIP == source.PodIP {
			t.Errorf("self or IP-duplicate peer leaked through: %+v", p)
		}
	}
}

func TestSchedulerUpdatePeers(t *testing.T) {
	source := checker.Target{AgentID: "test", NodeName: "test-node"}
	s := NewScheduler(source, func(_ model.CheckResult) {})

	s.UpdatePeers([]checker.Target{
		{AgentID: "p1", NodeName: "n1"},
	})

	s.mu.RLock()
	if len(s.peers) != 1 {
		t.Errorf("expected 1 peer, got %d", len(s.peers))
	}
	s.mu.RUnlock()

	s.UpdatePeers([]checker.Target{
		{AgentID: "p1", NodeName: "n1"},
		{AgentID: "p2", NodeName: "n2"},
		{AgentID: "p3", NodeName: "n3"},
	})

	s.mu.RLock()
	if len(s.peers) != 3 {
		t.Errorf("expected 3 peers after update, got %d", len(s.peers))
	}
	s.mu.RUnlock()
}

func TestSchedulerNodeLocalRunsOnce(t *testing.T) {
	source := checker.Target{AgentID: "test", NodeName: "test-node", Zone: "zone-a"}

	var mu sync.Mutex
	var results []model.CheckResult
	handler := func(r model.CheckResult) {
		mu.Lock()
		results = append(results, r)
		mu.Unlock()
	}

	s := NewScheduler(source, handler)
	mc := &mockChecker{name: model.CheckDNS}
	s.AddChecker(mc, SchedulerConfig{
		Interval:  50 * time.Millisecond,
		Jitter:    5 * time.Millisecond,
		NodeLocal: true,
	})

	/* peerCount is far above the ticks a 500ms window can produce (~11 at a 50ms interval), so the
	   per-peer regression stays unmistakable while the window itself is CI-sized: the old
	   120ms/5-peer shape left under 120ms of slack for the first round to be scheduled at all. */
	const peerCount = 50
	peers := make([]checker.Target, peerCount)
	for i := range peers {
		peers[i] = checker.Target{AgentID: fmt.Sprintf("p%d", i), NodeName: fmt.Sprintf("n%d", i)}
	}
	s.UpdatePeers(peers)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	s.Run(ctx)

	mu.Lock()
	resultsCopy := make([]model.CheckResult, len(results))
	copy(resultsCopy, results)
	mu.Unlock()

	calls := mc.CallCount()

	if calls == 0 {
		t.Fatal("expected at least one checker call")
	}

	// NodeLocal: checker is called once per tick, not once per peer per tick.
	// In ~500ms at 50ms interval we get at most a dozen ticks.
	// If it ran per-peer, calls would be peerCount × ticks — far more than ticks alone.
	if calls >= peerCount {
		t.Errorf("NodeLocal checker should not run per-peer: got %d calls with %d peers", calls, peerCount)
	}

	// Results count must equal checker call count (one result per invocation).
	if len(resultsCopy) != calls {
		t.Errorf("result count %d != call count %d", len(resultsCopy), calls)
	}

	// Destination must be empty for node-local checks.
	for _, r := range resultsCopy {
		if r.Destination != "" {
			t.Errorf("NodeLocal result should have empty Destination, got %q", r.Destination)
		}
	}
}

// A failing CONTINUOUS external check must never trigger an MTR trace.
func TestTriggerMTRSkipsExternalFailures(t *testing.T) {
	var mu sync.Mutex
	var results []model.CheckResult
	s := NewScheduler(checker.Target{AgentID: "a", NodeName: "self"}, func(r model.CheckResult) {
		mu.Lock()
		results = append(results, r)
		mu.Unlock()
	})

	mtr := checker.NewMTRChecker(5, time.Second, time.Minute)
	s.SetMTRChecker(mtr)

	// PodIP is deliberately empty: if the exclusion ever regresses, MTRChecker
	// bails on the invalid IP instead of tracing a real path from a unit test.
	peer := checker.Target{NodeName: "external-target"}
	failed := model.CheckResult{Type: checker.CheckExternal, Success: false, Error: "external check failed"}

	s.triggerMTR(context.Background(), peer, &failed)

	mu.Lock()
	got := len(results)
	mu.Unlock()
	if got != 0 {
		t.Fatalf("external failure must not produce an MTR result, got %d", got)
	}
	if !mtr.TryAcquire("self", "external-target") {
		t.Fatal("external failure consumed the MTR cooldown token, so triggerMTR did not return early")
	}
}

// Control for the test above: a peer TCP failure still triggers a trace, so the
// exclusion is narrow and not an accidental blanket disable.
func TestTriggerMTRStillFiresForPeerFailures(t *testing.T) {
	var mu sync.Mutex
	var results []model.CheckResult
	s := NewScheduler(checker.Target{AgentID: "a", NodeName: "self"}, func(r model.CheckResult) {
		mu.Lock()
		results = append(results, r)
		mu.Unlock()
	})
	s.SetMTRChecker(checker.NewMTRChecker(5, time.Second, time.Minute))

	// Empty PodIP makes MTRChecker fail fast on the invalid address, so the
	// trace never leaves the machine.
	peer := checker.Target{NodeName: "peer-node"}
	failed := model.CheckResult{Type: model.CheckTCP, Success: false, Error: "tcp failed"}

	s.triggerMTR(context.Background(), peer, &failed)

	/* The trace runs in its OWN goroutine now — inline, it stalled the whole peer-probe loop for up
	   to thirty seconds during an outage, which is exactly when traces fire and exactly when the
	   fleet must not stop measuring. So the result is awaited rather than read synchronously. */
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		done := len(results) > 0
		mu.Unlock()
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(results) != 1 || results[0].Type != model.CheckMTR {
		t.Fatalf("a peer TCP failure must still trigger MTR, got %+v", results)
	}
}

func TestSchedulerNoPeers(t *testing.T) {
	source := checker.Target{AgentID: "test", NodeName: "test-node"}

	callCount := 0
	var mu sync.Mutex
	handler := func(_ model.CheckResult) {
		mu.Lock()
		callCount++
		mu.Unlock()
	}

	s := NewScheduler(source, handler)
	mc := &mockChecker{name: model.CheckTCP}
	s.AddChecker(mc, SchedulerConfig{
		Interval: 50 * time.Millisecond,
		Jitter:   5 * time.Millisecond,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	s.Run(ctx)

	mu.Lock()
	if callCount != 0 {
		t.Errorf("expected 0 calls with no peers, got %d", callCount)
	}
	mu.Unlock()
}

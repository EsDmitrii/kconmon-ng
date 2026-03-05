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

	const peerCount = 5
	peers := make([]checker.Target, peerCount)
	for i := range peers {
		peers[i] = checker.Target{AgentID: fmt.Sprintf("p%d", i), NodeName: fmt.Sprintf("n%d", i)}
	}
	s.UpdatePeers(peers)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
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
	// In ~120ms at 50ms interval we get at most a handful of ticks.
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

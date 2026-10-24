package controller

import (
	"slices"
	"sync"
	"testing"

	"github.com/EsDmitrii/kconmon-ng/internal/model"
	corev1 "k8s.io/api/core/v1"
)

func newTestNodeWatcher() *NodeWatcher {
	return &NodeWatcher{
		nodes:              make(map[string]model.NodeInfo),
		schedulable:        make(map[string]bool),
		failureDomainLabel: "topology.kubernetes.io/zone",
		stopCh:             make(chan struct{}),
	}
}

func node(name string, unschedulable bool) *corev1.Node {
	n := &corev1.Node{}
	n.Name = name
	n.Spec.Unschedulable = unschedulable
	return n
}

func zonedNode(name, zone string) *corev1.Node {
	n := node(name, false)
	n.Labels = map[string]string{"topology.kubernetes.io/zone": zone}
	return n
}

func TestZoneFor(t *testing.T) {
	nw := newTestNodeWatcher()

	nw.onNodeEvent(zonedNode("node-a", "zone-a"))
	nw.onNodeEvent(zonedNode("node-c", "zone-c"))
	nw.onNodeEvent(node("node-b", false)) // no zone label

	if got := nw.ZoneFor("node-a"); got != "zone-a" {
		t.Errorf("expected zone-a, got %q", got)
	}
	if got := nw.ZoneFor("node-c"); got != "zone-c" {
		t.Errorf("expected zone-c, got %q", got)
	}
	if got := nw.ZoneFor("node-b"); got != "" {
		t.Errorf("expected empty zone for unlabelled node, got %q", got)
	}
	if got := nw.ZoneFor("node-missing"); got != "" {
		t.Errorf("expected empty zone for unknown node, got %q", got)
	}
}

func TestOnZoneChange(t *testing.T) {
	nw := newTestNodeWatcher()

	var mu sync.Mutex
	changes := map[string]string{}
	var calls int
	nw.OnZoneChange(func(nodeName, zone string) {
		mu.Lock()
		defer mu.Unlock()
		changes[nodeName] = zone
		calls++
	})

	// Initial add with a zone -> change from "" to zone-a.
	nw.onNodeEvent(zonedNode("node-a", "zone-a"))
	// Re-emit same zone -> no change, no callback.
	nw.onNodeEvent(zonedNode("node-a", "zone-a"))
	// Label changes -> callback fires.
	nw.onNodeEvent(zonedNode("node-a", "zone-b"))
	// Node with no zone label -> no callback (zone stays "").
	nw.onNodeEvent(node("node-b", false))

	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("expected 2 zone-change callbacks, got %d", calls)
	}
	if changes["node-a"] != "zone-b" {
		t.Errorf("expected node-a final zone zone-b, got %q", changes["node-a"])
	}
}

// A cordoned node keeps its agent (DaemonSet pods tolerate node.kubernetes.io/unschedulable), so it
// is expected while that agent is registered: leaving it out let one cordoned node with a live agent
// hide one missing agent elsewhere from KconmonAgentsMissing. A cordoned node whose agent is gone is
// in maintenance (drained, powered off) and is not expected.
func TestExpectedAgentsCountsACordonedNodeOnlyWhileItsAgentRuns(t *testing.T) {
	nw := newTestNodeWatcher()
	nw.onNodeEvent(node("node-a", false))
	nw.onNodeEvent(node("node-b", false))
	nw.onNodeEvent(node("node-c", true)) // cordoned

	has := func(nodes ...string) func(string) bool {
		return func(n string) bool { return slices.Contains(nodes, n) }
	}
	if got := nw.ExpectedAgents(has("node-a", "node-c")); got != 3 {
		t.Errorf("node-b lost its agent while cordoned node-c runs one: expected %d, want 3", got)
	}
	if got := nw.ExpectedAgents(has("node-a", "node-b")); got != 2 {
		t.Errorf("cordoned node-c without its agent: expected %d, want 2", got)
	}

	nw.onNodeEvent(node("node-c", false)) // uncordoned, still without an agent
	if got := nw.ExpectedAgents(has("node-a", "node-b")); got != 3 {
		t.Errorf("uncordoned node-c without an agent: expected %d, want 3", got)
	}
	nw.onNodeDelete(node("node-a", false))
	if got := nw.ExpectedAgents(has("node-b", "node-c")); got != 2 {
		t.Errorf("after deleting node-a: expected %d, want 2", got)
	}
}

func TestNodesChangeCallback(t *testing.T) {
	nw := newTestNodeWatcher()

	var mu sync.Mutex
	var calls int
	nw.OnNodesChange(func() {
		mu.Lock()
		defer mu.Unlock()
		calls++
	})

	nw.onNodeEvent(node("node-a", false))
	nw.onNodeEvent(node("node-b", true))
	nw.onNodeDelete(node("node-a", false))

	mu.Lock()
	defer mu.Unlock()
	if calls != 3 {
		t.Fatalf("expected callback on every event (3), got %d", calls)
	}
}

// An external agent runs on no cluster node, so a cordoned node that shares its name stays out.
func TestExpectedAgentsForIgnoresExternalAgents(t *testing.T) {
	nw := newTestNodeWatcher()
	nw.onNodeEvent(node("node-a", false))
	nw.onNodeEvent(node("edge", true))
	agents := []model.AgentInfo{
		{ID: "a", NodeName: "node-a"},
		{ID: "x", NodeName: "edge", Labels: map[string]string{model.LabelExternal: "true"}},
	}
	if got := expectedAgentsFor(nw, agents); got != 1 {
		t.Errorf("expectedAgentsFor = %d, want 1: the external agent must not keep cordoned node edge expected", got)
	}
}

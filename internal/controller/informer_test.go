package controller

import (
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

func TestSchedulableNodeCount(t *testing.T) {
	nw := newTestNodeWatcher()

	nw.onNodeEvent(node("node-a", false))
	nw.onNodeEvent(node("node-b", false))
	nw.onNodeEvent(node("node-c", true)) // cordoned, not schedulable

	if got := nw.SchedulableNodeCount(); got != 2 {
		t.Fatalf("expected 2 schedulable nodes, got %d", got)
	}

	// Cordon node-b -> update event with unschedulable=true
	nw.onNodeEvent(node("node-b", true))
	if got := nw.SchedulableNodeCount(); got != 1 {
		t.Fatalf("expected 1 schedulable node after cordon, got %d", got)
	}

	// Uncordon node-c -> update event with unschedulable=false
	nw.onNodeEvent(node("node-c", false))
	if got := nw.SchedulableNodeCount(); got != 2 {
		t.Fatalf("expected 2 schedulable nodes after uncordon, got %d", got)
	}

	// Delete node-a
	nw.onNodeDelete(node("node-a", false))
	if got := nw.SchedulableNodeCount(); got != 1 {
		t.Fatalf("expected 1 schedulable node after delete, got %d", got)
	}
}

func TestSchedulableNodeCountCallback(t *testing.T) {
	nw := newTestNodeWatcher()

	var mu sync.Mutex
	var lastCount int
	var calls int
	nw.OnCountChange(func(c int) {
		mu.Lock()
		defer mu.Unlock()
		lastCount = c
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
	if lastCount != 0 {
		t.Fatalf("expected final schedulable count 0, got %d", lastCount)
	}
}

package store

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"
)

// base is a fixed instant every fold test builds its event times from: the
// fold's answer must depend only on the ORDER of the records, never on the
// wall clock, so nothing here calls time.Now().
var base = time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

// topoEvent builds one topology_changed EventRecord whose details carry
// exactly the three keys events.topologyChangedDetails marshals
// (internal/console/events/live_event.go): reason, nodeName, agentId. Tests
// pass "" for a field the real controller does not populate today, which is
// how the "names nobody" case below stays honest rather than hypothetical.
func topoEvent(t *testing.T, offset time.Duration, reason, node, agent string) EventRecord {
	t.Helper()
	details, err := json.Marshal(map[string]string{
		"reason": reason, "nodeName": node, "agentId": agent,
	})
	if err != nil {
		t.Fatalf("marshal details: %v", err)
	}
	scope := node
	if scope == "" {
		scope = "cluster"
	}
	return EventRecord{
		EventTime: base.Add(offset),
		Type:      eventTypeTopologyChanged,
		Severity:  "info",
		Scope:     scope,
		Summary:   fmt.Sprintf("topology changed: %s", reason),
		Details:   details,
	}
}

func nodeNames(snap *TopologySnapshot) []string {
	out := make([]string, 0, len(snap.Nodes))
	for _, n := range snap.Nodes {
		out = append(out, n.Name)
	}
	return out
}

func agentIDs(snap *TopologySnapshot) []string {
	out := make([]string, 0, len(snap.Agents))
	for _, a := range snap.Agents {
		out = append(out, a.ID)
	}
	return out
}

func TestFoldTopologyEmptyHistory(t *testing.T) {
	snap := foldTopology(nil)

	if len(snap.Nodes) != 0 || len(snap.Agents) != 0 {
		t.Errorf("empty history folded to %d nodes / %d agents, want 0/0", len(snap.Nodes), len(snap.Agents))
	}
	if snap.Nodes == nil || snap.Agents == nil {
		t.Error("Nodes/Agents must be non-nil empty slices, never nil: the handler serves them as JSON arrays")
	}
	if snap.EventsFolded != 0 || snap.UnfoldableEvents != 0 || snap.Truncated {
		t.Errorf("counters on an empty fold = %+v, want all zero", snap)
	}
	if !snap.LastChange.IsZero() {
		t.Errorf("LastChange = %v, want the zero time when nothing was folded", snap.LastChange)
	}
}

func TestFoldTopologyAddsRegisteredAgents(t *testing.T) {
	snap := foldTopology([]EventRecord{
		topoEvent(t, 0, topologyReasonRegistered, "node-a", "agent-a"),
		topoEvent(t, time.Minute, topologyReasonRegistered, "node-b", "agent-b"),
	})

	if got, want := nodeNames(&snap), []string{"node-a", "node-b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("nodes = %v, want %v", got, want)
	}
	if got, want := agentIDs(&snap), []string{"agent-a", "agent-b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("agents = %v, want %v", got, want)
	}
	if snap.EventsFolded != 2 || snap.UnfoldableEvents != 0 {
		t.Errorf("EventsFolded/UnfoldableEvents = %d/%d, want 2/0", snap.EventsFolded, snap.UnfoldableEvents)
	}
	if want := base.Add(time.Minute); !snap.LastChange.Equal(want) {
		t.Errorf("LastChange = %v, want the last record's time %v", snap.LastChange, want)
	}
	// Zone and podIP are NOT in the details payload at all, so the fold must
	// leave them empty rather than guess -- this is the assertion that pins
	// "what is not reconstructible" as behaviour, not just a comment.
	for _, n := range snap.Nodes {
		if n.Zone != "" {
			t.Errorf("node %s zone = %q, want empty: zone is never recorded in a topology_changed event", n.Name, n.Zone)
		}
		if !n.Ready {
			t.Errorf("node %s ready = false, want true: presence in the fold is the only readiness the events carry", n.Name)
		}
	}
	for _, a := range snap.Agents {
		if a.PodIP != "" || a.Zone != "" {
			t.Errorf("agent %s = %+v, want empty podIP and zone: neither is ever recorded", a.ID, a)
		}
	}
	if snap.Agents[0].NodeName != "node-a" {
		t.Errorf("agent-a nodeName = %q, want node-a", snap.Agents[0].NodeName)
	}
}

func TestFoldTopologyRemovesOnDeregisterAndEvict(t *testing.T) {
	for _, reason := range []string{topologyReasonDeregistered, topologyReasonEvicted} {
		t.Run(reason, func(t *testing.T) {
			snap := foldTopology([]EventRecord{
				topoEvent(t, 0, topologyReasonRegistered, "node-a", "agent-a"),
				topoEvent(t, time.Minute, topologyReasonRegistered, "node-b", "agent-b"),
				topoEvent(t, 2*time.Minute, reason, "node-a", "agent-a"),
			})

			if got, want := nodeNames(&snap), []string{"node-b"}; !reflect.DeepEqual(got, want) {
				t.Errorf("nodes = %v, want %v", got, want)
			}
			if got, want := agentIDs(&snap), []string{"agent-b"}; !reflect.DeepEqual(got, want) {
				t.Errorf("agents = %v, want %v", got, want)
			}
			if snap.EventsFolded != 3 {
				t.Errorf("EventsFolded = %d, want 3", snap.EventsFolded)
			}
		})
	}
}

func TestFoldTopologyRemoveOfAnAbsentNodeIsANoOp(t *testing.T) {
	// The retention floor can cut a node's REGISTER away while keeping its
	// deregister, so the fold must survive a removal it never saw an add for.
	snap := foldTopology([]EventRecord{
		topoEvent(t, 0, topologyReasonDeregistered, "node-gone", "agent-gone"),
		topoEvent(t, time.Minute, topologyReasonRegistered, "node-a", "agent-a"),
	})

	if got, want := nodeNames(&snap), []string{"node-a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("nodes = %v, want %v", got, want)
	}
	if snap.UnfoldableEvents != 0 {
		t.Errorf("UnfoldableEvents = %d, want 0: the event named a node, it just was not present", snap.UnfoldableEvents)
	}
}

func TestFoldTopologyZoneChangeKeepsMembershipButCannotSetTheZone(t *testing.T) {
	// zone_updated is the one reason whose whole POINT is a value the details
	// payload does not carry. Membership survives; the zone stays empty, which
	// is the honest answer.
	snap := foldTopology([]EventRecord{
		topoEvent(t, 0, topologyReasonRegistered, "node-a", "agent-a"),
		topoEvent(t, time.Minute, topologyReasonZoneUpdated, "node-a", "agent-a"),
	})

	if got, want := nodeNames(&snap), []string{"node-a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("nodes = %v, want %v", got, want)
	}
	if snap.Nodes[0].Zone != "" {
		t.Errorf("zone = %q after zone_updated, want empty: the new zone is not in the event", snap.Nodes[0].Zone)
	}
	if snap.UnfoldableEvents != 0 {
		t.Errorf("UnfoldableEvents = %d, want 0", snap.UnfoldableEvents)
	}
}

func TestFoldTopologyDuplicateAddsAreIdempotent(t *testing.T) {
	snap := foldTopology([]EventRecord{
		topoEvent(t, 0, topologyReasonRegistered, "node-a", "agent-a"),
		topoEvent(t, time.Minute, topologyReasonRegistered, "node-a", "agent-a"),
		topoEvent(t, 2*time.Minute, topologyReasonRegistered, "node-a", "agent-a"),
	})

	if got, want := nodeNames(&snap), []string{"node-a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("nodes = %v, want %v", got, want)
	}
	if got, want := agentIDs(&snap), []string{"agent-a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("agents = %v, want %v", got, want)
	}
	if snap.EventsFolded != 3 {
		t.Errorf("EventsFolded = %d, want 3: every row is folded even when it changes nothing", snap.EventsFolded)
	}
}

func TestFoldTopologySameTimestampFollowsRowOrder(t *testing.T) {
	// Two events inside one microsecond: (event_time, id) is the total order
	// the SQL yields, so the LAST row in the slice wins. Feeding the same pair
	// in both orders must therefore give opposite answers -- that is what
	// proves the fold is order-driven and not set-driven.
	add := topoEvent(t, 0, topologyReasonRegistered, "node-a", "agent-a")
	remove := topoEvent(t, 0, topologyReasonDeregistered, "node-a", "agent-a")

	if snap := foldTopology([]EventRecord{add, remove}); len(snap.Nodes) != 0 {
		t.Errorf("add-then-remove at one timestamp = %v, want no nodes", nodeNames(&snap))
	}
	if snap := foldTopology([]EventRecord{remove, add}); !reflect.DeepEqual(nodeNames(&snap), []string{"node-a"}) {
		t.Errorf("remove-then-add at one timestamp = %v, want [node-a]", nodeNames(&snap))
	}
}

func TestFoldTopologyEventNamingNobodyIsCountedNotGuessed(t *testing.T) {
	// This is TODAY'S PRODUCTION SHAPE: internal/controller/controller.go
	// publishes pb.TopologyChanged{Reason: reason} and never sets node_name or
	// agent_id, so every persisted event names nobody. The fold must not
	// invent a node -- it counts the change and leaves the set alone.
	snap := foldTopology([]EventRecord{
		topoEvent(t, 0, topologyReasonRegistered, "", ""),
		topoEvent(t, time.Minute, topologyReasonRegistered, "node-a", "agent-a"),
		topoEvent(t, 2*time.Minute, topologyReasonEvicted, "", ""),
	})

	if got, want := nodeNames(&snap), []string{"node-a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("nodes = %v, want %v", got, want)
	}
	if snap.EventsFolded != 3 {
		t.Errorf("EventsFolded = %d, want 3", snap.EventsFolded)
	}
	if snap.UnfoldableEvents != 2 {
		t.Errorf("UnfoldableEvents = %d, want 2: both anonymous events must be reported, not silently dropped",
			snap.UnfoldableEvents)
	}
}

func TestFoldTopologyUnknownReasonAndBrokenDetailsAreUnfoldable(t *testing.T) {
	broken := topoEvent(t, 2*time.Minute, topologyReasonRegistered, "node-x", "agent-x")
	broken.Details = json.RawMessage(`{"reason":`)

	snap := foldTopology([]EventRecord{
		topoEvent(t, 0, topologyReasonRegistered, "node-a", "agent-a"),
		topoEvent(t, time.Minute, "quantum_tunnelled", "node-a", "agent-a"),
		broken,
	})

	// An unknown reason cannot be folded in EITHER direction without guessing,
	// so membership is left exactly as it was; unparseable details likewise.
	if got, want := nodeNames(&snap), []string{"node-a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("nodes = %v, want %v", got, want)
	}
	if snap.UnfoldableEvents != 2 {
		t.Errorf("UnfoldableEvents = %d, want 2 (unknown reason + broken JSON)", snap.UnfoldableEvents)
	}
}

func TestFoldTopologyOutputIsSorted(t *testing.T) {
	// Map iteration order is random; a snapshot that reorders itself between
	// identical requests would make every client-side diff useless.
	snap := foldTopology([]EventRecord{
		topoEvent(t, 0, topologyReasonRegistered, "node-c", "agent-c"),
		topoEvent(t, time.Minute, topologyReasonRegistered, "node-a", "agent-a"),
		topoEvent(t, 2*time.Minute, topologyReasonRegistered, "node-b", "agent-b"),
	})

	if got, want := nodeNames(&snap), []string{"node-a", "node-b", "node-c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("nodes = %v, want them sorted by name %v", got, want)
	}
	if got, want := agentIDs(&snap), []string{"agent-a", "agent-b", "agent-c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("agents = %v, want them sorted by id %v", got, want)
	}
}

func TestFoldTopologyNodeOnlyAndAgentOnlyEvents(t *testing.T) {
	// The two halves of the identity are independent fields, so an event may
	// name one without the other. Each half folds on its own.
	snap := foldTopology([]EventRecord{
		topoEvent(t, 0, topologyReasonRegistered, "node-a", ""),
		topoEvent(t, time.Minute, topologyReasonRegistered, "", "agent-b"),
	})

	if got, want := nodeNames(&snap), []string{"node-a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("nodes = %v, want %v", got, want)
	}
	if got, want := agentIDs(&snap), []string{"agent-b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("agents = %v, want %v", got, want)
	}
	if snap.UnfoldableEvents != 0 {
		t.Errorf("UnfoldableEvents = %d, want 0: each event named at least one subject", snap.UnfoldableEvents)
	}
}

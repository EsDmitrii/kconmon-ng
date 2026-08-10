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

// topoEvent builds one topology_changed EventRecord in the PRE-M7 details shape: reason.
func topoEvent(t *testing.T, offset time.Duration, reason, node, agent string) EventRecord {
	t.Helper()
	return topoRecord(t, offset, reason, node, map[string]string{
		"reason": reason, "nodeName": node, "agentId": agent,
	})
}

// topoEventZoned builds the CURRENT details shape, the four keys
// events.topologyChangedDetails marshals (internal/console/events/
// live_event.go): reason, nodeName, agentId, zone.
func topoEventZoned(t *testing.T, offset time.Duration, reason, node, agent, zone string) EventRecord {
	t.Helper()
	return topoRecord(t, offset, reason, node, map[string]string{
		"reason": reason, "nodeName": node, "agentId": agent, "zone": zone,
	})
}

func topoRecord(t *testing.T, offset time.Duration, reason, node string, payload map[string]string) EventRecord {
	t.Helper()
	details, err := json.Marshal(payload)
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
	// These records are the PRE-M7 shape with no zone key, and podIP is not in the payload at any
	// version.
	for _, n := range snap.Nodes {
		if n.Zone != "" {
			t.Errorf("node %s zone = %q, want empty: these events carry no zone key", n.Name, n.Zone)
		}
		if !n.Ready {
			t.Errorf("node %s ready = false, want true: presence in the fold is the only readiness the events carry", n.Name)
		}
	}
	for _, a := range snap.Agents {
		if a.PodIP != "" || a.Zone != "" {
			t.Errorf("agent %s = %+v, want empty podIP and zone: podIP is never recorded, and these rows predate zone", a.ID, a)
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

// Pre-M7 rows have no zone key, so zone_updated could only prove membership.
// That history is still inside retention and must keep folding exactly this
// way -- empty zone, zero unfoldable, membership intact.
func TestFoldTopologyPreM7ZoneChangeKeepsMembershipButHasNoZoneToSet(t *testing.T) {
	snap := foldTopology([]EventRecord{
		topoEvent(t, 0, topologyReasonRegistered, "node-a", "agent-a"),
		topoEvent(t, time.Minute, topologyReasonZoneUpdated, "node-a", "agent-a"),
	})

	if got, want := nodeNames(&snap), []string{"node-a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("nodes = %v, want %v", got, want)
	}
	if snap.Nodes[0].Zone != "" {
		t.Errorf("zone = %q after a pre-M7 zone_updated, want empty: the new zone is not in the event", snap.Nodes[0].Zone)
	}
	if snap.UnfoldableEvents != 0 {
		t.Errorf("UnfoldableEvents = %d, want 0", snap.UnfoldableEvents)
	}
}

// The controller puts the zone in the event.
func TestFoldTopologyAttributedEventsCarryTheZone(t *testing.T) {
	snap := foldTopology([]EventRecord{
		topoEventZoned(t, 0, topologyReasonRegistered, "node-a", "agent-a", "zone-a"),
		topoEventZoned(t, time.Minute, topologyReasonRegistered, "node-b", "agent-b", "zone-b"),
	})

	if got, want := nodeNames(&snap), []string{"node-a", "node-b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("nodes = %v, want %v", got, want)
	}
	if snap.Nodes[0].Zone != "zone-a" || snap.Nodes[1].Zone != "zone-b" {
		t.Errorf("node zones = %q/%q, want zone-a/zone-b", snap.Nodes[0].Zone, snap.Nodes[1].Zone)
	}
	if snap.Agents[0].Zone != "zone-a" || snap.Agents[1].Zone != "zone-b" {
		t.Errorf("agent zones = %q/%q, want zone-a/zone-b", snap.Agents[0].Zone, snap.Agents[1].Zone)
	}
	if snap.UnfoldableEvents != 0 {
		t.Errorf("UnfoldableEvents = %d, want 0", snap.UnfoldableEvents)
	}
}

// zone_updated's entire subject is the new zone: the fold must MOVE the node,
// not merely confirm it is still there. This is the assertion that makes a
// zone relabel visible in history at all.
func TestFoldTopologyZoneUpdateMovesTheNodeToTheNewZone(t *testing.T) {
	snap := foldTopology([]EventRecord{
		topoEventZoned(t, 0, topologyReasonRegistered, "node-a", "agent-a", "zone-a"),
		topoEventZoned(t, time.Minute, topologyReasonZoneUpdated, "node-a", "agent-a", "zone-b"),
	})

	if got, want := nodeNames(&snap), []string{"node-a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("nodes = %v, want %v", got, want)
	}
	if snap.Nodes[0].Zone != "zone-b" {
		t.Errorf("node zone = %q after zone_updated, want the NEW zone-b", snap.Nodes[0].Zone)
	}
	if snap.Agents[0].Zone != "zone-b" {
		t.Errorf("agent zone = %q after zone_updated, want zone-b", snap.Agents[0].Zone)
	}
}

// A later event that omits the zone must not ERASE a zone the fold already knows.
func TestFoldTopologyLaterZonelessEventDoesNotEraseAKnownZone(t *testing.T) {
	snap := foldTopology([]EventRecord{
		topoEventZoned(t, 0, topologyReasonRegistered, "node-a", "agent-a", "zone-a"),
		topoEvent(t, time.Minute, topologyReasonRegistered, "node-a", "agent-a"),
	})

	if snap.Nodes[0].Zone != "zone-a" {
		t.Errorf("node zone = %q, want zone-a kept: a zoneless event says nothing about the zone", snap.Nodes[0].Zone)
	}
	if snap.Agents[0].Zone != "zone-a" {
		t.Errorf("agent zone = %q, want zone-a kept", snap.Agents[0].Zone)
	}
}

// The Time Machine's actual question: what did the cluster look like AT t?
func TestFoldTopologyPrefixesGiveTheMembershipAtEachInstant(t *testing.T) {
	timeline := []EventRecord{
		topoEventZoned(t, 0, topologyReasonRegistered, "node-a", "agent-a", "zone-a"),
		topoEventZoned(t, time.Hour, topologyReasonRegistered, "node-b", "agent-b", "zone-b"),
		topoEventZoned(t, 2*time.Hour, topologyReasonEvicted, "node-b", "agent-b", "zone-b"),
	}

	for _, tc := range []struct {
		name  string
		upTo  int // rows with event_time <= the instant asked about
		nodes []string
	}{
		{"before node-b joined", 1, []string{"node-a"}},
		{"between the join and the eviction", 2, []string{"node-a", "node-b"}},
		{"after the eviction", 3, []string{"node-a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := foldTopology(timeline[:tc.upTo])

			if got := nodeNames(&snap); !reflect.DeepEqual(got, tc.nodes) {
				t.Errorf("nodes = %v, want %v", got, tc.nodes)
			}
			if snap.UnfoldableEvents != 0 {
				t.Errorf("UnfoldableEvents = %d, want 0: every row names its subject", snap.UnfoldableEvents)
			}
			if snap.Nodes[0].Zone != "zone-a" {
				t.Errorf("node-a zone = %q, want zone-a at every instant", snap.Nodes[0].Zone)
			}
		})
	}
}

// A node that leaves and comes back must not keep its old zone: re-registration
// is a fresh statement of placement, and the removal cleared what came before.
func TestFoldTopologyRejoinAfterRemovalStartsFromTheNewEvent(t *testing.T) {
	snap := foldTopology([]EventRecord{
		topoEventZoned(t, 0, topologyReasonRegistered, "node-a", "agent-a", "zone-a"),
		topoEventZoned(t, time.Minute, topologyReasonEvicted, "node-a", "agent-a", "zone-a"),
		topoEvent(t, 2*time.Minute, topologyReasonRegistered, "node-a", "agent-a"),
	})

	if got, want := nodeNames(&snap), []string{"node-a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("nodes = %v, want %v", got, want)
	}
	if snap.Nodes[0].Zone != "" {
		t.Errorf("node zone = %q after evict-then-zoneless-rejoin, want empty: the old zone was removed with the node",
			snap.Nodes[0].Zone)
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
	// Feeding the same pair in both orders must therefore give opposite answers.
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
	// This is TODAY'S PRODUCTION SHAPE: internal/controller/controller.go publishes
	// pb.TopologyChanged{Reason: reason} and never sets node_name or agent_id.
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

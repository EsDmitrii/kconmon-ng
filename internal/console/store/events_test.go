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

// topoEventLabelled builds the 2.4.0 details shape: the four keys plus the agent's own labels map.
func topoEventLabelled(t *testing.T, offset time.Duration, reason, node, agent, zone string, labels map[string]string) EventRecord {
	t.Helper()
	details, err := json.Marshal(map[string]any{
		"reason": reason, "nodeName": node, "agentId": agent, "zone": zone, "labels": labels,
	})
	if err != nil {
		t.Fatalf("marshal details: %v", err)
	}
	return EventRecord{
		EventTime: base.Add(offset),
		Type:      eventTypeTopologyChanged,
		Severity:  "info",
		Scope:     node,
		Summary:   fmt.Sprintf("topology changed: %s", reason),
		Details:   details,
	}
}

var externalLabels = map[string]string{"kconmon-ng.io/external": "true"}

// TestFoldTopologyKeepsTheLastLabelsPerAgent is the Time Machine half of the external badge: the
// fold must agree with the live snapshot about which agents are external.
func TestFoldTopologyKeepsTheLastLabelsPerAgent(t *testing.T) {
	snap := foldTopology([]EventRecord{
		topoEventLabelled(t, 0, topologyReasonRegistered, "edge-01", "edge-01-agent", "office", externalLabels),
		topoEventZoned(t, time.Minute, topologyReasonRegistered, "node-a", "agent-a", "zone-a"),
	})

	if len(snap.Agents) != 2 {
		t.Fatalf("agents = %v, want both", agentIDs(&snap))
	}
	// Sorted by ID: agent-a first, edge-01-agent second.
	if snap.Agents[0].Labels != nil {
		t.Errorf("agent-a labels = %v, want nil: its event carried no labels key", snap.Agents[0].Labels)
	}
	if !reflect.DeepEqual(snap.Agents[1].Labels, externalLabels) {
		t.Errorf("edge-01-agent labels = %v, want %v", snap.Agents[1].Labels, externalLabels)
	}
}

// A later event about the same agent that carries no labels (a zone_updated from a controller that
// attributes but does not label, or a mixed-version fleet) must not erase what an earlier event
// stated, exactly as the zone rule works.
func TestFoldTopologyLaterUnlabelledEventDoesNotEraseKnownLabels(t *testing.T) {
	snap := foldTopology([]EventRecord{
		topoEventLabelled(t, 0, topologyReasonRegistered, "edge-01", "edge-01-agent", "office", externalLabels),
		topoEventZoned(t, time.Minute, topologyReasonZoneUpdated, "edge-01", "edge-01-agent", "office-2"),
	})

	if len(snap.Agents) != 1 {
		t.Fatalf("agents = %v, want one", agentIDs(&snap))
	}
	if !reflect.DeepEqual(snap.Agents[0].Labels, externalLabels) {
		t.Errorf("labels = %v, want the registration's %v kept through the unlabelled zone_updated",
			snap.Agents[0].Labels, externalLabels)
	}
	if snap.Agents[0].Zone != "office-2" {
		t.Errorf("zone = %q, want office-2: the zone rule is unchanged", snap.Agents[0].Zone)
	}
}

// A newer labelled event replaces the older labels wholesale: the map is the agent's own current
// set, not an accumulation.
func TestFoldTopologyNewerLabelsReplaceOlderOnes(t *testing.T) {
	snap := foldTopology([]EventRecord{
		topoEventLabelled(t, 0, topologyReasonRegistered, "n", "a", "z", map[string]string{"role": "old"}),
		topoEventLabelled(t, time.Minute, topologyReasonZoneUpdated, "n", "a", "z", map[string]string{"role": "new"}),
	})

	if got := snap.Agents[0].Labels; !reflect.DeepEqual(got, map[string]string{"role": "new"}) {
		t.Errorf("labels = %v, want the newer event's map", got)
	}
}

// TestFoldTopologyOldShapeEventFoldsToNilLabels pins the compatibility case from the release skew
// table: history a pre-2.4.0 controller wrote has no labels key at all, and the fold must report
// nil rather than an empty map, so a consumer can tell "unknown" from "no labels".
func TestFoldTopologyOldShapeEventFoldsToNilLabels(t *testing.T) {
	snap := foldTopology([]EventRecord{
		topoEvent(t, 0, topologyReasonRegistered, "node-a", "agent-a"),
		topoEventZoned(t, time.Minute, topologyReasonRegistered, "node-b", "agent-b", "zone-b"),
	})

	for _, a := range snap.Agents {
		if a.Labels != nil {
			t.Errorf("agent %s labels = %v, want nil for an event without a labels key", a.ID, a.Labels)
		}
	}
}

// Leaving and rejoining starts the agent's labels from the rejoin event, like every other field.
func TestFoldTopologyRejoinWithoutLabelsStartsUnlabelled(t *testing.T) {
	snap := foldTopology([]EventRecord{
		topoEventLabelled(t, 0, topologyReasonRegistered, "edge-01", "edge-01-agent", "office", externalLabels),
		topoEventZoned(t, time.Minute, topologyReasonDeregistered, "edge-01", "edge-01-agent", "office"),
		topoEventZoned(t, 2*time.Minute, topologyReasonRegistered, "edge-01", "edge-01-agent", "office"),
	})

	if len(snap.Agents) != 1 || snap.Agents[0].Labels != nil {
		t.Errorf("agents = %+v, want one rejoined agent with nil labels", snap.Agents)
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

// TestFoldTopologyBareHostIsAnAgentNotANode: the live snapshot never lists a Kubernetes node for an
// external agent (there is none), so the fold must not invent one either -- with Ready true, that
// node read as a READY k8s node in the Time Machine while the live card said "—".
func TestFoldTopologyBareHostIsAnAgentNotANode(t *testing.T) {
	snap := foldTopology([]EventRecord{
		topoEventZoned(t, 0, topologyReasonRegistered, "node-a", "agent-a", "zone-a"),
		topoEventLabelled(t, time.Minute, topologyReasonRegistered, "edge-01", "edge-01-agent", "office", externalLabels),
	})

	if got, want := nodeNames(&snap), []string{"node-a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("nodes = %v, want %v: the bare host has no Kubernetes node to list", got, want)
	}
	if got, want := agentIDs(&snap), []string{"agent-a", "edge-01-agent"}; !reflect.DeepEqual(got, want) {
		t.Errorf("agents = %v, want %v: the host is still served, through Agents", got, want)
	}
	if a := snap.Agents[1]; a.NodeName != "edge-01" || a.Zone != "office" || !reflect.DeepEqual(a.Labels, externalLabels) {
		t.Errorf("edge-01-agent = %+v, want nodeName edge-01, zone office and the external label", a)
	}
}

// The labels rule carries an earlier registration's external label through a later unlabelled event,
// so the host stays out of Nodes whichever event was folded last.
func TestFoldTopologyBareHostStaysOutOfNodesThroughAnUnlabelledZoneUpdate(t *testing.T) {
	snap := foldTopology([]EventRecord{
		topoEventLabelled(t, 0, topologyReasonRegistered, "edge-01", "edge-01-agent", "office", externalLabels),
		topoEventZoned(t, time.Minute, topologyReasonZoneUpdated, "edge-01", "edge-01-agent", "office-2"),
	})

	if len(snap.Nodes) != 0 {
		t.Errorf("nodes = %v, want none: the zone_updated without labels does not turn the host into a node", nodeNames(&snap))
	}
	if len(snap.Agents) != 1 || snap.Agents[0].Zone != "office-2" {
		t.Errorf("agents = %+v, want the one host in office-2", snap.Agents)
	}
}

// Only the exact label value marks a bare host; a labelled in-cluster agent, or one whose history
// predates labels entirely, keeps its node -- unknown is not external.
func TestFoldTopologyOnlyTheExternalLabelHidesANode(t *testing.T) {
	snap := foldTopology([]EventRecord{
		topoEvent(t, 0, topologyReasonRegistered, "node-old", "agent-old"),
		topoEventLabelled(t, time.Minute, topologyReasonRegistered, "node-b", "agent-b", "zone-b", map[string]string{"role": "edge"}),
		topoEventLabelled(t, 2*time.Minute, topologyReasonRegistered, "node-c", "agent-c", "zone-c",
			map[string]string{"kconmon-ng.io/external": "false"}),
	})

	if got, want := nodeNames(&snap), []string{"node-b", "node-c", "node-old"}; !reflect.DeepEqual(got, want) {
		t.Errorf("nodes = %v, want %v", got, want)
	}
}

// A bare host that leaves takes its agent with it and leaves no node behind either.
func TestFoldTopologyBareHostDeregisterLeavesNothing(t *testing.T) {
	snap := foldTopology([]EventRecord{
		topoEventLabelled(t, 0, topologyReasonRegistered, "edge-01", "edge-01-agent", "office", externalLabels),
		topoEventZoned(t, time.Minute, topologyReasonDeregistered, "edge-01", "edge-01-agent", "office"),
	})

	if len(snap.Nodes) != 0 || len(snap.Agents) != 0 {
		t.Errorf("nodes/agents = %v/%v, want both empty", nodeNames(&snap), agentIDs(&snap))
	}
}

// baselineRecord builds one topology_baseline EventRecord: the controller's whole topology as the
// ingester read it when its stream came up, in the node/agent JSON shape GET /api/v1/topology uses.
func baselineRecord(t *testing.T, offset time.Duration, nodes []map[string]any, agents []map[string]any) EventRecord {
	t.Helper()
	details, err := json.Marshal(map[string]any{"nodes": nodes, "agents": agents})
	if err != nil {
		t.Fatalf("marshal baseline: %v", err)
	}
	return EventRecord{
		EventTime: base.Add(offset),
		Type:      eventTypeTopologyBaseline,
		Severity:  "info",
		Scope:     "cluster",
		Summary:   "topology baseline",
		Details:   details,
	}
}

// The stand case: the console started recording while the fleet was already up, so the only
// topology_changed rows it holds are an evict/re-register of SOME agents. The subjects that never
// changed after recording started (worker2, worker9, a node with no agent, the bare host) exist only
// in the baseline, and the fold must keep them.
func TestFoldTopologyBaselineKeepsSubjectsThatNeverChangedAfterRecordingStarted(t *testing.T) {
	snap := foldTopology([]EventRecord{
		baselineRecord(t, 0,
			[]map[string]any{
				{"name": "cp", "zone": "zone-a", "ready": true},
				{"name": "worker2", "zone": "zone-a", "ready": true},
				{"name": "worker9", "zone": "zone-d", "ready": true},
				{"name": "worker10", "zone": "zone-d", "ready": false},
			},
			[]map[string]any{
				{"id": "cp-agent", "nodeName": "cp", "zone": "zone-a"},
				{"id": "worker2-agent", "nodeName": "worker2", "zone": "zone-a"},
				{"id": "worker9-agent", "nodeName": "worker9", "zone": "zone-d"},
				{"id": "edge-agent", "nodeName": "edge-host-01", "zone": "office", "labels": externalLabels},
			}),
		topoEventZoned(t, time.Minute, topologyReasonEvicted, "cp", "cp-agent", "zone-a"),
		topoEventZoned(t, 2*time.Minute, topologyReasonRegistered, "cp", "cp-agent", "zone-a"),
	})

	if got, want := nodeNames(&snap), []string{"cp", "worker10", "worker2", "worker9"}; !reflect.DeepEqual(got, want) {
		t.Errorf("nodes = %v, want %v: the baseline's nodes survive, the bare host is not one", got, want)
	}
	if got, want := agentIDs(&snap), []string{"cp-agent", "edge-agent", "worker2-agent", "worker9-agent"}; !reflect.DeepEqual(got, want) {
		t.Errorf("agents = %v, want %v", got, want)
	}
	for _, a := range snap.Agents {
		if a.ID == "edge-agent" && (a.NodeName != "edge-host-01" || !reflect.DeepEqual(a.Labels, externalLabels)) {
			t.Errorf("edge-agent = %+v, want nodeName edge-host-01 with the external label, so the badge renders", a)
		}
	}
	for _, n := range snap.Nodes {
		if n.Name == "worker10" && n.Ready {
			t.Error("worker10 ready = true, want the baseline's false: no event ever touched it")
		}
	}
	if snap.EventsFolded != 3 || snap.UnfoldableEvents != 0 {
		t.Errorf("EventsFolded/UnfoldableEvents = %d/%d, want 3/0", snap.EventsFolded, snap.UnfoldableEvents)
	}
}

// A baseline is the whole truth at its instant: whatever the fold held before it is replaced, not
// merged, and events after it apply on top.
func TestFoldTopologyBaselineReplacesEarlierState(t *testing.T) {
	snap := foldTopology([]EventRecord{
		topoEventZoned(t, 0, topologyReasonRegistered, "gone", "gone-agent", "zone-a"),
		baselineRecord(t, time.Minute,
			[]map[string]any{{"name": "node-a", "zone": "zone-a", "ready": true}},
			[]map[string]any{{"id": "agent-a", "nodeName": "node-a", "zone": "zone-a"}}),
		topoEventZoned(t, 2*time.Minute, topologyReasonRegistered, "node-b", "agent-b", "zone-b"),
	})

	if got, want := nodeNames(&snap), []string{"node-a", "node-b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("nodes = %v, want %v", got, want)
	}
	if got, want := agentIDs(&snap), []string{"agent-a", "agent-b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("agents = %v, want %v", got, want)
	}
}

// A broken baseline is counted and skipped; it must not wipe the state folded so far.
func TestFoldTopologyBrokenBaselineIsUnfoldable(t *testing.T) {
	bad := baselineRecord(t, time.Minute, nil, nil)
	bad.Details = []byte("{not json")
	snap := foldTopology([]EventRecord{
		topoEventZoned(t, 0, topologyReasonRegistered, "node-a", "agent-a", "zone-a"),
		bad,
	})

	if got, want := nodeNames(&snap), []string{"node-a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("nodes = %v, want %v", got, want)
	}
	if snap.UnfoldableEvents != 1 {
		t.Errorf("UnfoldableEvents = %d, want 1", snap.UnfoldableEvents)
	}
}

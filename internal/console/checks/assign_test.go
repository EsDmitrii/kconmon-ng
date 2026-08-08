package checks_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/EsDmitrii/kconmon-ng/internal/console/checks"
)

// fiveNodesTwoZones is the topology most cases below plan against: node names
// deliberately NOT in sorted order, so a test that passes only because the
// input happened to be sorted cannot.
func fiveNodesTwoZones() []checks.AgentRef {
	return []checks.AgentRef{
		{NodeName: "node-e", Zone: "zone-b"},
		{NodeName: "node-a", Zone: "zone-a"},
		{NodeName: "node-d", Zone: "zone-b"},
		{NodeName: "node-b", Zone: "zone-a"},
		{NodeName: "node-c", Zone: "zone-a"},
	}
}

func tcpDef(id, name string, sel checks.Selection) checks.Definition {
	return checks.Definition{
		ID:                 id,
		Name:               name,
		Selection:          sel,
		CheckType:          "tcp",
		DestinationAddress: "10.0.0.1:443",
		Enabled:            true,
	}
}

// agentIDs returns the map's keys sorted, so a test can assert on WHICH agents
// were assigned without depending on Go's random map iteration order.
func agentIDs(t *testing.T, got map[string]checks.Assignment) []string {
	t.Helper()
	ids := make([]string, 0, len(got))
	for id := range got {
		ids = append(ids, id)
	}
	// insertion sort: tiny inputs, and it keeps the helper dependency-free
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && ids[j] < ids[j-1]; j-- {
			ids[j], ids[j-1] = ids[j-1], ids[j]
		}
	}
	return ids
}

func equalIDs(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("assigned agents = %v, want %v", got, want)
	}
}

func TestAssignAgentsAllSelectsEveryAgent(t *testing.T) {
	got, series, err := checks.AssignAgents(
		[]checks.Definition{tcpDef("def-1", "edge-tcp", checks.SelectAll)}, fiveNodesTwoZones())
	if err != nil {
		t.Fatalf("AssignAgents: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("assignments = %d, want 5", len(got))
	}
	equalIDs(t, agentIDs(t, got), []string{"node-a", "node-b", "node-c", "node-d", "node-e"})
	if series != 5 {
		t.Fatalf("series = %d, want 5", series)
	}
	want := checks.Assignment{AgentID: "node-c", Specs: []checks.AssignedSpec{
		{DefinitionID: "def-1", Name: "edge-tcp", CheckType: "tcp", DestinationAddress: "10.0.0.1:443"},
	}}
	if !reflect.DeepEqual(got["node-c"], want) {
		t.Fatalf("assignment[node-c] = %+v, want %+v", got["node-c"], want)
	}
}

// per-zone resolves to the SAME agent set as all -- it groups the agents by
// zone for metric purposes downstream, it does not shrink the set (Plan
// Decision 11, mirrored by httpapi's projectedAgents).
func TestAssignAgentsPerZoneYieldsEveryAgent(t *testing.T) {
	all, allSeries, err := checks.AssignAgents(
		[]checks.Definition{tcpDef("def-1", "edge-tcp", checks.SelectAll)}, fiveNodesTwoZones())
	if err != nil {
		t.Fatalf("AssignAgents(all): %v", err)
	}
	perZone, perZoneSeries, err := checks.AssignAgents(
		[]checks.Definition{tcpDef("def-1", "edge-tcp", checks.SelectPerZone)}, fiveNodesTwoZones())
	if err != nil {
		t.Fatalf("AssignAgents(per-zone): %v", err)
	}
	equalIDs(t, agentIDs(t, perZone), agentIDs(t, all))
	if perZoneSeries != allSeries {
		t.Fatalf("per-zone series = %d, want %d (same as all)", perZoneSeries, allSeries)
	}
}

// one-per-zone picks the FIRST agent by sorted node name in each zone, and
// zoneless agents (Zone == "") form ONE bucket of their own -- the same
// convention httpapi's projectedAgents counts with, so the planner and the
// projection guard can never disagree about the number of series.
func TestAssignAgentsOnePerZonePicksSortedFirst(t *testing.T) {
	agents := []checks.AgentRef{
		{NodeName: "node-e", Zone: "zone-b"},
		{NodeName: "node-z", Zone: ""},
		{NodeName: "node-c", Zone: "zone-a"},
		{NodeName: "node-d", Zone: "zone-b"},
		{NodeName: "node-a", Zone: "zone-a"},
		{NodeName: "node-y", Zone: ""},
	}
	got, series, err := checks.AssignAgents(
		[]checks.Definition{tcpDef("def-1", "edge-tcp", checks.SelectOnePerZone)}, agents)
	if err != nil {
		t.Fatalf("AssignAgents: %v", err)
	}
	// zone-a -> node-a, zone-b -> node-d, "" -> node-y
	equalIDs(t, agentIDs(t, got), []string{"node-a", "node-d", "node-y"})
	if series != 3 {
		t.Fatalf("series = %d, want 3 (one per zone, zoneless bucket included)", series)
	}
}

// Determinism is a correctness property, not a nicety: a non-deterministic
// pick would move the probe on every reconcile tick and churn half-populated
// Prometheus series (plan line 424).
func TestAssignAgentsIsDeterministic(t *testing.T) {
	defs := []checks.Definition{
		tcpDef("def-2", "b", checks.SelectOnePerZone),
		tcpDef("def-1", "a", checks.SelectAll),
		tcpDef("def-3", "c", checks.SelectPerZone),
	}
	first, firstSeries, err := checks.AssignAgents(defs, fiveNodesTwoZones())
	if err != nil {
		t.Fatalf("AssignAgents: %v", err)
	}
	second, secondSeries, err := checks.AssignAgents(defs, fiveNodesTwoZones())
	if err != nil {
		t.Fatalf("AssignAgents: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("second run = %+v, want deep-equal to %+v", second, first)
	}
	if firstSeries != secondSeries {
		t.Fatalf("series %d then %d, want equal", firstSeries, secondSeries)
	}
}

// A zone nobody runs in simply does not appear: zones are derived from the
// agent list, never from a separate zone roster, so there is no phantom entry
// to skip and nothing to error about.
func TestAssignAgentsSkipsZonesWithNoAgents(t *testing.T) {
	agents := []checks.AgentRef{
		{NodeName: "node-b", Zone: "zone-a"},
		{NodeName: "node-a", Zone: "zone-a"},
		{NodeName: "node-c", Zone: "zone-b"},
	}
	got, series, err := checks.AssignAgents(
		[]checks.Definition{tcpDef("def-1", "edge-tcp", checks.SelectOnePerZone)}, agents)
	if err != nil {
		t.Fatalf("AssignAgents: %v", err)
	}
	equalIDs(t, agentIDs(t, got), []string{"node-a", "node-c"})
	if series != 2 {
		t.Fatalf("series = %d, want 2", series)
	}
}

func TestAssignAgentsNoAgentsWithEnabledDefinition(t *testing.T) {
	got, series, err := checks.AssignAgents(
		[]checks.Definition{tcpDef("def-1", "edge-tcp", checks.SelectOnePerZone)}, nil)
	if !errors.Is(err, checks.ErrNoAgents) {
		t.Fatalf("err = %v, want ErrNoAgents", err)
	}
	if got != nil || series != 0 {
		t.Fatalf("got = %+v, series = %d, want nil, 0", got, series)
	}
}

// Only disabled definitions is not an error even with zero agents: nothing
// wanted an agent, so nothing is missing.
func TestAssignAgentsNoAgentsWithOnlyDisabledDefinitions(t *testing.T) {
	def := tcpDef("def-1", "edge-tcp", checks.SelectAll)
	def.Enabled = false
	got, series, err := checks.AssignAgents([]checks.Definition{def}, nil)
	if err != nil {
		t.Fatalf("AssignAgents: %v", err)
	}
	if len(got) != 0 || series != 0 {
		t.Fatalf("got = %+v, series = %d, want empty, 0", got, series)
	}
}

func TestAssignAgentsDisabledDefinitionContributesNothing(t *testing.T) {
	disabled := tcpDef("def-2", "draft", checks.SelectAll)
	disabled.Enabled = false
	got, series, err := checks.AssignAgents([]checks.Definition{
		tcpDef("def-1", "edge-tcp", checks.SelectOnePerZone),
		disabled,
	}, fiveNodesTwoZones())
	if err != nil {
		t.Fatalf("AssignAgents: %v", err)
	}
	equalIDs(t, agentIDs(t, got), []string{"node-a", "node-d"})
	for id, a := range got {
		if len(a.Specs) != 1 || a.Specs[0].DefinitionID != "def-1" {
			t.Fatalf("assignment[%s].Specs = %+v, want only def-1", id, a.Specs)
		}
	}
	if series != 2 {
		t.Fatalf("series = %d, want 2 (the disabled def contributes none)", series)
	}
}

// Projection: series == assigned agents x protocolsPerDefinition (1), summed
// across enabled definitions -- the same arithmetic httpapi's projection
// endpoint reports, computed here from the resolved assignment instead of an
// estimate.
func TestAssignAgentsProjectedSeries(t *testing.T) {
	tests := []struct {
		name       string
		defs       []checks.Definition
		wantSeries int
	}{
		{"single all", []checks.Definition{tcpDef("d1", "a", checks.SelectAll)}, 5},
		{"single per-zone", []checks.Definition{tcpDef("d1", "a", checks.SelectPerZone)}, 5},
		{"single one-per-zone", []checks.Definition{tcpDef("d1", "a", checks.SelectOnePerZone)}, 2},
		{"all plus one-per-zone", []checks.Definition{
			tcpDef("d1", "a", checks.SelectAll),
			tcpDef("d2", "b", checks.SelectOnePerZone),
		}, 7},
		{"no definitions", nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, series, err := checks.AssignAgents(tt.defs, fiveNodesTwoZones())
			if err != nil {
				t.Fatalf("AssignAgents: %v", err)
			}
			if series != tt.wantSeries {
				t.Fatalf("series = %d, want %d", series, tt.wantSeries)
			}
		})
	}
}

func TestAssignAgentsUnknownSelection(t *testing.T) {
	tests := []struct {
		name      string
		selection checks.Selection
	}{
		{"garbage", "everything"},
		{"empty", ""},
		{"wrong case", "One-Per-Zone"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := checks.AssignAgents(
				[]checks.Definition{tcpDef("def-1", "edge-tcp", tt.selection)}, fiveNodesTwoZones())
			if !errors.Is(err, checks.ErrUnknownSelection) {
				t.Fatalf("err = %v, want ErrUnknownSelection", err)
			}
		})
	}
}

// A malformed selection is rejected even on a DISABLED definition: it is a
// validation failure of the stored row, and letting it sit silently until an
// operator flips Enabled turns a config error into a runtime surprise.
func TestAssignAgentsUnknownSelectionOnDisabledDefinition(t *testing.T) {
	def := tcpDef("def-1", "edge-tcp", "everything")
	def.Enabled = false
	if _, _, err := checks.AssignAgents([]checks.Definition{def}, fiveNodesTwoZones()); !errors.Is(err, checks.ErrUnknownSelection) {
		t.Fatalf("err = %v, want ErrUnknownSelection", err)
	}
}

// Several definitions with different selections compose onto one agent, and
// that agent's Specs come back sorted by DefinitionID regardless of the order
// the definitions arrived in.
func TestAssignAgentsMixedSelectionsCompose(t *testing.T) {
	defs := []checks.Definition{
		tcpDef("def-3", "c", checks.SelectPerZone),
		tcpDef("def-1", "a", checks.SelectOnePerZone),
		tcpDef("def-2", "b", checks.SelectAll),
	}
	got, series, err := checks.AssignAgents(defs, fiveNodesTwoZones())
	if err != nil {
		t.Fatalf("AssignAgents: %v", err)
	}
	// node-a is zone-a's sorted-first, so it carries all three defs.
	wantA := []checks.AssignedSpec{
		{DefinitionID: "def-1", Name: "a", CheckType: "tcp", DestinationAddress: "10.0.0.1:443"},
		{DefinitionID: "def-2", Name: "b", CheckType: "tcp", DestinationAddress: "10.0.0.1:443"},
		{DefinitionID: "def-3", Name: "c", CheckType: "tcp", DestinationAddress: "10.0.0.1:443"},
	}
	if !reflect.DeepEqual(got["node-a"].Specs, wantA) {
		t.Fatalf("assignment[node-a].Specs = %+v, want %+v", got["node-a"].Specs, wantA)
	}
	// node-b is not any zone's first, so it carries only the two full-fleet defs.
	wantB := []string{"def-2", "def-3"}
	ids := make([]string, 0, len(got["node-b"].Specs))
	for _, s := range got["node-b"].Specs {
		ids = append(ids, s.DefinitionID)
	}
	equalIDs(t, ids, wantB)
	if series != 2+5+5 {
		t.Fatalf("series = %d, want 12", series)
	}
}

// A duplicated node name in the topology snapshot must not double-count: the
// agent is one agent, gets one copy of the spec, and contributes one series.
func TestAssignAgentsDuplicateAgentRefs(t *testing.T) {
	agents := []checks.AgentRef{
		{NodeName: "node-a", Zone: "zone-a"},
		{NodeName: "node-a", Zone: "zone-a"},
		{NodeName: "node-b", Zone: "zone-a"},
	}
	got, series, err := checks.AssignAgents(
		[]checks.Definition{tcpDef("def-1", "edge-tcp", checks.SelectAll)}, agents)
	if err != nil {
		t.Fatalf("AssignAgents: %v", err)
	}
	equalIDs(t, agentIDs(t, got), []string{"node-a", "node-b"})
	if len(got["node-a"].Specs) != 1 {
		t.Fatalf("assignment[node-a].Specs = %+v, want exactly one", got["node-a"].Specs)
	}
	if series != 2 {
		t.Fatalf("series = %d, want 2", series)
	}
}

// Agents whose NodeName is empty carry no identity to address a push to, so
// they are skipped rather than collapsed into one bogus "" assignment.
func TestAssignAgentsSkipsUnnamedAgents(t *testing.T) {
	agents := []checks.AgentRef{
		{NodeName: "", Zone: "zone-a"},
		{NodeName: "node-a", Zone: "zone-a"},
	}
	got, series, err := checks.AssignAgents(
		[]checks.Definition{tcpDef("def-1", "edge-tcp", checks.SelectAll)}, agents)
	if err != nil {
		t.Fatalf("AssignAgents: %v", err)
	}
	equalIDs(t, agentIDs(t, got), []string{"node-a"})
	if series != 1 {
		t.Fatalf("series = %d, want 1", series)
	}
}

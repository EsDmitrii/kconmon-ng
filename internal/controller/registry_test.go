package controller

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

// TestRegistryRetainsAgentCapabilities pins the round-trip the external destination gate depends.
func TestRegistryRetainsAgentCapabilities(t *testing.T) {
	srv, reg := newTestGRPCServer()

	resp, err := srv.Register(context.Background(), &pb.RegisterRequest{
		Agent: &pb.AgentMeta{
			Id: "agent-cap", NodeName: "node-cap", PodIp: "10.0.0.9",
			Capabilities: []string{capabilityExternalChecks},
		},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got := resp.GetAgent().GetCapabilities(); !slices.Equal(got, []string{capabilityExternalChecks}) {
		t.Errorf("RegisterResponse.agent.capabilities = %v, want [%s]", got, capabilityExternalChecks)
	}

	info, ok := reg.GetByNodeName("node-cap")
	if !ok {
		t.Fatal("agent not found by node name after Register")
	}
	if !slices.Equal(info.Capabilities, []string{capabilityExternalChecks}) {
		t.Errorf("AgentInfo.Capabilities = %v, want [%s]", info.Capabilities, capabilityExternalChecks)
	}
	if got := agentInfoToProto(info).GetCapabilities(); !slices.Equal(got, []string{capabilityExternalChecks}) {
		t.Errorf("agentInfoToProto capabilities = %v, want [%s]", got, capabilityExternalChecks)
	}
}

// A pre-M4 agent registers without the field; the registry must surface an
// empty capability set rather than inventing one.
func TestRegistryAgentWithoutCapabilities(t *testing.T) {
	srv, reg := newTestGRPCServer()

	if _, err := srv.Register(context.Background(), &pb.RegisterRequest{
		Agent: &pb.AgentMeta{Id: "agent-old", NodeName: "node-old"},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	info, ok := reg.GetByNodeName("node-old")
	if !ok {
		t.Fatal("agent not found by node name after Register")
	}
	if len(info.Capabilities) != 0 {
		t.Errorf("expected no capabilities for a pre-M4 agent, got %v", info.Capabilities)
	}
}

func TestRegistryRegisterAndGetPeers(t *testing.T) {
	r := NewRegistry(30 * time.Second)

	agent1 := model.AgentInfo{ID: "agent-1", NodeName: "node-1", PodIP: "10.0.0.1", Zone: "zone-a"}
	agent2 := model.AgentInfo{ID: "agent-2", NodeName: "node-2", PodIP: "10.0.0.2", Zone: "zone-b"}

	r.Register(agent1)
	r.Register(agent2)

	if r.Count() != 2 {
		t.Errorf("expected 2 agents, got %d", r.Count())
	}

	peers := r.GetPeers("agent-1")
	if len(peers) != 1 {
		t.Fatalf("expected 1 peer for agent-1, got %d", len(peers))
	}
	if peers[0].ID != "agent-2" {
		t.Errorf("expected peer agent-2, got %s", peers[0].ID)
	}
}

func TestRegistryDeregister(t *testing.T) {
	r := NewRegistry(30 * time.Second)

	r.Register(model.AgentInfo{ID: "agent-1", NodeName: "node-1"})
	r.Register(model.AgentInfo{ID: "agent-2", NodeName: "node-2"})

	r.Deregister("agent-1")

	if r.Count() != 1 {
		t.Errorf("expected 1 agent after deregister, got %d", r.Count())
	}

	peers := r.GetPeers("agent-2")
	if len(peers) != 0 {
		t.Errorf("expected 0 peers for agent-2, got %d", len(peers))
	}
}

func TestRegistryHeartbeat(t *testing.T) {
	r := NewRegistry(30 * time.Second)

	r.Register(model.AgentInfo{ID: "agent-1", NodeName: "node-1"})

	if !r.Heartbeat("agent-1") {
		t.Error("heartbeat should return true for registered agent")
	}
	if r.Heartbeat("agent-unknown") {
		t.Error("heartbeat should return false for unknown agent")
	}
}

func TestRegistryEvictStale(t *testing.T) {
	r := NewRegistry(100 * time.Millisecond)

	r.Register(model.AgentInfo{ID: "agent-1", NodeName: "node-1"})
	r.Register(model.AgentInfo{ID: "agent-2", NodeName: "node-2"})

	time.Sleep(150 * time.Millisecond)

	r.Heartbeat("agent-1")

	time.Sleep(60 * time.Millisecond)

	evicted := r.EvictStale()
	if evicted != 1 {
		t.Errorf("expected 1 evicted, got %d", evicted)
	}

	if r.Count() != 1 {
		t.Errorf("expected 1 agent remaining, got %d", r.Count())
	}

	all := r.GetAll()
	if len(all) != 1 || all[0].ID != "agent-1" {
		t.Error("expected agent-1 to survive eviction")
	}
}

func TestRegistryOnChange(t *testing.T) {
	r := NewRegistry(30 * time.Second)

	var received []model.AgentInfo
	var mu sync.Mutex

	r.OnChange(func(agents []model.AgentInfo, _ TopologyChange) {
		mu.Lock()
		received = agents
		mu.Unlock()
	})

	r.Register(model.AgentInfo{ID: "agent-1", NodeName: "node-1"})

	mu.Lock()
	if len(received) != 1 {
		t.Errorf("expected 1 agent in onChange, got %d", len(received))
	}
	mu.Unlock()
}

func TestRegistryConcurrency(t *testing.T) {
	r := NewRegistry(30 * time.Second)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "agent-" + time.Now().String() + fmt.Sprintf("%d", i)
			r.Register(model.AgentInfo{ID: id, NodeName: "node"})
			r.Heartbeat(id)
			r.GetPeers(id)
			r.GetAll()
			r.Count()
		}(i)
	}
	wg.Wait()
}

type stubZoneResolver struct {
	zones map[string]string
}

func (s stubZoneResolver) ZoneFor(nodeName string) string {
	return s.zones[nodeName]
}

func TestRegistryEnrichesEmptyZone(t *testing.T) {
	r := NewRegistry(30 * time.Second)
	r.SetZoneResolver(stubZoneResolver{zones: map[string]string{"node-1": "zone-a"}})

	info := r.Register(model.AgentInfo{ID: "agent-1", NodeName: "node-1"})
	if info.Zone != "zone-a" {
		t.Fatalf("expected enriched zone zone-a, got %q", info.Zone)
	}

	all := r.GetAll()
	if len(all) != 1 || all[0].Zone != "zone-a" {
		t.Fatalf("expected stored agent zone zone-a, got %+v", all)
	}
}

func TestRegistryDoesNotOverrideExplicitZone(t *testing.T) {
	r := NewRegistry(30 * time.Second)
	r.SetZoneResolver(stubZoneResolver{zones: map[string]string{"node-1": "zone-a"}})

	info := r.Register(model.AgentInfo{ID: "agent-1", NodeName: "node-1", Zone: "zone-explicit"})
	if info.Zone != "zone-explicit" {
		t.Fatalf("expected explicit zone preserved, got %q", info.Zone)
	}
}

func TestRegistryUpdateZone(t *testing.T) {
	r := NewRegistry(30 * time.Second)

	var mu sync.Mutex
	var notifications int
	var lastSnapshot []model.AgentInfo
	r.OnChange(func(agents []model.AgentInfo, _ TopologyChange) {
		mu.Lock()
		notifications++
		lastSnapshot = agents
		mu.Unlock()
	})

	// Two agents on node-1, one on node-2.
	r.Register(model.AgentInfo{ID: "agent-1", NodeName: "node-1"})
	r.Register(model.AgentInfo{ID: "agent-1b", NodeName: "node-1"})
	r.Register(model.AgentInfo{ID: "agent-2", NodeName: "node-2"})

	mu.Lock()
	base := notifications
	mu.Unlock()

	r.UpdateZone("node-1", "zone-a")

	mu.Lock()
	defer mu.Unlock()
	if notifications != base+1 {
		t.Fatalf("expected exactly one peer-update notification from UpdateZone, got %d extra", notifications-base)
	}
	byID := map[string]string{}
	for _, a := range lastSnapshot {
		byID[a.ID] = a.Zone
	}
	if byID["agent-1"] != "zone-a" || byID["agent-1b"] != "zone-a" {
		t.Errorf("expected both node-1 agents in zone-a, got %+v", byID)
	}
	if byID["agent-2"] != "" {
		t.Errorf("expected agent-2 unchanged, got %q", byID["agent-2"])
	}
}

func TestRegistryUpdateZoneNoAgentsNoNotify(t *testing.T) {
	r := NewRegistry(30 * time.Second)

	var notifications int
	r.OnChange(func([]model.AgentInfo, TopologyChange) { notifications++ })

	r.UpdateZone("node-unknown", "zone-a")
	if notifications != 0 {
		t.Errorf("expected no notification when no agents match, got %d", notifications)
	}
}

func TestRegistryGetAll(t *testing.T) {
	r := NewRegistry(30 * time.Second)

	r.Register(model.AgentInfo{ID: "a1", NodeName: "n1", Zone: "z1"})
	r.Register(model.AgentInfo{ID: "a2", NodeName: "n2", Zone: "z2"})
	r.Register(model.AgentInfo{ID: "a3", NodeName: "n3", Zone: "z1"})

	all := r.GetAll()
	if len(all) != 3 {
		t.Errorf("expected 3 agents, got %d", len(all))
	}
}

func TestRegistryDeregisterBroadcasts(t *testing.T) {
	r := NewRegistry(30 * time.Second)

	var mu sync.Mutex
	var notifications int
	var lastSnapshot []model.AgentInfo
	r.OnChange(func(agents []model.AgentInfo, _ TopologyChange) {
		mu.Lock()
		notifications++
		lastSnapshot = agents
		mu.Unlock()
	})

	r.Register(model.AgentInfo{ID: "agent-1", NodeName: "node-1"})
	r.Register(model.AgentInfo{ID: "agent-2", NodeName: "node-2"})

	mu.Lock()
	base := notifications
	mu.Unlock()

	r.Deregister("agent-1")

	mu.Lock()
	defer mu.Unlock()
	if notifications != base+1 {
		t.Fatalf("expected exactly one peer-update notification from Deregister, got %d extra", notifications-base)
	}
	if len(lastSnapshot) != 1 || lastSnapshot[0].ID != "agent-2" {
		t.Errorf("expected snapshot with only agent-2, got %+v", lastSnapshot)
	}
}

func TestRegistryDeregisterUnknownNoOp(t *testing.T) {
	r := NewRegistry(30 * time.Second)

	var notifications int
	r.OnChange(func([]model.AgentInfo, TopologyChange) { notifications++ })

	r.Register(model.AgentInfo{ID: "agent-1", NodeName: "node-1"})
	base := notifications

	r.Deregister("agent-unknown")

	if notifications != base {
		t.Errorf("expected no notification for unknown agent deregister, got %d extra", notifications-base)
	}
	if r.Count() != 1 {
		t.Errorf("expected registry unchanged at 1 agent, got %d", r.Count())
	}
}

// topology attribution Every registry mutation that fires OnChange must name WHO it was about.

// recordChanges subscribes to r and returns a snapshot-taking accessor for
// every TopologyChange observed since subscription.
func recordChanges(r *Registry) func() []TopologyChange {
	var mu sync.Mutex
	var seen []TopologyChange
	r.OnChange(func(_ []model.AgentInfo, change TopologyChange) {
		mu.Lock()
		seen = append(seen, change)
		mu.Unlock()
	})
	return func() []TopologyChange {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
}

func TestRegisterAttributesTheRegisteringAgent(t *testing.T) {
	r := NewRegistry(30 * time.Second)
	r.SetZoneResolver(stubZoneResolver{zones: map[string]string{"node-1": "zone-a"}})
	changes := recordChanges(r)

	r.Register(model.AgentInfo{ID: "agent-1", NodeName: "node-1"})

	got := changes()
	if len(got) != 1 {
		t.Fatalf("expected 1 change, got %d: %+v", len(got), got)
	}
	if got[0].Reason != "agent_registered" {
		t.Errorf("reason = %q, want agent_registered", got[0].Reason)
	}
	want := []TopologySubject{{AgentID: "agent-1", NodeName: "node-1", Zone: "zone-a"}}
	if !slices.Equal(got[0].Subjects, want) {
		t.Errorf("subjects = %+v, want %+v", got[0].Subjects, want)
	}
}

// The enriched zone, not the (empty) zone the agent sent, is what the event
// carries: the registry stores the enriched value, so history must match it.
func TestRegisterCarriesTheEnrichedZoneNotTheSubmittedOne(t *testing.T) {
	r := NewRegistry(30 * time.Second)
	r.SetZoneResolver(stubZoneResolver{zones: map[string]string{"node-1": "zone-a"}})
	changes := recordChanges(r)

	r.Register(model.AgentInfo{ID: "agent-1", NodeName: "node-1", Zone: ""})

	if got := changes()[0].Subjects[0].Zone; got != "zone-a" {
		t.Errorf("zone = %q, want the enriched zone-a", got)
	}
}

// zone_updated is the one reason whose whole subject is the NEW zone, and it
// can touch several agents on one node at once — each gets its own subject.
func TestUpdateZoneAttributesEveryAgentOnTheNodeWithTheNewZone(t *testing.T) {
	r := NewRegistry(30 * time.Second)
	r.Register(model.AgentInfo{ID: "agent-1", NodeName: "node-1"})
	r.Register(model.AgentInfo{ID: "agent-1b", NodeName: "node-1"})
	r.Register(model.AgentInfo{ID: "agent-2", NodeName: "node-2"})
	changes := recordChanges(r)

	r.UpdateZone("node-1", "zone-a")

	got := changes()
	if len(got) != 1 {
		t.Fatalf("expected 1 change, got %d: %+v", len(got), got)
	}
	if got[0].Reason != "zone_updated" {
		t.Errorf("reason = %q, want zone_updated", got[0].Reason)
	}
	want := []TopologySubject{
		{AgentID: "agent-1", NodeName: "node-1", Zone: "zone-a"},
		{AgentID: "agent-1b", NodeName: "node-1", Zone: "zone-a"},
	}
	if !slices.Equal(got[0].Subjects, want) {
		t.Errorf("subjects = %+v, want %+v (agent-2 is on another node)", got[0].Subjects, want)
	}
}

// A departure must still name the node and zone the agent HAD — the map entry
// is gone by the time the callback runs, so it has to be captured before the
// delete or the leave is unattributable and the fold can never remove anything.
func TestDeregisterAttributesTheDepartedAgentsLastKnownPlacement(t *testing.T) {
	r := NewRegistry(30 * time.Second)
	r.Register(model.AgentInfo{ID: "agent-1", NodeName: "node-1", Zone: "zone-a"})
	r.Register(model.AgentInfo{ID: "agent-2", NodeName: "node-2", Zone: "zone-b"})
	changes := recordChanges(r)

	r.Deregister("agent-1")

	got := changes()
	if len(got) != 1 {
		t.Fatalf("expected 1 change, got %d: %+v", len(got), got)
	}
	if got[0].Reason != "agent_deregistered" {
		t.Errorf("reason = %q, want agent_deregistered", got[0].Reason)
	}
	want := []TopologySubject{{AgentID: "agent-1", NodeName: "node-1", Zone: "zone-a"}}
	if !slices.Equal(got[0].Subjects, want) {
		t.Errorf("subjects = %+v, want %+v", got[0].Subjects, want)
	}
}

// One TTL sweep can take several agents out at once; they are DIFFERENT nodes, so a single subject
// would attribute the sweep to whichever one the map iteration happened to reach.
func TestEvictStaleAttributesEveryEvictedAgent(t *testing.T) {
	r := NewRegistry(time.Nanosecond)
	r.Register(model.AgentInfo{ID: "agent-2", NodeName: "node-2", Zone: "zone-b"})
	r.Register(model.AgentInfo{ID: "agent-1", NodeName: "node-1", Zone: "zone-a"})
	changes := recordChanges(r)

	time.Sleep(time.Millisecond)
	if n := r.EvictStale(); n != 2 {
		t.Fatalf("expected 2 evictions, got %d", n)
	}

	got := changes()
	if len(got) != 1 {
		t.Fatalf("expected 1 change, got %d: %+v", len(got), got)
	}
	if got[0].Reason != "agent_evicted" {
		t.Errorf("reason = %q, want agent_evicted", got[0].Reason)
	}
	want := []TopologySubject{
		{AgentID: "agent-1", NodeName: "node-1", Zone: "zone-a"},
		{AgentID: "agent-2", NodeName: "node-2", Zone: "zone-b"},
	}
	if !slices.Equal(got[0].Subjects, want) {
		t.Errorf("subjects = %+v, want %+v (sorted by agent id)", got[0].Subjects, want)
	}
}

// Events() is what the controller emits from, so it is pinned here rather than
// left to the caller: one event per subject, every field carried through.
func TestTopologyChangeEventsAreOnePerSubject(t *testing.T) {
	change := TopologyChange{
		Reason: "agent_evicted",
		Subjects: []TopologySubject{
			{AgentID: "agent-1", NodeName: "node-1", Zone: "zone-a"},
			{AgentID: "agent-2", NodeName: "node-2", Zone: "zone-b"},
		},
	}

	evs := change.Events()
	if len(evs) != 2 {
		t.Fatalf("expected 2 events, got %d", len(evs))
	}
	for i, ev := range evs {
		if ev.GetReason() != "agent_evicted" {
			t.Errorf("event %d reason = %q", i, ev.GetReason())
		}
		if ev.GetAgentId() != change.Subjects[i].AgentID ||
			ev.GetNodeName() != change.Subjects[i].NodeName ||
			ev.GetZone() != change.Subjects[i].Zone {
			t.Errorf("event %d = %+v, want subject %+v", i, ev, change.Subjects[i])
		}
	}
}

// The unattributed shape is not dead code to delete: it is what a future reason
// with no agent subject would emit, and dropping the event entirely would lose
// the refetch signal the live Console runs on. One event, reason only.
func TestTopologyChangeWithNoSubjectsStillEmitsTheRefetchSignal(t *testing.T) {
	evs := TopologyChange{Reason: "something_new"}.Events()
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	if evs[0].GetReason() != "something_new" || evs[0].GetNodeName() != "" || evs[0].GetAgentId() != "" {
		t.Errorf("unattributed event = %+v", evs[0])
	}
}

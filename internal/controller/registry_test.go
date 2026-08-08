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

// TestRegistryRetainsAgentCapabilities pins the round-trip the external
// destination gate depends on: what an agent advertises in RegisterRequest must
// survive registry storage and come back out of GetByNodeName and
// agentInfoToProto, otherwise the controller can never tell an M4 agent from a
// pre-M4 one.
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

	r.OnChange(func(agents []model.AgentInfo, _ string) {
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
	r.OnChange(func(agents []model.AgentInfo, _ string) {
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
	r.OnChange(func([]model.AgentInfo, string) { notifications++ })

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
	r.OnChange(func(agents []model.AgentInfo, _ string) {
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
	r.OnChange(func([]model.AgentInfo, string) { notifications++ })

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

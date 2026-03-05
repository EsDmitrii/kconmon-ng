package controller

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

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

	r.OnChange(func(agents []model.AgentInfo) {
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

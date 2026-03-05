package controller

import (
	"log/slog"
	"sync"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

type Registry struct {
	mu       sync.RWMutex
	agents   map[string]*registeredAgent
	ttl      time.Duration
	onChange []func([]model.AgentInfo)
}

type registeredAgent struct {
	info     model.AgentInfo
	lastSeen time.Time
}

func NewRegistry(ttl time.Duration) *Registry {
	return &Registry{
		agents: make(map[string]*registeredAgent),
		ttl:    ttl,
	}
}

func (r *Registry) Register(info model.AgentInfo) { //nolint:gocritic // hugeParam: public API uses value semantics intentionally
	r.mu.Lock()
	now := time.Now()
	info.JoinedAt = now
	info.LastSeen = now
	r.agents[info.ID] = &registeredAgent{
		info:     info,
		lastSeen: now,
	}
	snapshot := r.snapshotLocked()
	r.mu.Unlock()

	slog.Info("agent registered", "id", info.ID, "node", info.NodeName, "zone", info.Zone)
	r.notifyChange(snapshot)
}

func (r *Registry) Deregister(agentID string) {
	r.mu.Lock()
	_, existed := r.agents[agentID]
	if existed {
		delete(r.agents, agentID)
	}
	snapshot := r.snapshotLocked()
	r.mu.Unlock()

	if existed {
		slog.Info("agent deregistered", "id", agentID)
		r.notifyChange(snapshot)
	}
}

func (r *Registry) Heartbeat(agentID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	agent, ok := r.agents[agentID]
	if !ok {
		return false
	}

	now := time.Now()
	agent.lastSeen = now
	agent.info.LastSeen = now
	return true
}

func (r *Registry) GetPeers(excludeID string) []model.AgentInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	peers := make([]model.AgentInfo, 0, len(r.agents))
	for id, agent := range r.agents {
		if id != excludeID {
			peers = append(peers, agent.info)
		}
	}
	return peers
}

func (r *Registry) GetAll() []model.AgentInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	agents := make([]model.AgentInfo, 0, len(r.agents))
	for _, agent := range r.agents {
		agents = append(agents, agent.info)
	}
	return agents
}

func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.agents)
}

func (r *Registry) EvictStale() int {
	r.mu.Lock()
	evicted := 0
	cutoff := time.Now().Add(-r.ttl)

	type evictedEntry struct {
		id       string
		nodeName string
		lastSeen time.Time
	}
	var evictedList []evictedEntry

	for id, agent := range r.agents {
		if agent.lastSeen.Before(cutoff) {
			evictedList = append(evictedList, evictedEntry{id, agent.info.NodeName, agent.lastSeen})
			delete(r.agents, id)
			evicted++
		}
	}
	snapshot := r.snapshotLocked()
	r.mu.Unlock()

	for _, e := range evictedList {
		slog.Warn("agent evicted (TTL expired)", "id", e.id, "node", e.nodeName, "lastSeen", e.lastSeen)
	}
	if evicted > 0 {
		r.notifyChange(snapshot)
	}
	return evicted
}

func (r *Registry) OnChange(fn func([]model.AgentInfo)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onChange = append(r.onChange, fn)
}

func (r *Registry) snapshotLocked() []model.AgentInfo {
	agents := make([]model.AgentInfo, 0, len(r.agents))
	for _, a := range r.agents {
		agents = append(agents, a.info)
	}
	return agents
}

func (r *Registry) notifyChange(agents []model.AgentInfo) {
	r.mu.RLock()
	callbacks := make([]func([]model.AgentInfo), len(r.onChange))
	copy(callbacks, r.onChange)
	r.mu.RUnlock()

	for _, fn := range callbacks {
		fn(agents)
	}
}

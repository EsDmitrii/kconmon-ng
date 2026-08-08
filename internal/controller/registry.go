package controller

import (
	"log/slog"
	"sort"
	"sync"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

// ZoneResolver resolves a node's failure-domain zone. Implemented by
// *NodeWatcher; kept as an interface so the registry can be tested without one.
type ZoneResolver interface {
	ZoneFor(nodeName string) string
}

// The closed set of topology-change reasons. These strings are the wire
// contract: they travel in pb.TopologyChanged.reason, get persisted in the
// console's topology_events rows, and the console's fold branches on them
// (internal/console/store/events.go, which mirrors them by hand because the
// dependency runs the other way). Renaming one silently breaks history.
const (
	reasonAgentRegistered   = "agent_registered"
	reasonZoneUpdated       = "zone_updated"
	reasonAgentDeregistered = "agent_deregistered"
	reasonAgentEvicted      = "agent_evicted"
)

// TopologySubject names ONE agent a topology change was about, with the node
// and zone it occupied at that moment. For a departure (deregister/evict) that
// is the placement it HAD — captured before the map entry goes away, because
// nothing can look it up afterwards.
type TopologySubject struct {
	AgentID  string
	NodeName string
	Zone     string
}

// TopologyChange is what a registry mutation tells its OnChange subscribers:
// the reason, and every agent that reason was about.
//
// Subjects exists because a reason alone is not history. Until M7 the registry
// handed subscribers a bare reason string, the controller published
// pb.TopologyChanged{Reason: reason}, and the console's fold over persisted
// events could never say WHICH node joined or left — so reconstructed topology
// was structurally correct and permanently empty. Every mutation below knows
// its subject at the call site; this type is how that knowledge survives to
// the event.
//
// Subjects may hold several agents: a zone relabel moves every agent on the
// node, and one TTL sweep can evict several at once. It is empty only for a
// reason that genuinely has no agent subject — see Events.
type TopologyChange struct {
	Reason   string
	Subjects []TopologySubject
}

// Events renders the change as the events to publish: ONE per subject, so the
// console's fold sees each node join or leave separately rather than having to
// guess which of N agents a single event meant.
//
// With no subjects it still returns one reason-only event. That is deliberate:
// a TopologyChanged is also the live Console's refetch signal, and swallowing
// it would leave the UI stale until the next poll. Such an event is honest
// about naming nobody, and the fold counts it as unfoldable rather than
// guessing.
func (c TopologyChange) Events() []*pb.TopologyChanged {
	if len(c.Subjects) == 0 {
		return []*pb.TopologyChanged{{Reason: c.Reason}}
	}
	out := make([]*pb.TopologyChanged, 0, len(c.Subjects))
	for _, s := range c.Subjects {
		out = append(out, &pb.TopologyChanged{
			Reason:   c.Reason,
			NodeName: s.NodeName,
			AgentId:  s.AgentID,
			Zone:     s.Zone,
		})
	}
	return out
}

type Registry struct {
	mu           sync.RWMutex
	agents       map[string]*registeredAgent
	ttl          time.Duration
	onChange     []func(agents []model.AgentInfo, change TopologyChange)
	zoneResolver ZoneResolver
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

// SetZoneResolver injects the zone resolver used to enrich agents that
// register without an explicit zone. Safe to call before serving traffic.
func (r *Registry) SetZoneResolver(zr ZoneResolver) {
	r.mu.Lock()
	r.zoneResolver = zr
	r.mu.Unlock()
}

// Register stores the agent and returns its resolved metadata. When the agent
// provides no zone, the zone is enriched from the node's failure-domain label
// via the configured ZoneResolver (an explicit zone is never overridden).
func (r *Registry) Register(info model.AgentInfo) model.AgentInfo { //nolint:gocritic // hugeParam: public API uses value semantics intentionally
	r.mu.Lock()
	now := time.Now()
	info.JoinedAt = now
	info.LastSeen = now
	if info.Zone == "" && r.zoneResolver != nil {
		info.Zone = r.zoneResolver.ZoneFor(info.NodeName)
	}
	r.agents[info.ID] = &registeredAgent{
		info:     info,
		lastSeen: now,
	}
	snapshot := r.snapshotLocked()
	r.mu.Unlock()

	slog.Info("agent registered", "id", info.ID, "node", info.NodeName, "zone", info.Zone)
	// info is post-enrichment, so the event carries the zone the registry
	// actually stored rather than the (possibly empty) one the agent sent.
	r.notifyChange(snapshot, TopologyChange{
		Reason:   reasonAgentRegistered,
		Subjects: []TopologySubject{{AgentID: info.ID, NodeName: info.NodeName, Zone: info.Zone}},
	})
	return info
}

// UpdateZone sets the zone for every agent registered on nodeName and, if any
// were changed, broadcasts a peer update to subscribers. Agents that resolve
// their zone at registration time will keep the new value on re-registration.
func (r *Registry) UpdateZone(nodeName, zone string) {
	r.mu.Lock()
	var subjects []TopologySubject
	for _, agent := range r.agents {
		if agent.info.NodeName == nodeName && agent.info.Zone != zone {
			agent.info.Zone = zone
			subjects = append(subjects, TopologySubject{
				AgentID: agent.info.ID, NodeName: nodeName, Zone: zone,
			})
		}
	}
	var snapshot []model.AgentInfo
	if len(subjects) > 0 {
		snapshot = r.snapshotLocked()
	}
	r.mu.Unlock()

	if len(subjects) > 0 {
		slog.Info("agent zone updated", "node", nodeName, "zone", zone)
		// Map iteration order is random, so subjects are sorted: the order
		// decides the order of the published events, and a nondeterministic
		// event stream is untestable and needlessly hard to diff in history.
		sortSubjects(subjects)
		r.notifyChange(snapshot, TopologyChange{Reason: reasonZoneUpdated, Subjects: subjects})
	}
}

func (r *Registry) Deregister(agentID string) {
	r.mu.Lock()
	// The placement is read BEFORE the delete: after it, nothing in this
	// process knows which node the agent was on, and an unattributed departure
	// is one the console's fold can never apply.
	var subject TopologySubject
	agent, existed := r.agents[agentID]
	if existed {
		subject = TopologySubject{AgentID: agentID, NodeName: agent.info.NodeName, Zone: agent.info.Zone}
		delete(r.agents, agentID)
	}
	snapshot := r.snapshotLocked()
	r.mu.Unlock()

	if existed {
		slog.Info("agent deregistered", "id", agentID)
		r.notifyChange(snapshot, TopologyChange{
			Reason:   reasonAgentDeregistered,
			Subjects: []TopologySubject{subject},
		})
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

// GetByNodeName returns the registered agent running on nodeName. If several
// agents share a node (rolling restart overlap), the first match is returned.
// The bool is false when no agent is registered for that node.
func (r *Registry) GetByNodeName(nodeName string) (model.AgentInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, agent := range r.agents {
		if agent.info.NodeName == nodeName {
			return agent.info, true
		}
	}
	return model.AgentInfo{}, false
}

func (r *Registry) EvictStale() int {
	r.mu.Lock()
	evicted := 0
	cutoff := time.Now().Add(-r.ttl)

	type evictedEntry struct {
		subject  TopologySubject
		lastSeen time.Time
	}
	var evictedList []evictedEntry

	for id, agent := range r.agents {
		if agent.lastSeen.Before(cutoff) {
			evictedList = append(evictedList, evictedEntry{
				subject: TopologySubject{
					AgentID: id, NodeName: agent.info.NodeName, Zone: agent.info.Zone,
				},
				lastSeen: agent.lastSeen,
			})
			delete(r.agents, id)
			evicted++
		}
	}
	snapshot := r.snapshotLocked()
	r.mu.Unlock()

	for _, e := range evictedList {
		slog.Warn("agent evicted (TTL expired)",
			"id", e.subject.AgentID, "node", e.subject.NodeName, "lastSeen", e.lastSeen)
	}
	if evicted > 0 {
		// One sweep can take several agents on DIFFERENT nodes; each is its own
		// subject, or the fold would only ever see one of them leave.
		subjects := make([]TopologySubject, 0, len(evictedList))
		for _, e := range evictedList {
			subjects = append(subjects, e.subject)
		}
		sortSubjects(subjects)
		r.notifyChange(snapshot, TopologyChange{Reason: reasonAgentEvicted, Subjects: subjects})
	}
	return evicted
}

func (r *Registry) OnChange(fn func(agents []model.AgentInfo, change TopologyChange)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onChange = append(r.onChange, fn)
}

// sortSubjects orders subjects by agent id so a multi-subject change publishes
// its events in a deterministic order regardless of Go's map iteration.
func sortSubjects(subjects []TopologySubject) {
	sort.Slice(subjects, func(i, j int) bool { return subjects[i].AgentID < subjects[j].AgentID })
}

func (r *Registry) snapshotLocked() []model.AgentInfo {
	agents := make([]model.AgentInfo, 0, len(r.agents))
	for _, a := range r.agents {
		agents = append(agents, a.info)
	}
	return agents
}

func (r *Registry) notifyChange(agents []model.AgentInfo, change TopologyChange) {
	r.mu.RLock()
	callbacks := make([]func([]model.AgentInfo, TopologyChange), len(r.onChange))
	copy(callbacks, r.onChange)
	r.mu.RUnlock()

	for _, fn := range callbacks {
		fn(agents, change)
	}
}

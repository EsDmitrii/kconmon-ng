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

// The closed set of topology-change reasons.
const (
	reasonAgentRegistered   = "agent_registered"
	reasonZoneUpdated       = "zone_updated"
	reasonAgentDeregistered = "agent_deregistered"
	reasonAgentEvicted      = "agent_evicted"
)

// TopologySubject names ONE agent a topology change was about, with the node and zone it occupied
// at that moment.
type TopologySubject struct {
	AgentID  string
	NodeName string
	Zone     string
}

// TopologyChange is what a registry mutation tells its OnChange subscribers: the reason; until the
// registry handed subscribers a bare reason string.
type TopologyChange struct {
	Reason   string
	Subjects []TopologySubject
}

// Events renders the change as the events to publish: ONE per subject.
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
	mu     sync.RWMutex
	agents map[string]*registeredAgent
	ttl    time.Duration
	/* notifyMu ORDERS the publications, which r.mu alone did not.
	   Each mutator snapshots under r.mu and then published outside it, so two agents registering at
	   the same instant — a DaemonSet rollout, a two-node scale-up — could publish in either order:
	   goroutine A snapshots {A}, B snapshots {A,B}, and nothing stopped B's notify from running
	   first. Every peer update is sent as a FULL_SYNC and applied by wholesale replacement, so the
	   LAST word won: the fleet could be left believing in {A} alone, with B invisible to every other
	   agent until the next change or the TTL sweep.
	   ALWAYS taken BEFORE r.mu and held until the callbacks return. The other order deadlocks:
	   notifyChange itself reads the callback list under r.mu.RLock, so a goroutine holding r.mu and
	   waiting for notifyMu would be waiting on a goroutine holding notifyMu and waiting for r.mu. */
	notifyMu     sync.Mutex
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
	/* notifyMu BEFORE r.mu, always in that order, and held across the publication: it is what makes
	   the ORDER of the FULL_SYNC broadcasts match the order of the mutations. */
	r.notifyMu.Lock()
	defer r.notifyMu.Unlock()
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
	/* notifyMu BEFORE r.mu, always in that order, and held across the publication: it is what makes
	   the ORDER of the FULL_SYNC broadcasts match the order of the mutations. */
	r.notifyMu.Lock()
	defer r.notifyMu.Unlock()
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
	// notifyMu BEFORE r.mu, always; see the field.
	r.notifyMu.Lock()
	defer r.notifyMu.Unlock()
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

/*
 * ResetQuiet drops every registered agent WITHOUT notifying subscribers.
 *
 * This is the demotion path. Reset's notification goes to the streams still attached to THIS
 * replica — the agents and consoles that have not yet noticed the leadership change — and it says
 * "every agent deregistered": each attached agent applied peers=[] wholesale, wiped its peer gauges
 * and resumed probing nothing until its own leader check finally ended the stream, while the
 * replica published one false agent_deregistered event per agent into the console's timeline. A
 * replica that is no longer the leader has nothing to announce about a fleet it no longer owns.
 */
func (r *Registry) ResetQuiet() {
	r.notifyMu.Lock()
	defer r.notifyMu.Unlock()
	r.mu.Lock()
	count := len(r.agents)
	r.agents = make(map[string]*registeredAgent)
	r.mu.Unlock()

	if count > 0 {
		slog.Info("registry cleared after losing leadership", "agents", count)
	}
}

// Reset drops every registered agent and notifies subscribers as if each had deregistered.
func (r *Registry) Reset() {
	// notifyMu BEFORE r.mu, always; see the field.
	r.notifyMu.Lock()
	defer r.notifyMu.Unlock()
	r.mu.Lock()
	subjects := make([]TopologySubject, 0, len(r.agents))
	for id, agent := range r.agents {
		subjects = append(subjects, TopologySubject{
			AgentID: id, NodeName: agent.info.NodeName, Zone: agent.info.Zone,
		})
	}
	r.agents = make(map[string]*registeredAgent)
	snapshot := r.snapshotLocked()
	r.mu.Unlock()

	if len(subjects) == 0 {
		return
	}

	slog.Info("registry cleared after losing leadership", "agents", len(subjects))
	sortSubjects(subjects)
	r.notifyChange(snapshot, TopologyChange{Reason: reasonAgentDeregistered, Subjects: subjects})
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
	// notifyMu BEFORE r.mu, always; see the field.
	r.notifyMu.Lock()
	defer r.notifyMu.Unlock()
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

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

// TopologySubject names ONE agent a topology change was about, with the node, zone and labels it
// had at that moment. Labels ride along so the Console's history fold can tell an external host
// from a node at every event, departures included, the same way the live topology does.
type TopologySubject struct {
	AgentID  string
	NodeName string
	Zone     string
	Labels   map[string]string
}

// TopologyChange is what a registry mutation tells its OnChange subscribers: the reason and the
// agents it was about.
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
			Labels:   s.Labels,
		})
	}
	return out
}

type Registry struct {
	mu     sync.RWMutex
	agents map[string]*registeredAgent
	ttl    time.Duration
	/* notifyMu orders the publications to match the mutations. Every peer update is a FULL_SYNC
	   applied by wholesale replacement, so two concurrent registrations publishing out of order
	   would leave the fleet on the older snapshot until the next change.
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
	// zoneExplicit is set when the agent registered with its own zone; UpdateZone leaves it alone.
	zoneExplicit bool
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
	zoneExplicit := info.Zone != ""
	if !zoneExplicit && r.zoneResolver != nil {
		info.Zone = r.zoneResolver.ZoneFor(info.NodeName)
	}
	r.agents[info.ID] = &registeredAgent{
		info:         info,
		lastSeen:     now,
		zoneExplicit: zoneExplicit,
	}
	snapshot := r.snapshotLocked()
	r.mu.Unlock()

	slog.Info("agent registered", "id", info.ID, "node", info.NodeName, "zone", info.Zone)
	// info is post-enrichment, so the event carries the zone the registry
	// actually stored rather than the (possibly empty) one the agent sent.
	r.notifyChange(snapshot, TopologyChange{
		Reason:   reasonAgentRegistered,
		Subjects: []TopologySubject{{AgentID: info.ID, NodeName: info.NodeName, Zone: info.Zone, Labels: info.Labels}},
	})
	return info
}

// UpdateZone sets the zone for every agent registered on nodeName without a zone of its own and, if
// any were changed, broadcasts a peer update to subscribers. Agents that resolve their zone at
// registration time will keep the new value on re-registration.
func (r *Registry) UpdateZone(nodeName, zone string) {
	/* notifyMu BEFORE r.mu, always in that order, and held across the publication: it is what makes
	   the ORDER of the FULL_SYNC broadcasts match the order of the mutations. */
	r.notifyMu.Lock()
	defer r.notifyMu.Unlock()
	r.mu.Lock()
	var subjects []TopologySubject
	for _, agent := range r.agents {
		if agent.info.NodeName == nodeName && !agent.zoneExplicit && agent.info.Zone != zone {
			agent.info.Zone = zone
			subjects = append(subjects, TopologySubject{
				AgentID: agent.info.ID, NodeName: nodeName, Zone: zone, Labels: agent.info.Labels,
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
		subject = TopologySubject{
			AgentID: agentID, NodeName: agent.info.NodeName, Zone: agent.info.Zone, Labels: agent.info.Labels,
		}
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

// ResetQuiet drops every registered agent WITHOUT notifying subscribers; it is the demotion path.
// The streams still attached here would read a notification as "every agent deregistered" and
// probe an empty mesh, and a replica that no longer leads has nothing to announce about the fleet.
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

// GetByNodeName returns the registered agent running on nodeName. If several agents share a node
// (an old pod that left without deregistering, until its TTL runs out), the most recent
// registration wins: that is the replacement, the other one is dead. The bool is false when no
// agent is registered for that node.
func (r *Registry) GetByNodeName(nodeName string) (model.AgentInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var best *registeredAgent
	for _, agent := range r.agents {
		if agent.info.NodeName != nodeName {
			continue
		}
		if best == nil || newerRegistration(agent, best) {
			best = agent
		}
	}
	if best == nil {
		return model.AgentInfo{}, false
	}
	return best.info, true
}

// newerRegistration orders agents on one node: latest JoinedAt, then latest heartbeat, then id, so
// the choice never depends on map iteration order.
func newerRegistration(a, b *registeredAgent) bool {
	if !a.info.JoinedAt.Equal(b.info.JoinedAt) {
		return a.info.JoinedAt.After(b.info.JoinedAt)
	}
	if !a.lastSeen.Equal(b.lastSeen) {
		return a.lastSeen.After(b.lastSeen)
	}
	return a.info.ID > b.info.ID
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
					AgentID: id, NodeName: agent.info.NodeName, Zone: agent.info.Zone, Labels: agent.info.Labels,
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

// SetTTL changes the eviction TTL (a config reload); the next EvictStale sweep uses it.
func (r *Registry) SetTTL(ttl time.Duration) {
	r.mu.Lock()
	r.ttl = ttl
	r.mu.Unlock()
}

// WithSnapshot runs fn on the current agent list in order with every published change: it holds
// notifyMu like the mutators, so nothing fn derives from the list can overtake, or be overtaken by, a
// change's OnChange callbacks.
func (r *Registry) WithSnapshot(fn func(agents []model.AgentInfo)) {
	r.notifyMu.Lock()
	defer r.notifyMu.Unlock()
	r.mu.RLock()
	snapshot := r.snapshotLocked()
	r.mu.RUnlock()
	fn(snapshot)
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

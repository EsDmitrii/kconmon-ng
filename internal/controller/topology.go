package controller

import (
	"encoding/json"
	"net/http"
	"sort"
	"sync/atomic"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/controller/meshplan"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

type TopologyHandler struct {
	registry    *Registry
	nodeWatcher atomic.Pointer[NodeWatcher]
	gate        atomic.Pointer[leaderGate]
	// planSource answers "which probe plan is in force right now" (GRPCServer.CurrentPlan in
	// production); nil source and nil answer both mean full mesh, and the snapshot then carries no
	// probePlan at all — pre-M10 payloads are pinned byte for byte.
	planSource atomic.Pointer[func() meshplan.Plan]
}

// leaderGate is the handler's leadership check, hot-injected like the node watcher.
type leaderGate struct {
	enabled  bool
	isLeader func() bool
}

func NewTopologyHandler(registry *Registry, nodeWatcher *NodeWatcher) *TopologyHandler {
	h := &TopologyHandler{registry: registry}
	if nodeWatcher != nil {
		h.nodeWatcher.Store(nodeWatcher)
	}
	return h
}

// SetNodeWatcher hot-injects a NodeWatcher after initial construction.
// Safe to call concurrently with ServeHTTP.
func (h *TopologyHandler) SetNodeWatcher(nw *NodeWatcher) {
	h.nodeWatcher.Store(nw)
}

// SetPlanSource hot-injects the probe-plan accessor, mirroring SetNodeWatcher.
// Safe to call concurrently with ServeHTTP.
func (h *TopologyHandler) SetPlanSource(fn func() meshplan.Plan) {
	if fn == nil {
		h.planSource.Store(nil)
		return
	}
	h.planSource.Store(&fn)
}

// SetLeaderGate makes the snapshot leader-only, mirroring the diagnostics and external-check
// handlers. Until it is set the topology is served unconditionally.
func (h *TopologyHandler) SetLeaderGate(enabled bool, isLeader func() bool) {
	h.gate.Store(&leaderGate{enabled: enabled, isLeader: isLeader})
}

// lostLeadership mirrors GRPCServer.lostLeadership.
func (h *TopologyHandler) lostLeadership() bool {
	g := h.gate.Load()
	return g != nil && g.enabled && (g.isLeader == nil || !g.isLeader())
}

func (h *TopologyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// A standby's registry is empty by design, so answering it would report a topology with no
	// agents; the Console and CLI clients treat 503 as "ask another replica".
	if h.lostLeadership() {
		http.Error(w, "not the leader", http.StatusServiceUnavailable)
		return
	}

	snapshot := model.TopologySnapshot{
		Agents:    h.registry.GetAll(),
		Timestamp: time.Now(),
	}

	if nw := h.nodeWatcher.Load(); nw != nil {
		snapshot.Nodes = nw.GetNodes()
	}

	if src := h.planSource.Load(); src != nil {
		if plan := (*src)(); plan != nil {
			snapshot.ProbePlan = nodeProbePlan(plan, snapshot.Agents)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(snapshot); err != nil {
		http.Error(w, "failed to encode topology", http.StatusInternalServerError)
	}
}

/*
nodeProbePlan translates the agent-ID plan into the compact form the Console joins on: source node
name to the sorted node names it is planned to probe. Translation runs against the SAME agent
snapshot the body carries, so the surfaced plan never names a node the snapshot does not.

Iteration is over the agents, not the plan: an agent the plan fails to mention keeps the plan's own
fail-closed meaning — an EMPTY list ("probes nobody"), not a missing key — and a plan entry whose
agent has already left the registry surfaces nothing. Two agents on one node (rolling-restart
overlap) merge into one deduplicated list, minus same-node pairs the matrix has no cell for.
*/
func nodeProbePlan(plan meshplan.Plan, agents []model.AgentInfo) map[string][]string {
	idToNode := make(map[string]string, len(agents))
	for i := range agents {
		idToNode[agents[i].ID] = agents[i].NodeName
	}

	dests := make(map[string]map[string]struct{}, len(agents))
	for i := range agents {
		node := agents[i].NodeName
		if _, ok := dests[node]; !ok {
			dests[node] = map[string]struct{}{}
		}
		for _, peerID := range plan[agents[i].ID] {
			peerNode, ok := idToNode[peerID]
			if !ok || peerNode == node {
				continue
			}
			dests[node][peerNode] = struct{}{}
		}
	}

	out := make(map[string][]string, len(dests))
	for node, set := range dests {
		peers := make([]string, 0, len(set))
		for peerNode := range set {
			peers = append(peers, peerNode)
		}
		sort.Strings(peers)
		out[node] = peers
	}
	return out
}

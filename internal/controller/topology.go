package controller

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

type TopologyHandler struct {
	registry    *Registry
	nodeWatcher atomic.Pointer[NodeWatcher]
	gate        atomic.Pointer[leaderGate]
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

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(snapshot); err != nil {
		http.Error(w, "failed to encode topology", http.StatusInternalServerError)
	}
}

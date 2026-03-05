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

func (h *TopologyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

package controller

import (
	"encoding/json"
	"net/http"
	"sync/atomic"

	"github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type HTTPServer struct {
	mux             *http.ServeMux
	registry        *Registry
	promReg         *prometheus.Registry
	ready           atomic.Bool
	topologyHandler *TopologyHandler
}

func NewHTTPServer(registry *Registry, nodeWatcher *NodeWatcher, promReg *prometheus.Registry) *HTTPServer {
	s := &HTTPServer{
		mux:      http.NewServeMux(),
		registry: registry,
		promReg:  promReg,
	}

	s.topologyHandler = NewTopologyHandler(registry, nodeWatcher)

	s.mux.Handle("GET /metrics", promhttp.HandlerFor(promReg, promhttp.HandlerOpts{}))
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)
	s.mux.Handle("GET /api/v1/topology", s.topologyHandler)
	s.mux.HandleFunc("GET /api/v1/version", s.handleVersion)

	return s
}

// SetNodeWatcher injects a NodeWatcher into the topology handler.
// Can be called after construction, before or after the server starts accepting requests.
func (s *HTTPServer) SetNodeWatcher(nw *NodeWatcher) {
	s.topologyHandler.SetNodeWatcher(nw)
}

func (s *HTTPServer) Handler() http.Handler {
	return s.mux
}

func (s *HTTPServer) SetReady(ready bool) {
	s.ready.Store(ready)
}

func (s *HTTPServer) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *HTTPServer) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if !s.ready.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready"))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *HTTPServer) handleVersion(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"version": config.Version,
		"commit":  config.Commit,
	})
}

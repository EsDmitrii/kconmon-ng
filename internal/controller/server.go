package controller

import (
	"encoding/json"
	"net/http"
	"sync/atomic"

	"github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/EsDmitrii/kconmon-ng/internal/controller/meshplan"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type HTTPServer struct {
	mux             *http.ServeMux
	registry        *Registry
	promReg         *prometheus.Registry
	ready           atomic.Bool
	topologyHandler *TopologyHandler
	diagHandler     atomic.Pointer[DiagnosticsHandler]
	externalHandler atomic.Pointer[ExternalChecksHandler]
	// sdHandler is nil until EnablePrometheusSD: controller.prometheusSD.enabled=false is "no
	// route", so the API mux answers 404 exactly as it would for a path it never had.
	sdHandler atomic.Pointer[PrometheusSDHandler]
	// leaderGate is remembered so a route enabled after SetLeaderGate is still gated.
	leaderGate   atomic.Pointer[leaderGate]
	capabilities []string
	/* The CIDRs an agent will actually probe (config.checkers.external.allowedCidrs). Published
	   because the Console cannot otherwise know them -- they live in the AGENT's config, not the
	   Console's -- and a target outside them can never be reached, which is worth saying at the
	   moment it is created rather than as a timeout later. */
	externalAllowedCIDRs []string
}

func NewHTTPServer(registry *Registry, nodeWatcher *NodeWatcher, promReg *prometheus.Registry, capabilities []string) *HTTPServer {
	s := &HTTPServer{
		mux:                  http.NewServeMux(),
		registry:             registry,
		promReg:              promReg,
		capabilities:         capabilities,
		externalAllowedCIDRs: []string{},
	}
	// A nil slice would marshal as JSON null; an empty one keeps the field an
	// array the Console can iterate unconditionally.
	if s.capabilities == nil {
		s.capabilities = []string{}
	}

	s.topologyHandler = NewTopologyHandler(registry, nodeWatcher)

	s.mux.Handle("GET /metrics", promhttp.HandlerFor(promReg, promhttp.HandlerOpts{}))
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)
	s.mux.Handle("GET /api/v1/topology", s.topologyHandler)
	s.mux.HandleFunc("GET /api/v1/version", s.handleVersion)
	s.mux.HandleFunc("GET "+prometheusSDPath, s.handlePrometheusSD)
	s.mux.HandleFunc("POST /api/v1/diagnostics", s.handleDiagnostics)
	s.mux.HandleFunc("PUT /api/v1/external-checks", s.handleExternalChecks)

	return s
}

// SetLeaderGate makes GET /api/v1/topology and GET /api/v1/prometheus/sd leader-only, matching
// the diagnostics and external-check routes.
func (s *HTTPServer) SetLeaderGate(enabled bool, isLeader func() bool) {
	s.leaderGate.Store(&leaderGate{enabled: enabled, isLeader: isLeader})
	s.topologyHandler.SetLeaderGate(enabled, isLeader)
	if h := s.sdHandler.Load(); h != nil {
		h.SetLeaderGate(enabled, isLeader)
	}
}

// EnablePrometheusSD mounts the Prometheus SD body (controller.prometheusSD.enabled). metricsPort
// is the controller's own, the fallback target port for agents that reported none. The handler is
// returned so the metrics listener can mount the very same instance, gate included.
func (s *HTTPServer) EnablePrometheusSD(metricsPort int) *PrometheusSDHandler {
	h := NewPrometheusSDHandler(s.registry, metricsPort)
	if g := s.leaderGate.Load(); g != nil {
		h.SetLeaderGate(g.enabled, g.isLeader)
	}
	s.sdHandler.Store(h)
	return h
}

// SDHandler returns the SD handler, or nil when the route is disabled.
func (s *HTTPServer) SDHandler() *PrometheusSDHandler {
	return s.sdHandler.Load()
}

func (s *HTTPServer) handlePrometheusSD(w http.ResponseWriter, r *http.Request) {
	h := s.sdHandler.Load()
	if h == nil {
		http.NotFound(w, r)
		return
	}
	h.ServeHTTP(w, r)
}

// SetNodeWatcher injects a NodeWatcher into the topology handler.
// Can be called after construction, before or after the server starts accepting requests.
func (s *HTTPServer) SetNodeWatcher(nw *NodeWatcher) {
	s.topologyHandler.SetNodeWatcher(nw)
}

// SetDiagnosticsHandler hot-injects the on-demand diagnostics handler. Safe to
// call concurrently with request serving. Until set, the diagnostics route
// returns 503.
func (s *HTTPServer) SetDiagnosticsHandler(h *DiagnosticsHandler) {
	s.diagHandler.Store(h)
	/* The probe plan's owner (the gRPC server) already reaches this HTTP server here, as the
	   diagnostics event publisher; reusing that reference wires the topology snapshot's probePlan
	   without a new constructor dependency. A fake publisher (tests) simply fails the assertion
	   and the snapshot keeps its full-mesh shape. */
	if h != nil {
		if src, ok := h.events.(interface{ CurrentPlan() meshplan.Plan }); ok {
			s.topologyHandler.SetPlanSource(src.CurrentPlan)
		}
	}
}

func (s *HTTPServer) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	h := s.diagHandler.Load()
	if h == nil {
		http.Error(w, "diagnostics not available", http.StatusServiceUnavailable)
		return
	}
	h.ServeHTTP(w, r)
}

// SetExternalChecksHandler hot-injects the continuous external-check assignment handler, mirroring
// SetDiagnosticsHandler.
func (s *HTTPServer) SetExternalChecksHandler(h *ExternalChecksHandler) {
	s.externalHandler.Store(h)
}

func (s *HTTPServer) handleExternalChecks(w http.ResponseWriter, r *http.Request) {
	h := s.externalHandler.Load()
	if h == nil {
		http.Error(w, "external checks not available", http.StatusServiceUnavailable)
		return
	}
	h.ServeHTTP(w, r)
}

func (s *HTTPServer) Handler() http.Handler {
	return s.mux
}

func (s *HTTPServer) SetReady(ready bool) {
	s.ready.Store(ready)
}

// Ready is the readiness this component reports; the metrics listener shares it (metrics.NewListenerHandler).
func (s *HTTPServer) Ready() bool {
	return s.ready.Load()
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

// SetExternalAllowedCIDRs publishes the agent-side allowlist on GET /api/v1/version. A setter
// rather than a constructor argument, matching how the diagnostics and external handlers are wired.
func (s *HTTPServer) SetExternalAllowedCIDRs(cidrs []string) {
	if cidrs == nil {
		cidrs = []string{}
	}
	s.externalAllowedCIDRs = cidrs
}

func (s *HTTPServer) handleVersion(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"version":              config.Version,
		"commit":               config.Commit,
		"capabilities":         s.capabilities,
		"externalAllowedCidrs": s.externalAllowedCIDRs,
	})
}

package controller

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

// prometheusSDPath is the SD route, mounted on the API mux and on the metrics listener alike.
const prometheusSDPath = "/api/v1/prometheus/sd"

/*
PrometheusSDHandler serves GET /api/v1/prometheus/sd: the Prometheus HTTP service-discovery body
listing every EXTERNAL agent as a scrape target. In-cluster agents are already discovered through
their Service by the chart's ServiceMonitor; a bare host has no Service, so the controller, which
is the only party that knows the host registered and on which address, tells Prometheus.

The body is the full target list on every refresh, per the http_sd contract: there are no
incremental updates. That is also why a standby must answer 503 and never 200 [] (see ServeHTTP).
*/
type PrometheusSDHandler struct {
	registry *Registry
	gate     atomic.Pointer[leaderGate]
	// metricsPort is the controller's own config.metricsPort, which the shared ConfigMap pins
	// fleet-wide; it stands in for an agent that reported no port (older than 2.4.0).
	metricsPort int
	// assumed remembers the agents whose port was already logged as assumed, so the line fires
	// once per agent rather than once per refresh. External IDs are stable (<node>-<hostname>).
	assumed sync.Map
	logger  *slog.Logger
}

// targetGroup is one element of the http_sd body: {"targets": [...], "labels": {...}}.
type targetGroup struct {
	Targets []string          `json:"targets"`
	Labels  map[string]string `json:"labels"`
}

// NewPrometheusSDHandler builds the handler; metricsPort is the controller's own metrics port,
// captured at startup because ports are startup-only configuration.
func NewPrometheusSDHandler(registry *Registry, metricsPort int) *PrometheusSDHandler {
	return &PrometheusSDHandler{
		registry:    registry,
		metricsPort: metricsPort,
		logger:      slog.Default(),
	}
}

// SetLeaderGate makes the body leader-only, mirroring TopologyHandler.SetLeaderGate.
func (h *PrometheusSDHandler) SetLeaderGate(enabled bool, isLeader func() bool) {
	h.gate.Store(&leaderGate{enabled: enabled, isLeader: isLeader})
}

// lostLeadership mirrors TopologyHandler.lostLeadership.
func (h *PrometheusSDHandler) lostLeadership() bool {
	g := h.gate.Load()
	return g != nil && g.enabled && (g.isLeader == nil || !g.isLeader())
}

func (h *PrometheusSDHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	/* A standby's registry is empty by design. Prometheus reads a 200 as the complete new target
	   list, so [] from a standby would wipe every external target; on a non-200 it keeps the list
	   it has, which is exactly right until the next refresh lands on the leader. */
	if h.lostLeadership() {
		http.Error(w, "not the leader", http.StatusServiceUnavailable)
		return
	}

	groups := h.targetGroups(h.registry.GetAll())

	w.Header().Set("Content-Type", "application/json")
	// Every refresh must see the registry as it is now; a cached body would outlive an eviction.
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(groups); err != nil {
		http.Error(w, "failed to encode targets", http.StatusInternalServerError)
	}
}

/*
targetGroups turns the registry snapshot into one group per external agent, ordered by node name
and deduplicated by target address (a rolling restart on a host can briefly leave two records at
one address, which Prometheus would scrape twice under two label sets).

The label set is FIXED. An agent owns its own labels map (identity.go), so copying it through
would let a host inject arbitrary target labels, __address__ included, into Prometheus.
*/
func (h *PrometheusSDHandler) targetGroups(agents []model.AgentInfo) []targetGroup {
	external := make([]model.AgentInfo, 0, len(agents))
	for i := range agents {
		if agents[i].IsExternal() {
			external = append(external, agents[i])
		}
	}
	sort.Slice(external, func(i, j int) bool {
		if external[i].NodeName != external[j].NodeName {
			return external[i].NodeName < external[j].NodeName
		}
		return external[i].ID < external[j].ID
	})

	// A non-nil slice: an empty fleet must encode as the literal [], never null.
	groups := make([]targetGroup, 0, len(external))
	seen := make(map[string]struct{}, len(external))
	for i := range external {
		a := &external[i]
		target := net.JoinHostPort(a.PodIP, strconv.Itoa(h.portFor(a)))
		if _, dup := seen[target]; dup {
			continue
		}
		seen[target] = struct{}{}
		groups = append(groups, targetGroup{
			Targets: []string{target},
			Labels: map[string]string{
				"node":     a.NodeName,
				"zone":     a.Zone,
				"external": "true",
				"agent_id": a.ID,
			},
		})
	}
	return groups
}

// portFor is the agent's reported metrics port, or the controller's own when the agent reported
// none. The fallback is logged once per agent: a host on a non-default port would otherwise sit
// silently at up == 0.
func (h *PrometheusSDHandler) portFor(a *model.AgentInfo) int {
	if a.MetricsPort > 0 {
		return a.MetricsPort
	}
	if _, loaded := h.assumed.LoadOrStore(a.ID, struct{}{}); !loaded {
		h.logger.Info("metrics port assumed from controller config",
			"agent", a.ID, "node", a.NodeName, "address", a.PodIP, "port", h.metricsPort)
	}
	return h.metricsPort
}

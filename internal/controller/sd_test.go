package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
	"github.com/prometheus/client_golang/prometheus"
)

const testControllerMetricsPort = 9091

func externalAgent(id, node, ip string, metricsPort int) model.AgentInfo {
	return model.AgentInfo{
		ID:          id,
		NodeName:    node,
		PodIP:       ip,
		Zone:        "external",
		Labels:      map[string]string{model.LabelExternal: "true"},
		MetricsPort: metricsPort,
	}
}

// newSDServer builds an HTTP server with the SD route enabled, the way controller.New wires it.
func newSDServer(t *testing.T, reg *Registry) *HTTPServer {
	t.Helper()
	srv := NewHTTPServer(reg, nil, prometheus.NewRegistry(), nil)
	srv.EnablePrometheusSD(testControllerMetricsPort)
	return srv
}

func getSD(t *testing.T, h http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(),
		http.MethodGet, "/api/v1/prometheus/sd", http.NoBody)
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeSD(t *testing.T, rec *httptest.ResponseRecorder) []targetGroup {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %q", rec.Code, rec.Body.String())
	}
	var groups []targetGroup
	if err := json.Unmarshal(rec.Body.Bytes(), &groups); err != nil {
		t.Fatalf("body is not a target-group list: %v; body %q", err, rec.Body.String())
	}
	return groups
}

// Prometheus insists on application/json and treats anything else as a failed refresh; an empty
// fleet must still be the literal [] because null is not a target list either.
func TestPrometheusSDEmptyRegistryIsEmptyList(t *testing.T) {
	srv := newSDServer(t, NewRegistry(30*time.Second))

	rec := getSD(t, srv.Handler())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Errorf("body = %q, want []", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}

func TestPrometheusSDFiltersInClusterAgents(t *testing.T) {
	reg := NewRegistry(30 * time.Second)
	reg.Register(model.AgentInfo{ID: "pod-1", NodeName: "node-1", PodIP: "10.0.0.1", MetricsPort: 9091})
	reg.Register(model.AgentInfo{ID: "pod-2", NodeName: "node-2", PodIP: "10.0.0.2",
		Labels: map[string]string{model.LabelHostNetwork: "true"}})
	reg.Register(externalAgent("edge-1-edge-1", "edge-1", "10.20.30.40", 9091))
	srv := newSDServer(t, reg)

	groups := decodeSD(t, getSD(t, srv.Handler()))

	if len(groups) != 1 {
		t.Fatalf("groups = %+v, want exactly the external agent", groups)
	}
	if groups[0].Labels["node"] != "edge-1" {
		t.Errorf("node = %q, want edge-1", groups[0].Labels["node"])
	}
}

// The label set is fixed: an agent owns its own labels map, so passing it through would let a
// host inject arbitrary target labels (or __address__) into Prometheus.
func TestPrometheusSDExternalTargetAndFixedLabels(t *testing.T) {
	reg := NewRegistry(30 * time.Second)
	a := externalAgent("edge-host-01-edge-host-01", "edge-host-01", "10.20.30.40", 9191)
	a.Labels["__address__"] = "evil:1"
	a.Labels["team"] = "ops"
	reg.Register(a)
	srv := newSDServer(t, reg)

	groups := decodeSD(t, getSD(t, srv.Handler()))

	if len(groups) != 1 {
		t.Fatalf("groups = %+v, want one", groups)
	}
	g := groups[0]
	if len(g.Targets) != 1 || g.Targets[0] != "10.20.30.40:9191" {
		t.Errorf("targets = %v, want [10.20.30.40:9191]", g.Targets)
	}
	want := map[string]string{
		"node":     "edge-host-01",
		"zone":     "external",
		"external": "true",
		"agent_id": "edge-host-01-edge-host-01",
	}
	if len(g.Labels) != len(want) {
		t.Errorf("labels = %v, want exactly %v", g.Labels, want)
	}
	for k, v := range want {
		if g.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, g.Labels[k], v)
		}
	}
}

func TestPrometheusSDIPv6Target(t *testing.T) {
	reg := NewRegistry(30 * time.Second)
	reg.Register(externalAgent("v6-v6", "v6", "fd00::1", 9091))
	srv := newSDServer(t, reg)

	groups := decodeSD(t, getSD(t, srv.Handler()))

	if len(groups) != 1 || groups[0].Targets[0] != "[fd00::1]:9091" {
		t.Errorf("groups = %+v, want target [fd00::1]:9091", groups)
	}
}

func TestPrometheusSDSortedByNodeName(t *testing.T) {
	reg := NewRegistry(30 * time.Second)
	reg.Register(externalAgent("c-c", "c-node", "10.0.0.3", 9091))
	reg.Register(externalAgent("a-a", "a-node", "10.0.0.1", 9091))
	reg.Register(externalAgent("b-b", "b-node", "10.0.0.2", 9091))
	srv := newSDServer(t, reg)

	groups := decodeSD(t, getSD(t, srv.Handler()))

	nodes := make([]string, 0, len(groups))
	for _, g := range groups {
		nodes = append(nodes, g.Labels["node"])
	}
	if strings.Join(nodes, ",") != "a-node,b-node,c-node" {
		t.Errorf("order = %v, want a-node,b-node,c-node", nodes)
	}
}

// A rolling restart on a host can leave two records at one address; Prometheus would scrape it
// twice under two label sets, so the first by node name wins.
func TestPrometheusSDDedupesByTargetAddress(t *testing.T) {
	reg := NewRegistry(30 * time.Second)
	reg.Register(externalAgent("host-b", "host-b", "10.20.30.40", 9091))
	reg.Register(externalAgent("host-a", "host-a", "10.20.30.40", 9091))
	srv := newSDServer(t, reg)

	groups := decodeSD(t, getSD(t, srv.Handler()))

	if len(groups) != 1 {
		t.Fatalf("groups = %+v, want one group for the shared address", groups)
	}
	if groups[0].Labels["node"] != "host-a" {
		t.Errorf("kept node = %q, want host-a (first by node name)", groups[0].Labels["node"])
	}
}

// A 2.3.x host agent reports no ports; the fleet-wide metricsPort the controller runs on is the
// honest fallback, and it is logged once per agent so a host on another port is diagnosable.
func TestPrometheusSDPortFromAgentOrControllerFallback(t *testing.T) {
	reg := NewRegistry(30 * time.Second)
	reg.Register(externalAgent("new-new", "new", "10.0.0.1", 9191))
	reg.Register(externalAgent("old-old", "old", "10.0.0.2", 0))
	srv := newSDServer(t, reg)

	var logs bytes.Buffer
	srv.SDHandler().logger = slog.New(slog.NewTextHandler(&logs, nil))

	groups := decodeSD(t, getSD(t, srv.Handler()))
	groups2 := decodeSD(t, getSD(t, srv.Handler()))

	targets := map[string]string{}
	for _, g := range groups {
		targets[g.Labels["node"]] = g.Targets[0]
	}
	if targets["new"] != "10.0.0.1:9191" {
		t.Errorf("reported port: target = %q, want 10.0.0.1:9191", targets["new"])
	}
	if targets["old"] != "10.0.0.2:9091" {
		t.Errorf("assumed port: target = %q, want 10.0.0.2:9091", targets["old"])
	}
	if len(groups2) != len(groups) {
		t.Errorf("second refresh returned %d groups, want %d", len(groups2), len(groups))
	}

	assumed := strings.Count(logs.String(), "metrics port assumed from controller config")
	if assumed != 1 {
		t.Errorf("assumed-port log lines = %d over two refreshes, want exactly 1; logs:\n%s", assumed, logs.String())
	}
	if strings.Contains(logs.String(), "agent=new-new") {
		t.Errorf("an agent that reported its port must not be logged as assumed; logs:\n%s", logs.String())
	}
}

// A standby's registry is empty; answering it with 200 [] would make Prometheus drop every
// target, so the SD route shares the topology route's gate and answers 503 instead.
func TestPrometheusSDLeaderGate(t *testing.T) {
	tests := []struct {
		name     string
		enabled  bool
		isLeader func() bool
		want     int
	}{
		{"non-leader is refused", true, func() bool { return false }, http.StatusServiceUnavailable},
		{"leader serves", true, func() bool { return true }, http.StatusOK},
		{"no leader election serves", false, nil, http.StatusOK},
		{"missing callback is refused", true, nil, http.StatusServiceUnavailable},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reg := NewRegistry(30 * time.Second)
			reg.Register(externalAgent("edge-edge", "edge", "10.0.0.1", 9091))
			srv := newSDServer(t, reg)
			srv.SetLeaderGate(tc.enabled, tc.isLeader)

			rec := getSD(t, srv.Handler())

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
			if tc.want == http.StatusServiceUnavailable && !strings.Contains(rec.Body.String(), "not the leader") {
				t.Errorf("body = %q, want 'not the leader'", rec.Body.String())
			}
		})
	}
}

// The gate must hold whichever order the controller wires things in: a gate set before the route
// is enabled would otherwise be lost, and a standby would answer [].
func TestPrometheusSDLeaderGateSurvivesEnableOrder(t *testing.T) {
	reg := NewRegistry(30 * time.Second)
	srv := NewHTTPServer(reg, nil, prometheus.NewRegistry(), nil)
	srv.SetLeaderGate(true, func() bool { return false })
	srv.EnablePrometheusSD(testControllerMetricsPort)

	if rec := getSD(t, srv.Handler()); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 from a gate set before EnablePrometheusSD", rec.Code)
	}
}

// controller.prometheusSD.enabled=false: the route is simply not there, on either listener.
func TestPrometheusSDDisabledIs404(t *testing.T) {
	reg := NewRegistry(30 * time.Second)
	reg.Register(externalAgent("edge-edge", "edge", "10.0.0.1", 9091))
	srv := NewHTTPServer(reg, nil, prometheus.NewRegistry(), nil)

	if rec := getSD(t, srv.Handler()); rec.Code != http.StatusNotFound {
		t.Errorf("API mux status = %d, want 404 when SD is not enabled", rec.Code)
	}
	if srv.SDHandler() != nil {
		t.Errorf("SDHandler() = %v, want nil when SD is not enabled", srv.SDHandler())
	}
	metricsMux := metrics.NewListenerHandler(prometheus.NewRegistry(), srv.Ready)
	if rec := getSD(t, metricsMux); rec.Code != http.StatusNotFound {
		t.Errorf("metrics mux status = %d, want 404 when SD is not mounted", rec.Code)
	}
}

// The chart's NetworkPolicy opens metricsPort to Prometheus and nothing else, so the SD body has
// to be reachable there, served by the very same handler instance the API mux uses.
func TestPrometheusSDServedByMetricsListener(t *testing.T) {
	reg := NewRegistry(30 * time.Second)
	reg.Register(externalAgent("edge-edge", "edge", "10.20.30.40", 9091))
	srv := newSDServer(t, reg)

	metricsMux := metrics.NewListenerHandler(prometheus.NewRegistry(), srv.Ready,
		metrics.Route{Pattern: "GET " + prometheusSDPath, Handler: srv.SDHandler()})

	groups := decodeSD(t, getSD(t, metricsMux))
	if len(groups) != 1 || groups[0].Targets[0] != "10.20.30.40:9091" {
		t.Fatalf("groups = %+v, want the external target via the metrics listener", groups)
	}

	// And the gate applies there too: one handler, one gate.
	srv.SetLeaderGate(true, func() bool { return false })
	if rec := getSD(t, metricsMux); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("metrics mux status after demotion = %d, want 503", rec.Code)
	}
}

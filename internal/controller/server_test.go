package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
	"github.com/prometheus/client_golang/prometheus"
)

func TestHealthzEndpoint(t *testing.T) {
	reg := NewRegistry(30 * time.Second)
	promReg := prometheus.NewRegistry()
	srv := NewHTTPServer(reg, nil, promReg, nil)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/healthz", http.NoBody)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "ok" {
		t.Errorf("expected 'ok', got %q", w.Body.String())
	}
}

func TestReadyzEndpointNotReady(t *testing.T) {
	reg := NewRegistry(30 * time.Second)
	promReg := prometheus.NewRegistry()
	srv := NewHTTPServer(reg, nil, promReg, nil)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", http.NoBody)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 before SetReady, got %d", w.Code)
	}
}

func TestReadyzEndpointReady(t *testing.T) {
	reg := NewRegistry(30 * time.Second)
	promReg := prometheus.NewRegistry()
	srv := NewHTTPServer(reg, nil, promReg, nil)
	srv.SetReady(true)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", http.NoBody)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 after SetReady, got %d", w.Code)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	reg := NewRegistry(30 * time.Second)
	promReg := prometheus.NewRegistry()
	srv := NewHTTPServer(reg, nil, promReg, nil)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", http.NoBody)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestTopologyEndpoint(t *testing.T) {
	reg := NewRegistry(30 * time.Second)
	reg.Register(model.AgentInfo{ID: "a1", NodeName: "node-1", Zone: "zone-a"})
	reg.Register(model.AgentInfo{ID: "a2", NodeName: "node-2", Zone: "zone-b"})

	promReg := prometheus.NewRegistry()
	srv := NewHTTPServer(reg, nil, promReg, nil)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/topology", http.NoBody)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var topo model.TopologySnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &topo); err != nil {
		t.Fatal(err)
	}

	if len(topo.Agents) != 2 {
		t.Errorf("expected 2 agents in topology, got %d", len(topo.Agents))
	}
}

func TestTopologyEndpointWithNodeWatcher(t *testing.T) {
	reg := NewRegistry(30 * time.Second)
	promReg := prometheus.NewRegistry()
	srv := NewHTTPServer(reg, nil, promReg, nil)

	nw := &NodeWatcher{
		nodes: map[string]model.NodeInfo{
			"node-1": {Name: "node-1", Zone: "zone-a", Ready: true},
			"node-2": {Name: "node-2", Zone: "zone-b", Ready: true},
		},
		failureDomainLabel: "topology.kubernetes.io/zone",
		stopCh:             make(chan struct{}),
	}
	srv.SetNodeWatcher(nw)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/topology", http.NoBody)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var topo model.TopologySnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &topo); err != nil {
		t.Fatal(err)
	}

	if len(topo.Nodes) != 2 {
		t.Errorf("expected 2 nodes in topology after SetNodeWatcher, got %d", len(topo.Nodes))
	}
}

func TestVersionEndpoint(t *testing.T) {
	reg := NewRegistry(30 * time.Second)
	promReg := prometheus.NewRegistry()
	srv := NewHTTPServer(reg, nil, promReg, nil)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/version", http.NoBody)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	// map[string]any, not map[string]string: the payload now carries the
	// capabilities array alongside the string fields.
	var version map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &version); err != nil {
		t.Fatal(err)
	}

	if _, ok := version["version"]; !ok {
		t.Error("expected version field in response")
	}
}

func TestHandleVersionAdvertisesEventsCapability(t *testing.T) {
	reg := NewRegistry(30 * time.Second)
	s := NewHTTPServer(reg, nil, prometheus.NewRegistry(), []string{"events"})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/version", http.NoBody)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	var body struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Capabilities) != 1 || body.Capabilities[0] != "events" {
		t.Errorf("expected capabilities=[events], got %v", body.Capabilities)
	}
}

// TestHandleVersionCapabilitiesIsEmptyArrayNotNull asserts on the raw response
// body, not on a decoded []string: JSON null and [] both decode to a nil slice,
// so only the bytes can prove the empty-array normalization is still in place.
func TestHandleVersionCapabilitiesIsEmptyArrayNotNull(t *testing.T) {
	// Both paths must serialize as []: an explicit nil (normalized by
	// NewHTTPServer) and the real wiring with events turned off.
	cases := []struct {
		name string
		caps []string
	}{
		{name: "nil capabilities", caps: nil},
		{name: "events disabled in config", caps: capabilitiesFor(&config.Config{})},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := NewRegistry(30 * time.Second)
			s := NewHTTPServer(reg, nil, prometheus.NewRegistry(), tc.caps)

			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/version", http.NoBody)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, req)

			body := w.Body.String()
			if !strings.Contains(body, `"capabilities":[]`) {
				t.Errorf(`expected raw body to contain "capabilities":[], got %s`, body)
			}
			if strings.Contains(body, `"capabilities":null`) {
				t.Errorf("capabilities must never serialize as null, got %s", body)
			}
		})
	}
}

func TestHandleVersionNoCapabilitiesWhenDisabled(t *testing.T) {
	reg := NewRegistry(30 * time.Second)
	s := NewHTTPServer(reg, nil, prometheus.NewRegistry(), nil)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/version", http.NoBody)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	var body struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Capabilities) != 0 {
		t.Errorf("expected no capabilities, got %v", body.Capabilities)
	}
}

// TestTopologyEndpointLeaderGate closes the other half of the split-brain fix: a standby holds no
// agents, so answering 200 with an empty snapshot would make the Console's topology page a coin
// flip between the leader's view and nothing at all.
func TestTopologyEndpointLeaderGate(t *testing.T) {
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
			reg.Register(model.AgentInfo{ID: "agent-1", NodeName: "node-1"})
			srv := NewHTTPServer(reg, nil, prometheus.NewRegistry(), nil)
			srv.SetLeaderGate(tc.enabled, tc.isLeader)

			req := httptest.NewRequestWithContext(context.Background(),
				http.MethodGet, "/api/v1/topology", http.NoBody)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

// TestTopologyEndpointUngatedByDefault pins that a handler nobody gated still serves, so the
// existing constructions keep their behaviour.
func TestTopologyEndpointUngatedByDefault(t *testing.T) {
	reg := NewRegistry(30 * time.Second)
	reg.Register(model.AgentInfo{ID: "agent-1", NodeName: "node-1"})
	srv := NewHTTPServer(reg, nil, prometheus.NewRegistry(), nil)

	req := httptest.NewRequestWithContext(context.Background(),
		http.MethodGet, "/api/v1/topology", http.NoBody)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

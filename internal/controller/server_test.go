package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/model"
	"github.com/prometheus/client_golang/prometheus"
)

func TestHealthzEndpoint(t *testing.T) {
	reg := NewRegistry(30 * time.Second)
	promReg := prometheus.NewRegistry()
	srv := NewHTTPServer(reg, nil, promReg)

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
	srv := NewHTTPServer(reg, nil, promReg)

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
	srv := NewHTTPServer(reg, nil, promReg)
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
	srv := NewHTTPServer(reg, nil, promReg)

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
	srv := NewHTTPServer(reg, nil, promReg)

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
	srv := NewHTTPServer(reg, nil, promReg)

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
	srv := NewHTTPServer(reg, nil, promReg)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/version", http.NoBody)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var version map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &version); err != nil {
		t.Fatal(err)
	}

	if _, ok := version["version"]; !ok {
		t.Error("expected version field in response")
	}
}

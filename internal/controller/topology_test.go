package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/EsDmitrii/kconmon-ng/internal/controller/meshplan"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

func topologyGET(t *testing.T, h http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/topology", http.NoBody)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/topology: status %d: %s", rec.Code, rec.Body)
	}
	return rec
}

func registerInfo(reg *Registry, id, node string) {
	reg.Register(model.AgentInfo{ID: id, NodeName: node, PodIP: "10.0.0.1"})
}

// TestTopologySnapshotOmitsPlanWithoutASource pins back-compat at the handler level: a handler
// nobody wired a plan source into (every pre-M10 embedding) must serve a body with NO probePlan
// key at all — not null, not {}.
func TestTopologySnapshotOmitsPlanWithoutASource(t *testing.T) {
	reg := NewRegistry(time.Minute)
	registerInfo(reg, "agent-a", "node-1")
	h := NewTopologyHandler(reg, nil)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(topologyGET(t, h).Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := raw["probePlan"]; ok {
		t.Fatalf("probePlan present without a plan source: %s", raw["probePlan"])
	}
}

// TestTopologySnapshotOmitsPlanWhenSourceSaysFullMesh: a wired source answering nil (mode=full,
// below autoThreshold, or a demoted standby) serves the same body as no source at all.
func TestTopologySnapshotOmitsPlanWhenSourceSaysFullMesh(t *testing.T) {
	reg := NewRegistry(time.Minute)
	registerInfo(reg, "agent-a", "node-1")
	h := NewTopologyHandler(reg, nil)
	h.SetPlanSource(func() meshplan.Plan { return nil })

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(topologyGET(t, h).Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := raw["probePlan"]; ok {
		t.Fatalf("probePlan present although the source answers full mesh: %s", raw["probePlan"])
	}
}

/*
TestTopologySnapshotCarriesThePlanByNodeName pins the compact form the Console renders: the
agent-ID plan is translated to node names through the SAME registry snapshot the body's agents
come from, so the matrix can join on the names it already draws. Also pinned:

  - an agent the plan fails to mention gets an EMPTY list (the plan's own fail-closed contract),
    not a missing key — the row must read "probes nobody", not "unknown";
  - a plan entry whose agent is no longer registered is dropped, and so is a destination that
    resolves to no registered node — the surfaced plan never names nodes the snapshot does not;
  - two agents on one node (rolling-restart overlap) merge into one deduplicated, sorted list,
    and a same-node pair is dropped: the matrix has no cell for node → itself.
*/
func TestTopologySnapshotCarriesThePlanByNodeName(t *testing.T) {
	reg := NewRegistry(time.Minute)
	registerInfo(reg, "agent-a1", "node-1")
	registerInfo(reg, "agent-a2", "node-1") // second agent on the same node
	registerInfo(reg, "agent-b", "node-2")
	registerInfo(reg, "agent-c", "node-3")
	registerInfo(reg, "agent-d", "node-4") // registered but absent from the plan

	h := NewTopologyHandler(reg, nil)
	h.SetPlanSource(func() meshplan.Plan {
		return meshplan.Plan{
			"agent-a1":   {"agent-b", "agent-c"},
			"agent-a2":   {"agent-a1", "agent-c"}, // agent-a1 is node-1 too: self pair, dropped
			"agent-b":    {"agent-c", "agent-gone"},
			"agent-c":    {"agent-a1"},
			"agent-gone": {"agent-b"}, // no longer registered: dropped entirely
		}
	})

	var snap model.TopologySnapshot
	if err := json.Unmarshal(topologyGET(t, h).Body.Bytes(), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string][]string{
		"node-1": {"node-2", "node-3"},
		"node-2": {"node-3"},
		"node-3": {"node-1"},
		"node-4": {},
	}
	if len(snap.ProbePlan) != len(want) {
		t.Fatalf("probePlan = %v, want %v", snap.ProbePlan, want)
	}
	for node, peers := range want {
		got, ok := snap.ProbePlan[node]
		if !ok {
			t.Fatalf("probePlan misses %s: %v", node, snap.ProbePlan)
		}
		if !equalIDs(got, peers) {
			t.Fatalf("probePlan[%s] = %v, want %v", node, got, peers)
		}
	}
}

// TestTopologySnapshotPlanEmptyListMarshalsAsArray: the fail-closed empty list must reach the wire
// as [], never null — the web indexes into it.
func TestTopologySnapshotPlanEmptyListMarshalsAsArray(t *testing.T) {
	reg := NewRegistry(time.Minute)
	registerInfo(reg, "agent-a", "node-1")
	h := NewTopologyHandler(reg, nil)
	h.SetPlanSource(func() meshplan.Plan { return meshplan.Plan{} })

	body := topologyGET(t, h).Body.String()
	if want := `"probePlan":{"node-1":[]}`; !strings.Contains(body, want) {
		t.Fatalf("body %s does not contain %s", body, want)
	}
}

// TestSetPlanSourceIsHotSwapSafe hammers SetPlanSource against ServeHTTP under -race, the same
// guarantee SetNodeWatcher documents.
func TestSetPlanSourceIsHotSwapSafe(t *testing.T) {
	reg := NewRegistry(time.Minute)
	registerInfo(reg, "agent-a", "node-1")
	h := NewTopologyHandler(reg, nil)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			h.SetPlanSource(func() meshplan.Plan { return meshplan.Plan{"agent-a": {}} })
			h.SetPlanSource(func() meshplan.Plan { return nil })
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/topology", http.NoBody)
			h.ServeHTTP(httptest.NewRecorder(), req)
		}
	}()
	wg.Wait()
}

/*
TestControllerTopologyEndpointServesTheSparsePlan is the "plan reaches the operator" claim,
end-to-end through the production wiring: controller.New in sparse mode, agents registered through
the real gRPC Register path, and the HTTP endpoint the Console polls — no test-only plan source.
Ring degree 1 over node order gives node-1→node-2→node-3→node-1.
*/
func TestControllerTopologyEndpointServesTheSparsePlan(t *testing.T) {
	cfg := &config.Config{MetricsPrefix: "test"}
	cfg.Controller.AgentTTL = time.Minute
	cfg.Controller.LeaderElection = false
	cfg.Topology = config.TopologyConfig{
		Mode:   config.TopologyModeSparse,
		Sparse: config.SparseTopologyConfig{RingDegree: 1, ZoneChords: 0},
	}

	c := New(cfg)
	registerAgent(t, c.grpcServer, "agent-a", "node-1", "10.0.0.1")
	registerAgent(t, c.grpcServer, "agent-b", "node-2", "10.0.0.2")
	registerAgent(t, c.grpcServer, "agent-c", "node-3", "10.0.0.3")

	var snap model.TopologySnapshot
	if err := json.Unmarshal(topologyGET(t, c.httpServer.Handler()).Body.Bytes(), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string][]string{
		"node-1": {"node-2"},
		"node-2": {"node-3"},
		"node-3": {"node-1"},
	}
	if len(snap.ProbePlan) != len(want) {
		t.Fatalf("probePlan = %v, want %v", snap.ProbePlan, want)
	}
	for node, peers := range want {
		if !equalIDs(snap.ProbePlan[node], peers) {
			t.Fatalf("probePlan[%s] = %v, want %v", node, snap.ProbePlan[node], peers)
		}
	}
}

// TestControllerTopologyEndpointOmitsPlanInFullMode pins back-compat through the same production
// wiring: the default mode's body carries no probePlan key, so a full-mesh fleet's payload is
// byte-identical to pre-M10.
func TestControllerTopologyEndpointOmitsPlanInFullMode(t *testing.T) {
	cfg := &config.Config{MetricsPrefix: "test"}
	cfg.Controller.AgentTTL = time.Minute
	cfg.Controller.LeaderElection = false

	c := New(cfg)
	registerAgent(t, c.grpcServer, "agent-a", "node-1", "10.0.0.1")
	registerAgent(t, c.grpcServer, "agent-b", "node-2", "10.0.0.2")

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(topologyGET(t, c.httpServer.Handler()).Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := raw["probePlan"]; ok {
		t.Fatalf("probePlan present in full mode: %s", raw["probePlan"])
	}
}

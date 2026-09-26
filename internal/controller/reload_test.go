package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

type reloadLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *reloadLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *reloadLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureReloadLogs(t *testing.T) *reloadLogBuffer {
	t.Helper()
	logs := &reloadLogBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return logs
}

func cloneControllerConfig(cfg *config.Config) *config.Config {
	c := *cfg
	return &c
}

func versionBody(t *testing.T, c *Controller) (caps, cidrs []string) {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/version", http.NoBody)
	w := httptest.NewRecorder()
	c.httpServer.Handler().ServeHTTP(w, req)
	var body struct {
		Capabilities []string `json:"capabilities"`
		CIDRs        []string `json:"externalAllowedCidrs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding /api/v1/version: %v (%s)", err, w.Body.String())
	}
	return body.Capabilities, body.CIDRs
}

func TestControllerReloadRestartBoundChangeWarnsAndChangesNothing(t *testing.T) {
	logs := captureReloadLogs(t)
	cfg := config.DefaultConfig()
	c := New(cfg)

	next := cloneControllerConfig(cfg)
	next.GRPCPort++
	next.MetricsPrefix = "other_prefix"
	next.Controller.Events.Enabled = true
	next.Controller.ExternalGateway.Port = 9555
	next.Checkers.TCP.Interval = time.Minute // agent-only: the controller says nothing about it
	next.Agent.NodeName = "somewhere"        // agent-only
	c.ApplyConfig(next)

	out := logs.String()
	if n := strings.Count(out, "level=WARN"); n != 1 || !strings.Contains(out, "restart") {
		t.Fatalf("want exactly one WARN asking for a restart, got %d:\n%s", n, out)
	}
	for _, key := range []string{"controller.events.enabled", "controller.externalGateway.port", "grpcPort", "metricsPrefix"} {
		if !strings.Contains(out, key) {
			t.Errorf("the restart warning does not name %s:\n%s", key, out)
		}
	}
	for _, key := range []string{"checkers.tcp.interval", "agent.nodeName"} {
		if strings.Contains(out, key) {
			t.Errorf("an agent-only key %s is named in the controller's warning:\n%s", key, out)
		}
	}
	if got := c.appliedConfig(); got.GRPCPort != cfg.GRPCPort || got.Controller.Events.Enabled {
		t.Errorf("restart-bound values were applied: grpcPort=%d events=%v", got.GRPCPort, got.Controller.Events.Enabled)
	}
	if caps, _ := versionBody(t, c); slices.Contains(caps, "events") {
		t.Errorf("events became a capability without the restart that starts the stream: %v", caps)
	}
}

// A restart changes nothing for a key no code reads, so asking for one would be a false instruction.
func TestControllerReloadIgnoresUnreadKeys(t *testing.T) {
	for name, edit := range map[string]func(*config.Config){
		"mode": func(c *config.Config) { c.Mode = "controller" },
		"observability.otel": func(c *config.Config) {
			c.Observability.OTel.Enabled = !c.Observability.OTel.Enabled
			c.Observability.OTel.Endpoint = "otel:4317"
		},
	} {
		t.Run(name, func(t *testing.T) {
			logs := captureReloadLogs(t)
			cfg := config.DefaultConfig()
			c := New(cfg)

			next := cloneControllerConfig(cfg)
			edit(next)
			c.ApplyConfig(next)
			if out := logs.String(); strings.Contains(out, "level=WARN") {
				t.Fatalf("a change to the unread %s keys asked for a restart:\n%s", name, out)
			}
		})
	}
}

func TestControllerReloadAppliesExternalAllowedCIDRs(t *testing.T) {
	cfg := config.DefaultConfig()
	c := New(cfg)
	if _, cidrs := versionBody(t, c); len(cidrs) != 0 {
		t.Fatalf("external checks are off, yet /api/v1/version lists %v", cidrs)
	}

	on := cloneControllerConfig(cfg)
	on.Checkers.External.Enabled = true
	on.Checkers.External.AllowedCIDRs = []string{"10.0.0.0/8", "192.168.0.0/16"}
	c.ApplyConfig(on)
	if _, cidrs := versionBody(t, c); !slices.Equal(cidrs, on.Checkers.External.AllowedCIDRs) {
		t.Fatalf("after the reload /api/v1/version lists %v, want %v", cidrs, on.Checkers.External.AllowedCIDRs)
	}

	off := cloneControllerConfig(on)
	off.Checkers.External.Enabled = false
	c.ApplyConfig(off)
	if _, cidrs := versionBody(t, c); len(cidrs) != 0 {
		t.Fatalf("external checks were switched off, yet /api/v1/version lists %v", cidrs)
	}
}

func TestControllerReloadReplansTopology(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Controller.LeaderElection = false // the only replica leads: only a leader plans
	c := New(cfg)
	for i := range 6 {
		c.registry.Register(model.AgentInfo{
			ID: fmt.Sprintf("a%d", i), NodeName: fmt.Sprintf("n%d", i), PodIP: fmt.Sprintf("10.0.0.%d", i+1),
		})
	}
	if plan := c.ProbePlan(); plan != nil {
		t.Fatalf("full mesh has a plan: %v", plan)
	}

	sparse := cloneControllerConfig(cfg)
	sparse.Topology = config.TopologyConfig{Mode: config.TopologyModeSparse,
		Sparse: config.SparseTopologyConfig{RingDegree: 1}}
	c.ApplyConfig(sparse)
	plan := c.ProbePlan()
	if len(plan) != 6 {
		t.Fatalf("sparse plan after the reload covers %d agents, want 6: %v", len(plan), plan)
	}
	for id, peers := range plan {
		if len(peers) != 1 {
			t.Errorf("agent %s probes %v under ringDegree 1, want one ring successor", id, peers)
		}
	}

	// The next registry change plans with the reloaded topology, not the startup one.
	c.registry.Register(model.AgentInfo{ID: "a6", NodeName: "n6", PodIP: "10.0.0.7"})
	if plan := c.ProbePlan(); len(plan) != 7 {
		t.Errorf("a registration after the reload planned %d agents, want the sparse plan over 7", len(plan))
	}

	c.ApplyConfig(cfg)
	if plan := c.ProbePlan(); plan != nil {
		t.Errorf("back to full mesh, the plan is still %v", plan)
	}
}

func TestControllerReloadAppliesAgentTTL(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Controller.AgentTTL = time.Hour
	c := New(cfg)
	c.registry.Register(model.AgentInfo{ID: "a1", NodeName: "n1", PodIP: "10.0.0.1"})
	c.registry.mu.Lock()
	c.registry.agents["a1"].lastSeen = time.Now().Add(-time.Minute)
	c.registry.mu.Unlock()
	if n := c.registry.EvictStale(); n != 0 {
		t.Fatalf("evicted %d agents under a 1h TTL", n)
	}

	next := cloneControllerConfig(cfg)
	next.Controller.AgentTTL = 30 * time.Second
	c.ApplyConfig(next)
	if n := c.registry.EvictStale(); n != 1 {
		t.Fatalf("evicted %d agents after the TTL was reloaded to 30s, want the one silent for a minute", n)
	}
	select {
	case every := <-c.evictEvery:
		if every != 15*time.Second {
			t.Errorf("sweep period after the reload = %v, want half the TTL", every)
		}
	default:
		t.Error("the eviction sweep was not told about the new TTL")
	}
}

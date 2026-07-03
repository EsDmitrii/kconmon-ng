package controller

import (
	"context"
	"testing"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func newTestGRPCServer(ttl time.Duration) (*GRPCServer, *Registry) {
	reg := NewRegistry(ttl)
	m := metrics.NewPrometheusMetrics("test", prometheus.NewRegistry())
	return NewGRPCServer(reg, m), reg
}

func TestGRPCServerDeregisterRemovesAgent(t *testing.T) {
	srv, reg := newTestGRPCServer(30 * time.Second)

	reg.Register(model.AgentInfo{ID: "agent-1", NodeName: "node-1"})
	reg.Register(model.AgentInfo{ID: "agent-2", NodeName: "node-2"})
	srv.metrics.ControllerRegisteredAgents.WithLabelValues().Set(float64(reg.Count()))

	_, err := srv.Deregister(context.Background(), &pb.DeregisterRequest{AgentId: "agent-1"})
	if err != nil {
		t.Fatalf("Deregister returned error: %v", err)
	}

	if reg.Count() != 1 {
		t.Errorf("expected 1 agent after deregister, got %d", reg.Count())
	}
	if got := testutil.ToFloat64(srv.metrics.ControllerRegisteredAgents.WithLabelValues()); got != 1 {
		t.Errorf("expected registered-agents gauge to be 1, got %v", got)
	}
}

func TestGRPCServerDeregisterUnknownNoError(t *testing.T) {
	srv, reg := newTestGRPCServer(30 * time.Second)

	reg.Register(model.AgentInfo{ID: "agent-1", NodeName: "node-1"})
	srv.metrics.ControllerRegisteredAgents.WithLabelValues().Set(float64(reg.Count()))

	_, err := srv.Deregister(context.Background(), &pb.DeregisterRequest{AgentId: "does-not-exist"})
	if err != nil {
		t.Fatalf("Deregister of unknown agent should not error, got: %v", err)
	}

	if reg.Count() != 1 {
		t.Errorf("expected registry unchanged at 1 agent, got %d", reg.Count())
	}
	if got := testutil.ToFloat64(srv.metrics.ControllerRegisteredAgents.WithLabelValues()); got != 1 {
		t.Errorf("expected registered-agents gauge to stay 1, got %v", got)
	}
}

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
	"google.golang.org/grpc"
)

// fakeTaskStream implements grpc.ServerStreamingServer[pb.TaskRequest] for
// exercising WatchTasks without a real gRPC transport.
type fakeTaskStream struct {
	grpc.ServerStream
	ctx  context.Context
	sent chan *pb.TaskRequest
}

func newFakeTaskStream(ctx context.Context) *fakeTaskStream {
	return &fakeTaskStream{ctx: ctx, sent: make(chan *pb.TaskRequest, 16)}
}

func (f *fakeTaskStream) Context() context.Context     { return f.ctx }
func (f *fakeTaskStream) Send(t *pb.TaskRequest) error { f.sent <- t; return nil }

func newTestGRPCServer() (*GRPCServer, *Registry) {
	reg := NewRegistry(30 * time.Second)
	m := metrics.NewPrometheusMetrics("test", prometheus.NewRegistry())
	return NewGRPCServer(reg, m), reg
}

func TestGRPCServerDeregisterRemovesAgent(t *testing.T) {
	srv, reg := newTestGRPCServer()

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
	srv, reg := newTestGRPCServer()

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

func TestGRPCServerWatchTasksSubscribeCleanup(t *testing.T) {
	srv, _ := newTestGRPCServer()

	ctx, cancel := context.WithCancel(context.Background())
	stream := newFakeTaskStream(ctx)

	done := make(chan error, 1)
	go func() {
		done <- srv.WatchTasks(&pb.WatchTasksRequest{AgentId: "agent-1"}, stream)
	}()

	// Wait for the subscription to register.
	deadline := time.After(2 * time.Second)
	for srv.taskMgr.SubscriberCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("WatchTasks never registered a subscriber")
		case <-time.After(5 * time.Millisecond):
		}
	}

	if got := testutil.ToFloat64(srv.metrics.ControllerGRPCConnections.WithLabelValues()); got != 1 {
		t.Errorf("expected grpc connections gauge 1 while streaming, got %v", got)
	}

	// A dispatched task reaches the stream.
	go func() {
		_, _ = srv.taskMgr.Dispatch(context.Background(), "agent-1", &pb.TaskRequest{CheckType: "icmp"})
	}()
	select {
	case sent := <-stream.sent:
		if sent.GetCheckType() != "icmp" {
			t.Errorf("expected icmp task on stream, got %q", sent.GetCheckType())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dispatched task did not reach the stream")
	}

	// Closing the stream cleans up the subscription and the gauge.
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WatchTasks did not return after context cancel")
	}

	if got := srv.taskMgr.SubscriberCount(); got != 0 {
		t.Errorf("expected 0 subscribers after stream close, got %d", got)
	}
	if got := testutil.ToFloat64(srv.metrics.ControllerGRPCConnections.WithLabelValues()); got != 0 {
		t.Errorf("expected grpc connections gauge 0 after stream close, got %v", got)
	}
}

func TestGRPCServerReportTaskResultRoundtrip(t *testing.T) {
	srv, _ := newTestGRPCServer()

	sub, unsub := srv.taskMgr.Subscribe("agent-1")
	defer unsub()

	resultCh := make(chan *pb.TaskResult, 1)
	go func() {
		res, err := srv.taskMgr.Dispatch(context.Background(), "agent-1", &pb.TaskRequest{CheckType: "tcp"})
		if err != nil {
			t.Errorf("Dispatch error: %v", err)
		}
		resultCh <- res
	}()

	var task *pb.TaskRequest
	select {
	case task = <-sub:
	case <-time.After(2 * time.Second):
		t.Fatal("no task dispatched")
	}

	if _, err := srv.ReportTaskResult(context.Background(),
		&pb.TaskResult{TaskId: task.GetTaskId(), Success: true}); err != nil {
		t.Fatalf("ReportTaskResult error: %v", err)
	}

	select {
	case res := <-resultCh:
		if !res.GetSuccess() {
			t.Error("expected success result via ReportTaskResult")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Dispatch did not complete after ReportTaskResult")
	}
}

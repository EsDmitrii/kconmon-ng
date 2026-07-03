package agent

import (
	"context"
	"net"
	"testing"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/checker"
	"github.com/EsDmitrii/kconmon-ng/internal/controller"
	"github.com/EsDmitrii/kconmon-ng/internal/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// startTaskTestServer spins up a real controller GRPCServer over an in-memory
// bufconn listener and returns the server plus a GRPCClient dialled to it.
func startTaskTestServer(t *testing.T) (*controller.GRPCServer, *GRPCClient) {
	t.Helper()

	lis := bufconn.Listen(1 << 20)
	reg := controller.NewRegistry(30 * time.Second)
	m := metrics.NewPrometheusMetrics("test_"+t.Name(), prometheus.NewRegistry())
	srv := controller.NewGRPCServer(reg, m)

	gs := grpc.NewServer()
	srv.RegisterService(gs)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dialling bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	client := &GRPCClient{conn: conn, client: pb.NewAgentRegistryClient(conn), agentID: "agent-1"}
	return srv, client
}

// TestWatchTasksReceivesDispatchedTaskAndReportsResult exercises the full agent
// client path against a real controller: subscribe via WatchTasks, receive a
// dispatched task, run it through a TaskExecutor, and report the result back so
// the controller's Dispatch call returns.
func TestWatchTasksReceivesDispatchedTaskAndReportsResult(t *testing.T) {
	srv, client := startTaskTestServer(t)

	fc := &fakeChecker{name: model.CheckTCP, result: model.CheckResult{Success: true}}
	ex := NewTaskExecutor(
		map[model.CheckType]checker.Checker{model.CheckTCP: fc},
		nil,
		checker.Target{AgentID: "agent-1", NodeName: "node-a", Zone: "zone-a"},
		8080,
		client,
		4,
	)
	client.OnTask(func(taskCtx context.Context, task *pb.TaskRequest) {
		ex.Handle(taskCtx, task)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	watchErr := make(chan error, 1)
	go func() { watchErr <- client.WatchTasks(ctx) }()

	// Wait until the agent's subscription is registered on the controller.
	tm := srv.TaskManager()
	deadline := time.After(3 * time.Second)
	for tm.SubscriberCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("agent never subscribed to WatchTasks")
		case <-time.After(10 * time.Millisecond):
		}
	}

	dispatchCtx, dispatchCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer dispatchCancel()
	res, err := tm.Dispatch(dispatchCtx, "agent-1", &pb.TaskRequest{
		CheckType: "tcp",
		Target:    &pb.AgentMeta{NodeName: "node-b", PodIp: "10.0.0.2"},
		Plane:     "pod",
	})
	if err != nil {
		t.Fatalf("Dispatch failed: %v", err)
	}
	if !res.GetSuccess() {
		t.Errorf("expected success result, got error %q", res.GetError())
	}
	if res.GetAgentId() != "agent-1" {
		t.Errorf("agent_id not set: %q", res.GetAgentId())
	}
	if fc.callCount() != 1 {
		t.Errorf("expected checker to run once, ran %d", fc.callCount())
	}

	// Cancelling the context ends the stream; the reconnect discipline is that
	// WatchTasks returns an error the caller's loop uses to re-subscribe.
	cancel()
	select {
	case err := <-watchErr:
		if err == nil {
			t.Error("expected WatchTasks to return an error on stream teardown")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("WatchTasks did not return after context cancel")
	}
}

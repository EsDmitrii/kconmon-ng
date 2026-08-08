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
	// events disabled: this test only exercises the AgentRegistry task stream.
	srv := controller.NewGRPCServer(reg, m, false, nil, false)

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

// TestWatchExternalChecksReceivesAssignments exercises the agent client against
// a real controller: subscribe, receive the immediate current assignment (empty
// on a fresh controller — the send that tells a restarting agent to stop
// probing), then receive a pushed one.
func TestWatchExternalChecksReceivesAssignments(t *testing.T) {
	srv, client := startTaskTestServer(t)

	received := make(chan *pb.ExternalCheckAssignment, 4)
	client.OnExternalAssignment(func(a *pb.ExternalCheckAssignment) { received <- a })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	watchErr := make(chan error, 1)
	go func() { watchErr <- client.WatchExternalChecks(ctx) }()

	select {
	case a := <-received:
		if len(a.GetSpecs()) != 0 {
			t.Fatalf("a fresh controller must open with an EMPTY assignment, got %d specs", len(a.GetSpecs()))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no initial assignment received")
	}

	mgr := srv.ExternalCheckManager()
	mgr.Apply(map[string][]*pb.ExternalCheckSpec{
		"agent-1": {{
			DefinitionId: "def-1",
			Target:       &pb.ExternalTarget{Name: "dns-root", Kind: "host", Address: "10.0.0.53", Port: 53},
			CheckType:    "dns",
			IntervalNs:   int64(30 * time.Second),
			TimeoutNs:    int64(5 * time.Second),
			ParamsJson:   []byte(`{"query":"example.com"}`),
		}},
	})

	select {
	case a := <-received:
		if len(a.GetSpecs()) != 1 || a.GetSpecs()[0].GetDefinitionId() != "def-1" {
			t.Fatalf("pushed assignment not delivered intact: %+v", a.GetSpecs())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pushed assignment never arrived")
	}

	// Reconnect discipline: the stream returns an error the caller's loop uses
	// to re-subscribe, exactly as WatchTasks does.
	cancel()
	select {
	case err := <-watchErr:
		if err == nil {
			t.Error("expected WatchExternalChecks to return an error on stream teardown")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("WatchExternalChecks did not return after context cancel")
	}
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
		ExternalPolicy{},
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

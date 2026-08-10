package controller

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	grpcstatus "google.golang.org/grpc/status"
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

// fakePeerStream implements grpc.ServerStreamingServer[pb.PeerUpdate] for
// exercising WatchPeers without a real gRPC transport.
type fakePeerStream struct {
	grpc.ServerStream
	ctx  context.Context
	sent chan *pb.PeerUpdate
}

func newFakePeerStream(ctx context.Context) *fakePeerStream {
	return &fakePeerStream{ctx: ctx, sent: make(chan *pb.PeerUpdate, 16)}
}

func (f *fakePeerStream) Context() context.Context    { return f.ctx }
func (f *fakePeerStream) Send(u *pb.PeerUpdate) error { f.sent <- u; return nil }

func newTestGRPCServer() (*GRPCServer, *Registry) {
	reg := NewRegistry(30 * time.Second)
	m := metrics.NewPrometheusMetrics("test", prometheus.NewRegistry())
	// events disabled: these tests exercise the registry/task paths only.
	return NewGRPCServer(reg, m, false, nil, false), reg
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

// fakeEventStream implements grpc.ServerStreamingServer[pb.Event] for
// exercising WatchEvents without a real gRPC transport, mirroring
// fakeTaskStream above.
type fakeEventStream struct {
	grpc.ServerStream
	ctx  context.Context
	sent chan *pb.Event
}

func newFakeEventStream(ctx context.Context) *fakeEventStream {
	return &fakeEventStream{ctx: ctx, sent: make(chan *pb.Event, 16)}
}

func (f *fakeEventStream) Context() context.Context { return f.ctx }
func (f *fakeEventStream) Send(e *pb.Event) error   { f.sent <- e; return nil }

func TestGRPCServerWatchEventsRejectsNonLeader(t *testing.T) {
	reg := NewRegistry(30 * time.Second)
	m := metrics.NewPrometheusMetrics("test", prometheus.NewRegistry())
	srv := NewGRPCServer(reg, m, true, func() bool { return false }, true)

	err := srv.WatchEvents(&pb.WatchEventsRequest{}, newFakeEventStream(context.Background()))
	st, ok := grpcstatus.FromError(err)
	if !ok || st.Code() != codes.Unavailable {
		t.Fatalf("expected codes.Unavailable, got %v", err)
	}
}

func TestGRPCServerWatchEventsFanOutAndCleanup(t *testing.T) {
	reg := NewRegistry(30 * time.Second)
	m := metrics.NewPrometheusMetrics("test", prometheus.NewRegistry())
	srv := NewGRPCServer(reg, m, true, func() bool { return true }, true)

	ctx, cancel := context.WithCancel(context.Background())
	stream := newFakeEventStream(ctx)

	done := make(chan error, 1)
	go func() { done <- srv.WatchEvents(&pb.WatchEventsRequest{}, stream) }()

	deadline := time.After(2 * time.Second)
	for srv.EventSubscriberCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("WatchEvents never registered a subscriber")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if got := testutil.ToFloat64(m.ControllerEventSubscribers.WithLabelValues()); got != 1 {
		t.Errorf("expected event-subscribers gauge 1, got %v", got)
	}

	srv.PublishEvent(&pb.Event{Payload: &pb.Event_TopologyChanged{
		TopologyChanged: &pb.TopologyChanged{Reason: "agent_registered", NodeName: "node-1"},
	}})

	select {
	case ev := <-stream.sent:
		if ev.GetSeq() == 0 {
			t.Error("expected a non-zero seq assigned by PublishEvent")
		}
		if ev.GetTopologyChanged().GetNodeName() != "node-1" {
			t.Errorf("unexpected payload: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("published event did not reach the stream")
	}

	if got := testutil.ToFloat64(m.ControllerGRPCConnections.WithLabelValues()); got != 1 {
		t.Errorf("expected grpc connections gauge 1 while streaming events, got %v", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WatchEvents did not return after context cancel")
	}
	if got := srv.EventSubscriberCount(); got != 0 {
		t.Errorf("expected 0 subscribers after stream close, got %d", got)
	}
	if got := testutil.ToFloat64(m.ControllerEventSubscribers.WithLabelValues()); got != 0 {
		t.Errorf("expected event-subscribers gauge 0 after close, got %v", got)
	}
	if got := testutil.ToFloat64(m.ControllerGRPCConnections.WithLabelValues()); got != 0 {
		t.Errorf("expected grpc connections gauge 0 after event stream close, got %v", got)
	}
}

// TestGRPCServerWatchEventsWithoutLeaderElection covers the single-replica
// deployment: leader election off means every replica serves the stream, so an
// inverted leader gate cannot slip through green tests.
func TestGRPCServerWatchEventsWithoutLeaderElection(t *testing.T) {
	reg := NewRegistry(30 * time.Second)
	m := metrics.NewPrometheusMetrics("test", prometheus.NewRegistry())
	srv := NewGRPCServer(reg, m, false, nil, true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeEventStream(ctx)

	done := make(chan error, 1)
	go func() { done <- srv.WatchEvents(&pb.WatchEventsRequest{}, stream) }()

	waitForEventSubscriber(t, srv)

	srv.PublishEvent(&pb.Event{Payload: &pb.Event_TopologyChanged{
		TopologyChanged: &pb.TopologyChanged{Reason: "agent_registered", NodeName: "node-7"},
	}})

	select {
	case ev := <-stream.sent:
		if ev.GetTopologyChanged().GetNodeName() != "node-7" {
			t.Errorf("unexpected payload: %+v", ev)
		}
	case err := <-done:
		t.Fatalf("WatchEvents returned instead of streaming: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("published event did not reach the stream with leader election off")
	}
}

// TestGRPCServerWatchEventsStopsAfterDemotion verifies the periodic leadership
// re-check: a replica demoted mid-stream must end the stream instead of fanning
// events to subscribers it no longer owns.
func TestGRPCServerWatchEventsStopsAfterDemotion(t *testing.T) {
	reg := NewRegistry(30 * time.Second)
	m := metrics.NewPrometheusMetrics("test", prometheus.NewRegistry())

	var leader atomic.Bool
	leader.Store(true)
	srv := NewGRPCServer(reg, m, true, leader.Load, true)
	// Shortened before the stream goroutine starts, so the test never waits a
	// real leader-check interval.
	srv.leaderCheckInterval = 5 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeEventStream(ctx)

	done := make(chan error, 1)
	go func() { done <- srv.WatchEvents(&pb.WatchEventsRequest{}, stream) }()

	waitForEventSubscriber(t, srv)

	leader.Store(false)

	select {
	case err := <-done:
		st, ok := grpcstatus.FromError(err)
		if !ok || st.Code() != codes.Unavailable {
			t.Fatalf("expected codes.Unavailable after demotion, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WatchEvents kept streaming after the replica lost leadership")
	}

	if got := srv.EventSubscriberCount(); got != 0 {
		t.Errorf("expected 0 subscribers after demotion, got %d", got)
	}
	if got := testutil.ToFloat64(m.ControllerGRPCConnections.WithLabelValues()); got != 0 {
		t.Errorf("expected grpc connections gauge 0 after demotion, got %v", got)
	}
}

// TestGRPCServerPublishEventNoopWhenEventsDisabled pins the disabled-flag
// contract: no sequence number burned, no counter moved, no subscriber served.
func TestGRPCServerPublishEventNoopWhenEventsDisabled(t *testing.T) {
	reg := NewRegistry(30 * time.Second)
	m := metrics.NewPrometheusMetrics("test", prometheus.NewRegistry())
	srv := NewGRPCServer(reg, m, false, nil, false)

	ev := &pb.Event{Payload: &pb.Event_TopologyChanged{
		TopologyChanged: &pb.TopologyChanged{Reason: "agent_registered", NodeName: "node-1"},
	}}
	srv.PublishEvent(ev)

	if ev.GetSeq() != 0 {
		t.Errorf("expected no seq stamped with events disabled, got %d", ev.GetSeq())
	}
	if ev.GetTimestamp() != nil {
		t.Errorf("expected no timestamp stamped with events disabled, got %v", ev.GetTimestamp())
	}
	if got := srv.EventSubscriberCount(); got != 0 {
		t.Errorf("expected 0 event subscribers with events disabled, got %d", got)
	}
	if got := testutil.ToFloat64(m.ControllerEventSubscribers.WithLabelValues()); got != 0 {
		t.Errorf("expected event-subscribers gauge untouched at 0, got %v", got)
	}
	if got := testutil.ToFloat64(m.ControllerEventsPublished.WithLabelValues("topology_changed")); got != 0 {
		t.Errorf("expected events-published counter untouched at 0, got %v", got)
	}
}

// startEventStreamServer serves a real grpc.Server on a loopback listener with
// the given events flag and returns an EventStream client dialled to it, so the
// registration decision is observed exactly as a Console would see it.
func startEventStreamServer(t *testing.T, eventsEnabled bool) (*GRPCServer, pb.EventStreamClient) {
	t.Helper()

	reg := NewRegistry(30 * time.Second)
	m := metrics.NewPrometheusMetrics("test", prometheus.NewRegistry())
	srv := NewGRPCServer(reg, m, false, nil, eventsEnabled)

	lc := net.ListenConfig{}
	lis, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("loopback listen: %v", err)
	}

	gs := grpc.NewServer()
	srv.RegisterService(gs)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dialling %s: %v", lis.Addr(), err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return srv, pb.NewEventStreamClient(conn)
}

func TestEventStreamNotRegisteredWhenEventsDisabled(t *testing.T) {
	_, client := startEventStreamServer(t, false)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := client.WatchEvents(ctx, &pb.WatchEventsRequest{})
	if err == nil {
		_, err = stream.Recv()
	}
	st, ok := grpcstatus.FromError(err)
	if !ok || st.Code() != codes.Unimplemented {
		t.Fatalf("expected codes.Unimplemented with events disabled, got %v", err)
	}
}

func TestEventStreamRegisteredWhenEventsEnabled(t *testing.T) {
	srv, client := startEventStreamServer(t, true)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := client.WatchEvents(ctx, &pb.WatchEventsRequest{})
	if err != nil {
		t.Fatalf("WatchEvents call failed: %v", err)
	}

	waitForEventSubscriber(t, srv)

	srv.PublishEvent(&pb.Event{Payload: &pb.Event_TopologyChanged{
		TopologyChanged: &pb.TopologyChanged{Reason: "agent_registered", NodeName: "node-9"},
	}})

	ev, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv over loopback failed: %v", err)
	}
	if ev.GetTopologyChanged().GetNodeName() != "node-9" {
		t.Errorf("unexpected event over loopback: %+v", ev)
	}
}

// TestGRPCServerShutdownUnblocksWatchEvents pins the graceful-shutdown contract that
// grpc.Server.GracefulStop cannot provide on its own.
func TestGRPCServerShutdownUnblocksWatchEvents(t *testing.T) {
	reg := NewRegistry(30 * time.Second)
	m := metrics.NewPrometheusMetrics("test", prometheus.NewRegistry())
	// Leader election off is the configuration that made this hang in the wild:
	// lostLeadership() is permanently false, so the ticker branch never exits.
	srv := NewGRPCServer(reg, m, false, nil, true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeEventStream(ctx)

	done := make(chan error, 1)
	go func() { done <- srv.WatchEvents(&pb.WatchEventsRequest{}, stream) }()

	waitForEventSubscriber(t, srv)

	srv.Shutdown()

	select {
	case err := <-done:
		st, ok := grpcstatus.FromError(err)
		if !ok || st.Code() != codes.Unavailable {
			t.Fatalf("expected codes.Unavailable after Shutdown, got %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("WatchEvents did not return within 1s of Shutdown")
	}

	if got := srv.EventSubscriberCount(); got != 0 {
		t.Errorf("expected 0 event subscribers after Shutdown, got %d", got)
	}
	if got := testutil.ToFloat64(m.ControllerGRPCConnections.WithLabelValues()); got != 0 {
		t.Errorf("expected grpc connections gauge 0 after Shutdown, got %v", got)
	}
}

// TestGRPCServerShutdownUnblocksWatchTasks covers the same trap on the agent
// side. This loop selects only on the task channel and the stream context, so
// any connected agent pinned GracefulStop open indefinitely.
func TestGRPCServerShutdownUnblocksWatchTasks(t *testing.T) {
	srv, _ := newTestGRPCServer()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeTaskStream(ctx)

	done := make(chan error, 1)
	go func() { done <- srv.WatchTasks(&pb.WatchTasksRequest{AgentId: "agent-1"}, stream) }()

	deadline := time.After(3 * time.Second)
	for srv.taskMgr.SubscriberCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("WatchTasks never registered a subscriber")
		case <-time.After(5 * time.Millisecond):
		}
	}

	srv.Shutdown()

	select {
	case err := <-done:
		st, ok := grpcstatus.FromError(err)
		if !ok || st.Code() != codes.Unavailable {
			t.Fatalf("expected codes.Unavailable after Shutdown, got %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("WatchTasks did not return within 1s of Shutdown")
	}

	if got := srv.taskMgr.SubscriberCount(); got != 0 {
		t.Errorf("expected 0 task subscribers after Shutdown, got %d", got)
	}
	if got := testutil.ToFloat64(srv.metrics.ControllerGRPCConnections.WithLabelValues()); got != 0 {
		t.Errorf("expected grpc connections gauge 0 after Shutdown, got %v", got)
	}
}

// TestGRPCServerShutdownUnblocksWatchPeers covers the third handler with the same shape; every
// connected agent holds a WatchPeers stream alongside its WatchTasks.
func TestGRPCServerShutdownUnblocksWatchPeers(t *testing.T) {
	srv, _ := newTestGRPCServer()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakePeerStream(ctx)

	done := make(chan error, 1)
	go func() { done <- srv.WatchPeers(&pb.WatchPeersRequest{AgentId: "agent-1"}, stream) }()

	// The handler sends a FULL_SYNC before entering its select; consuming it is
	// what proves the loop, not the initial send, is where it parks.
	select {
	case <-stream.sent:
	case <-time.After(3 * time.Second):
		t.Fatal("WatchPeers never sent the initial full sync")
	}

	srv.Shutdown()

	select {
	case err := <-done:
		st, ok := grpcstatus.FromError(err)
		if !ok || st.Code() != codes.Unavailable {
			t.Fatalf("expected codes.Unavailable after Shutdown, got %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("WatchPeers did not return within 1s of Shutdown")
	}

	if got := testutil.ToFloat64(srv.metrics.ControllerGRPCConnections.WithLabelValues()); got != 0 {
		t.Errorf("expected grpc connections gauge 0 after Shutdown, got %v", got)
	}
}

// TestGRPCServerShutdownIdempotent guards the close-of-closed-channel panic:
// the shutdown path may be reached more than once (Run's ctx cancel plus a
// direct call in a test or an embedding caller).
func TestGRPCServerShutdownIdempotent(t *testing.T) {
	srv, _ := newTestGRPCServer()

	srv.Shutdown()
	srv.Shutdown()
	srv.Shutdown()

	// A stream opened after Shutdown must not hang either: it observes the
	// already-closed signal on its first select.
	stream := newFakeTaskStream(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.WatchTasks(&pb.WatchTasksRequest{AgentId: "late-agent"}, stream) }()

	select {
	case err := <-done:
		st, ok := grpcstatus.FromError(err)
		if !ok || st.Code() != codes.Unavailable {
			t.Fatalf("expected codes.Unavailable for a stream opened after Shutdown, got %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("WatchTasks opened after Shutdown did not return within 1s")
	}
}

// TestGRPCServerShutdownConcurrentWithPublishers pins the ordering hazard in the shutdown path.
func TestGRPCServerShutdownConcurrentWithPublishers(t *testing.T) {
	reg := NewRegistry(30 * time.Second)
	m := metrics.NewPrometheusMetrics("test", prometheus.NewRegistry())
	srv := NewGRPCServer(reg, m, false, nil, true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	eventStream := newFakeEventStream(ctx)
	taskStream := newFakeTaskStream(ctx)
	streams := make(chan error, 2)
	go func() { streams <- srv.WatchEvents(&pb.WatchEventsRequest{}, eventStream) }()
	go func() { streams <- srv.WatchTasks(&pb.WatchTasksRequest{AgentId: "agent-1"}, taskStream) }()

	waitForEventSubscriber(t, srv)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// fakeEventStream.Send blocks on a full buffer, so drain it: without this the
	// handler would be parked in Send rather than in the select the fix touches,
	// and the test would prove nothing.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-eventStream.sent:
			}
		}
	}()

	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				srv.PublishEvent(&pb.Event{Payload: &pb.Event_TopologyChanged{
					TopologyChanged: &pb.TopologyChanged{Reason: "agent_registered"},
				}})
				srv.BroadcastPeerUpdate([]model.AgentInfo{{ID: "agent-2", NodeName: "node-2"}})
			}
		}()
	}

	srv.Shutdown()

	for range 2 {
		select {
		case err := <-streams:
			st, ok := grpcstatus.FromError(err)
			if !ok || st.Code() != codes.Unavailable {
				t.Errorf("expected codes.Unavailable after Shutdown, got %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("a stream did not return within 2s of Shutdown under concurrent publishing")
		}
	}

	// Publishers keep running past Shutdown on purpose: a post-stop publish must
	// still be a safe no-op rather than a panic or a block.
	time.Sleep(20 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// waitForEventSubscriber blocks until a WatchEvents stream has registered.
func waitForEventSubscriber(t *testing.T, srv *GRPCServer) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for srv.EventSubscriberCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("WatchEvents never registered a subscriber")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

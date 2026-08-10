package controller

import (
	"context"
	"fmt"
	"net"
	"slices"
	"testing"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestCapabilitiesFor pins the capability advertisement that gates the whole Console realtime path.
func TestCapabilitiesFor(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
		want    []string
	}{
		{name: "events disabled advertises nothing", enabled: false, want: []string{}},
		{name: "events enabled advertises events", enabled: true, want: []string{"events"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Controller.Events.Enabled = tc.enabled

			got := capabilitiesFor(cfg)
			if got == nil {
				t.Fatal("capabilitiesFor returned nil; an empty slice is required so the JSON stays an array")
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("capabilitiesFor(events.enabled=%v) = %v, want %v", tc.enabled, got, tc.want)
			}
		})
	}
}

// Since this wiring published pb.TopologyChanged{Reason: reason} and threw the subject away; this
// asserts every emission site now names its agent.
func TestControllerPublishesAttributedTopologyEvents(t *testing.T) {
	cfg := &config.Config{MetricsPrefix: "test"}
	cfg.Controller.AgentTTL = time.Nanosecond
	cfg.Controller.LeaderElection = false
	cfg.Controller.Events.Enabled = true

	c := New(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeEventStream(ctx)
	go func() { _ = c.grpcServer.WatchEvents(&pb.WatchEventsRequest{}, stream) }()

	deadline := time.After(2 * time.Second)
	for c.grpcServer.EventSubscriberCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("WatchEvents never registered a subscriber")
		case <-time.After(5 * time.Millisecond):
		}
	}

	next := func() *pb.TopologyChanged {
		t.Helper()
		select {
		case ev := <-stream.sent:
			tc := ev.GetTopologyChanged()
			if tc == nil {
				t.Fatalf("expected a topology_changed event, got %+v", ev)
			}
			return tc
		case <-time.After(2 * time.Second):
			t.Fatal("expected a topology event that never arrived")
			return nil
		}
	}
	assertEvent := func(what string, got *pb.TopologyChanged, reason, agentID, node, zone string) {
		t.Helper()
		if got.GetReason() != reason || got.GetAgentId() != agentID ||
			got.GetNodeName() != node || got.GetZone() != zone {
			t.Errorf("%s: got %+v, want reason=%q agent=%q node=%q zone=%q",
				what, got, reason, agentID, node, zone)
		}
	}

	c.registry.Register(model.AgentInfo{ID: "agent-1", NodeName: "node-1", Zone: "zone-a"})
	assertEvent("register", next(), "agent_registered", "agent-1", "node-1", "zone-a")

	c.registry.Register(model.AgentInfo{ID: "agent-2", NodeName: "node-2", Zone: "zone-b"})
	assertEvent("register", next(), "agent_registered", "agent-2", "node-2", "zone-b")

	c.registry.UpdateZone("node-2", "zone-c")
	assertEvent("zone update", next(), "zone_updated", "agent-2", "node-2", "zone-c")

	// Both agents are already past the nanosecond TTL: one sweep, two events.
	if n := c.registry.EvictStale(); n != 2 {
		t.Fatalf("expected 2 evictions, got %d", n)
	}
	assertEvent("evict", next(), "agent_evicted", "agent-1", "node-1", "zone-a")
	assertEvent("evict", next(), "agent_evicted", "agent-2", "node-2", "zone-c")
}

// freePort reserves an ephemeral port and releases it, so Run can bind it. The
// window between close and bind is a theoretical race; on a test host with no
// competing binder it is not one in practice.
func freePort(t *testing.T) int {
	t.Helper()

	lc := net.ListenConfig{}
	lis, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a free port: %v", err)
	}
	addr, ok := lis.Addr().(*net.TCPAddr)
	if !ok {
		_ = lis.Close()
		t.Fatalf("unexpected listener address type %T", lis.Addr())
	}
	port := addr.Port
	if err := lis.Close(); err != nil {
		t.Fatalf("releasing the reserved port: %v", err)
	}
	return port
}

// Run must return on ctx cancel within the bounded shutdown window.
func TestControllerRunShutsDownWithActiveEventSubscriber(t *testing.T) {
	grpcPort := freePort(t)

	cfg := &config.Config{
		MetricsPrefix: "test",
		HTTPPort:      freePort(t),
		GRPCPort:      grpcPort,
	}
	cfg.Controller.AgentTTL = 30 * time.Second
	cfg.Controller.LeaderElection = false
	cfg.Controller.Events.Enabled = true

	c := New(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- c.Run(ctx) }()

	conn, err := grpc.NewClient(
		fmt.Sprintf("127.0.0.1:%d", grpcPort),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		cancel()
		t.Fatalf("dialling the controller: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	streamCtx, streamCancel := context.WithCancel(context.Background())
	defer streamCancel()

	// Retry the subscribe: Run binds its listener in a goroutine, so the first
	// dial can land before the server is accepting.
	var subscribed bool
	for range 100 {
		if _, err := pb.NewEventStreamClient(conn).WatchEvents(streamCtx, &pb.WatchEventsRequest{}); err == nil {
			subscribed = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !subscribed {
		cancel()
		t.Fatal("never opened a WatchEvents stream against the running controller")
	}

	// The client returns from WatchEvents before the server handler runs, so wait
	// for the server-side subscription: the hang needs an active handler.
	deadline := time.After(5 * time.Second)
	for c.grpcServer.EventSubscriberCount() == 0 {
		select {
		case <-deadline:
			cancel()
			t.Fatal("controller never registered the WatchEvents subscriber")
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()

	select {
	case err := <-runErr:
		if err != nil {
			t.Errorf("Run returned an error on shutdown: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after ctx cancel with an active WatchEvents subscriber")
	}
}

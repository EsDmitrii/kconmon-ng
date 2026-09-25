package controller

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"slices"
	"testing"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
	"github.com/prometheus/client_golang/prometheus/testutil"
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

	ctx := t.Context()
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

	streamCtx := t.Context()

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

// KconmonAgentsMissing subtracts controller_external_agents from registered_agents, so the gauge
// has to move with the registry exactly where registered_agents does, demotion included.
func TestControllerExternalAgentsGauge(t *testing.T) {
	cfg := &config.Config{MetricsPrefix: "test"}
	cfg.Controller.AgentTTL = 30 * time.Second
	cfg.Controller.LeaderElection = false

	c := New(cfg)
	registered := c.metrics.ControllerRegisteredAgents.WithLabelValues()
	external := c.metrics.ControllerExternalAgents.WithLabelValues()

	c.registry.Register(model.AgentInfo{ID: "pod-1", NodeName: "node-1", PodIP: "10.0.0.1"})
	c.registry.Register(externalAgent("edge-edge", "edge", "10.20.30.40", 9091))

	if got := testutil.ToFloat64(registered); got != 2 {
		t.Errorf("registered_agents = %v, want 2", got)
	}
	if got := testutil.ToFloat64(external); got != 1 {
		t.Errorf("external_agents = %v, want 1", got)
	}

	c.registry.Deregister("edge-edge")
	if got := testutil.ToFloat64(external); got != 0 {
		t.Errorf("external_agents after deregister = %v, want 0", got)
	}

	c.registry.Register(externalAgent("edge-edge", "edge", "10.20.30.40", 9091))
	c.SetLeader(false)
	if got := testutil.ToFloat64(external); got != 0 {
		t.Errorf("external_agents after demotion = %v, want 0 (registry dropped)", got)
	}
	if got := testutil.ToFloat64(registered); got != 0 {
		t.Errorf("registered_agents after demotion = %v, want 0", got)
	}
}

// The metrics listener is the one port the chart opens to Prometheus, so that is where the SD
// body must answer; and controller.prometheusSD.enabled=false must leave no route on it.
func TestControllerMountsPrometheusSDOnMetricsListener(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("enabled=%v", enabled), func(t *testing.T) {
			cfg := &config.Config{MetricsPrefix: "test", MetricsPort: 9191}
			cfg.Controller.AgentTTL = 30 * time.Second
			cfg.Controller.LeaderElection = false
			cfg.Controller.PrometheusSD.Enabled = enabled

			c := New(cfg)
			// A 2.3.x host agent reports no port: the target must carry the controller's own.
			c.registry.Register(externalAgent("edge-edge", "edge", "10.20.30.40", 0))

			for name, h := range map[string]http.Handler{
				"metrics listener": c.metricsListenerHandler(),
				"API mux":          c.httpServer.Handler(),
			} {
				rec := getSD(t, h)
				if !enabled {
					if rec.Code != http.StatusNotFound {
						t.Errorf("%s: status = %d, want 404 when disabled", name, rec.Code)
					}
					continue
				}
				groups := decodeSD(t, rec)
				if len(groups) != 1 || groups[0].Targets[0] != "10.20.30.40:9191" {
					t.Errorf("%s: groups = %+v, want target 10.20.30.40:9191", name, groups)
				}
			}
		})
	}
}

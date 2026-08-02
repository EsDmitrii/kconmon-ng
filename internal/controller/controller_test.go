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
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestCapabilitiesFor pins the capability advertisement that gates the whole
// Console realtime path: "events" appears only when the operator turned the
// events flag on, and the slice is never nil (a nil slice marshals as JSON
// null, which the Console cannot iterate).
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

// TestControllerRunShutsDownWithActiveEventSubscriber is the regression test for
// the M2 smoke-test hang. grpc.Server.GracefulStop waits for active handlers to
// return but does not cancel their stream contexts, so a single connected
// WatchEvents subscriber kept Run blocked forever and only SIGKILL ended the
// process. Run must return on ctx cancel within the bounded shutdown window.
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

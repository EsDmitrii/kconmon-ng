package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/checker"
	"github.com/EsDmitrii/kconmon-ng/internal/controller"
	"github.com/EsDmitrii/kconmon-ng/internal/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// countingListener counts accepted connections so a test can tell a redial from a retry on the
// connection the agent already has.
type countingListener struct {
	net.Listener
	accepted atomic.Int64
}

func (l *countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.accepted.Add(1)
	}
	return conn, err
}

// TestGRPCClientReconnectOpensANewTransport is what lets a rejected agent move to the leader: the
// controller Service is a ClusterIP, so only a new TCP connection is load-balanced again. Retrying
// on the existing one pins the agent to the standby that just refused it.
func TestGRPCClientReconnectOpensANewTransport(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	lc := net.ListenConfig{}
	base, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	lis := &countingListener{Listener: base}

	reg := controller.NewRegistry(30 * time.Second)
	m := metrics.NewPrometheusMetrics("test_reconnect", prometheus.NewRegistry())
	// events disabled: this test only exercises the AgentRegistry registration path.
	srv := controller.NewGRPCServer(reg, m, false, nil, false)

	gs := grpc.NewServer()
	srv.RegisterService(gs)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	client, err := NewGRPCClient(base.Addr().String(), ClientSecurity{})
	if err != nil {
		t.Fatalf("NewGRPCClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	// PodIP is required by the controller's registration validation: an agent that cannot say where
	// it is becomes a peer every other agent probes at "".
	info := model.AgentInfo{ID: "agent-1", NodeName: "node-1", PodIP: "10.0.0.1"}
	if _, _, err := client.Register(ctx, info, checker.PeerPorts{HTTP: 8080}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	before := lis.accepted.Load()
	if before != 1 {
		t.Fatalf("server accepted %d connections for the first Register, want 1", before)
	}

	if err := client.Reconnect(); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}

	if _, _, err := client.Register(ctx, info, checker.PeerPorts{HTTP: 8080}); err != nil {
		t.Fatalf("Register after Reconnect: %v", err)
	}
	if got := lis.accepted.Load(); got <= before {
		t.Errorf("server accepted %d connections after Reconnect, want more than %d; the agent "+
			"stayed pinned to the replica that refused it", got, before)
	}
}

// TestShouldRedialOnlyForUnavailable bounds when the agent tears its transport down. Redialling on
// any error strands in-flight diagnostic tasks: the task arrived on the leader's stream, and the
// result would be reported over a fresh connection the Service may route to a standby, where no
// Dispatch is waiting for it.
func TestShouldRedialOnlyForUnavailable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"not the leader", grpcstatus.Error(codes.Unavailable, "not the leader"), true},
		{"wrapped unavailable", fmt.Errorf("registering agent: %w",
			grpcstatus.Error(codes.Unavailable, "not the leader")), true},
		{"unknown agent", grpcstatus.Error(codes.NotFound, "agent not registered"), false},
		{"deadline", grpcstatus.Error(codes.DeadlineExceeded, "too slow"), false},
		{"plain error", errors.New("boom"), false},
		{"no error", nil, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldRedial(tc.err); got != tc.want {
				t.Errorf("shouldRedial(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

/*
protoToTargets is where a peer's reported ports become what this agent dials (2.4.0). A peer older
than that reports none, and is probed on this agent's OWN ports: the fleet-wide contract such an
agent still assumes. Each port falls back on its own, so a half-reported peer is not all-or-nothing.
*/
func TestProtoToTargetsPrefersReportedPortsAndFallsBackToOwn(t *testing.T) {
	own := checker.PeerPorts{HTTP: 8080, UDP: 9090}
	peers := []*pb.AgentMeta{
		{Id: "new", NodeName: "node-new", PodIp: "10.0.0.2", Zone: "z1", HttpPort: 18080, UdpPort: 19090, MetricsPort: 19091},
		{Id: "old", NodeName: "node-old", PodIp: "10.0.0.3", Zone: "z2"},
		{Id: "half", NodeName: "node-half", PodIp: "10.0.0.4", Zone: "z3", HttpPort: 18081},
	}

	got := protoToTargets(peers, own)

	want := []checker.Target{
		{AgentID: "new", NodeName: "node-new", PodIP: "10.0.0.2", Zone: "z1", Port: 18080, UDPPort: 19090},
		{AgentID: "old", NodeName: "node-old", PodIP: "10.0.0.3", Zone: "z2", Port: 8080, UDPPort: 9090},
		{AgentID: "half", NodeName: "node-half", PodIP: "10.0.0.4", Zone: "z3", Port: 18081, UDPPort: 9090},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("protoToTargets =\n  %+v\nwant\n  %+v", got, want)
	}
	if got := protoToTargets(nil, own); len(got) != 0 {
		t.Errorf("an empty peer list produced %d targets", len(got))
	}
}

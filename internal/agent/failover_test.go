package agent

import (
	"context"
	"io"
	"net"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/checker"
	"github.com/EsDmitrii/kconmon-ng/internal/controller"
	"github.com/EsDmitrii/kconmon-ng/internal/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
)

// serviceProxy stands in for the controller's ClusterIP Service: every new TCP connection is routed
// to the replica pick names, so a test decides which replica each dial lands on.
type serviceProxy struct {
	lis  net.Listener
	pick func() string
}

func newServiceProxy(t *testing.T, pick func() string) *serviceProxy {
	t.Helper()
	var lc net.ListenConfig
	lis, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &serviceProxy{lis: lis, pick: pick}
	go p.serve()
	t.Cleanup(func() { _ = lis.Close() })
	return p
}

func (p *serviceProxy) serve() {
	for {
		in, err := p.lis.Accept()
		if err != nil {
			return
		}
		out, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", p.pick())
		if err != nil {
			_ = in.Close()
			continue
		}
		pipe := func(dst, src net.Conn) {
			_, _ = io.Copy(dst, src)
			_ = dst.Close()
			_ = src.Close()
		}
		go pipe(in, out)
		go pipe(out, in)
	}
}

// replica is one controller pod: a real GRPCServer whose leadership the test flips.
type replica struct {
	reg    *controller.Registry
	leader atomic.Bool
	gs     *grpc.Server
	addr   string
}

func startReplica(t *testing.T, name string, leader bool) *replica {
	t.Helper()
	var lc net.ListenConfig
	lis, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &replica{reg: controller.NewRegistry(time.Minute), addr: lis.Addr().String()}
	r.leader.Store(leader)
	srv := controller.NewGRPCServer(r.reg, metrics.NewPrometheusMetrics("test_failover_"+name, prometheus.NewRegistry()),
		true, r.leader.Load, false)
	r.gs = grpc.NewServer()
	srv.RegisterService(r.gs)
	go func() { _ = r.gs.Serve(lis) }()
	t.Cleanup(r.gs.Stop)
	return r
}

func (r *replica) has(nodeName string) bool {
	return slices.ContainsFunc(r.reg.GetAll(), func(a model.AgentInfo) bool { return a.NodeName == nodeName })
}

func peerNames(a *Agent) []string {
	peers := a.scheduler.Peers()
	names := make([]string, 0, len(peers))
	for _, p := range peers {
		names = append(names, p.NodeName)
	}
	slices.Sort(names)
	return names
}

/*
The leader pod is deleted. The Service then holds the new leader and a fresh standby, and a redial
lands on either; this run is the unlucky tail where three dials in a row hit the standby, after a
second in which the lease had no holder at all. The agent must still be back on the leader within
one lease duration plus one probe interval, and while the rest of the fleet re-registers it keeps
probing the peers it had, instead of the new leader's partial list.
*/
func TestAgentReachesTheNewLeaderPromptlyAfterFailover(t *testing.T) {
	t.Setenv("KCONMON_NG_POD_NAME", "failover-agent-pod")
	const (
		leaseDuration = 15 * time.Second // controller's defaultLeaseDuration
		leaseGap      = time.Second
		standbyHits   = 3
	)

	oldLeader := startReplica(t, "old", true)
	newLeader := startReplica(t, "new", false)
	standby := startReplica(t, "standby", false)
	for _, reg := range []*controller.Registry{oldLeader.reg, newLeader.reg} {
		reg.Register(model.AgentInfo{ID: "p", NodeName: "peer-p", PodName: "p", PodIP: "192.0.2.21"})
	}
	oldLeader.reg.Register(model.AgentInfo{ID: "q", NodeName: "peer-q", PodName: "q", PodIP: "192.0.2.22"})

	var switched atomic.Bool
	var afterSwitch atomic.Int64
	proxy := newServiceProxy(t, func() string {
		if !switched.Load() {
			return oldLeader.addr
		}
		if afterSwitch.Add(1) <= standbyHits {
			return standby.addr
		}
		return newLeader.addr
	})

	cfg := testRunConfig(t, proxy.lis.Addr().String())
	cfg.Agent.NodeName = "failover-agent-node"
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	startRun(t, a)
	waitFor(t, 10*time.Second, "registration on the first leader", func() bool {
		return oldLeader.has("failover-agent-node") && slices.Equal(peerNames(a), []string{"peer-p", "peer-q"})
	})

	switched.Store(true)
	lost := time.Now()
	oldLeader.gs.Stop()
	time.AfterFunc(leaseGap, func() { newLeader.leader.Store(true) })

	bound := leaseDuration + cfg.Checkers.TCP.Interval
	waitFor(t, bound, "registration on the new leader", func() bool { return newLeader.has("failover-agent-node") })
	t.Logf("agent re-registered on the new leader %v after the old one went away (bound %v)", time.Since(lost), bound)

	// peer-q has not re-registered yet; the new leader's list lacks it, and dropping it would stop
	// probing a live pair until it does.
	if got := peerNames(a); !slices.Equal(got, []string{"peer-p", "peer-q"}) {
		t.Errorf("peers right after the failover = %v, want [peer-p peer-q]", got)
	}
}

// The rejoin grace ends: the newest list then applies as it is, so a peer that really left is not
// probed forever. A grace a later loss superseded does not end the newer one.
func TestRejoinGraceEndsWithTheNewestList(t *testing.T) {
	a, err := New(testRunConfig(t, "127.0.0.1:1"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p := checker.Target{AgentID: "p", NodeName: "peer-p", PodIP: "192.0.2.21"}
	q := checker.Target{AgentID: "q", NodeName: "peer-q", PodIP: "192.0.2.22"}
	a.applyControllerPeers([]checker.Target{p, q})

	stale := a.beginRejoin()
	gen := a.beginRejoin()
	a.applyControllerPeers([]checker.Target{p})
	if got := peerNames(a); !slices.Equal(got, []string{"peer-p", "peer-q"}) {
		t.Fatalf("peers while rejoining = %v, want [peer-p peer-q]", got)
	}
	a.endRejoinAfter(stale, 0)
	time.Sleep(50 * time.Millisecond)
	if got := peerNames(a); !slices.Equal(got, []string{"peer-p", "peer-q"}) {
		t.Fatalf("a superseded grace ended the current one: peers = %v", got)
	}
	a.endRejoinAfter(gen, 10*time.Millisecond)
	waitFor(t, 5*time.Second, "the newest list after the grace", func() bool {
		return slices.Equal(peerNames(a), []string{"peer-p"})
	})
}

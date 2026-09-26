package agent

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/EsDmitrii/kconmon-ng/internal/controller"
	"github.com/EsDmitrii/kconmon-ng/internal/controller/meshplan"
	"github.com/EsDmitrii/kconmon-ng/internal/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// freePorts reserves n distinct loopback TCP ports and releases them just
// before returning, so the caller can hand them to the agent under test.
func freePorts(t *testing.T, n int) []int {
	t.Helper()
	var lc net.ListenConfig
	listeners := make([]net.Listener, 0, n)
	ports := make([]int, 0, n)
	for range n {
		l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("reserving a port: %v", err)
		}
		listeners = append(listeners, l)
		ports = append(ports, l.Addr().(*net.TCPAddr).Port)
	}
	for _, l := range listeners {
		_ = l.Close()
	}
	return ports
}

// testRunConfig builds a config for a full Run() with every checker disabled
// and fresh ports, so tests opt in to exactly what they exercise. grpcPort is
// the agent's UDP echo port, so it is reserved as UDP: a free TCP number says
// nothing about a UDP socket another test package holds.
func testRunConfig(t *testing.T, controllerAddr string) *config.Config {
	t.Helper()
	ports := freePorts(t, 2)
	echoPort := freeUDPPort(t)
	for echoPort == ports[0] || echoPort == ports[1] {
		echoPort = freeUDPPort(t)
	}
	ports = append(ports, echoPort)
	cfg := config.DefaultConfig()
	cfg.ControllerAddress = controllerAddr
	// Autodetect over the loopback controller is refused (peers cannot probe 127.0.0.1), and nothing
	// dials the agent's own address in these tests, so a documentation address stands in.
	cfg.Agent.AdvertiseAddress = "192.0.2.10"
	cfg.HTTPPort = ports[0]
	cfg.MetricsPort = ports[1]
	cfg.GRPCPort = ports[2]
	cfg.Checkers.TCP.Enabled = false
	cfg.Checkers.UDP.Enabled = false
	cfg.Checkers.ICMP.Enabled = false
	cfg.Checkers.DNS.Enabled = false
	cfg.Checkers.HTTP.Enabled = false
	return cfg
}

// startRun launches a.Run in a goroutine and wires a cleanup that cancels the
// run context and waits for Run to return, so no agent outlives its test.
func startRun(t *testing.T, a *Agent) chan error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		runErr <- a.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Log("agent Run did not exit within 10s after cancel")
		}
	})
	return runErr
}

func httpStatus(url string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

func awaitHTTPStatus(t *testing.T, url string, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	lastNote := "no response yet"
	for time.Now().Before(deadline) {
		code, err := httpStatus(url)
		if err == nil && code == want {
			return
		}
		if err != nil {
			lastNote = err.Error()
		} else {
			lastNote = fmt.Sprintf("status %d", code)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s did not return %d within %v (last: %s)", url, want, timeout, lastNote)
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", timeout, what)
}

/*
M2-1: the health plane must be up while the very FIRST registration is still
retrying. The chart's startupProbe polls /healthz on the HTTP port with a 65s
budget, so an agent that stays dark while the controller is down takes the
whole DaemonSet into CrashLoopBackOff exactly when the cluster is in trouble.
*/
func TestHealthServesWhileControllerIsUnreachable(t *testing.T) {
	// A just-released loopback port refuses connections, so every Register
	// attempt fails fast with Unavailable and the retry loop spins forever.
	refused := freePorts(t, 1)[0]
	cfg := testRunConfig(t, fmt.Sprintf("127.0.0.1:%d", refused))

	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runErr := startRun(t, a)

	awaitHTTPStatus(t, fmt.Sprintf("http://127.0.0.1:%d/healthz", cfg.HTTPPort), http.StatusOK, 5*time.Second)
	awaitHTTPStatus(t, fmt.Sprintf("http://127.0.0.1:%d/healthz", cfg.MetricsPort), http.StatusOK, 5*time.Second)
	// Liveness without readiness: registration has not succeeded, so /readyz
	// must still refuse -- only the startup/liveness plane comes up early.
	awaitHTTPStatus(t, fmt.Sprintf("http://127.0.0.1:%d/readyz", cfg.HTTPPort), http.StatusServiceUnavailable, 5*time.Second)

	select {
	case exitErr := <-runErr:
		t.Fatalf("Run exited while the controller was merely unreachable: %v", exitErr)
	default:
	}
}

/*
M2-2: a controller drop (restart, upgrade, failover) must not stop the fleet's
probing. The agent keeps probing the last known peer list while it re-registers
in the background; pausing the scheduler here blinded every agent at once for
the whole duration of a controller outage.
*/
func TestProbesContinueAcrossControllerDrop(t *testing.T) {
	// The agent's own meta must pass the controller's validateAgentMeta.
	// Its advertised address (testRunConfig) is not loopback, so the
	// IPv4-loopback peer is not filtered out as self by Scheduler.UpdatePeers.
	// Node name and zone travel through the config now (M6-1); only the pod
	// env is still read directly.
	t.Setenv("KCONMON_NG_POD_NAME", "m2-agent-pod")

	var lc net.ListenConfig
	lis, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening for the controller: %v", err)
	}
	reg := controller.NewRegistry(30 * time.Second)
	// The fake peer's pod IP is loopback, so the agent's UDP probes land on
	// its own echo probe server and succeed.
	reg.Register(model.AgentInfo{ID: "m2-peer", NodeName: "m2-peer-node", PodName: "m2-peer-pod", PodIP: "127.0.0.1"})
	m := metrics.NewPrometheusMetrics("test_m2_ctrl", prometheus.NewRegistry())
	srv := controller.NewGRPCServer(reg, m, false, nil, false)
	gs := grpc.NewServer()
	srv.RegisterService(gs)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	cfg := testRunConfig(t, lis.Addr().String())
	cfg.Agent.NodeName = "m2-agent-node"
	cfg.Checkers.UDP.Enabled = true
	cfg.Checkers.UDP.Interval = 50 * time.Millisecond
	cfg.Checkers.UDP.Timeout = 250 * time.Millisecond
	cfg.Checkers.UDP.Packets = 1

	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Count emitted probe results; installed before Run starts any goroutine.
	var probes atomic.Int64
	a.scheduler.handler = func(model.CheckResult) { probes.Add(1) }

	startRun(t, a)

	waitFor(t, 10*time.Second, "steady probing against the registered peer", func() bool {
		return probes.Load() >= 3
	})

	// Drop the controller: every stream breaks and re-registration begins.
	gs.Stop()

	// Let the drop propagate and any in-flight probe round finish.
	time.Sleep(500 * time.Millisecond)
	before := probes.Load()
	time.Sleep(1500 * time.Millisecond)
	after := probes.Load()
	if after <= before {
		t.Fatalf("probing stopped during the controller outage: %d probes before the window, %d after", before, after)
	}
}

// freeUDPPort reserves a UDP port on the wildcard address, the way ProbeServer binds, and releases
// it for the caller to bind: a loopback-only reservation misses a socket held on another address.
func freeUDPPort(t *testing.T) int {
	t.Helper()
	var lc net.ListenConfig
	conn, err := lc.ListenPacket(context.Background(), "udp", ":0")
	if err != nil {
		t.Fatalf("reserving a UDP port: %v", err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	_ = conn.Close()
	return port
}

/*
The 2.4.0 sibling of the test above, which stays the regression for the zero-port fallback: a peer
that REPORTED its UDP echo port is probed there, end to end through Register and WatchPeers. The
fake peer's UDPPort names a second ProbeServer on loopback while the agent's own echo server keeps
answering on cfg.GRPCPort, so success alone proves nothing; closing the second server must make the
probes fail, which they only do if the peer's port, not the agent's own, was dialled.
*/
func TestProbesReachThePeerOnItsReportedUDPPort(t *testing.T) {
	t.Setenv("KCONMON_NG_POD_NAME", "m2-agent-pod")

	peerPort := freeUDPPort(t)
	peerEcho := NewProbeServer(peerPort)
	if err := peerEcho.ListenUDP(context.Background()); err != nil {
		t.Fatalf("starting the peer's echo server: %v", err)
	}
	t.Cleanup(func() { _ = peerEcho.Close() })

	var lc net.ListenConfig
	lis, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening for the controller: %v", err)
	}
	reg := controller.NewRegistry(30 * time.Second)
	reg.Register(model.AgentInfo{ID: "m2-peer", NodeName: "m2-peer-node", PodName: "m2-peer-pod", PodIP: "127.0.0.1", UDPPort: peerPort})
	m := metrics.NewPrometheusMetrics("test_peer_udp_port", prometheus.NewRegistry())
	srv := controller.NewGRPCServer(reg, m, false, nil, false)
	gs := grpc.NewServer()
	srv.RegisterService(gs)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	cfg := testRunConfig(t, lis.Addr().String())
	cfg.Agent.NodeName = "m2-agent-node"
	cfg.Checkers.UDP.Enabled = true
	cfg.Checkers.UDP.Interval = 50 * time.Millisecond
	cfg.Checkers.UDP.Timeout = 250 * time.Millisecond
	cfg.Checkers.UDP.Packets = 1

	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var succeeded, failed atomic.Int64
	a.scheduler.handler = func(r model.CheckResult) {
		if r.Success {
			succeeded.Add(1)
		} else {
			failed.Add(1)
		}
	}

	startRun(t, a)

	waitFor(t, 10*time.Second, "successful probes against the peer's reported echo port", func() bool {
		return succeeded.Load() >= 3
	})

	// Take the peer's echo away. The agent's own echo server is still up on cfg.GRPCPort, so a
	// probe that fell back to it would keep succeeding.
	_ = peerEcho.Close()
	waitFor(t, 10*time.Second, "probes failing once the peer's reported echo port is gone", func() bool {
		return failed.Load() >= 3
	})
}

/*
Under a sparse plan the agent's peer list is a few agents, and its echo refuses the echo endpoint of
every registered agent all the same: the controller sends the fleet's with each peer list. A stranger
registered on 0 (older than 2.4.0) echoes on this agent's own port.
*/
func TestRunRefusesTheEchoOfAnAgentOutsideItsPlan(t *testing.T) {
	t.Setenv("KCONMON_NG_POD_NAME", "fleet-agent-pod")

	var lc net.ListenConfig
	lis, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening for the controller: %v", err)
	}
	reg := controller.NewRegistry(30 * time.Second)
	reg.Register(model.AgentInfo{ID: "peer", NodeName: "peer-node", PodIP: "10.0.0.2", UDPPort: 9191})
	reg.Register(model.AgentInfo{ID: "stranger", NodeName: "stranger-node", PodIP: "10.0.0.3", UDPPort: 9192})
	reg.Register(model.AgentInfo{ID: "old", NodeName: "old-node", PodIP: "10.0.0.4"})
	srv := controller.NewGRPCServer(reg, metrics.NewPrometheusMetrics("test_fleet_echo", prometheus.NewRegistry()), false, nil, false)
	srv.SetPeerPlan(meshplan.Plan{"fleet-agent-node-fleet-agent-pod": {"peer"}})
	gs := grpc.NewServer()
	srv.RegisterService(gs)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	cfg := testRunConfig(t, lis.Addr().String())
	cfg.Agent.NodeName = "fleet-agent-node"
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	startRun(t, a)

	self := netip.MustParseAddr(cfg.Agent.AdvertiseAddress)
	refused := func(src string) bool {
		return !a.probeServer.answers(netip.MustParseAddrPort(src), self, cfg.GRPCPort)
	}
	waitFor(t, 10*time.Second, "the fleet's echo endpoints to reach the echo", func() bool {
		return refused("10.0.0.3:9192")
	})
	if peers := a.scheduler.Peers(); len(peers) != 1 || peers[0].AgentID != "peer" {
		t.Fatalf("peers = %+v, want the plan's one", peers)
	}
	for _, src := range []string{"10.0.0.2:9191", fmt.Sprintf("10.0.0.4:%d", cfg.GRPCPort)} {
		if !refused(src) {
			t.Errorf("the echo answered %s, a registered agent's echo", src)
		}
	}
	if refused("10.0.0.3:45000") {
		t.Error("the echo refused a probe from an agent's ordinary port")
	}
}

// invalidArgumentRegistry rejects every registration the way the controller
// rejects a payload failing validateAgentMeta (missing downward-API env).
type invalidArgumentRegistry struct {
	pb.UnimplementedAgentRegistryServer
}

func (s *invalidArgumentRegistry) Register(context.Context, *pb.RegisterRequest) (*pb.RegisterResponse, error) {
	return nil, grpcstatus.Error(codes.InvalidArgument, "register: node name is empty")
}

/*
M2-3: InvalidArgument from Register is a configuration error -- the payload is
built from env/config fixed at startup, so retrying it can never succeed. Run
must fail fast with a distinct error instead of logging "controller not ready,
retrying" forever.
*/
func TestRunFailsFastWhenRegistrationIsRejectedAsInvalid(t *testing.T) {
	var lc net.ListenConfig
	lis, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening for the fake controller: %v", err)
	}
	gs := grpc.NewServer()
	pb.RegisterAgentRegistryServer(gs, &invalidArgumentRegistry{})
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	cfg := testRunConfig(t, lis.Addr().String())
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runErr := startRun(t, a)

	select {
	case exitErr := <-runErr:
		if exitErr == nil {
			t.Fatal("Run returned nil for a registration the controller rejected as invalid")
		}
		st, ok := grpcstatus.FromError(exitErr)
		if !ok || st.Code() != codes.InvalidArgument {
			t.Fatalf("Run error must carry the InvalidArgument rejection, got: %v", exitErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run kept retrying a registration the controller rejected as invalid; a configuration error must fail fast")
	}
}

// lockedBuffer is a goroutine-safe io.Writer for capturing slog output written
// by the agent's background goroutines during a full Run().
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// rejectingReregisterRegistry accepts the first registration and rejects every
// later one as InvalidArgument, the way a controller whose validation tightened
// mid-life (upgrade) would. WatchPeers fails at once to force re-registration.
type rejectingReregisterRegistry struct {
	pb.UnimplementedAgentRegistryServer
	registerCalls atomic.Int64
}

func (s *rejectingReregisterRegistry) Register(_ context.Context, req *pb.RegisterRequest) (*pb.RegisterResponse, error) {
	if s.registerCalls.Add(1) > 1 {
		return nil, grpcstatus.Error(codes.InvalidArgument, "register: zone label is malformed")
	}
	return &pb.RegisterResponse{AgentId: req.GetAgent().GetId(), Agent: req.GetAgent()}, nil
}

func (s *rejectingReregisterRegistry) WatchPeers(*pb.WatchPeersRequest, grpc.ServerStreamingServer[pb.PeerUpdate]) error {
	return grpcstatus.Error(codes.Unavailable, "peer stream torn down")
}

/*
M2-3 symmetry for the RE-registration path: an InvalidArgument mid-life is a
configuration error and must be logged as one (ERROR, distinct message), not
as the generic "re-registration failed, retrying" WARN. Unlike the first
registration it must KEEP retrying: probes continue on the last known peer
list (M2-2), so the fleet loses nothing, and the rejection may be a transient
controller-side validation change during an upgrade.
*/
func TestReregisterLogsConfigRejectionAsErrorAndKeepsRetrying(t *testing.T) {
	logs := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	var lc net.ListenConfig
	lis, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening for the fake controller: %v", err)
	}
	reg := &rejectingReregisterRegistry{}
	gs := grpc.NewServer()
	pb.RegisterAgentRegistryServer(gs, reg)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	cfg := testRunConfig(t, lis.Addr().String())
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runErr := startRun(t, a)

	// Three Register calls = initial success + a rejection + a retry AFTER that
	// rejection, proving the loop logs the config error but does not fail fast.
	waitFor(t, 20*time.Second, "a re-registration retry after an InvalidArgument rejection", func() bool {
		return reg.registerCalls.Load() >= 3
	})

	// M9-2: entering re-registration after a lost stream is counted, once per
	// entry, not once per retry inside it.
	if got := testutil.ToFloat64(a.metrics.AgentControllerReconnects.WithLabelValues()); got < 1 {
		t.Errorf("agent_controller_reconnects_total = %v after a forced re-registration, want >= 1", got)
	}

	select {
	case exitErr := <-runErr:
		t.Fatalf("Run exited on a re-registration rejection: %v", exitErr)
	default:
	}

	out := logs.String()
	if !strings.Contains(out, `level=ERROR msg="controller rejected the re-registration payload`) {
		t.Errorf("no ERROR log for the InvalidArgument re-registration rejection; logs:\n%s", out)
	}
	if strings.Contains(out, "re-registration failed, retrying") {
		t.Errorf("InvalidArgument rejection was logged with the generic retry WARN instead of a config-error message; logs:\n%s", out)
	}
}

// rejectingReadvertiseRegistry keeps the peer watch open, so only a reload registers again.
type rejectingReadvertiseRegistry struct {
	rejectingReregisterRegistry
}

func (*rejectingReadvertiseRegistry) WatchPeers(_ *pb.WatchPeersRequest, stream grpc.ServerStreamingServer[pb.PeerUpdate]) error {
	<-stream.Context().Done()
	return nil
}

// Re-advertising capabilities after a reload is a re-registration: a payload rejection is logged as
// the config error it is, and retried.
func TestReadvertiseLogsConfigRejectionAsErrorAndKeepsRetrying(t *testing.T) {
	logs := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	var lc net.ListenConfig
	lis, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening for the fake controller: %v", err)
	}
	reg := &rejectingReadvertiseRegistry{}
	gs := grpc.NewServer()
	pb.RegisterAgentRegistryServer(gs, reg)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	cfg := testRunConfig(t, lis.Addr().String())
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	startRun(t, a)
	waitFor(t, 10*time.Second, "the first registration", func() bool { return reg.registerCalls.Load() >= 1 })

	next := cloneConfig(cfg)
	next.Checkers.ICMP.Enabled = true
	a.ApplyConfig(next)
	waitFor(t, 20*time.Second, "a re-advertise retry after an InvalidArgument rejection", func() bool {
		return reg.registerCalls.Load() >= 3
	})

	if out := logs.String(); !strings.Contains(out, `level=ERROR msg="controller rejected the re-registration payload`) {
		t.Errorf("no ERROR log for the InvalidArgument rejection of a re-advertise; logs:\n%s", out)
	}
}

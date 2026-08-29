package agent

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/EsDmitrii/kconmon-ng/internal/controller"
	"github.com/EsDmitrii/kconmon-ng/internal/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
	"github.com/prometheus/client_golang/prometheus"
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
// and fresh loopback ports, so tests opt in to exactly what they exercise.
func testRunConfig(t *testing.T, controllerAddr string) *config.Config {
	t.Helper()
	ports := freePorts(t, 3)
	cfg := config.DefaultConfig()
	cfg.ControllerAddress = controllerAddr
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
	// Its pod IP is IPv6 loopback so the IPv4-loopback peer is not filtered
	// out as self by Scheduler.UpdatePeers.
	t.Setenv("KCONMON_NG_NODE_NAME", "m2-agent-node")
	t.Setenv("KCONMON_NG_POD_NAME", "m2-agent-pod")
	t.Setenv("KCONMON_NG_POD_IP", "::1")
	t.Setenv("KCONMON_NG_ZONE", "")

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

package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/checker"
	"github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/EsDmitrii/kconmon-ng/internal/controller"
	"github.com/EsDmitrii/kconmon-ng/internal/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc"
)

// readyzPeer serves /readyz like a peer agent, so the real TCP checker succeeds against it.
func readyzPeer(t *testing.T) checker.Target {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)
	return checker.Target{AgentID: "peer-id", NodeName: "peer-node", PodIP: host, Port: port}
}

// reloadConfig is an agent config with every checker off and an address that is not the peer's.
func reloadConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := testRunConfig(t, "127.0.0.1:1")
	cfg.Agent.NodeName = "reload-node"
	cfg.Checkers.PMTU.Enabled = false
	return cfg
}

func cloneConfig(cfg *config.Config) *config.Config {
	c := *cfg
	return &c
}

// countingAgent builds an agent whose scheduler counts results per check type and runs it on a peer.
func countingAgent(t *testing.T, cfg *config.Config) (*Agent, func(model.CheckType) int64) {
	t.Helper()
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var mu sync.Mutex
	counts := map[model.CheckType]int64{}
	a.scheduler.handler = func(r model.CheckResult) {
		mu.Lock()
		counts[r.Type]++
		mu.Unlock()
	}
	a.scheduler.UpdatePeers([]checker.Target{readyzPeer(t)})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		a.scheduler.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("scheduler.Run did not return within 5s of cancel: a checker loop outlived it")
		}
	})
	return a, func(ct model.CheckType) int64 {
		mu.Lock()
		defer mu.Unlock()
		return counts[ct]
	}
}

func TestReloadAppliesANewCheckerInterval(t *testing.T) {
	cfg := reloadConfig(t)
	cfg.Checkers.TCP.Enabled = true
	cfg.Checkers.TCP.Interval = time.Hour
	cfg.Checkers.TCP.Timeout = time.Second
	a, count := countingAgent(t, cfg)

	next := cloneConfig(cfg)
	next.Checkers.TCP.Interval = 20 * time.Millisecond
	a.ApplyConfig(next)

	waitFor(t, 5*time.Second, "tcp rounds at the reloaded 20ms interval", func() bool {
		return count(model.CheckTCP) >= 5
	})
	if got := a.appliedConfig().Checkers.TCP.Interval; got != 20*time.Millisecond {
		t.Errorf("applied tcp interval = %v, want 20ms", got)
	}
}

func TestReloadDisabledCheckerStopsProducing(t *testing.T) {
	cfg := reloadConfig(t)
	cfg.Checkers.TCP.Enabled = true
	cfg.Checkers.TCP.Interval = 20 * time.Millisecond
	cfg.Checkers.TCP.Timeout = time.Second
	a, count := countingAgent(t, cfg)
	waitFor(t, 5*time.Second, "tcp probing before the reload", func() bool { return count(model.CheckTCP) >= 3 })

	next := cloneConfig(cfg)
	next.Checkers.TCP.Enabled = false
	a.ApplyConfig(next)

	stopped := count(model.CheckTCP)
	time.Sleep(300 * time.Millisecond)
	if got := count(model.CheckTCP); got != stopped {
		t.Fatalf("tcp produced %d results after it was disabled by a reload", got-stopped)
	}
	a.probeMu.RLock()
	_, still := a.checkers[model.CheckTCP]
	a.probeMu.RUnlock()
	if still {
		t.Error("a disabled checker is still offered to the on-demand executor")
	}
}

// A task arriving after a reload runs on the reloaded checkers, not on the startup set.
func TestReloadTaskExecutorSeesTheNewCheckers(t *testing.T) {
	cfg := reloadConfig(t)
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ex := a.newTaskExecutor(nil)
	peer := readyzPeer(t)
	task := &pb.TaskRequest{TaskId: "t1", CheckType: string(model.CheckTCP), Target: &pb.AgentMeta{
		Id: peer.AgentID, NodeName: peer.NodeName, PodIp: peer.PodIP, HttpPort: uint32(peer.Port), //nolint:gosec // test port
	}}
	if res := ex.executeOne(context.Background(), task); !strings.Contains(res.GetError(), "not enabled") {
		t.Fatalf("tcp is disabled at startup, task error = %q", res.GetError())
	}

	next := cloneConfig(cfg)
	next.Checkers.TCP.Enabled = true
	a.ApplyConfig(next)
	if res := ex.executeOne(context.Background(), task); !res.GetSuccess() {
		t.Fatalf("tcp task after the reload enabled tcp: success=false error=%q", res.GetError())
	}
}

// A restart-bound key is named in one warning and nothing it governs moves.
func TestReloadRestartBoundChangeWarnsAndChangesNothing(t *testing.T) {
	logs := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	cfg := reloadConfig(t)
	cfg.Checkers.UDP.Enabled = true
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	udpBefore := a.checkers[model.CheckUDP]
	infoBefore := a.info

	next := cloneConfig(cfg)
	next.HTTPPort++
	next.MetricsPrefix = "other_prefix"
	next.Agent.NodeName = "someone-else"
	next.Agent.TLS.CAFile = "/etc/ca.pem"
	next.Topology.Mode = config.TopologyModeSparse // controller-only: the agent says nothing about it
	a.ApplyConfig(next)

	out := logs.String()
	warns := strings.Count(out, "level=WARN")
	if warns != 1 || !strings.Contains(out, "restart") {
		t.Fatalf("want exactly one WARN asking for a restart, got %d:\n%s", warns, out)
	}
	for _, key := range []string{"agent.nodeName", "agent.tls.caFile", "httpPort", "metricsPrefix"} {
		if !strings.Contains(out, key) {
			t.Errorf("the restart warning does not name %s:\n%s", key, out)
		}
	}
	if strings.Contains(out, "topology.mode") {
		t.Errorf("a controller-only key is named in the agent's warning:\n%s", out)
	}
	if got := a.appliedConfig(); got.HTTPPort != cfg.HTTPPort || got.MetricsPrefix != cfg.MetricsPrefix ||
		got.Agent.NodeName != cfg.Agent.NodeName {
		t.Errorf("restart-bound values were applied: httpPort=%d metricsPrefix=%q nodeName=%q",
			got.HTTPPort, got.MetricsPrefix, got.Agent.NodeName)
	}
	if a.info.NodeName != infoBefore.NodeName || a.info.HTTPPort != infoBefore.HTTPPort {
		t.Errorf("identity moved on reload: %+v -> %+v", infoBefore, a.info)
	}
	if a.checkers[model.CheckUDP] != udpBefore {
		t.Error("an unchanged checker was rebuilt by a restart-bound-only reload")
	}
}

// Toggling a plane re-registers with the new plane:* set; a change that keeps the planes does not.
func TestReloadReadvertisesCapabilitiesWhenAPlaneToggles(t *testing.T) {
	var lc net.ListenConfig
	lis, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	reg := controller.NewRegistry(30 * time.Second)
	m := metrics.NewPrometheusMetrics("test_reload_ctrl", prometheus.NewRegistry())
	srv := controller.NewGRPCServer(reg, m, false, nil, false)
	gs := grpc.NewServer()
	srv.RegisterService(gs)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	cfg := reloadConfig(t)
	cfg.ControllerAddress = lis.Addr().String()
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	startRun(t, a)

	registered := func() (model.AgentInfo, bool) { return reg.GetByNodeName(cfg.Agent.NodeName) }
	waitFor(t, 10*time.Second, "the first registration", func() bool { _, ok := registered(); return ok })
	first, _ := registered()
	if slices.Contains(first.Capabilities, "plane:icmp") {
		t.Fatalf("icmp is off at startup but advertised: %v", first.Capabilities)
	}

	next := cloneConfig(cfg)
	next.Checkers.ICMP.Enabled = true
	a.ApplyConfig(next)
	waitFor(t, 10*time.Second, "plane:icmp re-advertised after the reload", func() bool {
		info, ok := registered()
		return ok && slices.Contains(info.Capabilities, "plane:icmp")
	})

	next2 := cloneConfig(next)
	next2.Checkers.ICMP.Enabled = false
	next2.Checkers.External.Enabled = true
	next2.Checkers.External.AllowedCIDRs = []string{"10.0.0.0/8"}
	next2.Checkers.External.MaxTargets = 10
	next2.Checkers.External.Timeout = time.Second
	a.ApplyConfig(next2)
	waitFor(t, 10*time.Second, "plane:icmp withdrawn and external-checks advertised", func() bool {
		info, ok := registered()
		return ok && !slices.Contains(info.Capabilities, "plane:icmp") &&
			slices.Contains(info.Capabilities, capabilityExternalChecks)
	})

	settled, _ := registered()
	same := cloneConfig(next2)
	same.Checkers.UDP.Interval = 7 * time.Second // udp stays off: no plane moves
	same.Checkers.ICMP.Timeout = 2 * time.Second
	a.ApplyConfig(same)
	time.Sleep(500 * time.Millisecond)
	if now, _ := registered(); !now.JoinedAt.Equal(settled.JoinedAt) {
		t.Errorf("a reload that moved no plane re-registered the agent (joinedAt %v -> %v)",
			settled.JoinedAt, now.JoinedAt)
	}
}

// Reload while every probe path runs: go test -race is the assertion, plus no loop outlives Run.
func TestReloadUnderConcurrentProbes(t *testing.T) {
	cfg := reloadConfig(t)
	cfg.Checkers.TCP.Enabled = true
	cfg.Checkers.TCP.Interval = 5 * time.Millisecond
	cfg.Checkers.TCP.Timeout = time.Second
	cfg.Checkers.External.Enabled = true
	cfg.Checkers.External.AllowedCIDRs = []string{"10.0.0.0/8"}
	cfg.Checkers.External.MaxTargets = 10
	cfg.Checkers.External.Timeout = time.Second
	baseline := runtime.NumGoroutine()

	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	peer := readyzPeer(t)
	a.applyPeers([]checker.Target{peer})
	ex := a.newTaskExecutor(nil)
	var delivered atomic.Int64
	deliver := a.scheduler.handler
	a.scheduler.handler = func(r model.CheckResult) {
		delivered.Add(1)
		deliver(r)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		a.scheduler.Run(ctx)
		close(runDone)
	}()

	var wg sync.WaitGroup
	var stop atomic.Bool
	spin := func(fn func(i int)) {
		wg.Go(func() {
			for i := 0; !stop.Load(); i++ {
				fn(i)
				time.Sleep(time.Millisecond)
			}
		})
	}
	spin(func(i int) {
		peers := []checker.Target{peer}
		if i%2 == 0 {
			peers = append(peers, checker.Target{AgentID: "p2", NodeName: "peer-2", PodIP: "127.0.0.1", Port: peer.Port})
		}
		a.applyPeers(peers)
	})
	spin(func(int) {
		task := &pb.TaskRequest{TaskId: "t", CheckType: string(model.CheckTCP), Target: &pb.AgentMeta{
			Id: peer.AgentID, NodeName: peer.NodeName, PodIp: peer.PodIP, HttpPort: uint32(peer.Port), //nolint:gosec // test port
		}}
		ex.executeOne(ctx, task)
	})
	spin(func(i int) {
		a.applyExternalAssignment(&pb.ExternalCheckAssignment{Specs: []*pb.ExternalCheckSpec{{
			DefinitionId: "d", CheckType: "tcp", IntervalNs: int64(time.Minute), TimeoutNs: int64(time.Second),
			Target: &pb.ExternalTarget{Name: fmt.Sprintf("t%d", i%3), Address: "10.1.2.3", Port: 443},
		}}})
	})

	for i := range 40 {
		next := cloneConfig(cfg)
		next.Checkers.TCP.Interval = time.Duration(5+i%3) * time.Millisecond
		next.Checkers.TCP.Enabled = i%4 != 3
		next.Checkers.UDP.Enabled = i%2 == 0
		next.Checkers.External.Enabled = i%5 != 4
		next.Checkers.MTR.Cooldown = time.Duration(1+i%2) * time.Minute
		next.LogLevel = []string{"info", "debug"}[i%2]
		a.ApplyConfig(next)
		time.Sleep(5 * time.Millisecond)
	}
	if delivered.Load() == 0 {
		t.Error("no probe result was delivered while reloading: the race check exercised nothing")
	}

	stop.Store(true)
	wg.Wait()
	cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler.Run did not return after cancel: a checker loop leaked across reloads")
	}
	waitFor(t, 5*time.Second, "goroutines back near the baseline", func() bool {
		return runtime.NumGoroutine() <= baseline+5
	})
}

// Turning external checks off drops the assignment and its series, and back on re-subscribes clean.
func TestReloadExternalRebuildKeepsTheAssignmentAndDisableRetiresIt(t *testing.T) {
	cfg := reloadConfig(t)
	cfg.Checkers.External.Enabled = true
	cfg.Checkers.External.AllowedCIDRs = []string{"10.0.0.0/8"}
	cfg.Checkers.External.MaxTargets = 10
	cfg.Checkers.External.Timeout = time.Second
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	a.applyExternalAssignment(&pb.ExternalCheckAssignment{Specs: []*pb.ExternalCheckSpec{{
		DefinitionId: "d", CheckType: "icmp", IntervalNs: int64(time.Minute), TimeoutNs: int64(time.Second),
		Target: &pb.ExternalTarget{Name: "gw", Address: "10.1.2.3"},
	}}})
	recordExternalDetail(a.metrics, "reload-node", "", &ExternalDetails{
		Name: "gw", CheckType: model.CheckICMP, Success: false, LossRatio: 1,
	})

	widened := cloneConfig(cfg)
	widened.Checkers.External.AllowedCIDRs = []string{"10.0.0.0/8", "192.168.0.0/16"}
	a.ApplyConfig(widened)
	if a.externalChecker == nil || a.externalChecker.SpecCount() != 1 {
		t.Fatalf("the rebuilt external checker lost the assignment: %v", a.externalChecker)
	}
	if !a.external.Allowlist.Allow(netip.MustParseAddr("192.168.1.1")) {
		t.Error("the widened allowlist is not in force after the reload")
	}

	off := cloneConfig(widened)
	off.Checkers.External.Enabled = false
	a.ApplyConfig(off)
	if a.externalChecker != nil || a.external.Enabled {
		t.Fatal("external checks are still wired after a reload turned them off")
	}
	if got := testutil.CollectAndCount(a.metrics.ExternalPacketLoss); got != 0 {
		t.Errorf("%d external packet-loss series survive disabling external checks", got)
	}
}

// A plane switched off stops writing its gauges, so the last loss ratio must not keep serving (and
// alerting) as current; its counters stay, they only stop growing.
func TestReloadDisabledPlaneRetiresItsGauges(t *testing.T) {
	cfg := reloadConfig(t)
	cfg.Checkers.UDP.Enabled = true
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	handle := NewResultHandler(a.metrics, checker.Target{NodeName: "reload-node"})
	handle(model.CheckResult{Type: model.CheckUDP, Source: "reload-node", Destination: "peer-node",
		Details: &model.UDPDetails{PacketsSent: 5, LossRatio: 1}})
	if got := testutil.CollectAndCount(a.metrics.UDPLossRatio); got != 1 {
		t.Fatalf("udp loss series before the reload = %d, want 1", got)
	}

	next := cloneConfig(cfg)
	next.Checkers.UDP.Enabled = false
	a.ApplyConfig(next)
	if got := testutil.CollectAndCount(a.metrics.UDPLossRatio); got != 0 {
		t.Errorf("%d udp loss series survive a reload that disabled udp", got)
	}
	if got := testutil.CollectAndCount(a.metrics.UDPResults); got == 0 {
		t.Error("the udp result counters were dropped; only gauges go when a plane is switched off")
	}
}

// The top-level mode key and observability.otel parse for old configs and are read by nothing, so a
// change to them asks for no restart.
func TestReloadIgnoresUnreadKeys(t *testing.T) {
	for name, edit := range map[string]func(*config.Config){
		"mode": func(c *config.Config) { c.Mode = "agent" },
		"observability.otel": func(c *config.Config) {
			c.Observability.OTel.Enabled = !c.Observability.OTel.Enabled
			c.Observability.OTel.Endpoint = "otel:4317"
		},
	} {
		t.Run(name, func(t *testing.T) {
			logs := &strings.Builder{}
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })

			cfg := reloadConfig(t)
			a, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			next := cloneConfig(cfg)
			edit(next)
			a.ApplyConfig(next)
			if out := logs.String(); strings.Contains(out, "level=WARN") {
				t.Fatalf("a change to the unread %s keys asked for a restart:\n%s", name, out)
			}
		})
	}
}

// The wiring main uses: a file rewrite reaches ApplyConfig through the loader's OnChange.
func TestLoaderReloadReachesTheAgent(t *testing.T) {
	cfg := reloadConfig(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	write := func(tcpEnabled bool) {
		body := fmt.Sprintf("controllerAddress: %q\nhttpPort: %d\nmetricsPort: %d\ngrpcPort: %d\n"+
			"agent:\n  nodeName: reload-node\n  advertiseAddress: \"192.0.2.10\"\n"+
			"checkers:\n  tcp:\n    enabled: %v\n  udp:\n    enabled: false\n  icmp:\n    enabled: false\n"+
			"  pmtu:\n    enabled: false\n  dns:\n    enabled: false\n",
			cfg.ControllerAddress, cfg.HTTPPort, cfg.MetricsPort, cfg.GRPCPort, tcpEnabled)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(false)
	loader := config.NewLoader(path)
	if err := loader.Load(); err != nil {
		t.Fatal(err)
	}
	a, err := New(loader.Get())
	if err != nil {
		t.Fatal(err)
	}
	loader.OnChange(a.ApplyConfig)
	if err := loader.WatchForChanges(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = loader.Close() })

	write(true)
	waitFor(t, 5*time.Second, "tcp enabled by the file rewrite", func() bool {
		_, ok := a.probes().checkers[model.CheckTCP]
		return ok
	})
}

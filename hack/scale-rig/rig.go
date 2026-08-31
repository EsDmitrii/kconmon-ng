package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/agent"
	"github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/EsDmitrii/kconmon-ng/internal/controller"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// RigConfig sizes one measurement run. Defaults implement the roadmap scenarios: cold start over
// 10s, 10% rolling churn over 30s, 60s of steady heartbeats, then sequential propagation probes.
type RigConfig struct {
	N                 int
	ColdSpread        time.Duration
	ChurnFraction     float64
	ChurnSpread       time.Duration
	Steady            time.Duration
	Probes            int
	ProbeSpacing      time.Duration
	ProbeGrace        time.Duration
	HeartbeatInterval time.Duration
	QuiesceIdle       time.Duration
	QuiesceMax        time.Duration
}

func defaultRigConfig(n int) RigConfig {
	return RigConfig{
		N:             n,
		ColdSpread:    10 * time.Second,
		ChurnFraction: 0.10,
		ChurnSpread:   30 * time.Second,
		Steady:        60 * time.Second,
		Probes:        40,
		ProbeSpacing:  300 * time.Millisecond,
		ProbeGrace:    10 * time.Second,
		// The interval internal/agent.Run hardcodes for the real fleet.
		HeartbeatInterval: 5 * time.Second,
		QuiesceIdle:       1200 * time.Millisecond,
		QuiesceMax:        30 * time.Second,
	}
}

type Rig struct {
	cfg        RigConfig
	logs       *logCounter
	grpcAddr   string
	httpPort   int
	metricsURL string

	counters  rigCounters
	tracker   propTracker
	regLat    durSamples
	agents    []*rigAgent
	observers []*observer
}

func newRig(cfg RigConfig, logs *logCounter) *Rig { //nolint:gocritic // hugeParam: value semantics intentional, the rig keeps a snapshot
	r := &Rig{cfg: cfg, logs: logs}
	r.agents = make([]*rigAgent, cfg.N)
	for i := range r.agents {
		r.agents[i] = newRigAgent(r, i)
	}
	return r
}

// Run executes the whole measurement: controller up, observers on, cold start, churn, steady
// heartbeats, propagation probes; returns the assembled report.
func (r *Rig) Run(ctx context.Context) (*Report, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if err := r.startController(ctx); err != nil {
		return nil, err
	}
	if err := r.waitReady(ctx); err != nil {
		return nil, err
	}
	r.startObservers(ctx)

	rep := &Report{Config: r.cfg}

	// Cold start: N agents register over ColdSpread.
	s0 := r.snap(ctx)
	coldWall := r.coldStart(ctx)
	coldQuiesced := r.quiesce(ctx)
	s1 := r.snap(ctx)

	// Rolling churn: a fraction of the fleet restarts (deregister + register under a new pod
	// identity) over ChurnSpread.
	churnEvents, churnWall := r.churn(ctx)
	churnQuiesced := r.quiesce(ctx)
	s2 := r.snap(ctx)

	// Steady state: heartbeats only.
	steadyWall := r.steady(ctx)
	s3 := r.snap(ctx)

	// Propagation probes: one registration at a time, measuring time-to-full-fan-out.
	probeFailures := r.runProbes(ctx)
	s4 := r.snap(ctx)

	rep.ColdWall = coldWall
	rep.Registered = r.counters.activeAgents.Load()
	rep.RegisterFailures = r.counters.registerFail.Load()
	rep.RegisterRetries = r.counters.registerRetry.Load()
	regs := r.regLat.snapshot()
	rep.RegP50, rep.RegP95, rep.RegMax = percentile(regs, 50), percentile(regs, 95), percentile(regs, 100)

	rep.Phases = []PhaseReport{
		phaseReport("cold start (incl quiesce)", &s0, &s1),
		phaseReport("rolling churn (incl quiesce)", &s1, &s2),
		phaseReport("steady heartbeats", &s2, &s3),
		phaseReport("propagation probes", &s3, &s4),
	}
	rep.ChurnEvents = churnEvents
	rep.ChurnWall = churnWall
	rep.SteadyWall = steadyWall
	rep.QuiesceTimeouts = 0
	if !coldQuiesced {
		rep.QuiesceTimeouts++
	}
	if !churnQuiesced {
		rep.QuiesceTimeouts++
	}

	done, incomplete := r.tracker.results()
	rep.ProbeCount = r.cfg.Probes
	rep.ProbeDone = len(done)
	rep.ProbeIncomplete = incomplete
	rep.ProbeRegisterFailures = probeFailures
	rep.PropP50, rep.PropP95, rep.PropMax = percentile(done, 50), percentile(done, 95), percentile(done, 100)

	rep.FullSyncPeers = r.cfg.N
	rep.FullSyncBytes = r.fullSyncSizeAt(r.cfg.N)
	var delays []time.Duration
	for _, o := range r.observers {
		delays = append(delays, o.delaySamples()...)
		rep.ObserverResubs += o.resubs.Load()
	}
	rep.DeliveryP50, rep.DeliveryP95 = percentile(delays, 50), percentile(delays, 95)

	rep.MaxRSS = maxRSSBytes()
	rep.LogLines = sortedLogLines(r.logs.snapshot())

	r.shutdownFleet()
	return rep, nil
}

// startController wires a real controller exactly like cmd/controller (config defaults, New, Run),
// with leader election off — no apiserver here — and fresh localhost ports.
func (r *Rig) startController(ctx context.Context) error {
	grpcPort, err := freePort()
	if err != nil {
		return fmt.Errorf("grpc port: %w", err)
	}
	httpPort, err := freePort()
	if err != nil {
		return fmt.Errorf("http port: %w", err)
	}
	metricsPort, err := freePort()
	if err != nil {
		return fmt.Errorf("metrics port: %w", err)
	}

	cfg := config.DefaultConfig()
	cfg.GRPCPort = grpcPort
	cfg.HTTPPort = httpPort
	cfg.MetricsPort = metricsPort
	cfg.Controller.LeaderElection = false

	r.grpcAddr = fmt.Sprintf("127.0.0.1:%d", grpcPort)
	r.httpPort = httpPort
	r.metricsURL = fmt.Sprintf("http://127.0.0.1:%d/metrics", metricsPort)

	ctrl := controller.New(cfg)
	go func() {
		if runErr := ctrl.Run(ctx); runErr != nil && ctx.Err() == nil {
			// Counted by the log tap and fatal to the canary below if it happens at startup.
			fmt.Printf("controller exited: %v\n", runErr)
		}
	}()
	return nil
}

// waitReady registers and deregisters a canary over a raw client until the controller answers.
func (r *Rig) waitReady(ctx context.Context) error {
	conn, err := grpc.NewClient(r.grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	client := pb.NewAgentRegistryClient(conn)

	canary := &pb.AgentMeta{
		Id: "rig-canary", NodeName: "rig-canary", PodName: "rig-canary", PodIp: "10.255.255.254",
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		rctx, cancel := context.WithTimeout(ctx, time.Second)
		_, err = client.Register(rctx, &pb.RegisterRequest{Agent: canary})
		cancel()
		if err == nil {
			rctx, cancel = context.WithTimeout(ctx, time.Second)
			_, _ = client.Deregister(rctx, &pb.DeregisterRequest{AgentId: "rig-canary"})
			cancel()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return errors.New("controller did not become ready within 15s")
}

func (r *Rig) startObservers(ctx context.Context) {
	r.observers = []*observer{newObserver(0, r.grpcAddr), newObserver(1, r.grpcAddr)}
	for _, o := range r.observers {
		go o.run(ctx)
	}
	// Wait for each observer's initial FULL_SYNC so every later broadcast is counted.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ready := 0
		for _, o := range r.observers {
			if o.initials.Load() > 0 {
				ready++
			}
		}
		if ready == len(r.observers) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (r *Rig) coldStart(ctx context.Context) time.Duration {
	start := time.Now()
	interval := r.cfg.ColdSpread / time.Duration(r.cfg.N)
	var wg sync.WaitGroup
	for i, a := range r.agents {
		wg.Add(1)
		go func(i int, a *rigAgent) {
			defer wg.Done()
			slot := start.Add(time.Duration(i) * interval)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(slot)):
			}
			a.launch(ctx, true)
		}(i, a)
	}
	wg.Wait()
	return time.Since(start)
}

func (r *Rig) churn(ctx context.Context) (events int, wall time.Duration) {
	n := int(math.Round(r.cfg.ChurnFraction * float64(r.cfg.N)))
	if n <= 0 {
		return 0, 0
	}
	step := max(1, r.cfg.N/n)
	interval := r.cfg.ChurnSpread / time.Duration(n)

	start := time.Now()
	var wg sync.WaitGroup
	for k := range n {
		idx := (k * step) % r.cfg.N
		wg.Add(1)
		go func(k, idx int) {
			defer wg.Done()
			slot := start.Add(time.Duration(k) * interval)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(slot)):
			}
			r.agents[idx].restart(ctx)
		}(k, idx)
	}
	wg.Wait()
	return n, time.Since(start)
}

func (r *Rig) steady(ctx context.Context) time.Duration {
	start := time.Now()
	select {
	case <-ctx.Done():
	case <-time.After(r.cfg.Steady):
	}
	return time.Since(start)
}

/*
runProbes registers ephemeral agents one at a time, spaced beyond the coalescing window, so each
lands in its own broadcast and every fleet watcher's peer-list length crosses a fresh, strictly
increasing threshold. Probes never subscribe or heartbeat; the run ends before the agent TTL can
evict them.
*/
func (r *Rig) runProbes(ctx context.Context) (failures int) {
	client, err := agent.NewGRPCClient(r.grpcAddr, agent.ClientSecurity{})
	if err != nil {
		return r.cfg.Probes
	}
	defer func() { _ = client.Close() }()

	// The thresholds build on how many agents the CONTROLLER holds, cross-checked over its real
	// metrics listener; the rig's own count is the fallback.
	base := r.cfg.N
	if sc := scrapeMetrics(ctx, r.metricsURL); sc.OK && sc.Registered > 0 {
		base = int(sc.Registered + 0.5)
	}
	expected := int(r.counters.activeAgents.Load())

	for p := 1; p <= r.cfg.Probes; p++ {
		threshold := base + p - 1
		r.tracker.addProbe(threshold, expected, time.Now())
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, _, rerr := client.Register(rctx, probeAgentInfo(p), r.httpPort)
		cancel()
		if rerr != nil {
			failures++
		} else {
			r.counters.registerOK.Add(1)
		}
		select {
		case <-ctx.Done():
			return failures
		case <-time.After(r.cfg.ProbeSpacing):
		}
	}

	deadline := time.Now().Add(r.cfg.ProbeGrace)
	for time.Now().Before(deadline) {
		if _, incomplete := r.tracker.results(); incomplete == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return failures
		case <-time.After(100 * time.Millisecond):
		}
	}
	return failures
}

// quiesce waits until the control plane stops moving (no new flushes, callbacks or registrations
// for QuiesceIdle), so phase boundaries do not smear one scenario's tail into the next. Reports
// whether it settled before QuiesceMax.
func (r *Rig) quiesce(ctx context.Context) bool {
	deadline := time.Now().Add(r.cfg.QuiesceMax)
	last := r.activity()
	lastChange := time.Now()
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(150 * time.Millisecond):
		}
		cur := r.activity()
		if cur != last {
			last = cur
			lastChange = time.Now()
			continue
		}
		if time.Since(lastChange) >= r.cfg.QuiesceIdle {
			return true
		}
	}
	return false
}

func (r *Rig) activity() uint64 {
	return r.maxFlushes() + r.counters.peerCallbacks.Load() +
		r.counters.registerOK.Load() + r.counters.watchResubs.Load()
}

// maxFlushes takes the most complete observer's count: a desynced observer undercounts, so the max
// of two independent stable subscribers is the best available broadcast denominator.
func (r *Rig) maxFlushes() uint64 {
	var m uint64
	for _, o := range r.observers {
		m = max(m, o.flushes.Load())
	}
	return m
}

func (r *Rig) fullSyncSizeAt(peers int) int {
	var s int
	for _, o := range r.observers {
		s = max(s, o.sizeAt(peers))
	}
	return s
}

func (r *Rig) snap(ctx context.Context) resSnap {
	return takeSnap(ctx, r.metricsURL, &r.counters, r.maxFlushes())
}

func (r *Rig) shutdownFleet() {
	for _, a := range r.agents {
		a.stop()
	}
}

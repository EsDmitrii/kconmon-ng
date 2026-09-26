package agent

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"sync"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/checker"
	"github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/EsDmitrii/kconmon-ng/internal/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// maxConcurrentTasks bounds simultaneous on-demand diagnostic executions so a
// burst of API calls cannot fork-bomb the agent. Tasks arriving while saturated
// get an immediate error result.
const maxConcurrentTasks = 4

// capabilityExternalChecks is the AgentMeta.capabilities flag this agent advertises when
// checkers.external.enabled.
const capabilityExternalChecks = "external-checks"

type Agent struct {
	cfg            *config.Config
	grpcClient     *GRPCClient
	scheduler      *Scheduler
	httpServer     *HTTPServer
	probeServer    *ProbeServer
	metrics        *metrics.PrometheusMetrics
	promReg        *prometheus.Registry
	info           model.AgentInfo
	configuredZone string
	// checkers holds the same checker instances registered with the scheduler,
	// reused by the on-demand task executor. mtrChecker is kept separately since
	// it is not part of the Checker map (it bypasses the cooldown on demand).
	checkers   map[model.CheckType]checker.Checker
	mtrChecker *checker.MTRChecker
	// external is the gate applied to every probe whose destination is not a
	// registered peer. Its zero value is a closed gate.
	external ExternalPolicy
	// externalChecker runs the CONTINUOUS external assignment pushed over
	// WatchExternalChecks. It is nil unless checkers.external.enabled, which is
	// also the switch that decides whether the agent subscribes at all.
	externalChecker *checker.ExternalChecker

	// peerMu guards the peer list, a.info.Zone and departed: the peer watch and the heartbeat loop
	// both re-register, and their steps must not interleave.
	peerMu   sync.Mutex
	departed map[string]departedPeer // peer node name -> when it left the plan, and its zone then
	now      func() time.Time        // test seam; nil means time.Now

	// rejoinMu guards the rejoin grace (see rejoinPeerGrace) and the controller's last peer list; it
	// is taken before peerMu.
	rejoinMu        sync.Mutex
	rejoining       bool
	rejoinGen       uint64
	controllerPeers []checker.Target

	// probeMu guards what ApplyConfig replaces: cfg, checkers, mtrChecker, external and
	// externalChecker. It is a leaf: nothing else is locked while it is held.
	probeMu sync.RWMutex
	// reloadMu serialises ApplyConfig with the external assignment handler, and guards
	// lastAssignment and externalWatch. Lock order: reloadMu, peerMu, deliverMu, the scheduler's mu.
	reloadMu       sync.Mutex
	lastAssignment *pb.ExternalCheckAssignment
	// externalWatch starts or stops the WatchExternalChecks subscription; nil until Run sets it.
	externalWatch func(on bool)
	// readvertise asks Run to register again, carrying capabilities a reload changed.
	readvertise chan struct{}
}

func New(cfg *config.Config) (*Agent, error) {
	promReg := prometheus.NewRegistry()
	promReg.MustRegister(collectors.NewGoCollector())
	promReg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	m := metrics.NewPrometheusMetrics(cfg.MetricsPrefix, promReg)

	// Identity comes from the config (already env-overridden by the loader) plus
	// the Downward API pod env, with bare-host fallbacks; see identity.go.
	info, idErr := resolveIdentity(cfg)
	if idErr != nil {
		return nil, fmt.Errorf("resolving agent identity: %w", idErr)
	}
	info.Capabilities = agentCapabilities(cfg)
	advertiseListenerPorts(&info, cfg)

	// The external-destination gate is built here, not lazily at first probe.
	probes, err := buildProbes(cfg, nil, nil)
	if err != nil {
		return nil, err
	}

	source := checker.Target{
		AgentID:  info.ID,
		NodeName: info.NodeName,
		PodIP:    info.PodIP,
		Zone:     info.Zone,
		Port:     cfg.HTTPPort,
	}

	a := &Agent{
		cfg:         cfg,
		httpServer:  NewHTTPServer(promReg),
		probeServer: NewProbeServer(cfg.GRPCPort),
		metrics:     m,
		promReg:     promReg,
		info:        info,
		// The zone the operator CONFIGURED (agent.zone, env-overridden by the loader) as opposed to
		// the effective one in info.Zone; registrationInfo explains why only this one is asserted.
		configuredZone: cfg.Agent.Zone,
		checkers:       probes.checkers,
		mtrChecker:     probes.mtr,
		external:       probes.external,

		externalChecker: probes.externalChecker,
		readvertise:     make(chan struct{}, 1),
	}

	// The filter reads the external checker in force, which a reload may rebuild or remove.
	sched := NewScheduler(source, assignedExternalOnly(a.currentExternalChecker, NewResultHandler(m, source)))
	for _, e := range probes.schedule {
		sched.AddChecker(e.Checker, e.Config)
	}
	sched.SetMTRChecker(probes.mtr)

	// Self-observation (M9-2): the scheduler records its own cadence, the age
	// gauge reads the peer-list stamp at scrape time, and the series that alert
	// expressions consume exist from the first scrape rather than first event.
	sched.SetSelfMetrics(m)
	m.EnablePeerListAge(sched.PeerListUpdatedAt)
	preinitSelfMetrics(m, probes.checkers, probes.externalChecker != nil)
	a.scheduler = sched

	return a, nil
}

// preinitSelfMetrics creates the agent self-observation series at zero, one per enabled checker
// where labelled, so increase()/rate() expressions see data before the first event they count.
func preinitSelfMetrics(m *metrics.PrometheusMetrics, enabled map[model.CheckType]checker.Checker, externalEnabled bool) {
	names := make([]string, 0, len(enabled)+1)
	for checkType := range enabled {
		names = append(names, string(checkType))
	}
	if externalEnabled {
		names = append(names, string(model.CheckExternal))
	}
	for _, name := range names {
		m.AgentProbeCycleDuration.WithLabelValues(name)
		m.AgentProbeCycleOverruns.WithLabelValues(name).Add(0)
	}
	m.AgentControllerReconnects.WithLabelValues().Add(0)
	// Created, never zeroed: a reload runs this while detached reactive traces still hold a slot.
	m.AgentMTRReactiveInflight.WithLabelValues()
	m.AgentMTRReactiveCoalesced.WithLabelValues("cooldown").Add(0)
	m.AgentMTRReactiveCoalesced.WithLabelValues("saturated").Add(0)
}

// agentCapabilities returns the opt-in feature flags this agent advertises at registration; it
// mirrors the controller's capabilitiesFor: feature detection, never version sniffing.
//
// The plane:* entries name the probe planes this agent runs, one per enabled checker; plane:mtr is
// unconditional because New always builds the MTR checker. Fail-open rule, stated once: an agent
// advertising no plane:* at all (every agent older than 2.4.0) means "unknown, assume all planes".
// Consumers must never read absence as "unsupported", or a rolling upgrade would turn every grey
// cell into a calming "not probed" frame.
func agentCapabilities(cfg *config.Config) []string {
	caps := []string{}
	if cfg.Checkers.External.Enabled {
		caps = append(caps, capabilityExternalChecks)
	}
	planes := []struct {
		name    string
		enabled bool
	}{
		{string(model.CheckTCP), cfg.Checkers.TCP.Enabled},
		{string(model.CheckUDP), cfg.Checkers.UDP.Enabled},
		{string(model.CheckICMP), cfg.Checkers.ICMP.Enabled},
		{string(model.CheckPMTU), cfg.Checkers.PMTU.Enabled},
		{string(model.CheckDNS), cfg.Checkers.DNS.Enabled},
		{string(model.CheckHTTP), cfg.Checkers.HTTP.Enabled},
		{string(model.CheckMTR), true},
	}
	for _, p := range planes {
		if p.enabled {
			caps = append(caps, model.CapabilityPlanePrefix+p.name)
		}
	}
	return caps
}

// advertiseListenerPorts stamps the config's listener ports onto what the agent registers. Kept out
// of resolveIdentity on purpose: identity is WHO the agent is, the ports are WHERE it listens.
func advertiseListenerPorts(info *model.AgentInfo, cfg *config.Config) {
	info.HTTPPort = cfg.HTTPPort
	info.UDPPort = cfg.GRPCPort // config.grpcPort is the UDP echo port on an agent
	info.MetricsPort = cfg.MetricsPort
}

// ownPorts is the fallback for a peer that reported no ports (older than 2.4.0): such an agent
// listens where this one does, which was the fleet-wide contract before per-agent ports.
func (a *Agent) ownPorts() checker.PeerPorts {
	return checker.PeerPorts{HTTP: a.info.HTTPPort, UDP: a.info.UDPPort}
}

func (a *Agent) Run(ctx context.Context) error {
	slog.Info("starting agent",
		"node", a.info.NodeName,
		"pod", a.info.PodName,
		"ip", a.info.PodIP,
		"zone", a.info.Zone,
		"version", config.Version,
	)

	// Listeners, identity and the controller connection are restart-bound: read once, here.
	cfg := a.appliedConfig()

	if err := a.probeServer.ListenUDP(ctx); err != nil {
		return fmt.Errorf("starting UDP probe server: %w", err)
	}
	defer func() { _ = a.probeServer.Close() }()

	grpcClient, err := NewGRPCClient(cfg.ControllerAddress, clientSecurityFromConfig(cfg))
	if err != nil {
		return fmt.Errorf("creating gRPC client: %w", err)
	}
	a.grpcClient = grpcClient
	defer func() { _ = grpcClient.Close() }()
	grpcClient.OnFleetEchoes(a.probeServer.SetFleetEchoAddrs)

	// The health/metrics plane comes up BEFORE the first registration: the chart's
	// startupProbe polls /healthz with a finite budget, and an agent that stays dark
	// through a controller outage takes the whole DaemonSet into CrashLoopBackOff.
	errCh := make(chan error, 2)
	httpSrv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.HTTPPort),
		Handler:      a.httpServer.Handler(),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		slog.Info("HTTP server listening", "port", cfg.HTTPPort)
		errCh <- httpSrv.ListenAndServe()
	}()

	// The metrics listener, on its own port: see internal/metrics/listener.go.
	metricsSrv := metrics.NewListener(
		fmt.Sprintf(":%d", cfg.MetricsPort),
		metrics.NewListenerHandler(a.promReg, a.httpServer.Ready),
	)

	go func() {
		slog.Info("metrics server listening", "port", cfg.MetricsPort)
		errCh <- metricsSrv.ListenAndServe()
	}()

	shutdownHTTP := func() error {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	}

	// The zone is adopted before the scheduler starts, so every series carries the right source_zone
	// from the first check.
	backoff := 1 * time.Second
	maxBackoff := 15 * time.Second
	for {
		err = a.register(ctx, grpcClient)
		if err == nil {
			break
		}
		// The payload is built from env/config fixed at startup, so a rejection of the
		// payload itself can never be retried into success: fail loudly instead of looping.
		if isConfigRejection(err) {
			slog.Error("controller rejected the registration payload, fix the agent configuration (downward API env, zone, pod IP)", "error", err)
			_ = shutdownHTTP()
			return fmt.Errorf("registration rejected: %w", err)
		}
		slog.Warn("controller not ready, retrying", "error", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			_ = shutdownHTTP()
			return ctx.Err()
		case serveErr := <-errCh:
			return serveErr
		case <-time.After(backoff):
		}
		// Redial only when this connection refused us, so the next attempt is load-balanced again: a
		// standby rejects registration, and retrying on the same connection would keep landing on it.
		grpcClient.redialIfRefused(err)
		backoff = nextRegisterWait(err, backoff*2, maxBackoff)
	}
	a.scheduler.Pause()

	peerWatchReady := make(chan struct{}, 1)
	reregisterCh := make(chan struct{}, 1)

	grpcClient.OnPeersUpdate(func(targets []checker.Target) {
		a.applyControllerPeers(targets)
		a.scheduler.Resume()
		select {
		case peerWatchReady <- struct{}{}:
		default:
		}
	})

	grpcClient.OnNeedReregister(func() {
		select {
		case reregisterCh <- struct{}{}:
		default:
		}
	})

	// On-demand diagnostic task executor; executions run in goroutines tied to the root ctx (via
	// OnTask below).
	taskExecutor := a.newTaskExecutor(grpcClient)
	grpcClient.OnTask(func(taskCtx context.Context, task *pb.TaskRequest) {
		taskExecutor.Handle(taskCtx, task)
	})

	go grpcClient.StartHeartbeat(ctx, 5*time.Second)

	// Deliberately NO scheduler.Pause() here: probing keeps running on the last known
	// peer list, because pausing blinded the whole fleet for the duration of every
	// controller restart, upgrade, or failover — exactly when measurements matter most.
	var reregMu sync.Mutex
	// reregister registers again until it succeeds or ctx ends, and reports which. wait is the pause
	// before the first attempt; every failure doubles it, jittered, within reregisterMin/MaxWait.
	reregister := func(wait time.Duration) bool {
		// The peer watch, the heartbeat loop and a reload all land here; one retry loop at a time.
		reregMu.Lock()
		defer reregMu.Unlock()
		for {
			if wait > 0 {
				select {
				case <-ctx.Done():
					return false
				case <-time.After(wait + randomDelay(wait/4)):
				}
			}
			regErr := a.register(ctx, grpcClient)
			if regErr == nil {
				return true
			}
			wait = nextRegisterWait(regErr, max(wait*2, reregisterMinWait), reregisterMaxWait)
			// A rejection here is a config error, but unlike the first registration we keep
			// retrying: probes continue on the last known peer list, and a mid-life
			// InvalidArgument may just be a controller upgrade tightening validation.
			if isConfigRejection(regErr) {
				slog.Error("controller rejected the re-registration payload, check agent configuration and controller/agent version skew", "error", regErr, "backoff", wait)
			} else {
				slog.Warn("re-registration failed, retrying", "error", regErr, "backoff", wait)
			}
			// Same reason as the first registration: only a fresh connection can reach the leader
			// after a failover moved it to another pod.
			grpcClient.redialIfRefused(regErr)
		}
	}
	reconnect := func() {
		// One increment per ENTRY into re-registration — a lost stream or a
		// heartbeat rejection — not per retry inside it: the counter answers
		// "how often does this agent lose its controller", and a long outage is
		// one loss however many backoff attempts it takes.
		a.metrics.AgentControllerReconnects.WithLabelValues().Inc()
		gen := a.beginRejoin()
		if reregister(reregisterMinWait) {
			a.endRejoinAfter(gen, rejoinPeerGrace)
			slog.Info("re-registered with controller after reconnect")
		}
	}

	go func() {
		for {
			err := grpcClient.WatchPeers(ctx, a.ownPorts())
			if ctx.Err() != nil {
				return
			}
			slog.Warn("peer watch disconnected, re-registering", "error", err)
			reconnect()
		}
	}()

	// WatchTasks only re-subscribes; peer re-registration is owned by the WatchPeers loop above.
	go resubscribeLoop(ctx, "task", grpcClient.WatchTasks)

	// WatchExternalChecks re-subscribes like WatchTasks; it runs ONLY while checkers.external.enabled,
	// which a reload can switch either way.
	grpcClient.OnExternalAssignment(a.applyExternalAssignment)
	var stopExternalWatch context.CancelFunc
	watchExternal := func(on bool) {
		if on == (stopExternalWatch != nil) {
			return
		}
		if !on {
			stopExternalWatch()
			stopExternalWatch = nil
			return
		}
		watchCtx, stop := context.WithCancel(ctx)
		stopExternalWatch = stop
		go resubscribeLoop(watchCtx, "external check", grpcClient.WatchExternalChecks)
	}
	a.reloadMu.Lock()
	a.externalWatch = watchExternal
	watchExternal(a.currentExternalChecker() != nil)
	a.reloadMu.Unlock()

	// A reload that changed the enabled planes registers again at once, so the controller, the Console
	// and the CLI gate on the capabilities this agent actually has now.
	go func() {
		for {
			select {
			case <-a.readvertise:
				if reregister(0) {
					slog.Info("re-advertised capabilities to the controller", "capabilities", a.registrationInfo().Capabilities)
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		for {
			select {
			case <-reregisterCh:
				slog.Info("heartbeat triggered re-registration")
				reconnect()
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				a.sweepDepartedPeers()
			case <-ctx.Done():
				return
			}
		}
	}()

	go a.scheduler.Run(ctx)

	select {
	case <-peerWatchReady:
		slog.Info("peer watch confirmed, agent fully ready")
	case <-time.After(30 * time.Second):
		slog.Warn("peer watch not confirmed within 30s, marking ready anyway")
		a.scheduler.Resume()
	case <-ctx.Done():
		_ = shutdownHTTP()
		return ctx.Err()
	}
	a.httpServer.SetReady(true)

	select {
	case <-ctx.Done():
		slog.Info("shutting down agent")
		a.httpServer.SetReady(false)

		// Stop probing, then tell the controller to drop us immediately so peers stop probing this pod IP
		// right away instead of after TTL eviction.
		a.scheduler.Pause()
		a.gracefulDeregister(grpcClient)

		return shutdownHTTP()
	case err := <-errCh:
		return err
	}
}

// applyExternalAssignment turns a controller assignment into validated agent specs and swaps them
// into the external checker; the swap replaces the whole target list under the CHECKER's own mutex
// and NEVER restarts the scheduler.
//
// Caller holds reloadMu. retireFrom is the checker whose applied targets the departures are counted
// against: nil means the one in force, a reload passes the checker it just replaced.
func (a *Agent) applyExternalAssignmentLocked(assignment *pb.ExternalCheckAssignment, retireFrom *checker.ExternalChecker) {
	p := a.probes()
	if p.externalChecker == nil {
		return
	}
	if retireFrom == nil {
		retireFrom = p.externalChecker
	}
	specs := assignment.GetSpecs()
	parsed := make([]checker.ExternalSpec, 0, len(specs))
	dropped := 0

	for _, s := range specs {
		in := checker.ExternalSpecInput{
			DefinitionID: s.GetDefinitionId(),
			Name:         s.GetTarget().GetName(),
			Address:      s.GetTarget().GetAddress(),
			Port:         s.GetTarget().GetPort(),
			CheckType:    s.GetCheckType(),
			Interval:     time.Duration(s.GetIntervalNs()),
			Timeout:      time.Duration(s.GetTimeoutNs()),
			ParamsJSON:   s.GetParamsJson(),
		}
		spec, err := checker.ParseExternalSpec(&in)
		if err != nil {
			dropped++
			/* And it is COUNTED, not only logged. A definition every agent refuses is invisible
			   otherwise: the Console lists it as enabled, the controller keeps pushing it, and the
			   only trace is a WARN in one pod's log that repeats every assignment. The counter is
			   what an operator can alert on and what makes "this check has never produced a result"
			   answerable without reading agent logs. */
			a.metrics.ExternalSpecsRejected.WithLabelValues(a.info.NodeName, s.GetCheckType()).Inc()
			// definitionId and checkType are controller-side identifiers; the
			// target address stays out of the message for the same reason the
			// on-demand refusal path keeps it out (see approveExternalTarget).
			slog.Warn("dropping invalid external check spec",
				"definitionId", s.GetDefinitionId(),
				"checkType", s.GetCheckType(),
				"error", err,
			)
			continue
		}
		parsed = append(parsed, spec)
	}

	/* checkers.external.maxTargets, ENFORCED. It was parsed, defaulted to 100, validated and logged
	   at boot, and then consulted by nothing: the controller's assignment was applied whole, however
	   long it was, so the documented per-agent ceiling bounded nothing at all. Truncation is loud —
	   an operator who set a ceiling needs to know it bit, and which end was dropped. */
	overflow := 0
	if limit := p.external.MaxTargets; limit > 0 && len(parsed) > limit {
		overflow = len(parsed) - limit
		parsed = parsed[:limit]
	}

	/* Targets that left the assignment lose their gauges HERE, which is the only event that knows
	   they left. Nothing used to do this: the external gauges were cleared only as collateral damage
	   from a peer update, so a target the controller had stopped assigning went on reporting its last
	   packet-loss reading for as long as the peer list held still — an alert firing on a check that
	   no longer runs. Truncated targets count as departed for the same reason: they are not probed. */
	a.scheduler.whileNoDelivery(func() {
		retireDepartedExternalTargets(a.metrics, retireFrom, parsed)
		p.externalChecker.SetSpecs(parsed)
	})
	if overflow > 0 {
		slog.Warn("external check assignment exceeds checkers.external.maxTargets; the tail is not probed",
			"applied", len(parsed), "dropped", overflow, "maxTargets", p.external.MaxTargets)
	}
	slog.Info("external check assignment applied", "targets", len(parsed), "dropped", dropped)
}

/*
retireDepartedExternalTargets drops the gauges of targets the incoming assignment no longer names.

Counts() is the applied set, read under the checker's own lock, and it is read BEFORE SetSpecs
swaps it — so the difference against next is exactly what left. A name is retired only when it is
absent from the whole incoming list: the same target may be assigned under two check types (a host
probe and a URL probe share the `target` label and differ in `target_kind`), and one of them ending
must not blank the other.
*/
func retireDepartedExternalTargets(m *metrics.PrometheusMetrics, c *checker.ExternalChecker, next []checker.ExternalSpec) {
	if c == nil {
		return
	}
	applied := c.Counts()
	if len(applied) == 0 {
		return
	}
	/* Retired per CHECK, not per target NAME.
	   The gauges are keyed by (target, target_kind, check_type), so a name-keyed sweep retired
	   nothing as long as ANY check still named the target: deleting the icmp check on a target that
	   also carries an http one left its packet-loss gauge serving its last value forever, and since
	   a failed icmp probe pins that value at 1, an operator who deleted a check BECAUSE it was
	   failing froze 100% loss on the target permanently. The name-level sweep stays for the case it
	   is right for -- every check on a name gone -- because it also clears series whose check_type
	   this build no longer produces. */
	keepName := make(map[string]struct{}, len(next))
	keepCheck := make(map[[2]string]struct{}, len(next))
	for i := range next {
		keepName[next[i].Name] = struct{}{}
		keepCheck[[2]string{next[i].Name, string(next[i].Type)}] = struct{}{}
	}
	seenName := make(map[string]struct{}, len(applied))
	seenCheck := make(map[[2]string]struct{}, len(applied))
	for i := range applied {
		name := applied[i].Name
		checkType := string(applied[i].Type)
		if _, ok := keepName[name]; !ok {
			if _, dup := seenName[name]; !dup {
				seenName[name] = struct{}{}
				m.RetireExternalTarget(name)
			}
			continue
		}
		key := [2]string{name, checkType}
		if _, ok := keepCheck[key]; ok {
			continue
		}
		if _, dup := seenCheck[key]; dup {
			continue
		}
		seenCheck[key] = struct{}{}
		m.ForgetExternalCheck(name, externalTargetKind(applied[i].Type), checkType)
	}
}

// deregisterer is the narrow slice of the gRPC client used at shutdown, kept as
// an interface so the deregistration path can be tested without a live server.
type deregisterer interface {
	Deregister(ctx context.Context) error
}

// gracefulDeregister makes a best-effort Deregister call with a short timeout.
// The parent context is already cancelled at this point, so it uses a fresh
// background context. Failures are logged and never block shutdown.
func (a *Agent) gracefulDeregister(d deregisterer) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := d.Deregister(ctx); err != nil {
		slog.Warn("graceful deregister failed, controller will evict on TTL", "error", err)
	}
}

/*
register registers this agent and takes on what the controller answered: the zone it resolved, unless
one is configured, and the peer list. The zone is the only thing taken from RegisterResponse.Agent:
ports never are, because the config is the truth about where this process listens.
*/
func (a *Agent) register(ctx context.Context, c *GRPCClient) error {
	peers, zone, err := c.Register(ctx, a.registrationInfo(), a.ownPorts())
	if err != nil {
		return err
	}
	if z := resolveZone(a.configuredZone, zone); a.adoptZone(z) {
		slog.Info("adopted zone from controller", "zone", z)
	}
	a.applyControllerPeers(peers)
	return nil
}

/*
registrationInfo is what this agent ASSERTS about itself, which is not the same as what it currently
believes.

The zone it carries is the CONFIGURED one (agent.zone) and nothing else — empty when there is none.
a.info.Zone holds the effective zone, which after the first registration is usually the one the
controller resolved from the node's failure-domain label and this agent then adopted. Sending that
back turned an answer into a claim: the registry only consults its ZoneResolver when the agent
supplies no zone, so from the second registration onward the agent's stale copy won. Relabel a node
and the informer corrects the registry (UpdateZone), the agent re-registers minutes later with the
zone it adopted before the change, and the topology is wrong again — permanently, because every
subsequent re-registration re-asserts it. Cross-zone matrix views, zone-scoped alerts and the
per-zone dashboards all read that field.

Asserting only what an operator configured keeps the registry's rule true: an explicit zone wins, an
absent one is resolved from the node, and the node's label stays authoritative for as long as nobody
overrides it.
*/
func (a *Agent) registrationInfo() model.AgentInfo {
	a.peerMu.Lock()
	info := a.info
	a.peerMu.Unlock()
	info.Zone = a.configuredZone
	return info
}

// resolveZone decides the agent's effective zone: an explicitly configured
// zone always wins; otherwise the controller-resolved zone is adopted.
func resolveZone(configuredZone, resolvedZone string) string {
	if configuredZone != "" {
		return configuredZone
	}
	return resolvedZone
}

/*
forgetDepartedPeers retires the gauges of peers that are in the CURRENT list and not in the next
one, and the series a peer's zone change leaves under its old zone; it touches nothing else.

The peer list is replaced wholesale on every update, so the departures are the difference between
the two. Anything still present keeps its readings: a gauge is only repopulated by the next probe of
that pair, and blanking a live peer's loss ratio for a check interval is a gap in the series alerts
evaluate, appearing once per pod event — a rolling DaemonSet restart is one per node.
*/
func (a *Agent) forgetDepartedPeers(next []checker.Target) {
	keep := make(map[string]string, len(next))
	for i := range next {
		name := next[i].NodeName
		keep[name] = next[i].Zone
		if d, ok := a.departed[name]; ok && d.zone != next[i].Zone {
			a.metrics.RetirePeerZone(name, d.zone)
		}
		delete(a.departed, name)
	}
	pmtu, _ := a.probes().checkers[model.CheckPMTU].(interface{ ForgetPeer(string) })
	current := a.scheduler.Peers()
	for i := range current {
		name := current[i].NodeName
		zone, ok := keep[name]
		switch {
		case !ok:
			a.metrics.ForgetPeer(name)
			if pmtu != nil {
				pmtu.ForgetPeer(name)
			}
			if a.departed == nil {
				a.departed = make(map[string]departedPeer)
			}
			if _, already := a.departed[name]; !already {
				a.departed[name] = departedPeer{at: a.clock(), zone: current[i].Zone}
			}
		case zone != current[i].Zone:
			a.metrics.RetirePeerZone(name, current[i].Zone)
		}
	}
}

// applyPeers installs a new peer list: departed peers' gauges go, the list is swapped, the per-pair
// series of the new list are pre-created, and the echo server learns the peers' echo addresses.
func (a *Agent) applyPeers(next []checker.Target) {
	a.peerMu.Lock()
	defer a.peerMu.Unlock()
	a.scheduler.ReplacePeers(next, func() { a.forgetDepartedPeers(next) })
	a.sweepDepartedPeersLocked()
	a.syncPeerMetrics()
	if a.probeServer != nil {
		echoes := make([]netip.AddrPort, 0, len(next))
		own := a.ownPorts()
		for i := range next {
			if ap, ok := echoEndpoint(next[i].PodIP, cmp.Or(next[i].UDPPort, own.UDP)); ok {
				echoes = append(echoes, ap)
			}
		}
		a.probeServer.SetPeerEchoAddrs(echoes)
	}
}

// adoptZone makes z this agent's effective zone and reports whether it changed; the series written
// under the old zone go, since nothing writes them again.
func (a *Agent) adoptZone(z string) bool {
	a.peerMu.Lock()
	defer a.peerMu.Unlock()
	if z == a.info.Zone {
		return false
	}
	old := a.info.Zone
	a.scheduler.whileNoDelivery(func() {
		if a.metrics != nil {
			a.metrics.RetireSourceZone(a.info.NodeName, old)
		}
		a.info.Zone = z
		a.scheduler.SetSourceZone(z)
	})
	return true
}

// departedPeerGrace delays deleting a departed peer's counters (its gauges go at once). A controller
// failover re-registers the fleet one agent at a time, and live peers must not get a counter reset.
const departedPeerGrace = 10 * time.Minute

// departedPeer is when a peer left the plan and the zone it left under: a peer back under another
// zone inside the grace period leaves that zone's counters to retire.
type departedPeer struct {
	at   time.Time
	zone string
}

func (a *Agent) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

// sweepDepartedPeers retires the counters of peers out of the plan for longer than departedPeerGrace.
func (a *Agent) sweepDepartedPeers() {
	a.peerMu.Lock()
	defer a.peerMu.Unlock()
	a.sweepDepartedPeersLocked()
}

func (a *Agent) sweepDepartedPeersLocked() {
	now := a.clock()
	for name, d := range a.departed {
		if now.Sub(d.at) >= departedPeerGrace {
			a.metrics.RetirePeer(name)
			delete(a.departed, name)
		}
	}
}

// reregisterMinWait and reregisterMaxWait bound the wait between registration attempts once the
// agent runs; after a lost controller the first attempt also waits reregisterMinWait.
const (
	reregisterMinWait = 2 * time.Second
	reregisterMaxWait = 30 * time.Second
	// standbyRetryWait replaces the backoff while a standby refuses: during a failover the redial
	// has an even chance of landing on it again, and a growing backoff left pairs unprobed ~100 s.
	standbyRetryWait = time.Second
)

// nextRegisterWait is the wait before the next registration attempt after err: standbyRetryWait for
// a standby's refusal, otherwise grown, capped at ceiling.
func nextRegisterWait(err error, grown, ceiling time.Duration) time.Duration {
	if isStandbyRefusal(err) {
		return standbyRetryWait
	}
	return min(grown, ceiling)
}

/*
rejoinPeerGrace is how long after re-registering the agent keeps probing the peers it already had.
The new leader starts with an empty registry and hands out lists of whoever has re-registered so
far; applied wholesale, they stop every pair towards the rest of the fleet until those agents are
back. Inside the grace, lists only add peers; at its end the newest list applies as it is.
*/
const rejoinPeerGrace = 30 * time.Second

// beginRejoin enters the rejoin grace, and returns its generation for endRejoinAfter.
func (a *Agent) beginRejoin() uint64 {
	a.rejoinMu.Lock()
	defer a.rejoinMu.Unlock()
	a.rejoinGen++
	a.rejoining = true
	return a.rejoinGen
}

// endRejoinAfter ends rejoin generation gen after d, unless a later loss started another.
func (a *Agent) endRejoinAfter(gen uint64, d time.Duration) {
	time.AfterFunc(d, func() {
		a.rejoinMu.Lock()
		defer a.rejoinMu.Unlock()
		if gen != a.rejoinGen || !a.rejoining {
			return
		}
		a.rejoining = false
		if a.controllerPeers != nil {
			a.applyPeers(a.controllerPeers)
		}
	})
}

// applyControllerPeers installs a peer list from the controller, merged into the current one while
// rejoining.
func (a *Agent) applyControllerPeers(next []checker.Target) {
	a.rejoinMu.Lock()
	defer a.rejoinMu.Unlock()
	a.controllerPeers = next
	if a.rejoining {
		next = mergePeers(next, a.scheduler.Peers())
	}
	a.applyPeers(next)
}

// mergePeers is next plus the peers of current it does not name; next's entry wins for a node in
// both, since it carries what the peer reported this time.
func mergePeers(next, current []checker.Target) []checker.Target {
	named := make(map[string]struct{}, len(next))
	for i := range next {
		named[next[i].NodeName] = struct{}{}
	}
	out := slices.Clone(next)
	for i := range current {
		if _, ok := named[current[i].NodeName]; !ok {
			out = append(out, current[i])
		}
	}
	return out
}

// resubscribeMinBackoff and resubscribeMaxBackoff bound the wait before a stream subscribes again.
const (
	resubscribeMinBackoff = time.Second
	resubscribeMaxBackoff = 15 * time.Second
)

// resubscribeLoop keeps a controller stream subscribed until ctx ends; stream names it in the log.
func resubscribeLoop(ctx context.Context, stream string, watch func(context.Context) error) {
	backoff := resubscribeMinBackoff
	for {
		subscribed := time.Now()
		err := watch(ctx)
		if ctx.Err() != nil {
			return
		}
		backoff = resubscribeWait(backoff, time.Since(subscribed))
		if isStandbyRefusal(err) {
			backoff = standbyRetryWait
		}
		slog.Warn(stream+" watch disconnected, re-subscribing", "error", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, resubscribeMaxBackoff)
	}
}

// resubscribeWait returns the wait before re-subscribing a stream that lived for lived. A stream that
// outlasted the backoff ceiling was a healthy session, so the backoff starts over.
func resubscribeWait(backoff, lived time.Duration) time.Duration {
	if lived >= resubscribeMaxBackoff {
		return resubscribeMinBackoff
	}
	return backoff
}

// assignedExternalOnly drops details of checks no longer assigned. A sweep in flight during an
// assignment swap would otherwise write back the series the swap just retired. current is the
// checker in force; nil (external checks switched off) keeps nothing.
func assignedExternalOnly(current func() *checker.ExternalChecker, next ResultHandler) ResultHandler {
	return func(r model.CheckResult) {
		if details, ok := r.Details.([]ExternalDetails); ok && r.Type == model.CheckExternal {
			assigned := make(map[[2]string]struct{})
			if c := current(); c != nil {
				for _, cnt := range c.Counts() {
					assigned[[2]string{cnt.Name, string(cnt.Type)}] = struct{}{}
				}
			}
			kept := make([]ExternalDetails, 0, len(details))
			for i := range details {
				if _, ok := assigned[[2]string{details[i].Name, string(details[i].CheckType)}]; ok {
					kept = append(kept, details[i])
				}
			}
			r.Details = kept
		}
		next(r)
	}
}

// syncPeerMetrics pre-creates the per-pair and zone series of the current peer list and publishes it
// as the probe plan. Caller holds peerMu.
func (a *Agent) syncPeerMetrics() {
	source := checker.Target{NodeName: a.info.NodeName, Zone: a.info.Zone}
	peers := a.scheduler.Peers()
	checkers := a.probes().checkers
	preinitPeerResults(a.metrics, source, peers, checkers)
	preinitZoneResults(a.metrics, source, peers, checkers)
	markProbeIntended(a.metrics, source, peers)
}

// markProbeIntended publishes the topology plan as kconmon_ng_probe_intended: 1 per peer this
// agent is assigned, source_node always self. The peer list IS the plan — under a sparse mesh the
// controller already sends the trimmed set, so full mesh is just the degenerate "every peer" case.
// Departed peers' series are deleted in ForgetPeer, so after every update the family reads as
// exactly the current assignment.
func markProbeIntended(
	m *metrics.PrometheusMetrics,
	source checker.Target, //nolint:gocritic // hugeParam: Target is passed by value throughout this package
	peers []checker.Target,
) {
	for i := range peers {
		m.ProbeIntended.WithLabelValues(source.NodeName, peers[i].NodeName).Set(1)
	}
}

// resultOutcomes is the closed set of values the "result" label takes on a peer probe counter.
var resultOutcomes = [...]string{"success", "fail"}

// preinitPeerResults creates both outcome series for every peer of every enabled peer-probing
// checker. Without it a pair that has never failed has no result="fail" series at all, and the
// console matrix renders null where it should render 0.
func preinitPeerResults(
	m *metrics.PrometheusMetrics,
	source checker.Target, //nolint:gocritic // hugeParam: Target is passed by value throughout this package
	peers []checker.Target,
	enabled map[model.CheckType]checker.Checker,
) {
	for checkType := range enabled {
		counter := m.PeerResultCounter(string(checkType))
		if counter == nil {
			continue
		}
		for _, peer := range peers {
			for _, outcome := range resultOutcomes {
				// Add(0) creates the series without recording an observation, and repeating it on
				// every peer update is a no-op.
				counter.WithLabelValues(
					source.NodeName, peer.NodeName, source.Zone, peer.Zone, outcome,
				).Add(0)
			}
		}
	}
}

// preinitZoneResults creates the zone-family counter series for every zone pair the current peer
// list implies, so zone alert expressions see data from the first scrape rather than absent series.
// Keyed per zone PAIR, not per peer, and never cleaned up: zones outlive peers by design.
func preinitZoneResults(
	m *metrics.PrometheusMetrics,
	source checker.Target, //nolint:gocritic // hugeParam: Target is passed by value throughout this package
	peers []checker.Target,
	enabled map[model.CheckType]checker.Checker,
) {
	destZones := make(map[string]struct{}, len(peers))
	for i := range peers {
		destZones[peers[i].Zone] = struct{}{}
	}
	for checkType := range enabled {
		counter := m.ZoneResultCounter(string(checkType))
		if counter == nil {
			continue
		}
		sent, received := m.ZonePacketCounters(string(checkType))
		for zone := range destZones {
			for _, outcome := range resultOutcomes {
				counter.WithLabelValues(source.Zone, zone, outcome).Add(0)
			}
			if sent != nil {
				sent.WithLabelValues(source.Zone, zone).Add(0)
				received.WithLabelValues(source.Zone, zone).Add(0)
			}
		}
	}
}

func NewResultHandler(m *metrics.PrometheusMetrics, source checker.Target) ResultHandler { //nolint:gocritic // hugeParam: Target is a VALUE by design -- a checker must not be able to mutate the caller's copy, and one 80-byte copy per probe is nothing next to the probe itself
	return func(result model.CheckResult) {
		labels := []string{result.Source, result.Destination, result.SourceZone, result.DestZone}
		resultStr := "success"
		if !result.Success {
			resultStr = "fail"
		}
		resultLabels := []string{result.Source, result.Destination, result.SourceZone, result.DestZone, resultStr}
		// The zone family is the SECOND write of the same probe; empty zones stay "" verbatim,
		// exactly as the per-pair labels above carry them.
		zoneLabels := []string{result.SourceZone, result.DestZone}
		zoneResultLabels := []string{result.SourceZone, result.DestZone, resultStr}

		switch result.Type {
		case model.CheckTCP:
			if d, ok := result.Details.(*TCPDetails); ok {
				m.TCPConnectDuration.WithLabelValues(labels...).Observe(d.ConnectTime.Seconds())
				m.TCPTotalDuration.WithLabelValues(labels...).Observe(d.TotalTime.Seconds())
				m.ZoneTCPConnect.WithLabelValues(zoneLabels...).Observe(d.ConnectTime.Seconds())
				m.ZoneTCPTotal.WithLabelValues(zoneLabels...).Observe(d.TotalTime.Seconds())
			}
			m.TCPResults.WithLabelValues(resultLabels...).Inc()
			m.ZoneTCPResults.WithLabelValues(zoneResultLabels...).Inc()

		case model.CheckUDP:
			if d, ok := result.Details.(*UDPDetails); ok {
				/* Only what was MEASURED. A probe that lost every packet leaves MeanRTT and Jitter at
				   zero, and observing those pulled the RTT quantiles and the jitter gauge DOWN during
				   an outage — the ICMP branch below has always guarded this; UDP did not. */
				if d.PacketsRecv > 0 {
					m.UDPRtt.WithLabelValues(labels...).Observe(d.MeanRTT.Seconds())
					m.UDPJitter.WithLabelValues(labels...).Set(d.Jitter.Seconds())
					m.ZoneUDPRtt.WithLabelValues(zoneLabels...).Observe(d.MeanRTT.Seconds())
				}
				m.UDPLossRatio.WithLabelValues(labels...).Set(d.LossRatio)
				// Zone loss is counters, never an averaged ratio: the real on-the-wire packet
				// counts keep sum(rate(received))/sum(rate(sent)) weighted by traffic.
				m.ZoneUDPPacketsSent.WithLabelValues(zoneLabels...).Add(float64(d.PacketsSent))
				m.ZoneUDPPacketsReceived.WithLabelValues(zoneLabels...).Add(float64(d.PacketsRecv))
			}
			m.UDPResults.WithLabelValues(resultLabels...).Inc()
			m.ZoneUDPResults.WithLabelValues(zoneResultLabels...).Inc()

		case model.CheckICMP:
			// The loss ratio is a GAUGE, so it keeps serving its last written value on every scrape until
			// something writes again.
			d, ok := result.Details.(*ICMPDetails)
			if ok {
				m.ICMPLossRatio.WithLabelValues(labels...).Set(d.LossRatio)
			} else if !result.Success {
				m.ICMPLossRatio.WithLabelValues(labels...).Set(1)
			}
			// A probe that got no reply has no round trip, so its duration is the configured timeout.
			if ok && result.Success {
				m.ICMPRtt.WithLabelValues(labels...).Observe(d.RTT.Seconds())
				m.ZoneICMPRtt.WithLabelValues(zoneLabels...).Observe(d.RTT.Seconds())
			}
			/* ICMPDetails carries no packet counts, but the checker sends exactly ONE echo per
			   probe and attaches Details only after the request went on the wire — so Details
			   present means 1 sent, success means 1 received. A probe that died before the write
			   (bad IP, listen/marshal error) put nothing on the wire and counts nothing. */
			if ok {
				m.ZoneICMPPacketsSent.WithLabelValues(zoneLabels...).Inc()
				if result.Success {
					m.ZoneICMPPacketsReceived.WithLabelValues(zoneLabels...).Inc()
				}
			}
			m.ICMPResults.WithLabelValues(resultLabels...).Inc()
			m.ZoneICMPResults.WithLabelValues(zoneResultLabels...).Inc()

		case model.CheckPMTU:
			/* unreachable is no verdict: the small datagram did not come back either, the udp plane
			   owns that failure, and a fail here would page PathMTUBlackHole for a peer that is
			   simply down. A probe that never sent (no details) writes nothing for the same reason. */
			d, ok := result.Details.(*PMTUDetails)
			if !ok || d.Verdict == model.PMTUVerdictUnreachable {
				break
			}
			m.PMTUBytes.WithLabelValues(labels...).Set(float64(d.PathMTU))
			m.SetPMTUProbe(labels, float64(d.ProbeMTU))
			m.PMTUResults.WithLabelValues(resultLabels...).Inc()
			m.ZonePMTUResults.WithLabelValues(zoneResultLabels...).Inc()

		case model.CheckDNS:
			if details, ok := result.Details.([]DNSDetails); ok {
				for _, d := range details {
					dnsLabels := make([]string, 0, 5)
					dnsLabels = append(dnsLabels, d.Host, d.Resolver, source.NodeName, result.SourceZone)
					m.DNSDuration.WithLabelValues(dnsLabels...).Observe(d.Duration.Seconds())
					r := "success"
					if len(d.ResolvedIPs) == 0 && !result.Success {
						r = "fail"
					}
					m.DNSResults.WithLabelValues(append(dnsLabels, r)...).Inc()
				}
			}

		case model.CheckHTTP:
			if details, ok := result.Details.([]HTTPDetails); ok {
				for _, d := range details {
					urlLabels := []string{checker.RedactURL(d.URL), source.NodeName, result.SourceZone}
					/* A phase is observed only if it RAN. httptrace fires no TLS callback for a
					   plain-http target and no DNS callback for an IP literal, so those durations
					   stay zero — and one 0 s sample per check, forever, is a handshake that never
					   happened drawn as an instant one. */
					if d.DNSTimed {
						m.HTTPDNSDuration.WithLabelValues(urlLabels...).Observe(d.DNSTime.Seconds())
					}
					if d.ConnectTime > 0 {
						m.HTTPConnectDuration.WithLabelValues(urlLabels...).Observe(d.ConnectTime.Seconds())
					}
					if d.TLSTimed {
						m.HTTPTLSDuration.WithLabelValues(urlLabels...).Observe(d.TLSTime.Seconds())
					}
					if d.TTFBTimed {
						m.HTTPTTFBDuration.WithLabelValues(urlLabels...).Observe(d.TTFBTime.Seconds())
					}
					m.HTTPTotalDuration.WithLabelValues(urlLabels...).Observe(d.TotalTime.Seconds())
					r := "success"
					/* StatusMismatch is the CHECKER's verdict on expectStatus. Re-deriving the
					   outcome from the status code alone counted a target that expected 204 and got
					   200 as a success — in the very counter an expectStatus alert reads. */
					/* And the same applies the other way. `d.StatusCode >= 400` also overrode the
					   checker for a target that ASKED for a 4xx: expectStatus: 401 is a normal way
					   to check that an auth gate is up, and the checker returns Success there while
					   this counter recorded fail — the two halves of one probe disagreeing, with the
					   alert reading the half that is wrong. StatusMismatch is the checker's verdict
					   for both the expectStatus and the no-expectStatus case; a 0 means no response
					   at all. */
					if d.StatusCode == 0 || d.BodyMismatch || d.StatusMismatch {
						r = "fail"
					}
					m.HTTPResults.WithLabelValues(urlLabels[0], d.Method, fmt.Sprintf("%d", d.StatusCode), source.NodeName, result.SourceZone, r).Inc()
				}
			}

		case model.CheckExternal:
			if details, ok := result.Details.([]ExternalDetails); ok {
				for i := range details {
					recordExternalDetail(m, source.NodeName, result.SourceZone, &details[i])
				}
			}

		case model.CheckMTR:
			m.MTRTriggered.WithLabelValues(labels...).Inc()
			if details, ok := result.Details.(*MTRDetails); ok {
				/* The hop COUNT is a path length, and a trace that never reached its destination has
				   no path length to publish — it has maxHops silent entries. Publishing that as
				   kconmon_ng_mtr_hops said a two-hop pod-to-pod route was thirty hops long.

				   But SKIPPING the write was not the answer either: the hop-RTT series below are
				   deleted on every trace, so an unreached trace left a stale gauge from an older,
				   successful one — "3 hops" describing a path that no longer exists — standing next
				   to no hop RTTs at all. Absence is absence here too: the gauge goes when the trace
				   that would justify it did not arrive. */
				if details.Reached {
					m.MTRHops.WithLabelValues(labels...).Set(float64(len(details.Hops)))
				} else {
					m.MTRHops.DeleteLabelValues(labels...)
				}
				/* The PREVIOUS trace's hops go first. The gauge is keyed by hop_ip, so a route
				   change left both the old path and the new one live and current — see
				   ForgetPeerTrace. */
				m.ForgetPeerTrace(result.Source, result.Destination)
				for _, hop := range details.Hops {
					/* A hop that did not answer has no round trip to publish. It used to be exported
					   with the tracer's read deadline as its value, under hop_ip="*", so the series
					   said every silent router replies in exactly one second. Absence is absence. */
					if hop.IP == "" || hop.IP == "*" || hop.RTT <= 0 {
						continue
					}
					m.MTRHopRTT.WithLabelValues(
						result.Source, result.Destination,
						fmt.Sprintf("%d", hop.Number), hop.IP,
					).Set(hop.RTT.Seconds())
				}
			}
		}
	}
}

// externalTargetKind maps a per-target check type onto the closed label set host|url; it is
// DERIVED, never taken from ExternalTarget.kind on the wire.
func externalTargetKind(t model.CheckType) string {
	if t == model.CheckHTTP {
		return "url"
	}
	return "host"
}

// recordExternalDetail drives the kconmon_ng_external_* family from ONE probed target; a DENIED
// probe never reached the network: it is neither a success nor a failure.
func recordExternalDetail(m *metrics.PrometheusMetrics, node, zone string, d *ExternalDetails) {
	kind := externalTargetKind(d.CheckType)
	checkType := string(d.CheckType)

	/* A probe that was abandoned before it touched the network is not a result.
	   The shutdown path fills one of these for every target still waiting on the concurrency
	   semaphore when the sweep's context dies, and recording it observed a 0 s sample into the
	   duration histogram (dragging every p50/p95 panel down on each rolling update), incremented a
	   counter whose Help says "results that reached the network", and -- for icmp -- published 100%
	   packet loss for a probe that sent no packets. */
	if d.NotRun {
		return
	}

	if d.Denied {
		reason := d.DenyReason
		if reason == "" {
			// A denial with no typed reason still has to land inside the closed
			// set rather than mint an empty label value.
			reason = model.ExternalDenyCIDR
		}
		m.ExternalDenied.WithLabelValues(node, zone, d.Name, kind, checkType, string(reason)).Inc()
		/* And its GAUGES go, because they describe a probe that no longer happens. A target whose
		   address starts resolving into a denied range (a DNS change, a CIDR dropped from
		   allowedCidrs) is refused before it reaches the network from then on, and nothing wrote
		   these series again — so the last packet-loss ratio and the last HTTP status code before
		   the denial stayed on the dashboard as current readings, indefinitely. A denial is not a
		   measurement; the honest answer is no series, next to the denial counter that IS climbing. */
		m.ForgetExternalCheck(d.Name, kind, checkType)
		return
	}

	m.ExternalDuration.WithLabelValues(node, zone, d.Name, kind, checkType).Observe(d.Duration.Seconds())

	/* RTT and loss ratio exist only for icmp; observing a zero for a tcp probe would report a
	   measurement that was never taken.

	   The RTT is observed ONLY on success, exactly as the peer ICMP branch above does. A probe that
	   got no reply has no round trip, and the checker fills RTT with the elapsed read deadline in
	   that case — so a failing external target used to feed its own timeout into the latency
	   histogram once per probe, and every p95/p99 panel and latency SLO on the target jumped to the
	   timeout value during the outage instead of going blank. The outage is the thing the check
	   exists to show; a fabricated latency hides it behind a plausible number.

	   The loss gauge takes the opposite rule: a failed probe that produced no usable ratio is 100%
	   loss, not the zero value of a struct nobody filled in. */
	if d.CheckType == model.CheckICMP {
		if d.Success {
			m.ExternalRtt.WithLabelValues(node, zone, d.Name, kind, checkType).Observe(d.RTT.Seconds())
		}
		loss := d.LossRatio
		if !d.Success && loss == 0 {
			loss = 1
		}
		m.ExternalPacketLoss.WithLabelValues(node, zone, d.Name, kind, checkType).Set(loss)
	}
	if d.CheckType == model.CheckHTTP {
		m.ExternalHTTPStatusCode.WithLabelValues(node, zone, d.Name, kind, checkType).Set(float64(d.StatusCode))
	}

	r := "success"
	if !d.Success {
		r = "fail"
	}
	m.ExternalResults.WithLabelValues(node, zone, d.Name, kind, checkType, r).Inc()
}

type TCPDetails = model.TCPDetails
type UDPDetails = model.UDPDetails
type ICMPDetails = model.ICMPDetails
type PMTUDetails = model.PMTUDetails
type DNSDetails = model.DNSDetails
type HTTPDetails = model.HTTPDetails
type MTRDetails = model.MTRDetails
type ExternalDetails = model.ExternalDetails

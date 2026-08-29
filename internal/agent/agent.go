package agent

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"regexp"
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

// capabilityExternalChecks is the AgentMeta.capabilities flag this agent advertises when.
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

	// The external-destination gate is built here, not lazily at first probe.
	external := ExternalPolicy{}
	if cfg.Checkers.External.Enabled {
		allowlist, err := checker.NewAllowlist(cfg.Checkers.External.AllowedCIDRs, cfg.Checkers.External.DeniedCIDRs)
		if err != nil {
			return nil, fmt.Errorf("checkers.external.%w", err)
		}
		external = ExternalPolicy{
			Enabled:   true,
			Allowlist: allowlist,
			// The system resolver: external destinations are named in cluster
			// DNS terms like everything else the agent probes.
			Resolver:   net.DefaultResolver,
			Timeout:    cfg.Checkers.External.Timeout,
			MaxTargets: cfg.Checkers.External.MaxTargets,
		}
		slog.Info("external destination checks enabled",
			"allowedCidrs", len(cfg.Checkers.External.AllowedCIDRs),
			"deniedCidrs", len(cfg.Checkers.External.DeniedCIDRs),
			"maxTargets", cfg.Checkers.External.MaxTargets,
			"authTimeout", cfg.Checkers.External.Timeout,
		)
	}

	source := checker.Target{
		AgentID:  info.ID,
		NodeName: info.NodeName,
		PodIP:    info.PodIP,
		Zone:     info.Zone,
		Port:     cfg.HTTPPort,
	}

	resultHandler := NewResultHandler(m, source)

	sched := NewScheduler(source, resultHandler)

	// checkers is the shared registry of enabled checker instances, reused by
	// both the scheduler and the on-demand task executor.
	checkers := make(map[model.CheckType]checker.Checker)

	if cfg.Checkers.TCP.Enabled {
		c := checker.NewTCPChecker(cfg.Checkers.TCP.Timeout)
		sched.AddChecker(c, SchedulerConfig{Interval: cfg.Checkers.TCP.Interval})
		checkers[model.CheckTCP] = c
		slog.Info("checker enabled", "type", "tcp", "interval", cfg.Checkers.TCP.Interval)
	}
	if cfg.Checkers.UDP.Enabled {
		c := checker.NewUDPChecker(cfg.Checkers.UDP.Timeout, cfg.Checkers.UDP.Packets, cfg.GRPCPort)
		sched.AddChecker(c, SchedulerConfig{Interval: cfg.Checkers.UDP.Interval})
		checkers[model.CheckUDP] = c
		slog.Info("checker enabled", "type", "udp", "interval", cfg.Checkers.UDP.Interval)
	}
	if cfg.Checkers.ICMP.Enabled {
		c := checker.NewICMPChecker(cfg.Checkers.ICMP.Timeout)
		sched.AddChecker(c, SchedulerConfig{Interval: cfg.Checkers.ICMP.Interval})
		checkers[model.CheckICMP] = c
		slog.Info("checker enabled", "type", "icmp", "interval", cfg.Checkers.ICMP.Interval)
	}
	if cfg.Checkers.DNS.Enabled && len(cfg.Checkers.DNS.Hosts) > 0 {
		c := checker.NewDNSChecker(cfg.Checkers.DNS.Hosts, cfg.Checkers.DNS.Resolvers, cfg.Checkers.DNS.Timeout)
		sched.AddChecker(c, SchedulerConfig{Interval: cfg.Checkers.DNS.Interval, NodeLocal: true})
		checkers[model.CheckDNS] = c
		slog.Info("checker enabled", "type", "dns", "interval", cfg.Checkers.DNS.Interval)
	}
	if cfg.Checkers.HTTP.Enabled && len(cfg.Checkers.HTTP.Targets) > 0 {
		httpTargets := make([]checker.HTTPCheckTarget, 0, len(cfg.Checkers.HTTP.Targets))
		for _, t := range cfg.Checkers.HTTP.Targets {
			ht := checker.HTTPCheckTarget{
				URL:                t.URL,
				Method:             t.Method,
				ExpectStatus:       t.ExpectStatus,
				InsecureSkipVerify: t.InsecureSkipVerify,
			}
			if t.BodyPattern != "" {
				re, err := regexp.Compile(t.BodyPattern)
				if err != nil {
					return nil, fmt.Errorf("invalid bodyPattern %q for target %s: %w", t.BodyPattern, t.URL, err)
				}
				ht.BodyPattern = re
			}
			httpTargets = append(httpTargets, ht)
		}
		c := checker.NewHTTPChecker(cfg.Checkers.HTTP.Timeout, httpTargets)
		sched.AddChecker(c, SchedulerConfig{Interval: cfg.Checkers.HTTP.Interval, NodeLocal: true})
		checkers[model.CheckHTTP] = c
		slog.Info("checker enabled", "type", "http", "interval", cfg.Checkers.HTTP.Interval, "targets", len(httpTargets))
	}

	// The continuous external checker is registered like any other NodeLocal checker.
	var externalChecker *checker.ExternalChecker
	if external.Enabled {
		externalChecker = checker.NewExternalChecker(external.Allowlist, external.Resolver, external.Timeout)
		sched.AddChecker(externalChecker, SchedulerConfig{Interval: checker.ExternalTick, NodeLocal: true})
		slog.Info("checker enabled", "type", "external", "tick", checker.ExternalTick)
	}

	mtrChecker := checker.NewMTRChecker(cfg.Checkers.MTR.MaxHops, 1*time.Second, cfg.Checkers.MTR.Cooldown)
	sched.SetMTRChecker(mtrChecker)
	slog.Info("mtr checker enabled", "maxHops", cfg.Checkers.MTR.MaxHops, "cooldown", cfg.Checkers.MTR.Cooldown)

	a := &Agent{
		cfg:         cfg,
		scheduler:   sched,
		httpServer:  NewHTTPServer(promReg),
		probeServer: NewProbeServer(cfg.GRPCPort),
		metrics:     m,
		promReg:     promReg,
		info:        info,
		// The zone the operator CONFIGURED (agent.zone, env-overridden by the loader) as opposed to
		// the effective one in info.Zone; registrationInfo explains why only this one is asserted.
		configuredZone: cfg.Agent.Zone,
		checkers:       checkers,
		mtrChecker:     mtrChecker,
		external:       external,

		externalChecker: externalChecker,
	}

	return a, nil
}

// agentCapabilities returns the opt-in feature flags this agent advertises at registration; it
// mirrors the controller's capabilitiesFor: feature detection, never version sniffing.
func agentCapabilities(cfg *config.Config) []string {
	caps := []string{}
	if cfg.Checkers.External.Enabled {
		caps = append(caps, capabilityExternalChecks)
	}
	return caps
}

func (a *Agent) Run(ctx context.Context) error {
	slog.Info("starting agent",
		"node", a.info.NodeName,
		"pod", a.info.PodName,
		"ip", a.info.PodIP,
		"zone", a.info.Zone,
		"version", config.Version,
	)

	if err := a.probeServer.ListenUDP(ctx); err != nil {
		return fmt.Errorf("starting UDP probe server: %w", err)
	}
	defer func() { _ = a.probeServer.Close() }()

	grpcClient, err := NewGRPCClient(a.cfg.ControllerAddress, clientSecurityFromConfig(a.cfg))
	if err != nil {
		return fmt.Errorf("creating gRPC client: %w", err)
	}
	a.grpcClient = grpcClient
	defer func() { _ = grpcClient.Close() }()

	// The health/metrics plane comes up BEFORE the first registration: the chart's
	// startupProbe polls /healthz with a finite budget, and an agent that stays dark
	// through a controller outage takes the whole DaemonSet into CrashLoopBackOff.
	errCh := make(chan error, 2)
	httpSrv := &http.Server{
		Addr:         fmt.Sprintf(":%d", a.cfg.HTTPPort),
		Handler:      a.httpServer.Handler(),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		slog.Info("HTTP server listening", "port", a.cfg.HTTPPort)
		errCh <- httpSrv.ListenAndServe()
	}()

	// The metrics listener, on its own port: see internal/metrics/listener.go.
	metricsSrv := metrics.NewListener(
		fmt.Sprintf(":%d", a.cfg.MetricsPort),
		metrics.NewListenerHandler(a.promReg, a.httpServer.Ready),
	)

	go func() {
		slog.Info("metrics server listening", "port", a.cfg.MetricsPort)
		errCh <- metricsSrv.ListenAndServe()
	}()

	shutdownHTTP := func() error {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	}

	var peers []checker.Target
	var resolvedZone string
	backoff := 1 * time.Second
	maxBackoff := 15 * time.Second
	for {
		peers, resolvedZone, err = grpcClient.Register(ctx, a.registrationInfo(), a.cfg.HTTPPort)
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
		if shouldRedial(err) {
			if rerr := grpcClient.Reconnect(); rerr != nil {
				slog.Warn("redialling the controller failed", "error", rerr)
			}
		}
		backoff = min(backoff*2, maxBackoff)
	}

	// Adopt the controller-resolved zone when no explicit zone was configured.
	// This happens before the scheduler starts, so all emitted metrics carry
	// the correct source_zone from the first check.
	if z := resolveZone(a.configuredZone, resolvedZone); z != a.info.Zone {
		slog.Info("adopted zone from controller", "zone", z)
		a.info.Zone = z
		a.scheduler.SetSourceZone(z)
	}

	a.scheduler.UpdatePeers(peers)
	a.syncPeerMetrics()
	a.scheduler.Pause()

	peerWatchReady := make(chan struct{}, 1)
	reregisterCh := make(chan struct{}, 1)

	grpcClient.OnPeersUpdate(func(targets []checker.Target) {
		a.forgetDepartedPeers(targets)
		a.scheduler.UpdatePeers(targets)
		a.syncPeerMetrics()
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
	taskExecutor := NewTaskExecutor(
		a.checkers,
		a.mtrChecker,
		checker.Target{
			AgentID:  a.info.ID,
			NodeName: a.info.NodeName,
			PodIP:    a.info.PodIP,
			Zone:     a.info.Zone,
			Port:     a.cfg.HTTPPort,
		},
		a.cfg.HTTPPort,
		grpcClient,
		maxConcurrentTasks,
		a.external,
	)
	grpcClient.OnTask(func(taskCtx context.Context, task *pb.TaskRequest) {
		taskExecutor.Handle(taskCtx, task)
	})

	go grpcClient.StartHeartbeat(ctx, 5*time.Second)

	// Deliberately NO scheduler.Pause() here: probing keeps running on the last known
	// peer list, because pausing blinded the whole fleet for the duration of every
	// controller restart, upgrade, or failover — exactly when measurements matter most.
	reregister := func() {
		wait := 2 * time.Second
		maxWait := 30 * time.Second
		for {
			jitter := time.Duration(rand.Int63n(int64(wait / 4))) //nolint:gosec // G404: non-cryptographic randomness is intentional for backoff jitter
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait + jitter):
			}
			newPeers, newZone, regErr := grpcClient.Register(ctx, a.registrationInfo(), a.cfg.HTTPPort)
			if regErr == nil {
				if z := resolveZone(a.configuredZone, newZone); z != a.info.Zone {
					slog.Info("adopted zone from controller on re-registration", "zone", z)
					a.info.Zone = z
					a.scheduler.SetSourceZone(z)
				}
				a.forgetDepartedPeers(newPeers)
				a.scheduler.UpdatePeers(newPeers)
				a.syncPeerMetrics()
				slog.Info("re-registered with controller after reconnect")
				return
			}
			// A rejection here is a config error, but unlike the first registration we keep
			// retrying: probes continue on the last known peer list, and a mid-life
			// InvalidArgument may just be a controller upgrade tightening validation.
			if isConfigRejection(regErr) {
				slog.Error("controller rejected the re-registration payload, check agent configuration and controller/agent version skew", "error", regErr, "backoff", wait+jitter)
			} else {
				slog.Warn("re-registration failed, retrying", "error", regErr, "backoff", wait+jitter)
			}
			// Same reason as the first registration: only a fresh connection can reach the leader
			// after a failover moved it to another pod.
			if shouldRedial(regErr) {
				if rerr := grpcClient.Reconnect(); rerr != nil {
					slog.Warn("redialling the controller failed", "error", rerr)
				}
			}
			wait = min(wait*2, maxWait)
		}
	}

	go func() {
		for {
			err := grpcClient.WatchPeers(ctx, a.cfg.HTTPPort)
			if ctx.Err() != nil {
				return
			}
			slog.Warn("peer watch disconnected, re-registering", "error", err)
			reregister()
		}
	}()

	// WatchTasks runs its own reconnect loop mirroring WatchPeers; peer re-registration is owned by
	// the WatchPeers loop above.
	go func() {
		backoff := 1 * time.Second
		maxBackoff := 15 * time.Second
		for {
			err := grpcClient.WatchTasks(ctx)
			if ctx.Err() != nil {
				return
			}
			slog.Warn("task watch disconnected, re-subscribing", "error", err, "backoff", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, maxBackoff)
		}
	}()

	// WatchExternalChecks runs its own reconnect loop mirroring WatchTasks; it is started ONLY when
	// checkers.external.enabled.
	if a.externalChecker != nil {
		grpcClient.OnExternalAssignment(a.applyExternalAssignment)
		go func() {
			backoff := 1 * time.Second
			maxBackoff := 15 * time.Second
			for {
				err := grpcClient.WatchExternalChecks(ctx)
				if ctx.Err() != nil {
					return
				}
				slog.Warn("external check watch disconnected, re-subscribing", "error", err, "backoff", backoff)
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				backoff = min(backoff*2, maxBackoff)
			}
		}()
	}

	go func() {
		for {
			select {
			case <-reregisterCh:
				slog.Info("heartbeat triggered re-registration")
				reregister()
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
func (a *Agent) applyExternalAssignment(assignment *pb.ExternalCheckAssignment) {
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
	if limit := a.external.MaxTargets; limit > 0 && len(parsed) > limit {
		overflow = len(parsed) - limit
		parsed = parsed[:limit]
	}

	/* Targets that left the assignment lose their gauges HERE, which is the only event that knows
	   they left. Nothing used to do this: the external gauges were cleared only as collateral damage
	   from a peer update, so a target the controller had stopped assigning went on reporting its last
	   packet-loss reading for as long as the peer list held still — an alert firing on a check that
	   no longer runs. Truncated targets count as departed for the same reason: they are not probed. */
	a.retireDepartedExternalTargets(parsed)

	a.externalChecker.SetSpecs(parsed)
	if overflow > 0 {
		slog.Warn("external check assignment exceeds checkers.external.maxTargets; the tail is not probed",
			"applied", len(parsed), "dropped", overflow, "maxTargets", a.external.MaxTargets)
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
func (a *Agent) retireDepartedExternalTargets(next []checker.ExternalSpec) {
	if a.externalChecker == nil {
		return
	}
	applied := a.externalChecker.Counts()
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
				a.metrics.ForgetExternalTarget(name)
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
		a.metrics.ForgetExternalCheck(name, externalTargetKind(applied[i].Type), checkType)
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
	info := a.info
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

// syncPeerMetrics re-initializes the per-pair result series after a peer list change.
/*
forgetDepartedPeers retires the gauges of peers that are in the CURRENT list and not in the next
one, and touches nothing else.

The peer list is replaced wholesale on every update, so the departures are the difference between
the two. Anything still present keeps its readings: a gauge is only repopulated by the next probe of
that pair, and blanking a live peer's loss ratio for a check interval is a gap in the series alerts
evaluate, appearing once per pod event — a rolling DaemonSet restart is one per node.
*/
func (a *Agent) forgetDepartedPeers(next []checker.Target) {
	current := a.scheduler.Peers()
	if len(current) == 0 {
		return
	}
	keep := make(map[string]struct{}, len(next))
	for i := range next {
		keep[next[i].NodeName] = struct{}{}
	}
	for i := range current {
		if _, ok := keep[current[i].NodeName]; !ok {
			a.metrics.ForgetPeer(current[i].NodeName)
		}
	}
}

func (a *Agent) syncPeerMetrics() {
	source := checker.Target{NodeName: a.info.NodeName, Zone: a.info.Zone}
	preinitPeerResults(a.metrics, source, a.scheduler.Peers(), a.checkers)
	preinitZoneResults(a.metrics, source, a.scheduler.Peers(), a.checkers)
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
					urlLabels := []string{d.URL, source.NodeName, result.SourceZone}
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
					m.HTTPResults.WithLabelValues(d.URL, d.Method, fmt.Sprintf("%d", d.StatusCode), source.NodeName, result.SourceZone, r).Inc()
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
type DNSDetails = model.DNSDetails
type HTTPDetails = model.HTTPDetails
type MTRDetails = model.MTRDetails
type ExternalDetails = model.ExternalDetails

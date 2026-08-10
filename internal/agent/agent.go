package agent

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"os"
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
	cfg         *config.Config
	grpcClient  *GRPCClient
	scheduler   *Scheduler
	httpServer  *HTTPServer
	probeServer *ProbeServer
	metrics     *metrics.PrometheusMetrics
	promReg     *prometheus.Registry
	info        model.AgentInfo
	envZone     string
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

	hostname, _ := os.Hostname()
	nodeName := os.Getenv("KCONMON_NG_NODE_NAME")
	podName := os.Getenv("KCONMON_NG_POD_NAME")
	podIP := os.Getenv("KCONMON_NG_POD_IP")
	zone := os.Getenv("KCONMON_NG_ZONE")

	if podName == "" {
		podName = hostname
	}

	info := model.AgentInfo{
		ID:           fmt.Sprintf("%s-%s", nodeName, podName),
		NodeName:     nodeName,
		PodName:      podName,
		PodIP:        podIP,
		Zone:         zone,
		Capabilities: agentCapabilities(cfg),
	}

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
			Resolver: net.DefaultResolver,
			Timeout:  cfg.Checkers.External.Timeout,
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
				URL:          t.URL,
				Method:       t.Method,
				ExpectStatus: t.ExpectStatus,
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
		envZone:     zone,
		checkers:    checkers,
		mtrChecker:  mtrChecker,
		external:    external,

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

	grpcClient, err := NewGRPCClient(a.cfg.ControllerAddress)
	if err != nil {
		return fmt.Errorf("creating gRPC client: %w", err)
	}
	a.grpcClient = grpcClient
	defer func() { _ = grpcClient.Close() }()

	var peers []checker.Target
	var resolvedZone string
	backoff := 1 * time.Second
	maxBackoff := 15 * time.Second
	for {
		peers, resolvedZone, err = grpcClient.Register(ctx, a.info, a.cfg.HTTPPort)
		if err == nil {
			break
		}
		slog.Warn("controller not ready, retrying", "error", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}

	// Adopt the controller-resolved zone when no explicit zone was configured.
	// This happens before the scheduler starts, so all emitted metrics carry
	// the correct source_zone from the first check.
	if z := resolveZone(a.envZone, resolvedZone); z != a.info.Zone {
		slog.Info("adopted zone from controller", "zone", z)
		a.info.Zone = z
		a.scheduler.SetSourceZone(z)
	}

	a.scheduler.UpdatePeers(peers)
	a.scheduler.Pause()

	peerWatchReady := make(chan struct{}, 1)
	reregisterCh := make(chan struct{}, 1)

	grpcClient.OnPeersUpdate(func(targets []checker.Target) {
		a.metrics.ResetPeerGauges()
		a.scheduler.UpdatePeers(targets)
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

	reregister := func() {
		a.scheduler.Pause()
		wait := 2 * time.Second
		maxWait := 30 * time.Second
		for {
			jitter := time.Duration(rand.Int63n(int64(wait / 4))) //nolint:gosec // G404: non-cryptographic randomness is intentional for backoff jitter
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait + jitter):
			}
			newPeers, newZone, regErr := grpcClient.Register(ctx, a.info, a.cfg.HTTPPort)
			if regErr == nil {
				if z := resolveZone(a.envZone, newZone); z != a.info.Zone {
					slog.Info("adopted zone from controller on re-registration", "zone", z)
					a.info.Zone = z
					a.scheduler.SetSourceZone(z)
				}
				a.metrics.ResetPeerGauges()
				a.scheduler.UpdatePeers(newPeers)
				slog.Info("re-registered with controller after reconnect")
				return
			}
			slog.Warn("re-registration failed, retrying", "error", regErr, "backoff", wait+jitter)
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

	errCh := make(chan error, 1)
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

	go a.scheduler.Run(ctx)

	select {
	case <-peerWatchReady:
		slog.Info("peer watch confirmed, agent fully ready")
	case <-time.After(30 * time.Second):
		slog.Warn("peer watch not confirmed within 30s, marking ready anyway")
		a.scheduler.Resume()
	case <-ctx.Done():
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

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
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

	a.externalChecker.SetSpecs(parsed)
	slog.Info("external check assignment applied", "targets", len(parsed), "dropped", dropped)
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

// resolveZone decides the agent's effective zone: an explicit env-provided
// zone always wins; otherwise the controller-resolved zone is adopted.
func resolveZone(envZone, resolvedZone string) string {
	if envZone != "" {
		return envZone
	}
	return resolvedZone
}

func NewResultHandler(m *metrics.PrometheusMetrics, source checker.Target) ResultHandler {
	return func(result model.CheckResult) {
		labels := []string{result.Source, result.Destination, result.SourceZone, result.DestZone}
		resultStr := "success"
		if !result.Success {
			resultStr = "fail"
		}
		resultLabels := []string{result.Source, result.Destination, result.SourceZone, result.DestZone, resultStr}

		switch result.Type {
		case model.CheckTCP:
			if d, ok := result.Details.(*TCPDetails); ok {
				m.TCPConnectDuration.WithLabelValues(labels...).Observe(d.ConnectTime.Seconds())
				m.TCPTotalDuration.WithLabelValues(labels...).Observe(d.TotalTime.Seconds())
			}
			m.TCPResults.WithLabelValues(resultLabels...).Inc()

		case model.CheckUDP:
			if d, ok := result.Details.(*UDPDetails); ok {
				m.UDPRtt.WithLabelValues(labels...).Observe(d.MeanRTT.Seconds())
				m.UDPJitter.WithLabelValues(labels...).Set(d.Jitter.Seconds())
				m.UDPLossRatio.WithLabelValues(labels...).Set(d.LossRatio)
			}
			m.UDPResults.WithLabelValues(resultLabels...).Inc()

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
			}
			m.ICMPResults.WithLabelValues(resultLabels...).Inc()

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
					m.HTTPDNSDuration.WithLabelValues(urlLabels...).Observe(d.DNSTime.Seconds())
					m.HTTPConnectDuration.WithLabelValues(urlLabels...).Observe(d.ConnectTime.Seconds())
					m.HTTPTLSDuration.WithLabelValues(urlLabels...).Observe(d.TLSTime.Seconds())
					m.HTTPTTFBDuration.WithLabelValues(urlLabels...).Observe(d.TTFBTime.Seconds())
					m.HTTPTotalDuration.WithLabelValues(urlLabels...).Observe(d.TotalTime.Seconds())
					r := "success"
					if d.StatusCode == 0 || d.StatusCode >= 400 || d.BodyMismatch {
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
				m.MTRHops.WithLabelValues(labels...).Set(float64(len(details.Hops)))
				for _, hop := range details.Hops {
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

	if d.Denied {
		reason := d.DenyReason
		if reason == "" {
			// A denial with no typed reason still has to land inside the closed
			// set rather than mint an empty label value.
			reason = model.ExternalDenyCIDR
		}
		m.ExternalDenied.WithLabelValues(node, zone, d.Name, kind, string(reason)).Inc()
		return
	}

	m.ExternalDuration.WithLabelValues(node, zone, d.Name, kind).Observe(d.Duration.Seconds())

	// RTT and loss ratio exist only for icmp; observing a zero for a tcp probe
	// would report a measurement that was never taken.
	if d.CheckType == model.CheckICMP {
		m.ExternalRtt.WithLabelValues(node, zone, d.Name, kind).Observe(d.RTT.Seconds())
		m.ExternalPacketLoss.WithLabelValues(node, zone, d.Name, kind).Set(d.LossRatio)
	}
	if d.CheckType == model.CheckHTTP {
		m.ExternalHTTPStatusCode.WithLabelValues(node, zone, d.Name, kind).Set(float64(d.StatusCode))
	}

	r := "success"
	if !d.Success {
		r = "fail"
	}
	m.ExternalResults.WithLabelValues(node, zone, d.Name, kind, r).Inc()
}

type TCPDetails = model.TCPDetails
type UDPDetails = model.UDPDetails
type ICMPDetails = model.ICMPDetails
type DNSDetails = model.DNSDetails
type HTTPDetails = model.HTTPDetails
type MTRDetails = model.MTRDetails
type ExternalDetails = model.ExternalDetails

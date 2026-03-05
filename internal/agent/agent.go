package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/checker"
	"github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/EsDmitrii/kconmon-ng/internal/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

type Agent struct {
	cfg         *config.Config
	grpcClient  *GRPCClient
	scheduler   *Scheduler
	httpServer  *HTTPServer
	probeServer *ProbeServer
	metrics     *metrics.PrometheusMetrics
	promReg     *prometheus.Registry
	info        model.AgentInfo
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
		ID:       fmt.Sprintf("%s-%s", nodeName, podName),
		NodeName: nodeName,
		PodName:  podName,
		PodIP:    podIP,
		Zone:     zone,
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

	if cfg.Checkers.TCP.Enabled {
		sched.AddChecker(
			checker.NewTCPChecker(cfg.Checkers.TCP.Timeout),
			SchedulerConfig{Interval: cfg.Checkers.TCP.Interval},
		)
		slog.Info("checker enabled", "type", "tcp", "interval", cfg.Checkers.TCP.Interval)
	}
	if cfg.Checkers.UDP.Enabled {
		sched.AddChecker(
			checker.NewUDPChecker(cfg.Checkers.UDP.Timeout, cfg.Checkers.UDP.Packets, cfg.GRPCPort),
			SchedulerConfig{Interval: cfg.Checkers.UDP.Interval},
		)
		slog.Info("checker enabled", "type", "udp", "interval", cfg.Checkers.UDP.Interval)
	}
	if cfg.Checkers.ICMP.Enabled {
		sched.AddChecker(
			checker.NewICMPChecker(cfg.Checkers.ICMP.Timeout),
			SchedulerConfig{Interval: cfg.Checkers.ICMP.Interval},
		)
		slog.Info("checker enabled", "type", "icmp", "interval", cfg.Checkers.ICMP.Interval)
	}
	if cfg.Checkers.DNS.Enabled && len(cfg.Checkers.DNS.Hosts) > 0 {
		sched.AddChecker(
			checker.NewDNSChecker(cfg.Checkers.DNS.Hosts, cfg.Checkers.DNS.Resolvers),
			SchedulerConfig{Interval: cfg.Checkers.DNS.Interval, NodeLocal: true},
		)
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
		sched.AddChecker(
			checker.NewHTTPChecker(cfg.Checkers.HTTP.Timeout, httpTargets),
			SchedulerConfig{Interval: cfg.Checkers.HTTP.Interval, NodeLocal: true},
		)
		slog.Info("checker enabled", "type", "http", "interval", cfg.Checkers.HTTP.Interval, "targets", len(httpTargets))
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
	}

	return a, nil
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
	backoff := 1 * time.Second
	maxBackoff := 15 * time.Second
	for {
		peers, err = grpcClient.Register(ctx, a.info, a.cfg.HTTPPort)
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

	go grpcClient.StartHeartbeat(ctx, 5*time.Second)

	reregister := func() {
		a.scheduler.Pause()
		wait := 2 * time.Second
		maxWait := 30 * time.Second
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			newPeers, regErr := grpcClient.Register(ctx, a.info, a.cfg.HTTPPort)
			if regErr == nil {
				a.metrics.ResetPeerGauges()
				a.scheduler.UpdatePeers(newPeers)
				slog.Info("re-registered with controller after reconnect")
				return
			}
			slog.Warn("re-registration failed, retrying", "error", regErr, "backoff", wait)
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

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
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
			if d, ok := result.Details.(*ICMPDetails); ok {
				m.ICMPRtt.WithLabelValues(labels...).Observe(d.RTT.Seconds())
				m.ICMPLossRatio.WithLabelValues(labels...).Set(d.LossRatio)
			}
			m.ICMPResults.WithLabelValues(resultLabels...).Inc()

		case model.CheckDNS:
			if details, ok := result.Details.([]DNSDetails); ok {
				for _, d := range details {
					dnsLabels := []string{d.Host, d.Resolver, source.NodeName, source.Zone}
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
					urlLabels := []string{d.URL, source.NodeName, source.Zone}
					m.HTTPDNSDuration.WithLabelValues(urlLabels...).Observe(d.DNSTime.Seconds())
					m.HTTPConnectDuration.WithLabelValues(urlLabels...).Observe(d.ConnectTime.Seconds())
					m.HTTPTLSDuration.WithLabelValues(urlLabels...).Observe(d.TLSTime.Seconds())
					m.HTTPTTFBDuration.WithLabelValues(urlLabels...).Observe(d.TTFBTime.Seconds())
					m.HTTPTotalDuration.WithLabelValues(urlLabels...).Observe(d.TotalTime.Seconds())
					r := "success"
					if d.StatusCode == 0 || d.StatusCode >= 400 {
						r = "fail"
					}
					m.HTTPResults.WithLabelValues(d.URL, d.Method, fmt.Sprintf("%d", d.StatusCode), source.NodeName, source.Zone, r).Inc()
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

type TCPDetails = model.TCPDetails
type UDPDetails = model.UDPDetails
type ICMPDetails = model.ICMPDetails
type DNSDetails = model.DNSDetails
type HTTPDetails = model.HTTPDetails
type MTRDetails = model.MTRDetails

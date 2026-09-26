package controller

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/EsDmitrii/kconmon-ng/internal/controller/meshplan"
	"github.com/EsDmitrii/kconmon-ng/internal/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type Controller struct {
	// cfgMu guards cfg, which ApplyConfig replaces; Run reads the restart-bound keys once at start.
	cfgMu      sync.RWMutex
	cfg        *config.Config
	registry   *Registry
	grpcServer *GRPCServer
	httpServer *HTTPServer
	metrics    *metrics.PrometheusMetrics
	promReg    *prometheus.Registry
	leader     atomic.Bool
	// newClientset builds the in-cluster client for the NodeWatcher and the lease; replaced by tests.
	newClientset func() (kubernetes.Interface, error)
	// topology is the mesh shape every plan is built with; a reload replaces it.
	topology atomic.Pointer[config.TopologyConfig]
	// evictEvery carries a reloaded sweep period (half the agent TTL) to Run's eviction loop.
	evictEvery chan time.Duration
}

// IsLeader reports whether this replica currently serves as the leader.
func (c *Controller) IsLeader() bool {
	return c.leader.Load()
}

// ProbePlan returns the CURRENT sparse probe plan — agent ID to the sorted peer IDs it probes —
// or nil when the fleet runs full mesh (topology.mode=full, or below the autoThreshold floor).
// The map is shared and read-only by contract; the topology snapshot and the probe_intended
// metric read it to say which pairs are MEANT to probe, so sparse gaps do not read as outages.
func (c *Controller) ProbePlan() meshplan.Plan {
	return c.grpcServer.CurrentPlan()
}

// SetLeader updates the leadership state and the controller_leader gauge. Demotion also drops the
// registry: agents accepted while leading belong to the new leader now, and a stale copy would keep
// this replica planning a second probe mesh over them.
func (c *Controller) SetLeader(leader bool) {
	was := c.leader.Swap(leader)
	if leader {
		c.metrics.ControllerLeader.WithLabelValues().Set(1)
	} else {
		c.metrics.ControllerLeader.WithLabelValues().Set(0)
	}
	if was && !leader {
		// The registry, its gauges, the in-flight tasks, the external assignment and the probe plan
		// all belong to the leader's view, which this replica no longer holds. Quietly: the streams
		// still attached here end on their own leader checks, and the new leader's FULL_SYNC
		// replaces their plan.
		c.registry.ResetQuiet()
		c.metrics.ControllerRegisteredAgents.WithLabelValues().Set(0)
		c.metrics.ControllerExternalAgents.WithLabelValues().Set(0)
		if c.grpcServer != nil {
			c.grpcServer.TaskManager().FailAll(ErrLeadershipLost)
			c.grpcServer.ExternalCheckManager().Reset()
			c.grpcServer.SetPeerPlan(nil)
		}
		c.metrics.ControllerExternalAssignments.WithLabelValues().Set(0)
	}
}

// leaseReleaseWait bounds how long shutdown waits for the lease hand-back, an apiserver round trip.
const leaseReleaseWait = 3 * time.Second

// The keepalive of both agent listeners, the in-cluster one and the external gateway: they serve the
// same agent client.
var (
	agentKeepalive       = keepalive.ServerParameters{Time: 10 * time.Second, Timeout: 5 * time.Second}
	agentKeepalivePolicy = keepalive.EnforcementPolicy{MinTime: 5 * time.Second, PermitWithoutStream: true}
)

func New(cfg *config.Config) *Controller {
	promReg := prometheus.NewRegistry()
	promReg.MustRegister(collectors.NewGoCollector())
	promReg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	m := metrics.NewPrometheusMetrics(cfg.MetricsPrefix, promReg)
	registry := NewRegistry(cfg.Controller.AgentTTL)

	c := &Controller{
		cfg:      cfg,
		registry: registry,
		metrics:  m,
		promReg:  promReg,
		newClientset: func() (kubernetes.Interface, error) {
			return buildInClusterClientset()
		},
		evictEvery: make(chan time.Duration, 1),
	}
	topology := cfg.Topology
	c.topology.Store(&topology)

	c.grpcServer = NewGRPCServer(registry, m, cfg.Controller.LeaderElection, c.IsLeader, cfg.Controller.Events.Enabled)
	c.httpServer = NewHTTPServer(registry, nil, promReg, capabilitiesFor(cfg))
	// The SD body carries the controller's metricsPort as the fallback target port: ports are
	// startup-only configuration, so the value captured here is the one every listener bound to.
	if cfg.Controller.PrometheusSD.Enabled {
		c.httpServer.EnablePrometheusSD(cfg.MetricsPort)
	}
	// Only when the checker is on: an allowlist nobody probes by is not a promise.
	if cfg.Checkers.External.Enabled {
		c.httpServer.SetExternalAllowedCIDRs(cfg.Checkers.External.AllowedCIDRs)
	}

	// The events are about the change itself, and a single event cannot name several agents.
	registry.OnChange(func(agents []model.AgentInfo, change TopologyChange) {
		/* The plan is rebuilt synchronously, BEFORE the broadcast is scheduled: Register answers
		   GetPeers right after this callback returns, and a plan lagging the registry would hand the
		   new agent an empty peer list. Synchronous is affordable — meshplan.Build is ~6ms at
		   N=1000 — and the fan-out itself stays coalesced. */
		c.grpcServer.SetPeerPlan(meshplan.Build(agents, *c.topology.Load()))
		// Coalesced, not immediate: a rollout's burst of changes must not fan out O(N²) FULL_SYNCs
		// (see SchedulePeerBroadcast); the events below stay per-change.
		c.grpcServer.SchedulePeerBroadcast(agents)
		m.ControllerRegisteredAgents.WithLabelValues().Set(float64(len(agents)))
		m.ControllerExternalAgents.WithLabelValues().Set(float64(countExternal(agents)))
		for _, tc := range change.Events() {
			c.grpcServer.PublishEvent(&pb.Event{Payload: &pb.Event_TopologyChanged{TopologyChanged: tc}})
		}
	})

	// With election off this replica is the only brain; with it on, leadership arrives from the
	// Lease loop started in Run. Either way the controller_leader gauge is published from the start.
	c.SetLeader(!cfg.Controller.LeaderElection)

	c.httpServer.SetLeaderGate(cfg.Controller.LeaderElection, c.IsLeader)

	c.httpServer.SetDiagnosticsHandler(NewDiagnosticsHandler(
		registry,
		c.grpcServer.TaskManager(),
		m,
		cfg.Controller.LeaderElection,
		c.IsLeader,
		c.grpcServer,
	))

	c.httpServer.SetExternalChecksHandler(NewExternalChecksHandler(
		registry,
		c.grpcServer.ExternalCheckManager(),
		m,
		cfg.Controller.LeaderElection,
		c.IsLeader,
	))

	return c
}

func (c *Controller) Run(ctx context.Context) error {
	// Listeners, the gateway, election and the NodeWatcher are restart-bound: read once, here.
	cfg := c.appliedConfig()
	slog.Info("starting controller",
		"httpPort", cfg.HTTPPort,
		"grpcPort", cfg.GRPCPort,
		"version", config.Version,
	)

	// One slot per listener goroutine, so none of them blocks forever on exit.
	errCh := make(chan error, 4)

	grpcSrv := grpc.NewServer(
		grpc.KeepaliveParams(agentKeepalive),
		grpc.KeepaliveEnforcementPolicy(agentKeepalivePolicy),
	)
	c.grpcServer.RegisterService(grpcSrv)

	lc := net.ListenConfig{}

	/* The external gateway is a SECOND listener over the SAME service instance: registering
	   c.grpcServer on both servers is the whole sharing story — registry, watchers and managers are
	   that struct's fields, so an external agent and an in-cluster one are indistinguishable past
	   the door. The gateway serves the agent registry only, never the Console's EventStream. Built
	   before anything starts serving, so a broken certificate or token file fails startup cleanly
	   instead of after the fleet has already connected. */
	var gatewaySrv *grpc.Server
	if gw := cfg.Controller.ExternalGateway; gw.Enabled {
		srv, gwErr := NewExternalGatewayServer(gw)
		if gwErr != nil {
			return fmt.Errorf("external gateway: %w", gwErr)
		}
		gatewaySrv = srv
		c.grpcServer.RegisterGatewayService(gatewaySrv)
	}

	grpcLis, err := lc.Listen(ctx, "tcp", fmt.Sprintf(":%d", cfg.GRPCPort))
	if err != nil {
		return fmt.Errorf("gRPC listen: %w", err)
	}

	go func() {
		slog.Info("gRPC server listening", "port", cfg.GRPCPort)
		errCh <- grpcSrv.Serve(grpcLis)
	}()

	if gatewaySrv != nil {
		gwLis, gwErr := lc.Listen(ctx, "tcp", fmt.Sprintf(":%d", cfg.Controller.ExternalGateway.Port))
		if gwErr != nil {
			return fmt.Errorf("external gateway listen: %w", gwErr)
		}
		go func() {
			slog.Info("external gateway listening",
				"port", cfg.Controller.ExternalGateway.Port,
				"mTLS", cfg.Controller.ExternalGateway.TLS.ClientCAFile != "",
			)
			errCh <- gatewaySrv.Serve(gwLis)
		}()
	}

	httpSrv := newControllerHTTPServer(fmt.Sprintf(":%d", cfg.HTTPPort), c.httpServer.Handler())

	go func() {
		slog.Info("HTTP server listening", "port", cfg.HTTPPort)
		errCh <- httpSrv.ListenAndServe()
	}()

	/* The METRICS listener, on a port of its own. The chart's scrape rule opens THIS one, so letting
	   a scraper in no longer lets its whole namespace reach the unauthenticated API above. */
	metricsSrv := metrics.NewListener(fmt.Sprintf(":%d", cfg.MetricsPort), c.metricsListenerHandler())

	go func() {
		slog.Info("metrics server listening", "port", cfg.MetricsPort)
		errCh <- metricsSrv.ListenAndServe()
	}()

	// Closed when the election goroutine returns, which is after ReleaseOnCancel handed the lease back.
	var electionDone chan struct{}
	if cfg.Controller.LeaderElection {
		clientset, err := c.newClientset()
		if err != nil {
			// No apiserver, so no lease to contend for: this process is the only brain it can see.
			slog.Warn("in-cluster k8s client unavailable, NodeWatcher and leader election disabled",
				"error", err)
			c.SetLeader(true)
		} else {
			nw := NewNodeWatcherWithContext(ctx, clientset, cfg.FailureDomainLabel)
			c.httpServer.SetNodeWatcher(nw)
			c.registry.SetZoneResolver(nw)
			nw.OnCountChange(func(n int) {
				c.metrics.ControllerExpectedAgents.WithLabelValues().Set(float64(n))
			})
			nw.OnZoneChange(c.registry.UpdateZone)
			c.metrics.ControllerExpectedAgents.WithLabelValues().Set(float64(nw.SchedulableNodeCount()))
			slog.Info("NodeWatcher started", "failureDomainLabel", cfg.FailureDomainLabel)

			opts := electionOptionsFor(clientset)
			if opts.namespace == "" {
				// Without a namespace there is no lease to contend for; lead rather than stall.
				slog.Error("cannot determine the lease namespace, assuming leadership")
				c.SetLeader(true)
			} else {
				electionDone = make(chan struct{})
				go func() {
					defer close(electionDone)
					c.runLeaderElection(ctx, opts)
				}()
			}
		}
	}

	c.httpServer.SetReady(true)

	evictTicker := time.NewTicker(cfg.Controller.AgentTTL / 2)
	defer evictTicker.Stop()

	go func() {
		for {
			select {
			case every := <-c.evictEvery:
				evictTicker.Reset(every)
			case <-evictTicker.C:
				if n := c.registry.EvictStale(); n > 0 {
					slog.Info("evicted stale agents", "count", n)
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	select {
	case <-ctx.Done():
		slog.Info("shutting down controller")
		// Flip readiness first: during a rolling restart the endpoint removal
		// races the gRPC stop, and now that graceful shutdown actually
		// completes, a ready-but-tearing-down replica is observable.
		c.httpServer.SetReady(false)
		stopGRPC(grpcSrv, c.grpcServer)
		if gatewaySrv != nil {
			// GRPCServer.Shutdown already ran above (idempotent), so gateway streams are ending;
			// this drains the gateway's own transport within the same bounded budget.
			stopGRPC(gatewaySrv, c.grpcServer)
		}

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = metricsSrv.Shutdown(shutdownCtx)
		err := httpSrv.Shutdown(shutdownCtx)
		if electionDone != nil {
			select {
			case <-electionDone:
			case <-time.After(leaseReleaseWait):
				slog.Warn("lease release did not finish before shutdown; the standby takes over when it expires")
			}
		}
		return err
	case err := <-errCh:
		return err
	}
}

// metricsListenerHandler is what the metrics listener serves: /metrics, the health endpoints and,
// when enabled, the Prometheus SD body, which has to answer on the one port the chart opens to the
// scraper. Same handler instance as the API mux, so the leader gate is shared.
func (c *Controller) metricsListenerHandler() http.Handler {
	var extra []metrics.Route
	if sd := c.httpServer.SDHandler(); sd != nil {
		extra = append(extra, metrics.Route{Pattern: "GET " + prometheusSDPath, Handler: sd})
	}
	return metrics.NewListenerHandler(c.promReg, c.httpServer.Ready, extra...)
}

// countExternal is the bare-host subset of a registry snapshot, for controller_external_agents.
func countExternal(agents []model.AgentInfo) int {
	n := 0
	for i := range agents {
		if agents[i].IsExternal() {
			n++
		}
	}
	return n
}

// The controller's HTTP budget. Every endpoint on this server answers in milliseconds -- /metrics,
// /healthz, /readyz, topology, version, external-checks -- so the write budget stays short.
//
// POST /api/v1/diagnostics is the one exception: it waits for an agent to finish a real probe and
// negotiates its own deadline per request (?timeout=, up to maxDiagnosticsTimeout). Go arms the write
// deadline when it reads the request, so that endpoint extends its OWN deadline
// (DiagnosticsHandler.ServeHTTP) rather than this constant being raised for everything.
const (
	controllerHTTPReadTimeout  = 10 * time.Second
	controllerHTTPWriteTimeout = 10 * time.Second
)

// newControllerHTTPServer builds the controller's HTTP server. Named so tests can pin the budget
// above against the diagnostics endpoint's own, much longer, negotiated deadline.
func newControllerHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:         addr,
		Handler:      h,
		ReadTimeout:  controllerHTTPReadTimeout,
		WriteTimeout: controllerHTTPWriteTimeout,
	}
}

// grpcGracefulStopTimeout bounds the wait for in-flight RPCs to drain before
// the server is stopped forcefully.
const grpcGracefulStopTimeout = 5 * time.Second

// stopGRPC shuts the gRPC server down without the GracefulStop trap; GracefulStop waits for every
// active handler to return but does not cancel their stream contexts.
func stopGRPC(grpcSrv *grpc.Server, streams *GRPCServer) {
	streams.Shutdown()

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		grpcSrv.GracefulStop()
	}()

	timer := time.NewTimer(grpcGracefulStopTimeout)
	defer timer.Stop()

	select {
	case <-stopped:
	case <-timer.C:
		slog.Warn("gRPC graceful stop timed out, forcing shutdown",
			"timeout", grpcGracefulStopTimeout)
		grpcSrv.Stop()
		<-stopped
	}
}

// capabilitiesFor returns the controller's advertised capability flags for
// GET /api/v1/version. "events" is only advertised when the operator has
// turned on controller.events.enabled — the Console never version-sniffs.
func capabilitiesFor(cfg *config.Config) []string {
	caps := []string{}
	if cfg.Controller.Events.Enabled {
		caps = append(caps, "events")
	}
	return caps
}

// buildInClusterClientset builds a Kubernetes clientset from the in-cluster service account.
// Returns an error when running outside a cluster (e.g. local development).
func buildInClusterClientset() (*kubernetes.Clientset, error) {
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	return clientset, nil
}

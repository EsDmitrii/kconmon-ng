package controller

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
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
	cfg        *config.Config
	registry   *Registry
	grpcServer *GRPCServer
	httpServer *HTTPServer
	metrics    *metrics.PrometheusMetrics
	promReg    *prometheus.Registry
	leader     atomic.Bool
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
		/* QUIETLY: the subscribers of this registry are the streams still attached to a replica that
		   has just stopped being the leader, and telling them "every agent deregistered" made every
		   one of them wipe its peer gauges and resume probing an empty mesh. They are ended by their
		   own leader checks; the new leader's FULL_SYNC is what replaces their plan. */
		c.registry.ResetQuiet()
		// The gauge belongs to this replica's own view, and it now holds nothing. It used to be
		// zeroed as a side effect of the notification ResetQuiet deliberately does not send.
		c.metrics.ControllerRegisteredAgents.WithLabelValues().Set(0)
		/* And the EXTERNAL assignment, which is the same kind of state and was left behind.

		   A demoted replica kept the assignment map it held as leader, so
		   controller_external_assignments went on reporting agents it no longer assigns anything to,
		   and Assignment() answered a re-subscribing agent with a plan only the real leader may
		   decide. Losing the lease without a restart is ordinary — a renewal glitch, a takeover, a
		   scale 2→1→2 that lands the sole replica back here — and the registry was already reset for
		   exactly this reason; the external half was simply missed. */
		if c.grpcServer != nil {
			c.grpcServer.ExternalCheckManager().Reset()
			// The probe plan is derived from the registry dropped above; kept, it would leak the
			// old leader's mesh through ProbePlan on a replica that owns no agents.
			c.grpcServer.SetPeerPlan(nil)
		}
		c.metrics.ControllerExternalAssignments.WithLabelValues().Set(0)
	}
}

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
	}

	c.grpcServer = NewGRPCServer(registry, m, cfg.Controller.LeaderElection, c.IsLeader, cfg.Controller.Events.Enabled)
	c.httpServer = NewHTTPServer(registry, nil, promReg, capabilitiesFor(cfg))
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
		c.grpcServer.SetPeerPlan(meshplan.Build(agents, cfg.Topology))
		// Coalesced, not immediate: a rollout's burst of changes must not fan out O(N²) FULL_SYNCs
		// (see SchedulePeerBroadcast); the events below stay per-change.
		c.grpcServer.SchedulePeerBroadcast(agents)
		m.ControllerRegisteredAgents.WithLabelValues().Set(float64(len(agents)))
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
	slog.Info("starting controller",
		"httpPort", c.cfg.HTTPPort,
		"grpcPort", c.cfg.GRPCPort,
		"version", config.Version,
	)

	// One slot per listener goroutine, so none of them blocks forever on exit.
	errCh := make(chan error, 4)

	grpcSrv := grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    10 * time.Second,
			Timeout: 5 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             5 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	c.grpcServer.RegisterService(grpcSrv)

	lc := net.ListenConfig{}

	/* The external gateway is a SECOND listener over the SAME service instance: registering
	   c.grpcServer on both servers is the whole sharing story — registry, watchers and managers are
	   that struct's fields, so an external agent and an in-cluster one are indistinguishable past
	   the door. Built before anything starts serving, so a broken certificate or token file fails
	   startup cleanly instead of after the fleet has already connected. */
	var gatewaySrv *grpc.Server
	if gw := c.cfg.Controller.ExternalGateway; gw.Enabled {
		srv, gwErr := NewExternalGatewayServer(gw)
		if gwErr != nil {
			return fmt.Errorf("external gateway: %w", gwErr)
		}
		gatewaySrv = srv
		c.grpcServer.RegisterService(gatewaySrv)
	}

	grpcLis, err := lc.Listen(ctx, "tcp", fmt.Sprintf(":%d", c.cfg.GRPCPort))
	if err != nil {
		return fmt.Errorf("gRPC listen: %w", err)
	}

	go func() {
		slog.Info("gRPC server listening", "port", c.cfg.GRPCPort)
		errCh <- grpcSrv.Serve(grpcLis)
	}()

	if gatewaySrv != nil {
		gwLis, gwErr := lc.Listen(ctx, "tcp", fmt.Sprintf(":%d", c.cfg.Controller.ExternalGateway.Port))
		if gwErr != nil {
			return fmt.Errorf("external gateway listen: %w", gwErr)
		}
		go func() {
			slog.Info("external gateway listening",
				"port", c.cfg.Controller.ExternalGateway.Port,
				"mTLS", c.cfg.Controller.ExternalGateway.TLS.ClientCAFile != "",
			)
			errCh <- gatewaySrv.Serve(gwLis)
		}()
	}

	httpSrv := newControllerHTTPServer(fmt.Sprintf(":%d", c.cfg.HTTPPort), c.httpServer.Handler())

	go func() {
		slog.Info("HTTP server listening", "port", c.cfg.HTTPPort)
		errCh <- httpSrv.ListenAndServe()
	}()

	/* The METRICS listener, on a port of its own. The chart's scrape rule opens THIS one, so letting
	   a scraper in no longer lets its whole namespace reach the unauthenticated API above. */
	metricsSrv := metrics.NewListener(
		fmt.Sprintf(":%d", c.cfg.MetricsPort),
		metrics.NewListenerHandler(c.promReg, c.httpServer.Ready),
	)

	go func() {
		slog.Info("metrics server listening", "port", c.cfg.MetricsPort)
		errCh <- metricsSrv.ListenAndServe()
	}()

	if c.cfg.Controller.LeaderElection {
		clientset, err := buildInClusterClientset()
		if err != nil {
			// No apiserver, so no lease to contend for: this process is the only brain it can see.
			slog.Warn("in-cluster k8s client unavailable, NodeWatcher and leader election disabled",
				"error", err)
			c.SetLeader(true)
		} else {
			nw := NewNodeWatcherWithContext(ctx, clientset, c.cfg.FailureDomainLabel)
			c.httpServer.SetNodeWatcher(nw)
			c.registry.SetZoneResolver(nw)
			nw.OnCountChange(func(n int) {
				c.metrics.ControllerExpectedAgents.WithLabelValues().Set(float64(n))
			})
			nw.OnZoneChange(c.registry.UpdateZone)
			c.metrics.ControllerExpectedAgents.WithLabelValues().Set(float64(nw.SchedulableNodeCount()))
			slog.Info("NodeWatcher started", "failureDomainLabel", c.cfg.FailureDomainLabel)

			opts := electionOptionsFor(clientset)
			if opts.namespace == "" {
				// Without a namespace there is no lease to contend for; lead rather than stall.
				slog.Error("cannot determine the lease namespace, assuming leadership")
				c.SetLeader(true)
			} else {
				go c.runLeaderElection(ctx, opts)
			}
		}
	}

	c.httpServer.SetReady(true)

	evictTicker := time.NewTicker(c.cfg.Controller.AgentTTL / 2)
	defer evictTicker.Stop()

	go func() {
		for {
			select {
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
		return httpSrv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// The controller's HTTP budget. Every endpoint on this server answers in milliseconds -- /metrics,
// /healthz, /readyz, topology, version, external-checks -- so the write budget stays short.
//
// POST /api/v1/diagnostics is the one exception: it waits for an agent to finish a real probe and
// negotiates its own deadline per request (?timeout=, up to maxDiagnosticsTimeout). Go arms the
// connection's write deadline when it READS the request, so a 30s MTR trace used to write into a
// connection whose deadline had expired 20s earlier: the response was dropped, the connection torn
// down, and the caller got a bare EOF with nothing logged here. That endpoint therefore extends its
// OWN write deadline (DiagnosticsHandler.ServeHTTP) instead of this constant being raised for
// everything.
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

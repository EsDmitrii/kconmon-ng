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

// SetLeader updates the leadership state and the controller_leader gauge.
func (c *Controller) SetLeader(leader bool) {
	c.leader.Store(leader)
	if leader {
		c.metrics.ControllerLeader.WithLabelValues().Set(1)
	} else {
		c.metrics.ControllerLeader.WithLabelValues().Set(0)
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

	// The events are about the change itself, and a single event cannot name several agents.
	registry.OnChange(func(agents []model.AgentInfo, change TopologyChange) {
		c.grpcServer.BroadcastPeerUpdate(agents)
		m.ControllerRegisteredAgents.WithLabelValues().Set(float64(len(agents)))
		for _, tc := range change.Events() {
			c.grpcServer.PublishEvent(&pb.Event{Payload: &pb.Event_TopologyChanged{TopologyChanged: tc}})
		}
	})

	// No leader-election loop is wired yet; default to leader so behavior is
	// unchanged. SetLeader also syncs the controller_leader gauge to 1.
	c.SetLeader(true)

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

	errCh := make(chan error, 2)

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
	grpcLis, err := lc.Listen(ctx, "tcp", fmt.Sprintf(":%d", c.cfg.GRPCPort))
	if err != nil {
		return fmt.Errorf("gRPC listen: %w", err)
	}

	go func() {
		slog.Info("gRPC server listening", "port", c.cfg.GRPCPort)
		errCh <- grpcSrv.Serve(grpcLis)
	}()

	httpSrv := &http.Server{
		Addr:         fmt.Sprintf(":%d", c.cfg.HTTPPort),
		Handler:      c.httpServer.Handler(),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		slog.Info("HTTP server listening", "port", c.cfg.HTTPPort)
		errCh <- httpSrv.ListenAndServe()
	}()

	if c.cfg.Controller.LeaderElection {
		clientset, err := buildInClusterClientset()
		if err != nil {
			slog.Warn("in-cluster k8s client unavailable, NodeWatcher disabled", "error", err)
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

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
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

package controller

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

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

	c.grpcServer = NewGRPCServer(registry, m)
	c.httpServer = NewHTTPServer(registry, nil, promReg)

	registry.OnChange(func(agents []model.AgentInfo) {
		c.grpcServer.BroadcastPeerUpdate(agents)
		m.ControllerRegisteredAgents.WithLabelValues().Set(float64(len(agents)))
	})

	m.ControllerLeader.WithLabelValues().Set(1)

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
		grpcSrv.GracefulStop()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
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

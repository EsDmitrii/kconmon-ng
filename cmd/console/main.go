package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	appconfig "github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/config"
	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/events"
	"github.com/EsDmitrii/kconmon-ng/internal/console/httpapi"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/promql"
	"github.com/EsDmitrii/kconmon-ng/internal/console/push"
	"github.com/EsDmitrii/kconmon-ng/internal/console/ui"
	"github.com/EsDmitrii/kconmon-ng/internal/console/ws"
	"github.com/prometheus/client_golang/prometheus"
)

// pushInterval is how often each snapshot pusher recomputes and broadcasts when
// nothing nudges it. It matches the frontend's MATRIX_POLL_MS/TOPOLOGY_POLL_MS
// (both 15s), so a browser on the WebSocket path never sees staler data than one
// on the M1 polling path. Not operator-tunable in M2: the nudge relay already
// covers the latency case that matters (topology changes).
const pushInterval = 15 * time.Second

func main() {
	configPath := os.Getenv("KCONMON_NG_CONSOLE_CONFIG")
	if configPath == "" {
		configPath = "/etc/kconmon-ng-console/config.yaml"
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		slog.Error("failed to load console config", "error", err)
		os.Exit(1)
	}

	appconfig.SetupLogger(cfg.LogLevel, cfg.LogFormat)

	slog.Info("kconmon-ng console starting", //nolint:gosec // G706: values come from the local config file, structured slog fields
		"version", appconfig.Version,
		"commit", appconfig.Commit,
		"buildDate", appconfig.BuildDate,
		"authMode", cfg.Auth.Mode,
	)
	if cfg.Auth.Mode == "anonymous" {
		slog.Warn("console is running in ANONYMOUS auth mode — do not use in production", //nolint:gosec // G706: role comes from the local config file, structured slog field
			"role", cfg.Auth.Anonymous.Role)
	}

	promReg := prometheus.NewRegistry()
	m := metrics.New(cfg.MetricsPrefix, promReg)

	uiHandler, err := ui.Handler()
	if err != nil {
		slog.Error("failed to initialize embedded UI", "error", err)
		os.Exit(1)
	}

	var ctrl *controllerclient.Client
	if cfg.Controller.URL != "" {
		ctrl = controllerclient.New(cfg.Controller.URL, cfg.Controller.Timeout)
	} else {
		slog.Warn("controller.url not set — topology API disabled (503)")
	}
	var prom *promql.Client
	if cfg.Prometheus.URL != "" {
		prom = promql.New(cfg.Prometheus.URL, promql.Guards{
			QueryTimeout:     cfg.Prometheus.QueryTimeout,
			MaxRange:         cfg.Prometheus.MaxRange,
			MaxResponseBytes: cfg.Prometheus.MaxResponseBytes,
		})
	} else {
		slog.Warn("prometheus.url not set — matrix and PromQL APIs disabled (503)")
	}

	// Two contexts on purpose. rootCtx is cancelled by SIGINT/SIGTERM and governs
	// the HTTP server only. bgCtx governs the realtime pipeline and is cancelled
	// by hand AFTER the HTTP server has finished shutting down — see the shutdown
	// block at the bottom of main.
	rootCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	bgCtx, stopBackground := context.WithCancel(context.Background())

	bus, closeBus := newBus(bgCtx, cfg)

	// The Hub exists unconditionally: snapshot topics (topology, matrix:*) are
	// pushed locally and do not depend on the event pipeline, so GET /ws is
	// useful even when realtime event ingestion is off.
	hub := ws.NewHub(bus, m)

	grpcAddr := cfg.Controller.GRPCAddr
	switch {
	case grpcAddr == "":
		slog.Warn("controller.grpcAddr not set — realtime event ingestion disabled, /ws serves snapshots only")
	case ctrl == nil:
		slog.Warn("controller.grpcAddr is set but controller.url is empty — realtime event ingestion disabled " +
			"(the capability precheck needs the controller HTTP API)")
		grpcAddr = ""
	}
	ingester := events.NewIngester(ctrl, grpcAddr, bus, m)

	var nudgers []push.Nudger
	var matrixPusher *push.MatrixPusher
	if prom != nil {
		matrixPusher = push.NewMatrixPusher(prom, hub, cfg.MetricsPrefix, pushInterval, m)
		nudgers = append(nudgers, matrixPusher)
	}
	var topologyPusher *push.TopologyPusher
	if ctrl != nil {
		topologyPusher = push.NewTopologyPusher(ctrl, hub, pushInterval, m)
		nudgers = append(nudgers, topologyPusher)
	}

	srv := httpapi.NewServer(cfg, m, promReg, uiHandler, ctrl, prom, hub, ingester)

	var wg sync.WaitGroup
	spawn := func(component string, run func(context.Context)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			run(bgCtx)
			slog.Debug("realtime component stopped", "component", component)
		}()
	}

	spawn("ws-hub", hub.Run)
	spawn("event-ingester", ingester.Run)
	if matrixPusher != nil {
		spawn("matrix-pusher", matrixPusher.Run)
	}
	if topologyPusher != nil {
		spawn("topology-pusher", topologyPusher.Run)
	}
	if len(nudgers) > 0 {
		spawn("nudge-relay", func(ctx context.Context) { push.RunNudgeRelay(ctx, bus, nudgers...) })
	}

	srv.SetReady(true)

	runErr := srv.Run(rootCtx)

	// Shutdown, in this order and no other:
	//  1. srv.Run has already returned, so the listener is closed and
	//     http.Server.Shutdown has drained the in-flight plain HTTP requests. No
	//     new WebSocket upgrade can arrive from here on.
	//  2. stopBackground stops the ingester, the pushers, the nudge relay and the
	//     Hub. Hijacked WebSocket connections are not tracked by
	//     http.Server.Shutdown, so this — not step 1 — is what releases them.
	//  3. wg.Wait guarantees nobody is inside Bus.Publish any more. It covers
	//     ONLY the components spawned above (hub.Run, ingester, pushers, relay)
	//     — the per-client read/write pumps (ws/conn.go) are tracked by nothing
	//     and are abandoned at process exit, so the ~2×writeWait (≈20s) linger a
	//     writePump may need against a wedged peer is best-effort (peer may see
	//     an RST instead of a 1001 close frame), not a budget honored here.
	//     wg.Wait itself returns promptly: every waited component exits on ctx
	//     cancellation.
	//  4. closeBus releases the Valkey client last. Closing it earlier would make
	//     in-flight publishes fail against a closed client and log errors during
	//     an otherwise clean shutdown.
	slog.Info("http server stopped, draining realtime pipeline")
	stopBackground()
	wg.Wait()
	closeBus()
	stopSignals()
	slog.Info("console shutdown complete")

	if runErr != nil {
		slog.Error("console exited with error", "error", runErr)
		os.Exit(1)
	}
}

// newBus selects the pub/sub backend the realtime pipeline runs on and returns a
// close func for it.
//
// A non-empty valkey.address means real cross-replica fan-out (cache.ValkeyBus):
// every replica's ingester publishes there, and every replica's Hub sees every
// event. An empty address — or a Valkey that cannot be dialled at startup —
// falls back to cache.NewInProcessBus(). That fallback is deliberate, not a
// papered-over error: a single-replica console on an in-process bus is a
// documented working state (ADR-002), so a Valkey outage must degrade realtime
// fan-out, not stop the console from booting and serving the M1 REST API.
//
// The fallback lasts until the process restarts: NewValkeyBus only fails on the
// initial dial. Disconnects after that are rueidis's problem — the client
// retries and resubscribes internally, with the bus's own receive loop as a
// backstop for the cases rueidis gives up on. The warning below is therefore
// the operator's signal to restart the console once Valkey is back.
//
// The returned closeBus must be called exactly once, after every publisher has
// stopped: cancelling ctx stops the ValkeyBus receive loop but does NOT release
// the rueidis client — only Close does.
func newBus(ctx context.Context, cfg *config.Config) (bus cache.Bus, closeBus func()) {
	if cfg.Valkey.Address == "" {
		slog.Info("valkey.address not set — using the in-process bus (realtime fan-out is single-replica only)")
		return cache.NewInProcessBus(), func() {}
	}

	vb, err := cache.NewValkeyBus(ctx, cfg.Valkey.Address, cfg.Valkey.DialTimeout)
	if err != nil {
		slog.Warn("valkey unreachable at startup — falling back to the in-process bus; "+ //nolint:gosec // G706: address comes from the local config file, structured slog field
			"realtime fan-out is single-replica only until the console is restarted",
			"address", cfg.Valkey.Address, "error", err)
		return cache.NewInProcessBus(), func() {}
	}

	slog.Info("valkey pub/sub enabled", "address", cfg.Valkey.Address) //nolint:gosec // G706: address comes from the local config file, structured slog field
	return vb, vb.Close
}

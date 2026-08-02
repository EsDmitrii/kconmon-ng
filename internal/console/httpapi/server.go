// Package httpapi is the Console HTTP server: health/readiness/metrics, a
// minimal version/config API, and the embedded SPA. M0 has no data endpoints.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	appconfig "github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/EsDmitrii/kconmon-ng/internal/console/config"
	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/promql"
	"github.com/EsDmitrii/kconmon-ng/internal/console/ws"
	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// capabilityEvents is the flag the console advertises on GET /api/v1/version
// while its realtime event pipeline is actually live.
const capabilityEvents = "events"

// RealtimeStatus reports live realtime-pipeline health. Satisfied by
// *events.Ingester. An interface keeps httpapi from importing events — the
// handler needs one bit, not a pipeline.
type RealtimeStatus interface{ Healthy() bool }

// Server serves the Console HTTP surface.
type Server struct {
	cfg      *config.Config
	metrics  *metrics.Metrics
	router   chi.Router
	ready    atomic.Bool
	ctrl     *controllerclient.Client
	prom     *promql.Client
	hub      *ws.Hub
	realtime RealtimeStatus
}

// NewServer wires the router, middleware, and routes. ctrl, prom, hub and
// realtime may all be nil (feature unset); the endpoints that need them answer
// 503, and /api/v1/version simply advertises no capabilities.
func NewServer(cfg *config.Config, m *metrics.Metrics, promReg *prometheus.Registry,
	ui http.Handler, ctrl *controllerclient.Client, prom *promql.Client,
	hub *ws.Hub, realtime RealtimeStatus) *Server {
	s := &Server{cfg: cfg, metrics: m, ctrl: ctrl, prom: prom, hub: hub, realtime: realtime}

	r := chi.NewRouter()
	r.Use(s.instrument)

	r.Get("/healthz", s.handleHealthz)
	r.Get("/readyz", s.handleReadyz)
	r.Handle("/metrics", promhttp.HandlerFor(promReg, promhttp.HandlerOpts{}))
	r.Get("/api/v1/version", s.handleVersion)
	r.Get("/api/v1/config", s.handleConfig)
	r.Get("/api/v1/topology", s.handleTopology)
	r.Get("/api/v1/matrix", s.handleMatrix)
	r.Post("/api/v1/promql/query", s.handlePromQLQuery)
	r.Post("/api/v1/promql/query_range", s.handlePromQLQueryRange)
	// /ws is TOP LEVEL, not under /api/v1 (docs/console/architecture/API.md,
	// ADR-003). One long-lived upgrade is recorded once by s.instrument as
	// path="/ws" when the socket closes — its "duration" is the connection
	// LIFETIME, so it lands in the histogram's top (+Inf) bucket and inflates
	// _sum by connection-seconds; don't read request latency from that series
	// (cardinality is unaffected — path is the route pattern). Live connection
	// count is the ws_clients gauge.
	r.Get("/ws", s.handleWS)
	// SPA + static assets: everything not matched above.
	r.NotFound(ui.ServeHTTP)

	s.router = r
	m.BuildInfo.WithLabelValues(appconfig.Version, appconfig.Commit).Set(1)
	return s
}

// Handler exposes the router for tests and embedding.
func (s *Server) Handler() http.Handler { return s.router }

// SetReady flips the readiness gate.
func (s *Server) SetReady(ready bool) { s.ready.Store(ready) }

// Run starts the HTTP server and shuts it down gracefully when ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Handler:           s.router,
		ReadHeaderTimeout: 10 * time.Second,
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", ":"+strconv.Itoa(s.cfg.HTTPPort))
	if err != nil {
		return fmt.Errorf("listen on :%d: %w", s.cfg.HTTPPort, err)
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("console http server listening", "addr", ln.Addr().String())
		errCh <- srv.Serve(ln)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if !s.ready.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready"))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"version":      appconfig.Version,
		"commit":       appconfig.Commit,
		"capabilities": s.capabilities(),
	})
}

// capabilities is computed per request from live pipeline health, never from
// config: the browser feature-detects against it every 15s and must see the
// capability disappear the moment the event stream drops mid-session.
//
// Scope: THIS replica's own ingester only. cache.Bus is deliberately frozen with
// no liveness API, so there is nothing to OR in — and that leaves a known,
// accepted asymmetry. A replica whose own gRPC stream is down can still fan out
// live events that OTHER replicas published to Valkey, yet it advertises no
// "events" capability here, so a browser pinned to it falls back to 15s
// Prometheus polling with the "delayed data" badge while its socket would in fact
// have kept delivering. That is safe-but-conservative by choice: under-claiming
// costs freshness, over-claiming would strand a browser on a silent socket. The
// same conservatism covers the ≤2s post-reconnect grace window in
// events.Ingester.Healthy, which correctly reads as "not yet".
//
// make([]string, 0, 1) rather than a nil slice on purpose — the JSON has to stay
// [] and never null, because the frontend indexes into it.
func (s *Server) capabilities() []string {
	caps := make([]string, 0, 1)
	if s.realtime != nil && s.realtime.Healthy() {
		caps = append(caps, capabilityEvents)
	}
	return caps
}

// handleWS upgrades to the multiplexed WebSocket protocol. A nil hub answers
// 503 with an RFC 7807 body, matching how a missing controller/Prometheus is
// handled: the route always exists, so the browser gets a diagnosable answer
// instead of the SPA fallback HTML.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	if s.hub == nil {
		writeProblem(w, http.StatusServiceUnavailable, "realtime not available",
			"this console instance has no websocket hub wired")
		return
	}
	s.hub.ServeWS(w, r)
}

func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"auth": map[string]string{
			"mode": s.cfg.Auth.Mode,
			"role": s.cfg.Auth.Anonymous.Role,
		},
		"anonymousBanner": s.cfg.Auth.Mode == "anonymous",
		"controller":      map[string]bool{"configured": s.ctrl != nil},
		"prometheus":      map[string]bool{"configured": s.prom != nil},
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// statusRecorder captures the response status for metrics.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Unwrap exposes the ResponseWriter this recorder wraps, which is what lets a
// WebSocket upgrade take the connection over: gorilla's Upgrade hijacks through
// http.NewResponseController, and the controller only finds the real
// http.Hijacker by following Unwrap. Without this method every /ws upgrade
// through the middleware chain fails with HTTP 500 "websocket: hijack: feature
// not supported" — embedding an interface promotes only that interface's
// methods, so the recorder hides net/http's Hijacker.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// instrument records request count and duration keyed by method + route
// pattern (bounded cardinality) + status.
func (s *Server) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		pattern := chi.RouteContext(r.Context()).RoutePattern()
		if pattern == "" {
			pattern = "spa" // NotFound / SPA fallback bucket
		}
		s.metrics.HTTPRequests.WithLabelValues(r.Method, pattern, strconv.Itoa(rec.status)).Inc()
		s.metrics.HTTPDuration.WithLabelValues(r.Method, pattern).Observe(time.Since(start).Seconds())
	})
}

package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Route is an extra endpoint a component mounts on its metrics listener next to the fixed three.
// The controller's Prometheus SD body lives here because metricsPort is the one port the chart's
// NetworkPolicy opens to the scraper; the agent and the console mount nothing extra.
type Route struct {
	Pattern string
	Handler http.Handler
}

// NewListenerHandler builds the metrics-listener mux: /metrics plus the two health endpoints, plus
// any extra routes. `ready` reports readiness; a nil func means always ready. It gets a port of its
// own because the controller's API port is unauthenticated and a NetworkPolicy opens ports, not
// paths: a scraper let in here reaches nothing that drives the fleet.
func NewListenerHandler(promReg *prometheus.Registry, ready func() bool, extra ...Route) http.Handler {
	mux := http.NewServeMux()
	for _, r := range extra {
		mux.Handle(r.Pattern, r.Handler)
	}
	mux.Handle("GET /metrics", promhttp.HandlerFor(promReg, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if ready != nil && !ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

// listenerTimeouts are deliberately short: every endpoint on this listener answers from memory.
const (
	listenerReadTimeout  = 10 * time.Second
	listenerWriteTimeout = 10 * time.Second
)

// NewListener builds the metrics HTTP server for addr.
func NewListener(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:         addr,
		Handler:      h,
		ReadTimeout:  listenerReadTimeout,
		WriteTimeout: listenerWriteTimeout,
	}
}

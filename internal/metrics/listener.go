package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

/*
Package-level: the METRICS listener, separate from whatever API a component also serves.

The controller serves its whole API on one port — GET /api/v1/topology, POST /api/v1/diagnostics,
PUT /api/v1/external-checks — and authenticates none of it; the only gate is leader election, which
is availability, not authorization. A NetworkPolicy rule admitting a scraper to that port therefore
admitted whoever else lives in the scraper's namespace to the fleet's control plane: on a real
cluster that namespace also holds Grafana, node-exporter, kube-state-metrics and the operator, any
of which is one compromise away from dispatching probes and rewriting external-check assignments.
Narrowing the rule from "every namespace" to "the monitoring namespace" moved the blast radius; it
did not remove it, because a NetworkPolicy cannot say "this port, but only these paths".

Two listeners can. Scraping needs /metrics and nothing else, so /metrics gets a port of its own and
the policy opens that one. The API port keeps serving /metrics too — anyone scraping it directly is
unaffected — but nothing in the chart opens it to a scraper any more.

The health endpoints ride along because a probe and a scrape want the same thing: a port that is
safe to expose.
*/

// NewListenerHandler builds the metrics-listener mux: /metrics plus the two health endpoints.
// `ready` reports readiness; a nil func means always ready.
func NewListenerHandler(promReg *prometheus.Registry, ready func() bool) http.Handler {
	mux := http.NewServeMux()
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

// Package metrics defines the Console's self-monitoring Prometheus metrics.
// All names are namespaced <prefix>_console_* per DESIGN.md §12.
package metrics //nolint:revive // intentional: "metrics" is clearer than alternatives for this package

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var durationBuckets = []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0}

// Metrics holds the Console HTTP self-metrics.
type Metrics struct {
	BuildInfo    *prometheus.GaugeVec
	HTTPRequests *prometheus.CounterVec
	HTTPDuration *prometheus.HistogramVec

	// Realtime pipeline (M2): event ingestion, WebSocket fan-out, snapshot
	// pushers. Deliberately label-poor — no node, pod, query-text or
	// connection-ID labels anywhere.
	EventsReceived     *prometheus.CounterVec
	EventsDeduped      *prometheus.CounterVec
	IngesterConnected  *prometheus.GaugeVec
	IngesterReconnects *prometheus.CounterVec
	WSClients          *prometheus.GaugeVec
	WSMessagesSent     *prometheus.CounterVec
	WSDroppedClients   *prometheus.CounterVec
	PushSnapshots      *prometheus.CounterVec
}

// New registers and returns the Console metrics under <prefix>_console_*.
// If reg is nil the default registerer is used.
func New(prefix string, reg prometheus.Registerer) *Metrics {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	f := promauto.With(reg)
	ns := prefix + "_console"

	return &Metrics{
		BuildInfo: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: ns + "_build_info",
			Help: "Console build info; value is always 1.",
		}, []string{"version", "commit"}),
		HTTPRequests: f.NewCounterVec(prometheus.CounterOpts{
			Name: ns + "_http_requests_total",
			Help: "Total Console HTTP requests.",
		}, []string{"method", "path", "status"}),
		HTTPDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    ns + "_http_request_duration_seconds",
			Help:    "Console HTTP request duration in seconds.",
			Buckets: durationBuckets,
		}, []string{"method", "path"}),
		EventsReceived: f.NewCounterVec(prometheus.CounterOpts{
			Name: ns + "_events_received_total",
			Help: "Controller domain events received by this replica's ingester, by type.",
		}, []string{"type"}),
		EventsDeduped: f.NewCounterVec(prometheus.CounterOpts{
			Name: ns + "_events_deduped_total",
			Help: "Live events dropped by the WebSocket hub as duplicates of an event another replica already ingested.",
		}, []string{}),
		IngesterConnected: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: ns + "_ingester_connected",
			Help: "1 while this replica holds an established WatchEvents stream to the controller, 0 otherwise.",
		}, []string{}),
		IngesterReconnects: f.NewCounterVec(prometheus.CounterOpts{
			Name: ns + "_ingester_reconnects_total",
			Help: "Ingester reconnect attempts, by reason (dial, stream, capability).",
		}, []string{"reason"}),
		WSClients: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: ns + "_ws_clients",
			Help: "Currently connected WebSocket clients on this replica.",
		}, []string{}),
		WSMessagesSent: f.NewCounterVec(prometheus.CounterOpts{
			Name: ns + "_ws_messages_sent_total",
			Help: "Envelopes handed to a WebSocket client's send buffer, by topic.",
		}, []string{"topic"}),
		WSDroppedClients: f.NewCounterVec(prometheus.CounterOpts{
			Name: ns + "_ws_dropped_clients_total",
			Help: "WebSocket clients closed because their send buffer overflowed.",
		}, []string{}),
		PushSnapshots: f.NewCounterVec(prometheus.CounterOpts{
			Name: ns + "_push_snapshots_total",
			Help: "Server-side snapshot pushes, by topic and result (ok, error).",
		}, []string{"topic", "result"}),
	}
}

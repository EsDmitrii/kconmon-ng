// Package metrics defines the Console's self-monitoring Prometheus metrics.
// All names are namespaced <prefix>_console_* per DESIGN.md §12.
package metrics //nolint:revive // intentional: "metrics" is clearer than alternatives for this package

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var durationBuckets = []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0}

// runDurationBuckets covers a whole diagnostics run, not one HTTP request.
var runDurationBuckets = []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600}

// Metrics holds the Console HTTP self-metrics.
type Metrics struct {
	BuildInfo    *prometheus.GaugeVec
	HTTPRequests *prometheus.CounterVec
	HTTPDuration *prometheus.HistogramVec

	// Realtime pipeline: event ingestion, WebSocket fan-out, snapshot pushers.
	EventsReceived     *prometheus.CounterVec
	EventsDeduped      *prometheus.CounterVec
	IngesterConnected  *prometheus.GaugeVec
	IngesterReconnects *prometheus.CounterVec
	WSClients          *prometheus.GaugeVec
	WSMessagesSent     *prometheus.CounterVec
	WSDroppedClients   *prometheus.CounterVec
	PushSnapshots      *prometheus.CounterVec

	// WSTopics is the ephemeral run:{id} topic registry size; zero labels, like WSClients -- a run ID
	// must never become a label value (see ws.Hub.deliver).
	WSTopics *prometheus.GaugeVec

	// StorePoolConns' state is its own closed set (acquired, idle, total) and is written by exactly
	// one owner.
	StoreQueries       *prometheus.CounterVec
	StoreQueryDuration *prometheus.HistogramVec
	StorePoolConns     *prometheus.GaugeVec
	EventsPersisted    *prometheus.CounterVec

	// RetentionDeleted is store.Pruner's own metric.
	RetentionDeleted *prometheus.CounterVec

	// Neither metric is written by the store package itself -- AuthRequests is the authn layer's own
	// metric: {mode,result}.
	AuthRequests *prometheus.CounterVec
	AuthzDenied  *prometheus.CounterVec

	// AuditDropped counts audit_log rows the async write buffer discarded because it was full.
	AuditDropped *prometheus.CounterVec

	// ProjectionGuardFailOpen counts definition writes the cardinality projection guard ALLOWED
	// because the topology could not be read.
	ProjectionGuardFailOpen *prometheus.CounterVec

	// RateLimited counts requests the fixed-window rate limiter REFUSED with a 429; the limiter never
	// puts a username, a subject ID, or a source IP in a label.
	RateLimited       *prometheus.CounterVec
	RateLimitFailOpen *prometheus.CounterVec

	// Diagnostics runner: checks.Runner's fan-out lifecycle; closed label sets, same convention as
	// WSTopics -- a run ID must never become a label value.
	RunsTotal   *prometheus.CounterVec
	RunPairs    *prometheus.CounterVec
	RunDuration *prometheus.HistogramVec

	// Schedule loop: internal/console/scheduler's per-tick lifecycle; SchedulerTicks' result is
	// ok|not-leader|error, one increment per tick on EVERY replica.
	SchedulerTicks   *prometheus.CounterVec
	SchedulerFired   *prometheus.CounterVec
	SchedulerSkipped *prometheus.CounterVec
	RunsReaped       *prometheus.CounterVec

	// Continuous external-check reconciler: internal/console/checks' assignment ticker.
	ExternalSeriesProjected *prometheus.GaugeVec
	ExternalReconciles      *prometheus.CounterVec
	ExternalSpecsSkipped    *prometheus.CounterVec

	// MTRSnapshots is the path-history projector's metric.
	MTRSnapshots *prometheus.CounterVec

	// Hop enrichment: internal/console/enrich's TTL cache over rDNS + mmdb; TWO counters rather than
	// one, because a cache hit and a source lookup are not the same event and cannot share a label set
	// honestly.
	EnrichmentCache   *prometheus.CounterVec
	EnrichmentLookups *prometheus.CounterVec

	// K8sEvents is the Kubernetes event reader's metric; a rising filtered with a flat stored is the
	// shape of a controller outage.
	K8sEvents *prometheus.CounterVec

	// WebhookDeliveries is the outbound dispatcher's metric; ONE increment per delivery the dispatcher
	// reached a TERMINAL decision about.
	WebhookDeliveries *prometheus.CounterVec
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
		WSTopics: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: ns + "_ws_topics",
			Help: "Ephemeral run:{id} WebSocket topics currently registered (open or awaiting reap).",
		}, []string{}),
		StoreQueries: f.NewCounterVec(prometheus.CounterOpts{
			Name: ns + "_store_queries_total",
			Help: "Database queries issued by the store package, by generated query name and result (ok, conflict, error).",
		}, []string{"query", "result"}),
		StoreQueryDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    ns + "_store_query_duration_seconds",
			Help:    "Database query duration in seconds, by generated query name.",
			Buckets: durationBuckets,
		}, []string{"query"}),
		StorePoolConns: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: ns + "_store_pool_conns",
			Help: "Database connection pool size, by state (acquired, idle, total).",
		}, []string{"state"}),
		EventsPersisted: f.NewCounterVec(prometheus.CounterOpts{
			Name: ns + "_events_persisted_total",
			Help: "Controller events written to topology_events, by result (ok, conflict, error).",
		}, []string{"result"}),
		RetentionDeleted: f.NewCounterVec(prometheus.CounterOpts{
			Name: ns + "_retention_deleted_total",
			Help: "Rows deleted by the retention pruner, by table (topology_events, audit_log, check_runs, " +
				"mtr_path_snapshots, mtr_hop_enrichment, annotations).",
		}, []string{"table"}),
		AuthRequests: f.NewCounterVec(prometheus.CounterOpts{
			Name: ns + "_auth_requests_total",
			Help: "Console authentication attempts, by mode and result (ok, invalid, expired, error).",
		}, []string{"mode", "result"}),
		AuthzDenied: f.NewCounterVec(prometheus.CounterOpts{
			Name: ns + "_authz_denied_total",
			Help: "Requests denied by the authz policy, by permission.",
		}, []string{"permission"}),
		AuditDropped: f.NewCounterVec(prometheus.CounterOpts{
			Name: ns + "_audit_dropped_total",
			Help: "Audit log entries dropped because the async write buffer was full.",
		}, []string{}),
		ProjectionGuardFailOpen: f.NewCounterVec(prometheus.CounterOpts{
			Name: ns + "_projection_guard_failopen_total",
			Help: "Definition writes the cardinality projection guard allowed because the topology was unreadable (fail-open, Decision 8).",
		}, []string{}),
		RateLimited: f.NewCounterVec(prometheus.CounterOpts{
			Name: ns + "_rate_limited_total",
			Help: "Requests refused with 429 by the fixed-window rate limiter, by limit (runs, login).",
		}, []string{"limit"}),
		RateLimitFailOpen: f.NewCounterVec(prometheus.CounterOpts{
			Name: ns + "_rate_limit_failopen_total",
			Help: "Requests the rate limiter allowed because the KV backend was unreadable (fail-open, Decision 8), by limit (runs, login).",
		}, []string{"limit"}),
		RunsTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name: ns + "_runs_total",
			Help: "Diagnostics runs completed, by check type and terminal status (succeeded, partial, failed).",
		}, []string{"type", "status"}),
		RunPairs: f.NewCounterVec(prometheus.CounterOpts{
			Name: ns + "_run_pairs_total",
			Help: "Diagnostics run pairs dispatched, by result (ok, failed, timeout).",
		}, []string{"result"}),
		RunDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    ns + "_run_duration_seconds",
			Help:    "Diagnostics run wall-clock duration in seconds, by check type.",
			Buckets: runDurationBuckets,
		}, []string{"type"}),
		SchedulerTicks: f.NewCounterVec(prometheus.CounterOpts{
			Name: ns + "_scheduler_ticks_total",
			Help: "Schedule loop ticks, by result (ok, not-leader, error). not-leader is the normal case on every replica but one.",
		}, []string{"result"}),
		SchedulerFired: f.NewCounterVec(prometheus.CounterOpts{
			Name: ns + "_scheduler_fired_total",
			Help: "Runs started by the schedule loop, by schedule kind (once, interval).",
		}, []string{"kind"}),
		SchedulerSkipped: f.NewCounterVec(prometheus.CounterOpts{
			Name: ns + "_scheduler_skipped_total",
			Help: "Due schedules the loop declined to fire, by reason (overrun, disabled).",
		}, []string{"reason"}),
		RunsReaped: f.NewCounterVec(prometheus.CounterOpts{
			Name: ns + "_runs_reaped_total",
			Help: "Runs force-finished as cancelled by the stuck-run reaper.",
		}, []string{}),
		ExternalSeriesProjected: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: ns + "_external_series_projected",
			Help: "Prometheus series the currently assigned continuous external checks project to.",
		}, []string{}),
		ExternalReconciles: f.NewCounterVec(prometheus.CounterOpts{
			Name: ns + "_external_reconciles_total",
			Help: "Continuous external-check reconcile ticks, by result (pushed, unchanged, not-leader, error).",
		}, []string{"result"}),
		ExternalSpecsSkipped: f.NewCounterVec(prometheus.CounterOpts{
			Name: ns + "_external_specs_skipped_total",
			Help: "Continuous definitions left out of the desired assignment, by reason (check-type, destination-kind).",
		}, []string{"reason"}),
		MTRSnapshots: f.NewCounterVec(prometheus.CounterOpts{
			Name: ns + "_mtr_snapshots_total",
			Help: "MTR traces projected into path history, by result (new-path, repeat, error). " +
				"new-path means the pair took a route it had never taken before -- the route-changed alerting primitive.",
		}, []string{"result"}),
		EnrichmentCache: f.NewCounterVec(prometheus.CounterOpts{
			Name: ns + "_enrichment_cache_total",
			Help: "Hop addresses the enrichment TTL cache was asked about, by result (hit, miss). " +
				"hit/(hit+miss) is the cache hit ratio.",
		}, []string{"result"}),
		EnrichmentLookups: f.NewCounterVec(prometheus.CounterOpts{
			Name: ns + "_enrichment_lookups_total",
			Help: "Enrichment source lookups performed for cache misses, by source (rdns, asn, city) and " +
				"result (ok, miss, error). miss means the source knew nothing about the address; " +
				"error means the lookup failed. Disabled sources are never counted.",
		}, []string{"source", "result"}),
		K8sEvents: f.NewCounterVec(prometheus.CounterOpts{
			Name: ns + "_k8s_events_total",
			Help: "Kubernetes events the console's reader decided about, by result " +
				"(stored, duplicate, filtered, error). duplicate is the normal outcome of a relist, " +
				"not a failure; filtered is the fail-closed drop of a node event with no topology to " +
				"vouch for the node.",
		}, []string{"result"}),
		WebhookDeliveries: f.NewCounterVec(prometheus.CounterOpts{
			Name: ns + "_webhook_deliveries_total",
			Help: "Webhook deliveries the dispatcher reached a terminal decision about, by result " +
				"(ok, failed, filtered). One per delivery, never per HTTP attempt; filtered is the " +
				"steady state of an endpoint that does not subscribe to the event, and a disabled " +
				"endpoint is not counted at all.",
		}, []string{"result"}),
	}
}

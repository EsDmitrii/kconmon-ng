// Package metrics defines the Console's self-monitoring Prometheus metrics.
// All names are namespaced <prefix>_console_* per DESIGN.md §12.
package metrics //nolint:revive // intentional: "metrics" is clearer than alternatives for this package

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var durationBuckets = []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0}

// runDurationBuckets covers a whole diagnostics run, not one HTTP request: at
// 400 pairs / 8 concurrent / up to 120s per pair the theoretical ceiling is
// hours, so this buckets minutes-to-tens-of-minutes instead of durationBuckets'
// sub-5-second range.
var runDurationBuckets = []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600}

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

	// WSTopics is the M3 ephemeral run:{id} topic registry size (Task 20):
	// how many are currently open or awaiting reap, bounded by
	// ws.maxEphemeralTopics (256). Zero labels, like WSClients -- a run ID
	// must never become a label value (see ws.Hub.deliver).
	WSTopics *prometheus.GaugeVec

	// Persistence (M3). query is a closed set of sqlc method names; result is
	// ok|conflict|error. No table, row-count, user or run-ID labels anywhere.
	// StorePoolConns' state is its own closed set (acquired, idle, total) and
	// is written by exactly one owner, store.PoolStatsPoller, spawned by
	// cmd/console alongside the retention pruner -- nothing else samples the
	// pool, so the gauge never has two writers disagreeing about it.
	StoreQueries       *prometheus.CounterVec
	StoreQueryDuration *prometheus.HistogramVec
	StorePoolConns     *prometheus.GaugeVec
	EventsPersisted    *prometheus.CounterVec

	// RetentionDeleted is store.Pruner's own metric: table is a closed set
	// (topology_events, audit_log, check_runs) -- never widened with row
	// counts, cutoff times, or per-sweep identifiers.
	RetentionDeleted *prometheus.CounterVec

	// Auth (M3). Neither metric is written by the store package itself --
	// AuthRequests is the authn layer's own metric (Task 14): {mode,result},
	// result in ok|invalid|expired|error. AuthzDenied is the authz
	// middleware's own metric: {permission}, bounded by authz.AllPermissions
	// (asserted in metrics_test.go). Both are declared here, alongside every
	// other Console metric, so there is one place metrics live.
	AuthRequests *prometheus.CounterVec
	AuthzDenied  *prometheus.CounterVec

	// AuditDropped counts audit_log rows the async write buffer discarded
	// because it was full (httpapi's audit middleware, Task 17) -- the
	// documented lossiness a best-effort, latency-free audit trail
	// requires: a full buffer must never block or fail the request it
	// describes, so it drops the entry and counts it here instead.
	AuditDropped *prometheus.CounterVec

	// ProjectionGuardFailOpen counts definition writes the cardinality
	// projection guard ALLOWED because the topology could not be read (M4
	// Task 4, Decision 8: the guard fails open on a backend error and
	// increments a metric so failing-open is visible rather than silent --
	// a controller outage must never become a config-write outage, but it
	// must be alertable). Zero labels.
	ProjectionGuardFailOpen *prometheus.CounterVec

	// RateLimited counts requests the fixed-window rate limiter REFUSED with
	// a 429 (M4 Task 8). RateLimitFailOpen counts requests it ALLOWED because
	// the KV backend could not be counted against -- the same Decision 8
	// posture ProjectionGuardFailOpen above states: fail open, but make
	// failing-open visible and alertable rather than silent, because a Valkey
	// outage must never become a login outage.
	//
	// Both carry ONE label, limit, whose value set is CLOSED: runs|login. The
	// limiter never puts a username, a subject ID, or a source IP in a label
	// -- those are unbounded and attacker-chosen, which is exactly the
	// cardinality bomb the whole package's label discipline exists to prevent.
	RateLimited       *prometheus.CounterVec
	RateLimitFailOpen *prometheus.CounterVec

	// Diagnostics runner (M3, Task 22): checks.Runner's fan-out lifecycle.
	// Closed label sets, same convention as WSTopics -- a run ID must never
	// become a label value. RunsTotal's status is succeeded|partial|failed
	// (checks.Runner never reports a boolean: a 400-pair run with two
	// failures is the interesting case a boolean would hide). RunPairs'
	// result is ok|failed|timeout, one increment per dispatched pair.
	RunsTotal   *prometheus.CounterVec
	RunPairs    *prometheus.CounterVec
	RunDuration *prometheus.HistogramVec

	// Schedule loop (M4 Task 13): internal/console/scheduler's per-tick
	// lifecycle. Every label set below is CLOSED, and none of them ever
	// carries a schedule ID, a definition ID or a run ID -- the same
	// discipline WSTopics and RunsTotal follow.
	//
	// SchedulerTicks' result is ok|not-leader|error, one increment per tick
	// on EVERY replica: not-leader is the expected steady state on N-1 of
	// them (the advisory lock admits one), so a fleet-wide sum of ok is what
	// says the loop is alive, and error is the only one worth alerting on.
	// SchedulerFired's kind is once|interval -- 'continuous' is deliberately
	// NOT a value here: those schedules are agent-side and this loop never
	// fires one, so a series that could only ever be zero would just invite
	// the wrong conclusion. SchedulerSkipped's reason is overrun|disabled:
	// a due schedule whose previous run is still in flight, and a due
	// schedule whose definition is switched off. RunsReaped counts run rows
	// the stuck-run reaper force-finished (checks.Runner.ReapStuckRuns),
	// zero labels like AuditDropped.
	SchedulerTicks   *prometheus.CounterVec
	SchedulerFired   *prometheus.CounterVec
	SchedulerSkipped *prometheus.CounterVec
	RunsReaped       *prometheus.CounterVec

	// Continuous external-check reconciler (M4 Task 17):
	// internal/console/checks' assignment ticker. Same closed-label discipline
	// as the scheduler block above -- no definition ID, no agent ID, no target
	// name anywhere.
	//
	// ExternalSeriesProjected is the number of Prometheus series the CURRENTLY
	// assigned continuous checks project to (checks.AssignAgents' second
	// return). Zero labels, and a GAUGE rather than a counter because it
	// describes a standing state, not an event: it is refreshed on every
	// reconcile whose outcome is known-good, which includes the "nothing
	// changed" outcome -- the number is current either way, and letting it go
	// stale on the steady state (which is almost every tick) would make it
	// useless.
	//
	// ExternalReconciles' result is pushed|unchanged|not-leader|error.
	// not-leader is the expected steady state on N-1 replicas (the reconciler
	// shares the scheduler's advisory lock), unchanged is the expected steady
	// state on the leader, and pushed marks a tick that actually PUT. error
	// covers both a failed read and a failed PUT: in every error case the
	// last-pushed state is left dirty so the next tick retries.
	//
	// ExternalSpecsSkipped counts DEFINITIONS dropped from the desired state,
	// by reason: check-type (mtr/udp -- the controller 400s the whole PUT on
	// those, so one ineligible definition would cost every other agent its
	// assignment) and destination-kind (a continuous definition pointed at
	// cluster nodes, which is the agents' own peer mesh and not an external
	// check). It increments once per skipped definition per tick, so a
	// misconfiguration shows up as a steady rate rather than a single spike
	// that scrolls out of the retention window.
	ExternalSeriesProjected *prometheus.GaugeVec
	ExternalReconciles      *prometheus.CounterVec
	ExternalSpecsSkipped    *prometheus.CounterVec
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
			Help: "Rows deleted by the retention pruner, by table (topology_events, audit_log, check_runs).",
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
	}
}

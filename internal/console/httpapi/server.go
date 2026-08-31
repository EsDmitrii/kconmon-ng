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
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	appconfig "github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/config"
	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/promql"
	"github.com/EsDmitrii/kconmon-ng/internal/console/ws"
	metricslistener "github.com/EsDmitrii/kconmon-ng/internal/metrics"
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

// OIDCFlow is the subset of *authn.OIDCAuthenticator's surface handleOIDCStart/handleOIDCCallback
// actually need.
type OIDCFlow interface {
	AuthorizeURL(ctx context.Context, returnTo string) (authURL string, err error)
	Callback(ctx context.Context, state, code string) (sessionID, returnTo string, err error)
}

var _ OIDCFlow = (*authn.OIDCAuthenticator)(nil)

// Server serves the Console HTTP surface.
type Server struct {
	cfg *config.Config
	/* The trusted-proxy networks, parsed once at construction: the ONLY place this package believes
	   a forwarding header (ratelimit.go's clientIP). Empty by default, and then r.RemoteAddr is the
	   whole truth. */
	trustedProxies []*net.IPNet
	metrics        *metrics.Metrics
	router         chi.Router
	ready          atomic.Bool
	ctrl           *controllerclient.Client
	/* The agent-side external allowlist, cached from the controller; see targets_reachable.go. */
	allowlistMu sync.Mutex
	allowlist   externalAllowlist
	prom        *promql.Client
	hub         *ws.Hub
	realtime    RealtimeStatus
	events      EventLister

	// Auth. authenticator and policy are never nil (NewServer defaults both); roles, sessions, users
	// and oidc may all be nil.
	authenticator authn.Authenticator
	policy        *authz.Policy
	roles         RoleResolver
	sessions      *authn.SessionStore
	users         authn.UserStore
	oidc          OIDCFlow

	// Audit. audit is the async audit-log writer/reader (nil = database.mode=disabled -- the audit
	// middleware is then a complete no-op and GET /api/v1/audit answers 503).
	audit     Auditor
	auditCh   chan auditJob
	roleAdmin RoleAdmin
	tokens    TokenAdmin

	// runner backs POST/GET /api/v1/runs and GET /api/v1/runs/{id}.
	runner RunService

	// targets backs CRUD /api/v1/targets. nil means database.mode=disabled and all five routes answer
	// 503.
	targets TargetService

	// definitions and schedules back CRUD /api/v1/checks and /api/v1/schedules under.
	definitions DefinitionService
	schedules   ScheduleService
	topology    TopologySource

	// topologyHistory backs GET /api/v1/topology?at= ONLY.
	topologyHistory TopologyHistory

	// Same rule as targets: nil = database.mode=disabled = 503, never an in-memory fallback.
	mtr         MTRService
	annotations AnnotationService

	// incidents, maintenance and webhooks back the three; same rule as targets: nil =
	// database.mode=disabled = 503, never an in-memory fallback.
	incidents   IncidentService
	maintenance MaintenanceService
	webhooks    WebhookService
	k8sEvents   K8sEventService

	// alertRules backs the console-managed Prometheus alert rules.
	alertRules AlertRuleService

	// ruleSync is the PrometheusRule reconciler.
	ruleSync RuleSyncer

	// webhookSealer encrypts a plaintext endpoint secret on its way to the store; BOTH are implemented
	// by the dispatcher package.
	webhookSealer SecretSealer
	webhookTester TestDispatcher

	// notifier is the outbound half of the same dispatcher.
	notifier IncidentNotifier

	// enricher OVERRIDES the enrichment half of s.mtr. nil; the seam is EnrichmentReader either way --
	// the resolver satisfies the read-only interface deliberately.
	enricher EnrichmentReader

	// promReg backs both /metrics endpoints: the one on the API router and the one on the metrics
	// listener Run starts.
	promReg *prometheus.Registry

	// kv is the short-TTL key/value store the fixed-window rate limiter counts in (ratelimit.go).
	kv cache.KV

	// kvBus is the pub/sub side of the same server; the ONLY thing this package publishes on it is
	// the RBAC-change kick (see RBACChangedTopic). nil is the single-replica case and a no-op.
	kvBus cache.Bus

	// rateLimitWarnOnce keeps a KV outage to ONE warning per process; every request during the outage
	// increments RateLimitFailOpen (that is the alertable signal).
	rateLimitWarnOnce sync.Once

	// auditDropped counts the audit rows recordAudit had to throw away because the drain could not
	// keep up; it duplicates metrics.AuditDropped on purpose.
	auditDropped atomic.Int64
}

// Deps is everything NewServer needs.
type Deps struct {
	Config       *config.Config
	Metrics      *metrics.Metrics
	PromRegistry *prometheus.Registry
	UI           http.Handler

	Controller *controllerclient.Client
	Prometheus *promql.Client
	Hub        *ws.Hub
	Realtime   RealtimeStatus
	// Events is EventLister — an interface, not a concrete store type; assigning a nil *store.DB (or
	// any nil concrete pointer) here instead would produce a typed-nil interface that compares != nil.
	Events EventLister

	// Every field below is optional, same convention as Controller/Prometheus/Hub/Realtime/Events
	// above.
	Authenticator authn.Authenticator
	// Policy answers permission questions. nil defaults to authz.NewPolicy(nil): the four built-in
	// roles, no custom roles.
	Policy *authz.Policy
	// Roles maps a Subject's identity+groups to role names via
	// role_bindings. nil = built-in roles only (database.mode=disabled):
	// every non-anonymous subject falls back to auth.defaultRole.
	Roles RoleResolver
	// Sessions persists login sessions (POST /api/v1/auth/login, GET /api/v1/auth/oidc/callback) and
	// deletes them on logout.
	Sessions *authn.SessionStore
	// Users backs POST /api/v1/auth/login's password verification
	// (auth.mode=local only). nil means that endpoint answers 503 when
	// local mode is otherwise configured.
	Users authn.UserStore
	// OIDC backs GET /api/v1/auth/oidc/start and .../callback (auth.mode=oidc only) with the
	// AuthorizeURL/Callback methods that are not part of the Authenticator interface.
	OIDC OIDCFlow

	// Audit backs the audit middleware (write) and GET /api/v1/audit (read).
	Audit Auditor
	// RBAC backs GET/POST/DELETE /api/v1/rbac/roles[/{name}] and .../bindings[/{id}].
	RBAC RoleAdmin
	// Tokens backs GET/POST/DELETE /api/v1/tokens[/{id}]. nil means 503.
	Tokens TokenAdmin

	// Runner backs POST/GET /api/v1/runs and GET /api/v1/runs/{id}.
	Runner RunService

	// Targets backs CRUD /api/v1/targets. nil means database.mode=disabled and all five routes answer
	// 503.
	Targets TargetService

	// Definitions backs CRUD /api/v1/checks and Schedules backs CRUD /api/v1/schedules.
	Definitions DefinitionService
	Schedules   ScheduleService

	// MTR backs the three GET /api/v1/mtr/* routes (path history + the hop enrichment cache read) and
	// Annotations backs GET/POST/DELETE /api/v1/annotations; same rule as Targets: nil means
	// database.mode=disabled and every one of those routes answers 503. Typed to the local
	// MTRService/AnnotationService interfaces.
	MTR         MTRService
	Annotations AnnotationService

	// Incidents, Maintenance and Webhooks back the three; same rule as Targets: nil means
	// database.mode=disabled and every one of those routes answers 503. Typed to the local service
	// interfaces.
	Incidents   IncidentService
	Maintenance MaintenanceService
	Webhooks    WebhookService
	K8sEvents   K8sEventService

	// AlertRules backs the alert_rules table for both the /api/v1/alert-rules CRUD routes and the
	// configuration export/import pair; same rule as Targets otherwise: nil means
	// database.mode=disabled.
	AlertRules AlertRuleService

	// RuleSync is the PrometheusRule reconciler, used by POST /api/v1/alert-rules/{id}/sync and GET
	// /api/v1/alert-rules/foreign.
	RuleSync RuleSyncer

	// WebhookSealer seals a plaintext endpoint secret and WebhookTestDispatcher enqueues the /test
	// ping; in production both are the dispatcher, built only when console.webhooks.encryptionKey is
	// configured.
	WebhookSealer         SecretSealer
	WebhookTestDispatcher TestDispatcher

	// IncidentNotifier is the THIRD face of the same dispatcher, and the one that makes webhooks do
	// anything on their own.
	IncidentNotifier IncidentNotifier

	// Enricher swaps the cache-only hop enrichment read for a resolving one.
	Enricher EnrichmentReader

	// Bus carries the RBAC-change kick to the other replicas; nil is a no-op (see RBACChangedTopic).
	Bus cache.Bus

	// KV backs the fixed-window rate limiter (ratelimit.go); in production this is the very same
	// cache.KV cmd/console builds for Sessions and the OIDC state stash.
	KV cache.KV

	// Topology is the snapshot source the projection guard resolves a definition's agent selection
	// against.
	Topology TopologySource

	// TopologyHistory folds topology_events into the node/agent set as of an instant; typed to the
	// local TopologyHistory interface.
	TopologyHistory TopologyHistory
}

// NewServer wires the router, middleware, and routes from d.
func NewServer(d Deps) *Server { //nolint:gocritic // hugeParam: Deps is the pinned public signature (task-5-brief.md: "func NewServer(d Deps) *Server"), value semantics intentional
	authenticator := d.Authenticator
	if authenticator == nil {
		// Defensive default for an incomplete composition, never a bypass: see Deps.Authenticator's doc
		// comment.
		authenticator = authn.NewAnonymous("viewer")
	}
	policy := d.Policy
	if policy == nil {
		policy = authz.NewPolicy(nil)
	}

	s := &Server{
		cfg: d.Config, trustedProxies: parseCIDRs(d.Config.Auth.Header.TrustedProxyCIDRs),
		metrics: d.Metrics, ctrl: d.Controller, prom: d.Prometheus,
		hub: d.Hub, realtime: d.Realtime, events: d.Events, promReg: d.PromRegistry, kvBus: d.Bus,
		authenticator: authenticator, policy: policy, roles: d.Roles, sessions: d.Sessions,
		users: d.Users, oidc: d.OIDC,
		audit: d.Audit, roleAdmin: d.RBAC, tokens: d.Tokens,
		runner: d.Runner, targets: d.Targets,
		definitions: d.Definitions, schedules: d.Schedules, topology: d.Topology,
		topologyHistory: d.TopologyHistory,
		mtr:             d.MTR, annotations: d.Annotations, enricher: d.Enricher,
		incidents: d.Incidents, maintenance: d.Maintenance, webhooks: d.Webhooks, k8sEvents: d.K8sEvents,
		alertRules: d.AlertRules, ruleSync: d.RuleSync,
		webhookSealer: d.WebhookSealer, webhookTester: d.WebhookTestDispatcher,
		notifier: d.IncidentNotifier,
		kv:       d.KV,
	}

	// A configured rate limit with no KV wired would be a security control that reports itself as on
	// and enforces nothing.
	if s.kv == nil && d.Config != nil &&
		(d.Config.RateLimit.RunsPerMinute > 0 || d.Config.RateLimit.LoginPerMinute > 0) {
		s.kv = cache.NewInProcessKV()
	}

	// The projection guard reads the same topology GET /api/v1/topology serves.
	if s.topology == nil && d.Controller != nil {
		s.topology = d.Controller
	}

	// The audit drain goroutine (runAuditDrain, audit.go) is started here, exactly once, only when a
	// real Auditor is wired.
	if s.audit != nil {
		s.auditCh = make(chan auditJob, auditBufferSize)
		go s.runAuditDrain()
	}

	r := chi.NewRouter()
	// Order is load-bearing. instrument stays OUTERMOST so a panic-born 500 is still counted with its
	// route pattern (the recoverer writes the status through instrument's own statusRecorder).
	r.Use(s.instrument, s.recoverer, limitBody, rejectControlPath)

	// Never authenticated: kubelet probes and the Prometheus scrape would fail every pod if these
	// required credentials.
	r.Get("/healthz", s.handleHealthz)
	r.Get("/readyz", s.handleReadyz)
	r.Handle("/metrics", promhttp.HandlerFor(d.PromRegistry, promhttp.HandlerOpts{}))

	r.Group(func(api chi.Router) {
		// authenticate always runs first and only enriches the request context (never wraps w -- see its
		// own doc comment for why that matters for /ws's Hijacker).
		api.Use(s.authenticate, s.authorize)

		api.Get("/api/v1/version", s.handleVersion)
		api.Get("/api/v1/config", s.handleConfig)
		api.Get("/api/v1/topology", s.handleTopology)
		api.Get("/api/v1/matrix", s.handleMatrix)
		api.Get("/api/v1/events", s.handleEvents)
		api.Post("/api/v1/promql/query", s.handlePromQLQuery)
		api.Post("/api/v1/promql/query_range", s.handlePromQLQueryRange)

		api.Get("/api/v1/audit", s.handleAudit)

		api.Post("/api/v1/runs", s.handleRunsCreate)
		api.Post("/api/v1/runs/zone-pair", s.handleRunsZonePairCreate)
		api.Get("/api/v1/runs", s.handleRunsList)
		api.Get("/api/v1/runs/{id}", s.handleRunsGet)
		api.Post("/api/v1/runs/{id}/cancel", s.handleRunsCancel)

		api.Get("/api/v1/targets", s.handleTargetsList)
		api.Post("/api/v1/targets", s.handleTargetsCreate)
		api.Get("/api/v1/targets/{id}", s.handleTargetsGet)
		api.Put("/api/v1/targets/{id}", s.handleTargetsUpdate)
		api.Delete("/api/v1/targets/{id}", s.handleTargetsDelete)

		// /api/v1/checks/projection is registered BEFORE the {id} routes for readability only.
		api.Get("/api/v1/checks", s.handleChecksList)
		api.Post("/api/v1/checks", s.handleChecksCreate)
		api.Post("/api/v1/checks/projection", s.handleChecksProjection)
		api.Get("/api/v1/checks/{id}", s.handleChecksGet)
		api.Put("/api/v1/checks/{id}", s.handleChecksUpdate)
		api.Delete("/api/v1/checks/{id}", s.handleChecksDelete)

		api.Get("/api/v1/schedules", s.handleSchedulesList)
		api.Post("/api/v1/schedules", s.handleSchedulesCreate)
		api.Get("/api/v1/schedules/{id}", s.handleSchedulesGet)
		api.Put("/api/v1/schedules/{id}", s.handleSchedulesUpdate)
		api.Delete("/api/v1/schedules/{id}", s.handleSchedulesDelete)

		// MTR path history is READ-ONLY over HTTP: snapshots are written by the checks runner's projector
		// at result-ingest time.
		api.Get("/api/v1/mtr/destinations", s.handleMTRDestinations)
		api.Get("/api/v1/mtr/snapshots", s.handleMTRSnapshots)
		api.Get("/api/v1/mtr/snapshots/{id}", s.handleMTRSnapshotGet)
		api.Get("/api/v1/mtr/snapshots/{id}/traces", s.handleMTRSnapshotTraces)

		// Annotations are create/list/delete and deliberately have no update:
		// an annotation is a mark, not a document (M5 Decision 10).
		api.Get("/api/v1/annotations", s.handleAnnotationsList)
		api.Post("/api/v1/annotations", s.handleAnnotationsCreate)
		api.Delete("/api/v1/annotations/{id}", s.handleAnnotationsDelete)

		// Incidents are the ONE resource in this API with a PATCH, and the one exception to the repo's
		// full-replace PUT convention.
		api.Get("/api/v1/incidents", s.handleIncidentsList)
		api.Post("/api/v1/incidents", s.handleIncidentsCreate)
		api.Get("/api/v1/incidents/{id}", s.handleIncidentsGet)
		api.Patch("/api/v1/incidents/{id}", s.handleIncidentsUpdate)
		api.Delete("/api/v1/incidents/{id}", s.handleIncidentsDelete)

		// Maintenance windows are create/list/delete and deliberately have no
		// update: a window is two timestamps and a reason, so delete and
		// recreate is both the correction path and the whole of it.
		api.Get("/api/v1/maintenance", s.handleMaintenanceList)
		api.Post("/api/v1/maintenance", s.handleMaintenanceCreate)
		api.Delete("/api/v1/maintenance/{id}", s.handleMaintenanceDelete)

		// Webhooks are full CRUD plus a test ping. /api/v1/webhooks/{id}/test
		// is registered after the {id} routes for readability only -- chi
		// matches the longer pattern regardless of registration order.
		api.Get("/api/v1/webhooks", s.handleWebhooksList)
		api.Post("/api/v1/webhooks", s.handleWebhooksCreate)
		api.Get("/api/v1/webhooks/{id}", s.handleWebhooksGet)
		api.Put("/api/v1/webhooks/{id}", s.handleWebhooksUpdate)
		api.Delete("/api/v1/webhooks/{id}", s.handleWebhooksDelete)
		api.Post("/api/v1/webhooks/{id}/test", s.handleWebhooksTest)

		// Console-managed Prometheus alert rules.
		api.Get("/api/v1/alert-rules", s.handleAlertRulesList)
		api.Post("/api/v1/alert-rules", s.handleAlertRulesCreate)
		api.Get("/api/v1/alert-rules/foreign", s.handleAlertRulesForeign)
		api.Post("/api/v1/alert-rules/import", s.handleAlertRulesImport)
		api.Post("/api/v1/alert-rules/preview", s.handleAlertRulesPreview)
		api.Get("/api/v1/alert-rules/{id}", s.handleAlertRulesGet)
		api.Put("/api/v1/alert-rules/{id}", s.handleAlertRulesUpdate)
		api.Delete("/api/v1/alert-rules/{id}", s.handleAlertRulesDelete)
		api.Post("/api/v1/alert-rules/{id}/sync", s.handleAlertRulesSync)

		// Read from Prometheus, never evaluated here, and it is NOT under /alert-rules: an alert is an
		// observation.
		api.Get("/api/v1/alerts", s.handleAlerts)

		// Captured Kubernetes events are READ-ONLY over HTTP.
		api.Get("/api/v1/k8s-events", s.handleK8sEvents)

		// Configuration export/import; two routes, one admin-only permission, and no {id} anywhere.
		api.Get("/api/v1/export", s.handleExport)
		api.Post("/api/v1/import", s.handleImport)

		api.Get("/api/v1/rbac/permissions", s.handleRBACPermissions)
		api.Get("/api/v1/rbac/roles", s.handleRBACRolesList)
		api.Post("/api/v1/rbac/roles", s.handleRBACRolesCreate)
		api.Delete("/api/v1/rbac/roles/{name}", s.handleRBACRolesDelete)
		api.Get("/api/v1/rbac/bindings", s.handleRBACBindingsList)
		api.Post("/api/v1/rbac/bindings", s.handleRBACBindingsCreate)
		api.Delete("/api/v1/rbac/bindings/{id}", s.handleRBACBindingsDelete)

		api.Get("/api/v1/tokens", s.handleTokensList)
		api.Post("/api/v1/tokens", s.handleTokensCreate)
		api.Delete("/api/v1/tokens/{id}", s.handleTokensDelete)

		api.Get("/api/v1/auth/me", s.handleAuthMe)
		api.Post("/api/v1/auth/login", s.handleAuthLogin)
		api.Post("/api/v1/auth/logout", s.handleAuthLogout)
		api.Get("/api/v1/auth/oidc/start", s.handleOIDCStart)
		api.Get(config.OIDCCallbackPath, s.handleOIDCCallback)

		// /ws is TOP LEVEL, not under /api/v1 — a protocol upgrade, not a REST resource.
		api.Get("/ws", s.handleWS)
	})

	// An unknown route UNDER /api is a 404.
	r.Route("/api", func(api chi.Router) {
		api.NotFound(func(w http.ResponseWriter, _ *http.Request) {
			writeProblem(w, http.StatusNotFound, "no such API route",
				"this path is not part of the console API; see docs/console-api.yaml for the routes it serves")
		})
	})

	// SPA + static assets: everything not matched above.
	r.NotFound(d.UI.ServeHTTP)

	s.router = r
	d.Metrics.BuildInfo.WithLabelValues(appconfig.Version, appconfig.Commit).Set(1)
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
		/* IdleTimeout bounds a KEPT-ALIVE connection between requests. Without it a client could
		   hold a connection open forever having asked for nothing, and n such connections cost n
		   goroutines and n sockets with no request to show for them.
		   There is deliberately no ReadTimeout: it would apply to /ws as well and would tear down
		   every WebSocket at the deadline. The body phase is bounded per request instead — see
		   limitBody, which arms a read deadline for EVERY request; handleWS clears it once the
		   router, not the client's headers, has established that the request is the upgrade. */
		IdleTimeout: 120 * time.Second,
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", ":"+strconv.Itoa(s.cfg.HTTPPort))
	if err != nil {
		return fmt.Errorf("listen on :%d: %w", s.cfg.HTTPPort, err)
	}

	/* The METRICS listener, on a port of its own.

	   /metrics is still served on the API port for anyone scraping it directly, but nothing in the
	   chart opens that port to a scraper any more: a NetworkPolicy cannot say "this port, but only
	   these paths", so a scrape rule on the API port handed the console's whole API to everything
	   sharing the scraper's namespace — precisely undoing the narrowing
	   console.networkPolicy.ingressFrom exists to express. Same split, same reasoning and the same
	   helper as the agent and the controller — internal/metrics, imported as metricslistener because
	   this package's own `metrics` is the console's metric set. */
	metricsSrv := metricslistener.NewListener(
		fmt.Sprintf(":%d", s.cfg.MetricsPort),
		metricslistener.NewListenerHandler(s.promReg, s.ready.Load),
	)
	metricsLn, err := lc.Listen(ctx, "tcp", fmt.Sprintf(":%d", s.cfg.MetricsPort))
	if err != nil {
		return fmt.Errorf("listen on :%d: %w", s.cfg.MetricsPort, err)
	}

	errCh := make(chan error, 2)
	go func() {
		slog.Info("console http server listening", "addr", ln.Addr().String())
		errCh <- srv.Serve(ln)
	}()
	go func() {
		slog.Info("console metrics listening", "addr", metricsLn.Addr().String())
		errCh <- metricsSrv.Serve(metricsLn)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = metricsSrv.Shutdown(shutdownCtx)
		err := srv.Shutdown(shutdownCtx)
		// Shutdown has already drained the in-flight requests, so nothing new
		// can be enqueued by the time this runs -- whatever is still in the
		// buffer is the last of it.
		s.flushAudit(shutdownCtx)
		return err
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

// capabilities is computed per request from live pipeline health, never from config.
func (s *Server) capabilities() []string {
	caps := make([]string, 0, 1)
	if s.realtime != nil && s.realtime.Healthy() {
		caps = append(caps, capabilityEvents)
	}
	return caps
}

// handleWS upgrades to the multiplexed WebSocket protocol; a nil hub answers 503 with an RFC 7807
// body.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	if s.hub == nil {
		writeProblem(w, http.StatusServiceUnavailable, "realtime not available",
			"this console instance has no websocket hub wired")
		return
	}
	/* The router has now decided this really is /ws, so the body-phase deadline limitBody arms for
	   every request comes off. Doing it here rather than in the middleware is the point: the
	   middleware cannot tell an upgrade from a request that merely claims to be one. */
	clearBodyReadDeadline(w)

	subject, _ := SubjectFrom(r.Context())
	// One cell, shared by the topic gate and the revalidator, so both answer from the SAME, current
	// roles rather than from two copies of the upgrade-time snapshot.
	current := &atomic.Pointer[authz.Subject]{}
	current.Store(&subject)
	s.hub.ServeWSWithOptions(w, r, ws.ConnOptions{
		Authorize: s.wsTopicAuthorizer(current),
		/* The upgrade's answer does not stand forever: a revoked token or a deleted role binding
		   used to leave the socket streaming until the browser closed it. See wsRevalidator. */
		Revalidate: s.wsRevalidator(r, current),
	})
}

// authLoginPath returns the endpoint the frontend should navigate to (GET, full-page navigation for
// oidc; the login form POSTs to it for local) to start mode's login flow.
func authLoginPath(mode string) string {
	switch mode {
	case "local":
		return "/api/v1/auth/login"
	case "oidc":
		return "/api/v1/auth/oidc/start"
	default:
		return ""
	}
}

func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"auth": map[string]string{
			"mode":      s.cfg.Auth.Mode,
			"role":      s.cfg.Auth.Anonymous.Role,
			"loginPath": authLoginPath(s.cfg.Auth.Mode),
		},
		"anonymousBanner": s.cfg.Auth.Mode == "anonymous",
		"controller":      map[string]bool{"configured": s.ctrl != nil},
		"prometheus":      map[string]bool{"configured": s.prom != nil},
		// s.events is non-nil exactly when cmd/console wired a database (Deps.Events is assigned only
		// inside its own "if db != nil" branch).
		"database": map[string]any{
			"configured": s.events != nil,
			// Told to the browser so Settings/About can print real numbers; meaningful only with a
			// database, and 0 means pruning is off.
			"retentionDays": s.cfg.Database.RetentionDays,
		},
		// Schedules can be created and stored while the loop that fires them is off — the UI needs
		// this flag to say so instead of letting them silently never run.
		"scheduler": map[string]bool{"enabled": s.cfg.Scheduler.Enabled},
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

// Unwrap exposes the ResponseWriter this recorder wraps, which is what lets a WebSocket upgrade
// take the connection over.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// panicRecorder is the ResponseWriter wrapper s.recoverer installs so the panic path can tell
// whether the handler already committed a status line before it blew up.
type panicRecorder struct {
	http.ResponseWriter
	wrote bool
}

func (r *panicRecorder) WriteHeader(code int) {
	r.wrote = true
	r.ResponseWriter.WriteHeader(code)
}

// Write marks the response committed too: a handler that writes a body
// without an explicit WriteHeader has still sent 200 and its headers.
func (r *panicRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b) //nolint:wrapcheck // pass-through wrapper, wrapping would corrupt the io.Writer contract
}

func (r *panicRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

/*
maxRequestBodyBytes caps every request body the console will read.

encoding/json buffers the whole top-level value before it unmarshals, so the field schema decides
nothing about memory: one ~200 MB POST to any route -- including the PUBLIC login route, which
decodes before it checks a credential -- took the pod past its 256Mi limit and the kubelet killed it,
along with every in-flight request and every WebSocket session on that replica. Repeatable at will,
which makes it a crash loop rather than an incident.

16 MiB is far above any real import (the largest legitimate body is a rules/targets export) and far
below the container's limit. The socket already caps a frame at 4 KiB and the PromQL proxy caps a
RESPONSE at 8 MiB; this is the request side of the same idea.
*/
const maxRequestBodyBytes = 16 << 20

// bodyReadTimeout bounds the BODY phase of one request. Generous next to any real client and finite
// next to a client that sends its headers and then stops.
const bodyReadTimeout = 30 * time.Second

/*
 * limitBody makes an oversized body an ERROR the decoder reports rather than memory the process
 * commits: http.MaxBytesReader fails the read at the ceiling, so every handler's existing decode
 * error path turns it into a 400 without knowing this exists.
 *
 * It also bounds the body in TIME. ReadHeaderTimeout is satisfied the moment the headers land, and
 * MaxBytesReader bounds size but not duration, so a client that announced a Content-Length and then
 * sent nothing parked a handler goroutine for as long as it liked. A server-wide ReadTimeout cannot
 * be used here because it would also apply to /ws and cut every WebSocket at the deadline; the
 * deadline is therefore set per request, and skipped for upgrades.
 */
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		}
		/* UNCONDITIONAL. The deadline used to be skipped for anything that LOOKED like a protocol
		   upgrade, and "looked like" meant two request headers — so `Connection: upgrade` on a plain
		   POST to a public route removed the only bound on the body phase. There is no ReadTimeout on
		   the server (it would cut every WebSocket), and IdleTimeout only covers a connection between
		   requests, so such a request held its goroutine and its socket for as long as the client
		   cared to keep the connection open, with no credentials and a couple of hundred bytes each.

		   The real upgrade clears this deadline itself, from inside handleWS, once the ROUTER has
		   decided the request is /ws. That decision is the server's; a header is the client's.

		   Best effort: a ResponseWriter that cannot set deadlines (httptest's, a wrapper) simply
		   leaves the phase unbounded, exactly as before. */
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(bodyReadTimeout))
		next.ServeHTTP(w, r)
	})
}

/*
rejectControlPath refuses a request whose PATH carries a control character, before routing.

net/http hands the handler a percent-DECODED URL.Path, so `%00` in a URL arrives as a literal NUL
byte in a path parameter — and PostgreSQL cannot store one in a text column. Every route that put a
path parameter anywhere near the database therefore answered 502 "<subsystem> unavailable" for it:
DELETE /api/v1/rbac/roles/{name}, DELETE /api/v1/tokens/{id} and their siblings told the operator
their RBAC store or token store was DOWN, and wrote an ERROR log line, in response to a client's own
typo-shaped input. The audit row for the same request was lost outright (auditResource).

One check at the door instead of eleven downstream. A control character in a path is never
legitimate — these are UUIDs and names — so 400 is the whole answer, and it is given before any
handler, any store call or any authorization decision runs.

Query parameters have had this guard for a while (rejectControlChars); this is the path's.
*/
func rejectControlPath(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.IndexFunc(r.URL.Path, unicode.IsControl) >= 0 {
			writeProblem(w, http.StatusBadRequest, "invalid path",
				"the request path contains a control character")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clearBodyReadDeadline lifts the deadline limitBody armed. Only the WebSocket handler calls it, and
// only after the router has routed the request there: an upgrade's whole point is to stay open.
func clearBodyReadDeadline(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
}

// recoverer turns a panic from any inner middleware or handler into a 500 problem+json AND one
// audit row with outcome "error"; the subject comes from the holder authenticate fills in, not from
// SubjectFrom(r.Context).
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		holder := &subjectHolder{}
		r = r.WithContext(contextWithSubjectHolder(r.Context(), holder))
		rec := &panicRecorder{ResponseWriter: w}

		defer func() {
			rv := recover()
			if rv == nil {
				return
			}
			if err, ok := rv.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(rv)
			}

			slog.Error("httpapi: recovered from panic in handler", //nolint:gosec // G706: structured slog fields, not string-built log injection
				"method", r.Method, "pattern", chi.RouteContext(r.Context()).RoutePattern(),
				"panic", fmt.Sprint(rv), "stack", string(debug.Stack()))
			s.recordAudit(r, holder.subject, auditOutcomeError, nil)

			if !rec.wrote {
				// The panic value never reaches the client: it routinely
				// carries internal state, and a 500 has nothing actionable to
				// say beyond its own status.
				writeProblem(rec, http.StatusInternalServerError, "internal error", "")
			}
		}()

		next.ServeHTTP(rec, r)
	})
}

// flushAudit gives the audit drain a bounded window to write whatever is still queued at shutdown;
// it does not close s.auditCh -- the drain keeps the never-stopped.
func (s *Server) flushAudit(ctx context.Context) {
	if s.audit == nil {
		return
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for len(s.auditCh) > 0 {
		select {
		case <-ctx.Done():
			slog.Warn("httpapi: audit flush timed out at shutdown",
				"queued", len(s.auditCh), "dropped", s.auditDropped.Load())
			return
		case <-ticker.C:
		}
	}
	slog.Info("httpapi: audit drain flushed at shutdown", "dropped", s.auditDropped.Load())
}

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

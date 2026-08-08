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
	"sync"
	"sync/atomic"
	"time"

	appconfig "github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
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

// OIDCFlow is the subset of *authn.OIDCAuthenticator's surface
// handleOIDCStart/handleOIDCCallback actually need: mint the authorization
// redirect, and exchange a callback's state+code for a session. A small
// local interface owned by this package (I-4 review carry-forward), not
// *authn.OIDCAuthenticator itself — Deps.OIDC and Server.oidc are typed to
// THIS, so a test double can satisfy it without dragging in provider
// discovery, a KV, or a SessionStore, and this package's contract with
// authn no longer grows every time OIDCAuthenticator grows a new exported
// method for some other caller. Method signatures match oidc.go verbatim
// (checked against AuthorizeURL/Callback there), so *authn.OIDCAuthenticator
// satisfies this today with no adapter — asserted at compile time below
// (httpapi already imports authn for the session-store and cookie types).
type OIDCFlow interface {
	AuthorizeURL(ctx context.Context, returnTo string) (authURL string, err error)
	Callback(ctx context.Context, state, code string) (sessionID, returnTo string, err error)
}

var _ OIDCFlow = (*authn.OIDCAuthenticator)(nil)

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
	events   EventLister

	// Auth (M3 Phase B). authenticator and policy are never nil (NewServer
	// defaults both); roles, sessions, users and oidc may all be nil --
	// see Deps' doc comments for what each nil means.
	authenticator authn.Authenticator
	policy        *authz.Policy
	roles         RoleResolver
	sessions      *authn.SessionStore
	users         authn.UserStore
	oidc          OIDCFlow

	// Audit (M3 Phase C, Task 17). audit is the async audit-log writer/reader
	// (nil = database.mode=disabled -- the audit middleware is then a
	// complete no-op and GET /api/v1/audit answers 503). auditCh is the
	// small buffered channel recordAudit sends to and runAuditDrain (the one
	// drain goroutine, started by NewServer) reads from; it stays nil
	// alongside audit and is never closed. roleAdmin backs the RBAC admin
	// API (GET /api/v1/rbac/permissions is the one exception -- it needs
	// neither); tokens backs the tokens admin API. Both nil = 503, same
	// convention as every other optional Deps field.
	audit     Auditor
	auditCh   chan auditJob
	roleAdmin RoleAdmin
	tokens    TokenAdmin

	// runner backs POST/GET /api/v1/runs and GET /api/v1/runs/{id} (Task
	// 23). nil means cmd/console never configured a controller -- there is
	// no meaningful run path without one -- and all three routes answer 503,
	// the same convention every other optional dependency in this struct
	// follows.
	runner RunService

	// targets backs CRUD /api/v1/targets (M4 Task 3). nil means
	// database.mode=disabled and all five routes answer 503 -- targets are
	// CONFIGURATION and get NO in-memory fallback (Decision 13), unlike
	// runner.
	targets TargetService

	// definitions and schedules back CRUD /api/v1/checks and
	// /api/v1/schedules (M4 Task 4) under the same Decision 13 rule as
	// targets: nil = database.mode=disabled = 503, never an in-memory
	// fallback. topology backs the projection guard -- nil means POST
	// /api/v1/checks/projection answers 503 and the create/update guard
	// fails open (enforceProjection, definitions.go).
	definitions DefinitionService
	schedules   ScheduleService
	topology    TopologySource

	// topologyHistory backs GET /api/v1/topology?at= ONLY (M5 Task 9). The
	// live, param-less topology stays controller-backed and is completely
	// unaffected by this being nil -- a console with database.mode=disabled
	// keeps serving live topology and answers 503 for ?at= alone, because a
	// reconstruction has nowhere to read its events from.
	topologyHistory TopologyHistory

	// mtr backs GET /api/v1/mtr/* and annotations backs GET/POST/DELETE
	// /api/v1/annotations (M5 Task 4). Same Decision 13 rule as targets: nil
	// = database.mode=disabled = 503, never an in-memory fallback -- path
	// history and operator notes are worth exactly as much as they are
	// durable.
	mtr         MTRService
	annotations AnnotationService

	// incidents, maintenance and webhooks back the three M6 CRUD resources
	// (M6 Task 4), and k8sEvents backs GET /api/v1/k8s-events. Same Decision
	// 13 rule as targets: nil = database.mode=disabled = 503, never an
	// in-memory fallback -- a saved investigation, a declared window and a
	// configured endpoint are worth exactly as much as they are durable.
	incidents   IncidentService
	maintenance MaintenanceService
	webhooks    WebhookService
	k8sEvents   K8sEventService

	// alertRules backs the console-managed Prometheus alert rules (M7 Task 1
	// store): the /api/v1/alert-rules CRUD routes and the configuration
	// export/import pair, which needs every config table at once. Same
	// Decision 13 rule as targets: nil = database.mode=disabled, and every
	// alert-rule route -- plus GET /api/v1/export and POST /api/v1/import,
	// which refuse to serve a bundle with a hole in it -- answers 503.
	alertRules AlertRuleService

	// ruleSync is the PrometheusRule reconciler (M7 Task 3), and it is the
	// ONE dependency in this struct whose absence is a 409 rather than a 503.
	// nil does not mean "the database is off": it means console.alerting.enabled
	// is false, or the console is not running in a cluster -- the rules are all
	// still there and still editable, and only the two routes that need the
	// cluster (sync, foreign) refuse. See alertingDisabledDetail for the full
	// reasoning.
	ruleSync RuleSyncer

	// webhookSealer encrypts a plaintext endpoint secret on its way to the
	// store, and webhookTester enqueues the /test ping. BOTH are implemented
	// by M6 Task 5's dispatcher package, which owns the AES-GCM key
	// (console.webhooks.encryptionKey); this package only ever holds the two
	// narrow interfaces. nil is not "database off" but "no key configured",
	// which Decision 4 makes a legitimate state -- so the operations that
	// need the cipher answer 503 naming the key, and every other webhook
	// route keeps working.
	webhookSealer SecretSealer
	webhookTester TestDispatcher

	// notifier is the outbound half of the same dispatcher: the incident
	// handlers hand it created/resolved/reopened and it decides who hears
	// about it. nil is the ordinary no-key state, and every incident route
	// behaves identically with or without it -- see IncidentNotifier.
	notifier IncidentNotifier

	// enricher OVERRIDES the enrichment half of s.mtr (M5 Task 5). nil is the
	// Task 4 behaviour, unchanged: hop enrichment is served cache-only,
	// straight out of mtr_hop_enrichment. Non-nil is an *enrich.Resolver,
	// which reads the same cache and additionally resolves what it does not
	// find. The seam is EnrichmentReader either way -- the resolver satisfies
	// the read-only interface deliberately, so the handler cannot write the
	// cache through it and never learns which one it is talking to.
	enricher EnrichmentReader

	// kv is the short-TTL key/value store the fixed-window rate limiter counts
	// in (ratelimit.go) -- the SAME cache.KV cmd/console already builds for
	// SessionStore and the OIDC state stash, so a Valkey-backed console gets
	// cluster-wide limits with no extra dependency and a
	// console.valkey.mode=disabled one gets per-replica limits (weaker, never
	// stronger). Never nil once NewServer has run: it falls back to an
	// in-process KV rather than leave a configured limit silently unenforced.
	kv cache.KV

	// rateLimitWarnOnce keeps a KV outage to ONE warning per process. Every
	// request during the outage increments RateLimitFailOpen (that is the
	// alertable signal); logging each one would turn a backend outage into a
	// log-volume incident.
	rateLimitWarnOnce sync.Once

	// auditDropped counts the audit rows recordAudit had to throw away
	// because the drain could not keep up, for the shutdown log flushAudit
	// writes. It duplicates metrics.AuditDropped on purpose: a pod that is
	// going away may never be scraped again, and the count then only exists
	// in its own logs.
	auditDropped atomic.Int64
}

// Deps is everything NewServer needs. Every field except Config, Metrics,
// PromRegistry and UI is OPTIONAL: a nil value means the feature is unset and
// the endpoints that need it answer 503 — the same convention NewServer's old
// doc comment stated for ctrl/prom/hub/realtime, now extended to Events.
// Replaces eight positional parameters that M3 would otherwise push to twelve.
type Deps struct {
	Config       *config.Config
	Metrics      *metrics.Metrics
	PromRegistry *prometheus.Registry
	UI           http.Handler

	Controller *controllerclient.Client
	Prometheus *promql.Client
	Hub        *ws.Hub
	Realtime   RealtimeStatus
	// Events is EventLister — an interface, not a concrete store type — so an
	// unset dependency is Deps{}'s ordinary zero value: a genuine nil
	// interface. Assigning a nil *store.DB (or any nil concrete pointer) here
	// instead would produce a typed-nil interface that compares != nil, and
	// handleEvents's "s.events == nil" gate below would then call ListEvents
	// on a nil receiver instead of answering 503. M3 Phase A: nil = GET
	// /api/v1/events answers 503.
	Events EventLister

	// Auth (M3 Phase B). Every field below is optional, same convention as
	// Controller/Prometheus/Hub/Realtime/Events above.
	//
	// Authenticator resolves a request into an authz.Subject. nil defaults
	// to a fixed anonymous "viewer" authenticator (authn.NewAnonymous) --
	// a defensive default for an incomplete composition, NEVER a bypass:
	// that Subject flows through the exact same authenticate+authorize
	// chain as a real one, at the safe read-only role.
	Authenticator authn.Authenticator
	// Policy answers permission questions. nil defaults to
	// authz.NewPolicy(nil): the four built-in roles, no custom roles --
	// correct with database.mode=disabled (Decision 7).
	Policy *authz.Policy
	// Roles maps a Subject's identity+groups to role names via
	// role_bindings. nil = built-in roles only (database.mode=disabled):
	// every non-anonymous subject falls back to auth.defaultRole.
	Roles RoleResolver
	// Sessions persists login sessions (POST /api/v1/auth/login,
	// GET /api/v1/auth/oidc/callback) and deletes them on logout. nil
	// means those two mutating paths answer 503 -- there is nowhere to
	// put or remove a session.
	Sessions *authn.SessionStore
	// Users backs POST /api/v1/auth/login's password verification
	// (auth.mode=local only). nil means that endpoint answers 503 when
	// local mode is otherwise configured.
	Users authn.UserStore
	// OIDC backs GET /api/v1/auth/oidc/start and .../callback
	// (auth.mode=oidc only) with the AuthorizeURL/Callback methods that
	// are not part of the Authenticator interface. Typed to the narrow
	// OIDCFlow interface above, not *authn.OIDCAuthenticator directly
	// (I-4). nil means those two endpoints answer 404, same as any other
	// mode.
	OIDC OIDCFlow

	// Audit backs the audit middleware (write) and GET /api/v1/audit
	// (read) -- Task 17. nil means database.mode=disabled: the audit
	// middleware becomes a complete no-op (recordAudit's own nil gate,
	// audit.go) and GET /api/v1/audit answers 503.
	Audit Auditor
	// RBAC backs GET/POST/DELETE /api/v1/rbac/roles[/{name}] and
	// .../bindings[/{id}]. nil means those endpoints answer 503; GET
	// /api/v1/rbac/permissions is unaffected -- it serves the static,
	// compiled-in authz.AllPermissions list and needs no store at all.
	RBAC RoleAdmin
	// Tokens backs GET/POST/DELETE /api/v1/tokens[/{id}]. nil means 503.
	Tokens TokenAdmin

	// Runner backs POST/GET /api/v1/runs and GET /api/v1/runs/{id} (Task
	// 23) -- in production a *checks.Runner, constructed by cmd/console only
	// when controller.url is configured (checks.NewMemoryStore() as its
	// Store when database.mode is disabled, Decision 15: runs still work
	// in-memory). nil means all three routes answer 503.
	Runner RunService

	// Targets backs CRUD /api/v1/targets. nil means database.mode=disabled
	// and all five routes answer 503 -- targets are CONFIGURATION and get NO
	// in-memory fallback (Plan Decision 13), unlike Runner.
	Targets TargetService

	// Definitions backs CRUD /api/v1/checks and Schedules backs CRUD
	// /api/v1/schedules. Same Decision 13 rule as Targets: nil means
	// database.mode=disabled and every one of those routes answers 503.
	Definitions DefinitionService
	Schedules   ScheduleService

	// MTR backs the three GET /api/v1/mtr/* routes (path history + the hop
	// enrichment cache read) and Annotations backs GET/POST/DELETE
	// /api/v1/annotations. Same Decision 13 rule as Targets: nil means
	// database.mode=disabled and every one of those routes answers 503.
	// Typed to the local MTRService/AnnotationService interfaces, so leaving
	// them unset is Deps{}'s ordinary zero value -- a genuine nil interface,
	// never the typed-nil trap Deps.Events' doc comment describes.
	MTR         MTRService
	Annotations AnnotationService

	// Incidents, Maintenance and Webhooks back the three M6 CRUD resources,
	// and K8sEvents backs GET /api/v1/k8s-events. Same Decision 13 rule as
	// Targets: nil means database.mode=disabled and every one of those routes
	// answers 503. Typed to the local service interfaces, so leaving them
	// unset is Deps{}'s ordinary zero value -- a genuine nil interface, never
	// the typed-nil trap Deps.Events' doc comment describes.
	Incidents   IncidentService
	Maintenance MaintenanceService
	Webhooks    WebhookService
	K8sEvents   K8sEventService

	// AlertRules backs the alert_rules table (M7 Task 1) for both the
	// /api/v1/alert-rules CRUD routes and the configuration export/import
	// pair, which reads and writes it alongside every other config table --
	// GET /api/v1/export refuses to answer at all unless this is wired, since
	// a bundle missing one collection because a seam happened to be nil is a
	// restore point with a hole in it. Same Decision 13 rule as Targets
	// otherwise: nil means database.mode=disabled.
	AlertRules AlertRuleService

	// RuleSync is the PrometheusRule reconciler (M7 Task 3), used by POST
	// /api/v1/alert-rules/{id}/sync and GET /api/v1/alert-rules/foreign, and
	// nudged after every alert-rule write so an operator does not wait out the
	// jittered 60s loop.
	//
	// nil is NOT this struct's usual "feature off, answer 503": it means
	// console.alerting.enabled is false or the console has no in-cluster
	// Kubernetes config, and the two routes that genuinely need the cluster
	// answer 409 naming the flag while everything else -- list, create, edit,
	// delete, preview -- keeps working against the database alone. cmd/console
	// builds it only in the same branch that spawns the reconcile loop, so
	// there is no state where this is wired and nothing is converging.
	RuleSync RuleSyncer

	// WebhookSealer seals a plaintext endpoint secret and WebhookTestDispatcher
	// enqueues the /test ping. In production both are M6 Task 5's dispatcher,
	// built only when console.webhooks.encryptionKey is configured -- the key
	// is OPTIONAL at boot (Decision 4), because a console that never declares
	// a webhook must not fail to start over a cipher it will never use.
	//
	// nil therefore does NOT mean "webhooks off": listing, reading, deleting,
	// disabling and re-pointing an endpoint all keep working. Only the two
	// operations that actually need the cipher -- creating an endpoint (its
	// secret must be sealed) and testing one (its secret must be unsealed to
	// sign the ping) -- answer 503, naming the key.
	WebhookSealer         SecretSealer
	WebhookTestDispatcher TestDispatcher

	// IncidentNotifier is the THIRD face of the same dispatcher, and the one
	// that makes webhooks do anything on their own: the incident handlers call
	// it after a successful create and after a status-CHANGING patch. It is
	// wired from the same construction as the two above, so a console with a
	// key notifies and a console without one does not -- there is no third
	// state where endpoints exist but nothing ever fires.
	//
	// Deliberately fire-and-forget (no error return): see IncidentNotifier.
	IncidentNotifier IncidentNotifier

	// Enricher swaps the cache-only hop enrichment read for a resolving one
	// (M5 Task 5). cmd/console builds an *enrich.Resolver here only when
	// mtr.enrichment.enabled is true AND a database is configured -- the TTL
	// cache lives in PostgreSQL, so a resolver without one would re-resolve
	// every hop on every read.
	//
	// nil is not "feature off, answer 503" like every other field above: it is
	// "serve what the cache already knows", which is exactly what Task 4
	// shipped. Enrichment is decoration on a trace and its absence must never
	// change a status code.
	Enricher EnrichmentReader

	// KV backs the fixed-window rate limiter (ratelimit.go). In production
	// this is the very same cache.KV cmd/console builds for Sessions and the
	// OIDC state stash -- Valkey-backed when console.valkey.mode=valkey, so
	// the limits are cluster-wide, and in-process otherwise. Left nil,
	// NewServer substitutes its OWN in-process KV whenever any limit is
	// configured: unlike every other optional field here, a nil KV must not
	// mean "feature off", because that would turn a forgotten wire-up into a
	// silently unenforced security control.
	KV cache.KV

	// Topology is the snapshot source the projection guard resolves a
	// definition's agent selection against (Decision 12). Left nil, NewServer
	// falls back to Controller when one is wired -- which is what production
	// gets, with no extra wiring in cmd/console -- so this field exists
	// chiefly so a test can pin a FIXED topology without a controller. nil
	// with no Controller either means POST /api/v1/checks/projection answers
	// 503 and the create/update guard fails open (see enforceProjection).
	Topology TopologySource

	// TopologyHistory folds topology_events into the node/agent set as of an
	// instant, for GET /api/v1/topology?at= (M5 Task 9). In production this is
	// the SAME store.EventStore Events carries, so a folded snapshot is built
	// from exactly the rows the Live page pages through. nil means
	// database.mode=disabled: ?at= answers 503 and the param-less live route
	// is untouched. Typed to the local TopologyHistory interface, so leaving
	// it unset is Deps{}'s ordinary zero value -- a genuine nil interface,
	// never the typed-nil trap Deps.Events' doc comment describes.
	TopologyHistory TopologyHistory
}

// NewServer wires the router, middleware, and routes from d. Controller,
// Prometheus, Hub, Realtime and Events may all be nil (feature unset); the
// endpoints that need them answer 503, and /api/v1/version simply advertises
// no capabilities.
func NewServer(d Deps) *Server { //nolint:gocritic // hugeParam: Deps is the pinned public signature (task-5-brief.md: "func NewServer(d Deps) *Server"), value semantics intentional
	authenticator := d.Authenticator
	if authenticator == nil {
		// Defensive default for an incomplete composition, never a bypass:
		// see Deps.Authenticator's doc comment. cmd/console always wires a
		// real Authenticator once Task 18 lands; this only fires for a
		// test, or today's cmd/console which does not wire auth yet.
		authenticator = authn.NewAnonymous("viewer")
	}
	policy := d.Policy
	if policy == nil {
		policy = authz.NewPolicy(nil)
	}

	s := &Server{
		cfg: d.Config, metrics: d.Metrics, ctrl: d.Controller, prom: d.Prometheus,
		hub: d.Hub, realtime: d.Realtime, events: d.Events,
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

	// A configured rate limit with no KV wired would be a security control
	// that reports itself as on and enforces nothing, so NewServer substitutes
	// an in-process one -- the same single-replica fallback cmd/console itself
	// uses when Valkey is disabled (ADR-002). Built only when a limit is
	// actually enabled, so the overwhelming majority of compositions (and
	// every test that leaves rateLimit at its zero value) start no sweeper
	// goroutine they would never use.
	if s.kv == nil && d.Config != nil &&
		(d.Config.RateLimit.RunsPerMinute > 0 || d.Config.RateLimit.LoginPerMinute > 0) {
		s.kv = cache.NewInProcessKV()
	}

	// The projection guard reads the same topology GET /api/v1/topology
	// serves, so a console with a controller configured needs no second
	// dependency wired for it. Assigned through the explicit nil check rather
	// than "d.Topology or d.Controller" in one expression because
	// *controllerclient.Client is a CONCRETE pointer: assigning a nil one
	// straight into the interface field would produce a typed-nil that
	// compares != nil, and projectDefinition's own nil gate would then call
	// Topology on a nil receiver instead of reporting errTopologyUnavailable
	// (the exact trap Deps.Events' doc comment documents).
	if s.topology == nil && d.Controller != nil {
		s.topology = d.Controller
	}

	// The audit drain goroutine (runAuditDrain, audit.go) is started here,
	// exactly once, only when a real Auditor is wired -- s.auditCh stays nil
	// (and recordAudit's own nil check on s.audit makes every send site a
	// no-op) for the database-disabled default. It is intentionally never
	// stopped: same fire-and-forget-for-the-life-of-the-process lifecycle as
	// every other realtime component cmd/console spawns, none of which have
	// an explicit Stop either.
	if s.audit != nil {
		s.auditCh = make(chan auditJob, auditBufferSize)
		go s.runAuditDrain()
	}

	r := chi.NewRouter()
	// Order is load-bearing. instrument stays OUTERMOST so a panic-born 500
	// is still counted with its route pattern (the recoverer writes the
	// status through instrument's own statusRecorder); recoverer sits
	// immediately inside it so every other middleware -- authenticate,
	// authorize, the audit wrapper -- and every handler is inside its
	// deferred recover. M3 follow-up #4: M4 roughly doubles this package's
	// mutating surface, and an unrecovered panic there would otherwise drop
	// the connection with no response and no audit row.
	r.Use(s.instrument, s.recoverer)

	// Never authenticated: kubelet probes and the Prometheus scrape would
	// fail every pod if these required credentials (task-16-brief.md route
	// table). Registered directly on r, outside the authenticated group
	// below, so SubjectFrom's ctx key is never even set for these three.
	r.Get("/healthz", s.handleHealthz)
	r.Get("/readyz", s.handleReadyz)
	r.Handle("/metrics", promhttp.HandlerFor(d.PromRegistry, promhttp.HandlerOpts{}))

	r.Group(func(api chi.Router) {
		// authenticate always runs first and only enriches the request
		// context (never wraps w -- see its own doc comment for why that
		// matters for /ws's Hijacker); authorize is the sole 401/403
		// decision point, driven by routeTable (middleware_auth.go).
		// instrument (above, wrapping the whole router) stays outermost,
		// so a 401/403 here is still counted with its route pattern.
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
		api.Get("/api/v1/runs", s.handleRunsList)
		api.Get("/api/v1/runs/{id}", s.handleRunsGet)
		api.Post("/api/v1/runs/{id}/cancel", s.handleRunsCancel)

		api.Get("/api/v1/targets", s.handleTargetsList)
		api.Post("/api/v1/targets", s.handleTargetsCreate)
		api.Get("/api/v1/targets/{id}", s.handleTargetsGet)
		api.Put("/api/v1/targets/{id}", s.handleTargetsUpdate)
		api.Delete("/api/v1/targets/{id}", s.handleTargetsDelete)

		// /api/v1/checks/projection is registered BEFORE the {id} routes for
		// readability only -- chi matches a static segment ahead of a
		// wildcard regardless of registration order, and there is no POST
		// /api/v1/checks/{id} for it to compete with in any case.
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

		// MTR path history is READ-ONLY over HTTP: snapshots are written by
		// the checks runner's projector (M5 Task 2) at result-ingest time, and
		// there is no operation a client could ask for that a trace does not
		// already produce. /api/v1/mtr/snapshots is registered before the
		// {id} route for readability only -- chi matches a static segment
		// ahead of a wildcard regardless of registration order.
		api.Get("/api/v1/mtr/destinations", s.handleMTRDestinations)
		api.Get("/api/v1/mtr/snapshots", s.handleMTRSnapshots)
		api.Get("/api/v1/mtr/snapshots/{id}", s.handleMTRSnapshotGet)

		// Annotations are create/list/delete and deliberately have no update:
		// an annotation is a mark, not a document (M5 Decision 10).
		api.Get("/api/v1/annotations", s.handleAnnotationsList)
		api.Post("/api/v1/annotations", s.handleAnnotationsCreate)
		api.Delete("/api/v1/annotations/{id}", s.handleAnnotationsDelete)

		// Incidents are the ONE resource in this API with a PATCH, and the one
		// exception to the repo's full-replace PUT convention: an incident
		// evolves under collaboration, so a full replace would let the last
		// writer discard a colleague's notes (handleIncidentsUpdate says why
		// at length).
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

		// Console-managed Prometheus alert rules (M7 Task 4). /foreign and
		// /preview are registered before the {id} routes for readability only
		// -- chi matches a static segment ahead of a wildcard regardless of
		// registration order, and neither competes with a same-method {id}
		// route in any case (there is no POST /api/v1/alert-rules/{id}).
		//
		// PUT, not PATCH: a rule is a definition one person edits in a form, so
		// a full replace is what makes "what you see is what is stored" true.
		// The incidents PATCH stays the ONE exception in this API.
		api.Get("/api/v1/alert-rules", s.handleAlertRulesList)
		api.Post("/api/v1/alert-rules", s.handleAlertRulesCreate)
		api.Get("/api/v1/alert-rules/foreign", s.handleAlertRulesForeign)
		api.Post("/api/v1/alert-rules/import", s.handleAlertRulesImport)
		api.Post("/api/v1/alert-rules/preview", s.handleAlertRulesPreview)
		api.Get("/api/v1/alert-rules/{id}", s.handleAlertRulesGet)
		api.Put("/api/v1/alert-rules/{id}", s.handleAlertRulesUpdate)
		api.Delete("/api/v1/alert-rules/{id}", s.handleAlertRulesDelete)
		api.Post("/api/v1/alert-rules/{id}/sync", s.handleAlertRulesSync)

		// The firing set (M7 Decision 6). Read from Prometheus, never
		// evaluated here, and it is NOT under /alert-rules: an alert is an
		// observation, and most of them may not belong to a rule this console
		// manages at all.
		api.Get("/api/v1/alerts", s.handleAlerts)

		// Captured Kubernetes events are READ-ONLY over HTTP: they are written
		// by internal/console/kubectx (M6 Task 2) from the cluster's own event
		// stream, and there is nothing a client could legitimately add.
		api.Get("/api/v1/k8s-events", s.handleK8sEvents)

		// Configuration export/import (M7 Task 6, Decision 9). Two routes, one
		// admin-only permission, and no {id} anywhere: the unit is the WHOLE
		// declarative configuration, not a row. The import is a POST rather
		// than a PUT even though it is idempotent under its natural keys --
		// it is a merge with a per-item result, not a replace of a named
		// resource, and there is no path it could replace.
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

		// /ws is TOP LEVEL, not under /api/v1 (docs/console/architecture/API.md,
		// ADR-003). One long-lived upgrade is recorded once by s.instrument as
		// path="/ws" when the socket closes — its "duration" is the connection
		// LIFETIME, so it lands in the histogram's top (+Inf) bucket and inflates
		// _sum by connection-seconds; don't read request latency from that series
		// (cardinality is unaffected — path is the route pattern). Live connection
		// count is the ws_clients gauge. chi.Router.Group adds no path prefix, so
		// registering it here (for authenticate+authorize) keeps it top-level.
		api.Get("/ws", s.handleWS)
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
//
// The socket carries a SECOND authorization decision beyond the route's own
// (M7, M3 follow-up #10): the upgrade admits events:read OR runs:read, and
// wsTopicAuthorizer (ws_authz.go) narrows what the runs:read-only half may
// then subscribe to. See that file for the whole rationale.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	if s.hub == nil {
		writeProblem(w, http.StatusServiceUnavailable, "realtime not available",
			"this console instance has no websocket hub wired")
		return
	}
	subject, _ := SubjectFrom(r.Context())
	s.hub.ServeWSAuthorized(w, r, s.wsTopicAuthorizer(subject))
}

// authLoginPath returns the endpoint the frontend should navigate to (GET,
// full-page navigation for oidc; the login form POSTs to it for local) to
// start mode's login flow, or "" when mode has none -- anonymous has no
// credentials to present, and header mode authenticates on every request
// via the trusted proxy, never through a browser-visible login step. Task
// 18: GET /api/v1/config advertises this so the frontend feature-detects
// the login flow instead of hardcoding auth.mode's four cases itself.
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
		// s.events is non-nil exactly when cmd/console wired a database (Deps.Events
		// is assigned only inside its own "if db != nil" branch), so it is the
		// same signal handleEvents' 503 gate uses — no separate database health
		// dependency, and readiness (SetReady) stays entirely unaffected by it.
		"database": map[string]bool{"configured": s.events != nil},
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

// panicRecorder is the ResponseWriter wrapper s.recoverer installs so the
// panic path can tell whether the handler already committed a status line
// before it blew up: writing a second header would be both useless and a
// "superfluous WriteHeader call" from net/http, and the partial body already
// on the wire cannot be taken back.
//
// Unwrap is NOT optional. gorilla's /ws upgrade hijacks the connection
// through http.NewResponseController, which finds the real http.Hijacker only
// by following Unwrap through every wrapper in the chain -- embedding an
// interface promotes that interface's methods and hides net/http's Hijacker.
// Without this method every /ws upgrade fails with "websocket: hijack:
// feature not supported". Same standing warning as statusRecorder's, and this
// wrapper sits in front of statusRecorder on the exact same path.
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

// recoverer turns a panic from any inner middleware or handler into a 500
// problem+json AND one audit row with outcome "error" -- M3 follow-up #4.
// chi's own middleware.Recoverer does the first half only; the audit row is
// the half that matters here, because a 500 nobody can attribute to a subject
// is the one class of failure the audit log exists to catch.
//
// The subject comes from the holder authenticate fills in, not from
// SubjectFrom(r.Context()): authenticate hands its subject DOWN on a child
// context, and this middleware only ever holds the original request, so the
// child context is unreachable from here (see subjectHolder, middleware_auth.go).
//
// http.ErrAbortHandler is re-panicked untouched, exactly as net/http and chi
// specify: it is the documented "abort this connection quietly" signal, not a
// bug, and swallowing it would turn every deliberate abort into a bogus 500
// plus a bogus audit row.
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

// flushAudit gives the audit drain a bounded window to write whatever is
// still queued at shutdown, then logs how many rows this process dropped in
// total. A no-op when no Auditor is wired (database.mode=disabled: there is
// no drain goroutine and s.auditCh is nil).
//
// It does not close s.auditCh -- the drain keeps the never-stopped,
// fire-and-forget lifecycle NewServer documents, and closing a channel other
// goroutines may still send on would panic. Waiting for the buffer to empty
// is therefore best-effort by construction: an entry the drain has already
// dequeued but not yet written is invisible here.
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

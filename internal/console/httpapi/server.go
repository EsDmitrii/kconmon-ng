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
	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
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
		runner: d.Runner,
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
	r.Use(s.instrument)

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

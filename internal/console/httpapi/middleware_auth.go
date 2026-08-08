package httpapi

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/config"
)

// subjectContextKey is the context.Value key the authenticate middleware
// stores the resolved authz.Subject under. An unexported struct type, not a
// string, so no other package can collide with or forge this key.
type subjectContextKey struct{}

// contextWithSubject returns a child of ctx carrying s, retrievable with
// SubjectFrom.
func contextWithSubject(ctx context.Context, s authz.Subject) context.Context { //nolint:gocritic // Subject is a value type by design (authz package doc)
	return context.WithValue(ctx, subjectContextKey{}, s)
}

// SubjectFrom returns the Subject the authenticate middleware stored on ctx.
// The second return is false only for requests that bypassed the middleware
// entirely (/healthz, /readyz, /metrics) -- handlers under /api/v1 (and /ws)
// can rely on it being true, even when no credentials were presented: in
// that case the Subject is the zero value (Kind == "").
func SubjectFrom(ctx context.Context) (authz.Subject, bool) {
	s, ok := ctx.Value(subjectContextKey{}).(authz.Subject)
	return s, ok
}

// subjectHolderKey is the context key the recoverer stores its subjectHolder
// under. Unexported struct type, same forgery-proof convention as
// subjectContextKey.
type subjectHolderKey struct{}

// subjectHolder is a one-slot mailbox s.recoverer installs on the request
// context BEFORE the auth chain runs and authenticate fills in the moment it
// has resolved a Subject.
//
// It exists because the two middlewares see different requests: authenticate
// passes its Subject DOWN on a child context (r.WithContext), while the
// recoverer -- sitting outside the whole chain so it can catch a panic from
// any of it -- only ever holds the ORIGINAL request, whose context will never
// contain that child value. Without this, every panic-path audit row would
// carry an empty subject, which is precisely the attribution the row exists
// to provide (M3 follow-up #4).
//
// No mutex: the write (authenticate) happens-before the read (the recoverer's
// deferred recover) on the request's own goroutine, ordered by the call stack
// unwinding. A handler that spawns goroutines never touches this value -- and
// a panic on one of those is not recoverable from here in any case.
type subjectHolder struct{ subject authz.Subject }

// contextWithSubjectHolder returns a child of ctx carrying h.
func contextWithSubjectHolder(ctx context.Context, h *subjectHolder) context.Context {
	return context.WithValue(ctx, subjectHolderKey{}, h)
}

// subjectHolderFrom returns the holder the recoverer installed, or nil for a
// request that never went through it (a router assembled by hand in a test).
func subjectHolderFrom(ctx context.Context) *subjectHolder {
	h, _ := ctx.Value(subjectHolderKey{}).(*subjectHolder)
	return h
}

// routeRule is the route->permission table's value: exactly one closed
// authz.Permission the caller must hold, or public == true meaning the
// route is reachable with no permission decision at all (the login flow
// itself, and the pre-login version/config probes), or -- for the ONE route
// that needs it -- anyOf, a closed set of which the caller must hold at
// least one.
//
// anyOf exists for GET /ws and should stay that way. An OR of permissions is
// a weaker, harder-to-audit statement than a single one ("what can this role
// reach" stops being a map lookup), and every other route in this API has a
// single permission that names what it does. /ws is the exception because the
// socket is not one resource: it multiplexes topics whose permissions genuinely
// differ, and the second, per-topic decision that makes the OR honest lives on
// the connection (wsTopicAuthorizer, ws_authz.go). A route reaching for anyOf
// WITHOUT such a second decision is almost certainly asking for a new
// permission instead.
//
// permission and anyOf are mutually exclusive; a public rule uses neither.
// TestRouteTableRulesAreWellFormed pins that no row sets both.
type routeRule struct {
	permission authz.Permission
	anyOf      []authz.Permission
	public     bool
}

// accepted returns, in table order, the permissions any ONE of which
// satisfies r: the single permission, or every member of anyOf. Nil for a
// public rule. It is the one place the two spellings are reconciled, so
// satisfiedBy, the metric label, the 403 detail and
// TestRoutePermissionTable's coverage can never disagree about what a row
// requires.
func (r routeRule) accepted() []authz.Permission {
	if len(r.anyOf) > 0 {
		return r.anyOf
	}
	if r.permission == "" {
		return nil
	}
	return []authz.Permission{r.permission}
}

// satisfiedBy reports whether subject holds what r requires. A public rule is
// never asked (authorize short-circuits it first), and would be denied here if
// it were -- accepted() is empty, so nothing satisfies it: fail closed.
func (r routeRule) satisfiedBy(policy *authz.Policy, subject authz.Subject) bool { //nolint:gocritic // Subject is a value type by design (authz package doc)
	for _, p := range r.accepted() {
		if policy.Can(subject, p) {
			return true
		}
	}
	return false
}

// deniedLabel is r's value for metrics.AuthzDenied's permission label. The
// label set stays CLOSED (metrics.go's convention): every value it can produce
// comes from routeTable, a compile-time constant, so an anyOf row contributes
// exactly one extra series ("events:read|runs:read") and never a per-request
// string.
func (r routeRule) deniedLabel() string {
	accepted := r.accepted()
	if len(accepted) == 0 {
		return ""
	}
	label := string(accepted[0])
	for _, p := range accepted[1:] {
		label += "|" + string(p)
	}
	return label
}

// deniedDetail is the RFC 7807 detail for a 403 against r -- the same
// "missing permission: x" sentence the single-permission rules have always
// produced, widened to name all the acceptable ones for an anyOf row so the
// caller can ask an admin for either.
func (r routeRule) deniedDetail() string {
	if len(r.anyOf) == 0 {
		return "missing permission: " + string(r.permission)
	}
	return "missing permission: one of " + r.deniedLabel()
}

// routeTable is the authoritative route->permission map task-16-brief.md
// pins verbatim (method + " " + chi route pattern -> routeRule).
// TestEveryAPIRouteHasAPermissionDecision walks the live chi router and
// fails if any registered /api/v1/* or /ws pattern is missing here -- the
// whole point being that a future task cannot add an unguarded endpoint by
// omission. The oidc callback key is built from config.OIDCCallbackPath,
// never re-literalled (Task 11 carry-forward), so it can never drift from
// the path the route is actually registered at in server.go.
var routeTable = map[string]routeRule{
	"GET /api/v1/version": {public: true},
	"GET /api/v1/config":  {public: true},

	"GET /api/v1/auth/me":            {public: true},
	"POST /api/v1/auth/login":        {public: true},
	"POST /api/v1/auth/logout":       {public: true},
	"GET /api/v1/auth/oidc/start":    {public: true},
	"GET " + config.OIDCCallbackPath: {public: true},

	"GET /api/v1/topology":            {permission: authz.PermTopologyRead},
	"GET /api/v1/matrix":              {permission: authz.PermMatrixRead},
	"GET /api/v1/events":              {permission: authz.PermEventsRead},
	"POST /api/v1/promql/query":       {permission: authz.PermPromQLQuery},
	"POST /api/v1/promql/query_range": {permission: authz.PermPromQLQuery},

	"GET /api/v1/audit": {permission: authz.PermAuditRead},

	"POST /api/v1/runs":     {permission: authz.PermRunsCreate},
	"GET /api/v1/runs":      {permission: authz.PermRunsRead},
	"GET /api/v1/runs/{id}": {permission: authz.PermRunsRead},
	// Cancelling is runs:create, not runs:read and not a permission of its
	// own: starting fleet-wide probe traffic and stopping it are the same
	// operational class. Gating it on runs:read would let every viewer stop
	// an operator's diagnostic mid-flight; giving it a third permission would
	// mean an operator who can start a 400-pair run needs a second grant to
	// stop it, which is the wrong failure mode for the one control that makes
	// a runaway run stoppable.
	"POST /api/v1/runs/{id}/cancel": {permission: authz.PermRunsCreate},

	// Targets are the fleet's probe CONFIGURATION, so the split is read vs
	// write, not one permission for the whole resource: targets:read is in
	// operator and admin only (Decision 3 -- viewer must not gain it, since
	// viewer is what auth.anonymous.role defaults to), and every mutation
	// needs targets:write.
	"GET /api/v1/targets":         {permission: authz.PermTargetsRead},
	"POST /api/v1/targets":        {permission: authz.PermTargetsWrite},
	"GET /api/v1/targets/{id}":    {permission: authz.PermTargetsRead},
	"PUT /api/v1/targets/{id}":    {permission: authz.PermTargetsWrite},
	"DELETE /api/v1/targets/{id}": {permission: authz.PermTargetsWrite},

	// Check definitions split read vs write exactly as targets do, for the
	// same Decision 3 reason: checks:read stops at operator and admin, so the
	// viewer role that auth.anonymous.role defaults to never learns what the
	// fleet probes.
	//
	// POST /api/v1/checks/projection is a WRITE row despite persisting
	// nothing. It is read-only in effect, but its body is a draft definition
	// and its answer is the number that gates creating one -- a caller who
	// cannot create a definition has no question to ask it, and gating it on
	// checks:read would hand every reader a topology-derived agent count
	// through a side door.
	"GET /api/v1/checks":             {permission: authz.PermChecksRead},
	"POST /api/v1/checks":            {permission: authz.PermChecksWrite},
	"POST /api/v1/checks/projection": {permission: authz.PermChecksWrite},
	"GET /api/v1/checks/{id}":        {permission: authz.PermChecksRead},
	"PUT /api/v1/checks/{id}":        {permission: authz.PermChecksWrite},
	"DELETE /api/v1/checks/{id}":     {permission: authz.PermChecksWrite},

	// Schedules have no schedules:read of their own (M4 Task 2 defined
	// exactly one schedules permission): reading a cadence tells you nothing
	// the definition it belongs to does not already tell you, so the read
	// side rides on checks:read and only mutations need schedules:write. A
	// role holding schedules:write but not checks:read can therefore change a
	// cadence it cannot list -- deliberate, and the reason the built-in
	// operator role holds both.
	"GET /api/v1/schedules":         {permission: authz.PermChecksRead},
	"POST /api/v1/schedules":        {permission: authz.PermSchedulesWrite},
	"GET /api/v1/schedules/{id}":    {permission: authz.PermChecksRead},
	"PUT /api/v1/schedules/{id}":    {permission: authz.PermSchedulesWrite},
	"DELETE /api/v1/schedules/{id}": {permission: authz.PermSchedulesWrite},

	// MTR path history is TELEMETRY, not configuration, so mtr:read reaches
	// VIEWER (M5 Decision 11) -- M4's Decision 3 line, which keeps
	// targets:read/checks:read out of the role auth.mode=anonymous defaults
	// to, deliberately does not apply here: a stored traceroute is the same
	// class of thing as an event or a run result, all of which viewer already
	// reads. Launching a NEW trace stays behind runs:create; there is no
	// write route here at all.
	"GET /api/v1/mtr/destinations":   {permission: authz.PermMTRRead},
	"GET /api/v1/mtr/snapshots":      {permission: authz.PermMTRRead},
	"GET /api/v1/mtr/snapshots/{id}": {permission: authz.PermMTRRead},

	// Annotations split read from write on the same Decision 11 line: the
	// notes drawn on a chart are telemetry a viewer reads, while PINNING one
	// is an operator statement about the fleet's history -- so
	// annotations:write stops at operator and admin.
	"GET /api/v1/annotations":         {permission: authz.PermAnnotationsRead},
	"POST /api/v1/annotations":        {permission: authz.PermAnnotationsWrite},
	"DELETE /api/v1/annotations/{id}": {permission: authz.PermAnnotationsWrite},

	// Incidents and maintenance windows split read from write on M5's
	// Decision 11 line, extended by M6 Decision 8: a saved investigation and a
	// declared window are CONTEXT on the charts every role already reads, so
	// the reads reach viewer (and alert-editor); writing one is an operator
	// statement about the fleet's history, so the writes stop at operator and
	// admin -- exactly where annotations:write stops.
	//
	// PATCH /api/v1/incidents/{id} is the only PATCH in this table. It is a
	// write row like any other; the method is a data-shape decision (an
	// incident evolves under collaboration), not an authorization one.
	"GET /api/v1/incidents":           {permission: authz.PermIncidentsRead},
	"POST /api/v1/incidents":          {permission: authz.PermIncidentsWrite},
	"GET /api/v1/incidents/{id}":      {permission: authz.PermIncidentsRead},
	"PATCH /api/v1/incidents/{id}":    {permission: authz.PermIncidentsWrite},
	"DELETE /api/v1/incidents/{id}":   {permission: authz.PermIncidentsWrite},
	"GET /api/v1/maintenance":         {permission: authz.PermMaintenanceRead},
	"POST /api/v1/maintenance":        {permission: authz.PermMaintenanceWrite},
	"DELETE /api/v1/maintenance/{id}": {permission: authz.PermMaintenanceWrite},

	// Webhooks get ONE permission for the whole resource, admin-only, and no
	// read/write split (M6 Decision 8). Two reasons, both about the secret:
	// an endpoint row is credential-adjacent, which is the tokens:manage and
	// rbac:manage posture, and there is no useful "read webhooks" audience --
	// the only thing a reader learns is where this console sends incident
	// notifications, which is precisely the fact worth keeping to the people
	// who can change it. An operator therefore CANNOT reach these routes,
	// deliberately, even though operator holds incidents:write.
	//
	// POST /api/v1/webhooks/{id}/test is a write row despite persisting
	// nothing directly: it makes this console emit an outbound signed request
	// to an operator-supplied URL, which is the single most abusable thing in
	// this API.
	"GET /api/v1/webhooks":            {permission: authz.PermWebhooksManage},
	"POST /api/v1/webhooks":           {permission: authz.PermWebhooksManage},
	"GET /api/v1/webhooks/{id}":       {permission: authz.PermWebhooksManage},
	"PUT /api/v1/webhooks/{id}":       {permission: authz.PermWebhooksManage},
	"DELETE /api/v1/webhooks/{id}":    {permission: authz.PermWebhooksManage},
	"POST /api/v1/webhooks/{id}/test": {permission: authz.PermWebhooksManage},

	// Alert rules split read from write on M5's Decision 11 line, extended by
	// M6 Decision 8 and applied unchanged in M7: the rule list, the expression
	// the console rendered from each rule and the set Prometheus is currently
	// firing are CONTEXT on charts every role already reads (the Overview card
	// showing them is the landing page), so the reads reach viewer; declaring
	// "page someone when X" is an operator statement about the fleet, so the
	// writes stop at operator and admin -- exactly where incidents:write stops.
	//
	// Two rows need their own justification:
	//
	//   - POST /api/v1/alert-rules/preview is a READ row despite being a POST,
	//     the mirror image of POST /api/v1/checks/projection's write row. It
	//     persists nothing AND asks nothing a reader could not ask directly:
	//     its answer is "how many series does this expression match right now",
	//     which is a PromQL read, and anyone holding alerts:read can already
	//     see every stored rule's expression. Gating it on alerts:manage would
	//     mean an operator cannot check an expression before proposing it.
	//   - POST /api/v1/alert-rules/{id}/sync is a write row even though it
	//     changes no row: it makes this console server-side-apply a
	//     PrometheusRule into its own namespace, which is a change to the
	//     CLUSTER, and that is the most consequential thing on this resource.
	//   - POST /api/v1/alert-rules/import is alerts:MANAGE even though it
	//     reads a foreign object (which alerts:read may already list through
	//     /foreign), because reading is not what it does: it CREATES rules,
	//     potentially dozens in one call, and those rules then reach the
	//     cluster on the next reconcile. It is the same permission a rule
	//     declared by hand needs, which is what it produces.
	//
	// GET /api/v1/alerts rides alerts:read rather than promql:query, even
	// though it proxies Prometheus: what it serves is this API's own DTO of the
	// firing set, not a query surface, and a role able to see the rules should
	// be able to see whether they are firing.
	"GET /api/v1/alert-rules":            {permission: authz.PermAlertsRead},
	"POST /api/v1/alert-rules":           {permission: authz.PermAlertsManage},
	"GET /api/v1/alert-rules/foreign":    {permission: authz.PermAlertsRead},
	"POST /api/v1/alert-rules/import":    {permission: authz.PermAlertsManage},
	"POST /api/v1/alert-rules/preview":   {permission: authz.PermAlertsRead},
	"GET /api/v1/alert-rules/{id}":       {permission: authz.PermAlertsRead},
	"PUT /api/v1/alert-rules/{id}":       {permission: authz.PermAlertsManage},
	"DELETE /api/v1/alert-rules/{id}":    {permission: authz.PermAlertsManage},
	"POST /api/v1/alert-rules/{id}/sync": {permission: authz.PermAlertsManage},
	"GET /api/v1/alerts":                 {permission: authz.PermAlertsRead},

	// Captured Kubernetes events ride events:read rather than a permission of
	// their own (M6 Decision 8): they ARE events, and a new permission would
	// gate nothing an operator holding events:read could not already infer
	// from the topology stream they read.
	"GET /api/v1/k8s-events": {permission: authz.PermEventsRead},

	// Configuration export/import (M7 Decision 9) takes ONE permission for
	// both routes, admin-only, and no read/write split -- webhooks:manage's
	// posture, for a stronger version of webhooks:manage's reason.
	//
	// The permission is settings:write, which has existed in the closed list
	// since M3 and has never gated a route. Three alternatives were weighed
	// and rejected:
	//
	//   - webhooks:manage. A bundle is not a webhook. Gating the fleet's
	//     probe configuration, alert rules and maintenance windows behind the
	//     endpoint-management permission would make webhooks:manage mean
	//     "everything", which is the drift a closed permission list exists to
	//     prevent.
	//   - rbac:manage. Same objection, plus it would tie configuration
	//     restore to identity administration -- two different jobs.
	//   - a NEW config:manage. It would sit next to an existing, admin-only,
	//     never-used permission that already means what it would mean; adding
	//     a second spelling of one idea is how a closed list stops being one.
	//     (The Settings page is where Decision 10 puts both routes, so
	//     settings:write is also where an operator would look for them.)
	//
	// The read is gated on a :write permission deliberately, and that is not
	// an oversight but the same statement webhooks' single permission makes:
	// an export is every webhook URL, every probe address and every alert
	// expression this console holds, in one file. There is no audience that
	// should be able to READ all of that and not be trusted to write it, so
	// splitting the pair would only create a role that can exfiltrate the
	// whole configuration while looking read-only.
	"GET /api/v1/export":  {permission: authz.PermSettingsWrite},
	"POST /api/v1/import": {permission: authz.PermSettingsWrite},

	"GET /api/v1/rbac/permissions":      {permission: authz.PermRBACManage},
	"GET /api/v1/rbac/roles":            {permission: authz.PermRBACManage},
	"POST /api/v1/rbac/roles":           {permission: authz.PermRBACManage},
	"DELETE /api/v1/rbac/roles/{name}":  {permission: authz.PermRBACManage},
	"GET /api/v1/rbac/bindings":         {permission: authz.PermRBACManage},
	"POST /api/v1/rbac/bindings":        {permission: authz.PermRBACManage},
	"DELETE /api/v1/rbac/bindings/{id}": {permission: authz.PermRBACManage},

	"GET /api/v1/tokens":         {permission: authz.PermTokensManage},
	"POST /api/v1/tokens":        {permission: authz.PermTokensManage},
	"DELETE /api/v1/tokens/{id}": {permission: authz.PermTokensManage},

	// The ONE anyOf row (M7, M3 follow-up #10), and the only route in this
	// table whose authorization does not end here. Until M7 it read
	// {permission: PermEventsRead}, and that single decision covered every
	// topic multiplexed over the socket, because ws.Hub could not tell its
	// connections apart -- so a custom role holding runs:read could START a
	// diagnostics run and then not WATCH it, while simply lowering the row to
	// runs:read would have handed every run watcher the fleet-wide "live"
	// stream that events:read gates on GET /api/v1/events. The hub change
	// SECURITY.md §10.2 named as the prerequisite for splitting them
	// (ws.Hub.ServeWSAuthorized taking a per-connection ws.TopicAuthorizer)
	// now exists, so the UPGRADE admits either permission and the second,
	// per-topic decision is made on the connection by
	// Server.wsTopicAuthorizer (ws_authz.go): events:read keeps every topic,
	// runs:read alone gets run:{id} topics and nothing else. Read that
	// function before touching this row -- the two halves are only correct
	// together, and widening this one alone would restore exactly the leak
	// M4 refused.
	"GET /ws": {anyOf: []authz.Permission{authz.PermEventsRead, authz.PermRunsRead}},
}

// routeRuleFor looks up r's matched chi route pattern (already resolved by
// chi's tree walk before any route-scoped middleware runs -- see
// mux.go:routeHTTP in go-chi/chi/v5) in routeTable.
func routeRuleFor(r *http.Request) (routeRule, bool) {
	pattern := chi.RouteContext(r.Context()).RoutePattern()
	rule, ok := routeTable[r.Method+" "+pattern]
	return rule, ok
}

// authResultLabel maps an authn error to metrics.AuthRequests's closed
// result label set (ok|invalid|expired|error; "ok" is handled by the
// caller before this is ever called). ErrDisabled collapses into "invalid"
// deliberately -- token.go's authenticateToken already establishes the
// precedent of not giving a distinct outcome its own label/response when
// doing so would only narrow an enumeration oracle, and AuthRequests'
// label set is declared closed in metrics.go.
func authResultLabel(err error) string {
	switch {
	case errors.Is(err, authn.ErrInvalid), errors.Is(err, authn.ErrDisabled):
		return "invalid"
	case errors.Is(err, authn.ErrExpired):
		return "expired"
	default:
		return "error"
	}
}

// authenticate resolves s.authenticator against r and stores the result on
// the request context for authorize (and handlers) to read via SubjectFrom.
// It NEVER writes a response and NEVER wraps w -- every 401/403 decision
// lives in authorize, and leaving w untouched is what keeps the /ws
// Hijacker reachable through instrument's statusRecorder.Unwrap chain (see
// server.go's statusRecorder doc comment; this middleware is exactly the
// place the plan calls out as most likely to reintroduce that regression).
//
// Any authenticator error -- ErrNoCredentials, ErrInvalid, ErrExpired,
// ErrDisabled, or an opaque backend failure -- degrades identically to "no
// subject" here (the zero authz.Subject{}, Kind == ""). Collapsing every
// failure mode into the same non-response is deliberate: it lets a public
// route (GET /api/v1/version, the login flow itself) stay reachable even
// when the request happens to carry a stale/invalid session cookie, and it
// means the ONLY place that turns "no subject" into a 401 is authorize,
// driven by routeTable, so that decision is table-checked in one place
// instead of duplicated per authenticator error branch.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mode := s.authenticator.Mode()
		subject, err := s.authenticator.Authenticate(r)

		switch {
		case err == nil:
			subject = s.resolveRoles(r.Context(), subject)
			// M-11: anonymousAuthenticator.Authenticate never fails by
			// construction (NewAnonymous, authn.go) -- every single
			// request in auth.mode=anonymous lands here, always with "ok",
			// so incrementing AuthRequests for it carries no diagnostic
			// signal beyond what HTTPRequests (instrument, server.go)
			// already counts; it would just inflate this metric 1:1 with
			// total traffic on the M1/M2 default deployment.
			if mode != "anonymous" {
				s.metrics.AuthRequests.WithLabelValues(mode, "ok").Inc()
			}
		case errors.Is(err, authn.ErrNoCredentials):
			// Not an "attempt" -- nothing was presented -- so no metric.
			subject = authz.Subject{}
		default:
			subject = authz.Subject{}
			s.metrics.AuthRequests.WithLabelValues(mode, authResultLabel(err)).Inc()
		}

		// Hand the resolved subject back UP to the recoverer as well, which
		// sits outside this middleware and cannot see the child context
		// below (subjectHolder's doc comment). Nil for a request that never
		// went through the recoverer.
		if h := subjectHolderFrom(r.Context()); h != nil {
			h.subject = subject
		}

		next.ServeHTTP(w, r.WithContext(contextWithSubject(r.Context(), subject)))
	})
}

// resolveRoles fills in subject.Roles for a subject authenticate just
// resolved. Anonymous subjects are returned unchanged -- NewAnonymous
// already stamped the single configured role, and "anonymous" is never a
// bindable identity (authz.go's Subject.ID doc: it is the fixed literal
// string, not a users.id or a group). Every other subject either gets the
// roles s.roles resolves from role_bindings, or -- when s.roles is nil
// (database.mode=disabled) or RolesFor errors -- degrades to
// auth.defaultRole (empty means no roles, so the subject is authenticated
// but holds nothing, which authorize turns into 403, never 500). A
// RolesFor error is deliberately NOT surfaced as a request failure: a
// database blip must not lock every operator out of a console whose read
// surface still works under the configured default role.
func (s *Server) resolveRoles(ctx context.Context, subject authz.Subject) authz.Subject { //nolint:gocritic // Subject is a value type by design
	if subject.Kind == authz.SubjectAnonymous {
		return subject
	}
	if s.roles != nil {
		roles, err := s.roles.RolesFor(ctx, subject)
		switch {
		case err != nil:
			slog.Warn("httpapi: resolve roles failed, degrading to auth.defaultRole",
				"subject_kind", subject.Kind, "error", err)
		case len(roles) > 0:
			subject.Roles = roles
			return subject
		}
	}
	subject.Roles = s.defaultRoles()
	return subject
}

// defaultRoles is the config-derived fallback resolveRoles uses when no
// role_bindings resolution is available or possible: auth.defaultRole as
// the subject's sole role, or no roles at all when it is unset (config.go:
// "role for an authenticated subject with no binding; empty = none (403)").
func (s *Server) defaultRoles() []string {
	if s.cfg.Auth.DefaultRole == "" {
		return nil
	}
	return []string{s.cfg.Auth.DefaultRole}
}

// authorize is the sole 401/403 decision point in the chain. It looks up
// the matched route in routeTable, checks the permission (skipped
// entirely for a public route), and -- only for a request that would
// otherwise be let through -- applies the CSRF double-submit check for
// cookie-authenticated mutations. Like authenticate, it never wraps w.
//
// The audit middleware (Task 17, audit.go) lives INSIDE this function, not
// as a separate outer middleware: every 403 this function writes is
// recorded with outcome "denied" right where the decision is made (a
// separate later middleware would never see a request authorize itself
// rejected), and a request that clears every check is handed to
// auditMutation instead of next directly whenever it is a non-idempotent
// method and s.audit is wired -- recordAudit's own nil check on s.audit
// keeps every one of these call sites a true no-op when
// database.mode=disabled.
func (s *Server) authorize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		subject, _ := SubjectFrom(r.Context())

		rule, ok := routeRuleFor(r)
		if !ok {
			// Structurally prevented for every real route by
			// TestEveryAPIRouteHasAPermissionDecision, which walks the live
			// router and fails if routeTable and server.go's registration
			// ever drift apart. Reaching this branch at runtime would mean
			// that guard was bypassed somehow -- fail closed, not open.
			s.recordAudit(r, subject, auditOutcomeDenied, nil)
			writeProblem(w, http.StatusForbidden, "permission denied", "no permission decision for this route")
			return
		}

		if !rule.public {
			if subject.Kind == "" {
				write401(w)
				return
			}
			if !rule.satisfiedBy(s.policy, subject) {
				s.metrics.AuthzDenied.WithLabelValues(rule.deniedLabel()).Inc()
				s.recordAudit(r, subject, auditOutcomeDenied, nil)
				writeProblem(w, http.StatusForbidden, "permission denied", rule.deniedDetail())
				return
			}
		}

		s.maybeMintCSRFCookie(w, r, subject)

		if !s.csrfOK(r, subject) {
			s.recordAudit(r, subject, auditOutcomeDenied, nil)
			writeProblem(w, http.StatusForbidden, "permission denied",
				"missing or invalid CSRF token: mutations must echo the csrf cookie's value in the "+csrfHeaderName+" header; "+
					"the cookie is minted on login/oidc-callback, or -- for header mode, which has neither -- lazily on the first "+
					"authenticated GET (see maybeMintCSRFCookie)")
			return
		}

		if s.audit != nil && isMutatingMethod(r.Method) {
			s.auditMutation(w, r, subject, next)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// maybeMintCSRFCookie closes header mode's CSRF gap (I-2). Local and oidc
// sessions get their csrf cookie from setCSRFCookie at the one moment a
// session is created (handleAuthLogin / handleOIDCCallback); header mode
// has no equivalent moment -- the trusted proxy injects an authz.SubjectUser
// on every request from a header alone, with no login handler to hook. So
// authorize mints the cookie lazily here instead: the first time it sees an
// authenticated SubjectUser make a safe (GET) request with no csrf cookie
// yet, it sets one, so the SPA can read it via document.cookie and echo it
// back as X-CSRF-Token on its next mutation -- the same double-submit
// pattern, just with a different mint point. A no-op once the cookie
// exists (including for local/oidc sessions, which already got one from
// login/callback and never hit the missing-cookie branch here).
func (s *Server) maybeMintCSRFCookie(w http.ResponseWriter, r *http.Request, subject authz.Subject) { //nolint:gocritic // Subject is a value type by design
	if r.Method != http.MethodGet || subject.Kind != authz.SubjectUser {
		return
	}
	if cookie, err := r.Cookie(csrfCookieName); err == nil && cookie.Value != "" {
		return
	}
	if err := s.setCSRFCookie(w); err != nil {
		slog.Warn("httpapi: mint csrf cookie on safe request failed", "error", err)
	}
}

// write401 writes the RFC 7807 "authentication required" denial every
// unauthenticated access to a non-public route gets, with the
// WWW-Authenticate challenge header task-16-brief.md requires.
func write401(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeProblem(w, http.StatusUnauthorized, "authentication required", "")
}

// csrfHeaderName and csrfCookieName are the double-submit pair SECURITY.md
// §12 requires for cookie-authenticated mutations. csrfCookieName is
// deliberately NOT __Host--prefixed (M-6 review carry-forward, correcting
// an earlier wrong comment here): __Host- guards against a WRITE -- a
// compromised or malicious sibling subdomain "tossing" a cookie with this
// same name and an attacker-known value, which would let it forge a
// matching header -- not against a cross-site page READING this cookie
// (ordinary same-origin cookie access rules already block that, __Host- or
// not, and this cookie is non-HttpOnly precisely so the SAME-origin SPA
// can read it). What actually closes the subdomain-tossing gap here,
// instead of __Host-, is two other layers: SameSite=Lax on the session
// cookie already stops it riding along on a cross-site simple request
// (e.g. a bare HTML form POST, which cannot carry a custom header at all),
// and X-CSRF-Token being a non-simple header means any cross-origin
// fetch/XHR attempt at a match triggers a CORS preflight this server never
// grants (no Access-Control-Allow-Origin on any mutating route), so the
// browser blocks the request before it lands. __Host- was left off on
// purpose beyond that: it would force Secure=true unconditionally,
// breaking every non-TLS dev/test deployment that runs with
// auth.session.secure=false (this package's own tests included -- see
// authTestConfig, auth_test.go). Giving it its own fixed name (rather than
// deriving it from auth.session.cookieName) keeps it simple and
// independent of that config value.
const (
	csrfHeaderName = "X-CSRF-Token" //nolint:gosec // G101: header/cookie NAME, not a credential value
	csrfCookieName = "csrf"         //nolint:gosec // G101: cookie NAME, not a credential value
)

// isMutatingMethod reports whether method is one CSRF protection applies
// to (task-16-brief.md: "POST/PUT/PATCH/DELETE").
func isMutatingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// csrfOK reports whether r passes the double-submit CSRF check for
// cookie-authenticated mutations. I-2 review carry-forward: the exemption
// is keyed on subject.Kind, not on whether a session cookie happens to be
// present on THIS request -- the old cookie-presence check was blind to
// header mode, where a trusted proxy (oauth2-proxy) injects identity from
// ITS OWN cookie on every request: kconmon never sees a session cookie, so
// the old check exempted every header-mode mutation from CSRF entirely,
// even though a cross-site request rides the proxy's ambient cookie exactly
// like a browser session does. The invariant now:
//
//   - authz.SubjectToken (Bearer/PAT): exempt, unconditionally. A PAT is
//     never sent ambiently by a browser, so nothing can forge it cross-site,
//     and requiring the header would break every CLI/script.
//   - "" (no subject resolved at all -- the login request itself, or a
//     mutation with no session to speak of): exempt. There is no ambient
//     credential a forged cross-site request could ride on.
//   - authz.SubjectAnonymous: exempt ONLY when s.cfg.Auth.Mode ==
//     "anonymous" -- a genuine no-auth deployment, which has no login flow
//     and no CSRF-cookie mint point, so requiring the pair would just break
//     every mutating M1/M2 route (TestAnonymousDefaultServesEveryM1M2Route).
//     An anonymous Subject reaching here under any OTHER configured mode is
//     NewServer's defensive fallback for an incomplete composition (nil
//     Authenticator), not a real no-auth deployment, and falls through to
//     the same bar as SubjectUser below.
//   - authz.SubjectUser (session-cookie-authenticated -- local/oidc -- OR
//     header-injected): the double-submit pair is required, no exceptions.
//     Both carry an ambient credential a cross-site request could ride on;
//     header mode's cookie half comes from maybeMintCSRFCookie's lazy mint
//     instead of a login handler, since it has none.
func (s *Server) csrfOK(r *http.Request, subject authz.Subject) bool { //nolint:gocritic // Subject is a value type by design
	if !isMutatingMethod(r.Method) {
		return true
	}
	switch subject.Kind {
	case authz.SubjectToken, "":
		return true
	case authz.SubjectAnonymous:
		if s.cfg.Auth.Mode == "anonymous" {
			return true
		}
	case authz.SubjectUser:
		// Falls through to the double-submit pair check below -- listed
		// explicitly (rather than left for switch's default) so this
		// switch stays exhaustive over authz.SubjectKind and a future
		// SubjectKind addition fails the exhaustive linter here instead of
		// silently inheriting whichever behavior an implicit default would
		// have given it.
	}

	csrfCookie, err := r.Cookie(csrfCookieName)
	if err != nil || csrfCookie.Value == "" {
		return false
	}
	header := r.Header.Get(csrfHeaderName)
	if header == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(header), []byte(csrfCookie.Value)) == 1
}

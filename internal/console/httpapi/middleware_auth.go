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

// SubjectFrom returns the Subject the authenticate middleware stored on ctx; the second return is
// false only for requests that bypassed the middleware entirely (/healthz, /readyz, /metrics).
func SubjectFrom(ctx context.Context) (authz.Subject, bool) {
	s, ok := ctx.Value(subjectContextKey{}).(authz.Subject)
	return s, ok
}

// subjectHolderKey is the context key the recoverer stores its subjectHolder
// under. Unexported struct type, same forgery-proof convention as
// subjectContextKey.
type subjectHolderKey struct{}

// subjectHolder is a one-slot mailbox s.recoverer installs on the request context BEFORE the auth
// chain runs and authenticate fills in the moment it has resolved a Subject; it exists because the
// two middlewares see different requests.
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

// routeRule is the route->permission table's value: exactly one closed authz.Permission the caller
// must hold.
type routeRule struct {
	permission authz.Permission
	anyOf      []authz.Permission
	public     bool
}

// accepted returns, in table order, the permissions any ONE of which satisfies r; it is the one
// place the two spellings are reconciled, so satisfiedBy, the metric label.
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

// deniedLabel is r's value for metrics.AuthzDenied's permission label; the label set stays CLOSED
// (metrics.go's convention).
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

// deniedDetail is the RFC 7807 detail for a 403 against r.
func (r routeRule) deniedDetail() string {
	if len(r.anyOf) == 0 {
		return "missing permission: " + string(r.permission)
	}
	return "missing permission: one of " + r.deniedLabel()
}

// routeTable is the authoritative route->permission map; TestEveryAPIRouteHasAPermissionDecision
// walks the live chi router and fails if any registered /api/v1/* or /ws pattern is missing here.
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
	// Cancelling is runs:create, not runs:read and not a permission of its own; gating it on runs:read
	// would let every viewer stop an operator's diagnostic mid-flight.
	"POST /api/v1/runs/{id}/cancel": {permission: authz.PermRunsCreate},

	// Targets are the fleet's probe CONFIGURATION, so the split is read vs write, not one permission
	// for the whole resource.
	"GET /api/v1/targets":         {permission: authz.PermTargetsRead},
	"POST /api/v1/targets":        {permission: authz.PermTargetsWrite},
	"GET /api/v1/targets/{id}":    {permission: authz.PermTargetsRead},
	"PUT /api/v1/targets/{id}":    {permission: authz.PermTargetsWrite},
	"DELETE /api/v1/targets/{id}": {permission: authz.PermTargetsWrite},

	// Check definitions split read vs write exactly as targets do; it is read-only in effect, but its
	// body is a draft definition and its answer is the number that gates creating.
	"GET /api/v1/checks":             {permission: authz.PermChecksRead},
	"POST /api/v1/checks":            {permission: authz.PermChecksWrite},
	"POST /api/v1/checks/projection": {permission: authz.PermChecksWrite},
	"GET /api/v1/checks/{id}":        {permission: authz.PermChecksRead},
	"PUT /api/v1/checks/{id}":        {permission: authz.PermChecksWrite},
	"DELETE /api/v1/checks/{id}":     {permission: authz.PermChecksWrite},

	// Schedules have no schedules:read of their own.
	"GET /api/v1/schedules":         {permission: authz.PermChecksRead},
	"POST /api/v1/schedules":        {permission: authz.PermSchedulesWrite},
	"GET /api/v1/schedules/{id}":    {permission: authz.PermChecksRead},
	"PUT /api/v1/schedules/{id}":    {permission: authz.PermSchedulesWrite},
	"DELETE /api/v1/schedules/{id}": {permission: authz.PermSchedulesWrite},

	// MTR path history is TELEMETRY, not configuration, so mtr:read reaches VIEWER.
	"GET /api/v1/mtr/destinations":          {permission: authz.PermMTRRead},
	"GET /api/v1/mtr/snapshots":             {permission: authz.PermMTRRead},
	"GET /api/v1/mtr/snapshots/{id}/traces": {permission: authz.PermMTRRead},
	"GET /api/v1/mtr/snapshots/{id}":        {permission: authz.PermMTRRead},

	// Annotations split read from write.
	"GET /api/v1/annotations":         {permission: authz.PermAnnotationsRead},
	"POST /api/v1/annotations":        {permission: authz.PermAnnotationsWrite},
	"DELETE /api/v1/annotations/{id}": {permission: authz.PermAnnotationsWrite},

	// Incidents and maintenance windows split read from write; PATCH /api/v1/incidents/{id} is the
	// only PATCH in this table.
	"GET /api/v1/incidents":           {permission: authz.PermIncidentsRead},
	"POST /api/v1/incidents":          {permission: authz.PermIncidentsWrite},
	"GET /api/v1/incidents/{id}":      {permission: authz.PermIncidentsRead},
	"PATCH /api/v1/incidents/{id}":    {permission: authz.PermIncidentsWrite},
	"DELETE /api/v1/incidents/{id}":   {permission: authz.PermIncidentsWrite},
	"GET /api/v1/maintenance":         {permission: authz.PermMaintenanceRead},
	"POST /api/v1/maintenance":        {permission: authz.PermMaintenanceWrite},
	"DELETE /api/v1/maintenance/{id}": {permission: authz.PermMaintenanceWrite},

	// Webhooks get ONE permission for the whole resource, admin-only, and no read/write split.
	"GET /api/v1/webhooks":            {permission: authz.PermWebhooksManage},
	"POST /api/v1/webhooks":           {permission: authz.PermWebhooksManage},
	"GET /api/v1/webhooks/{id}":       {permission: authz.PermWebhooksManage},
	"PUT /api/v1/webhooks/{id}":       {permission: authz.PermWebhooksManage},
	"DELETE /api/v1/webhooks/{id}":    {permission: authz.PermWebhooksManage},
	"POST /api/v1/webhooks/{id}/test": {permission: authz.PermWebhooksManage},

	// Alert rules split read from write; gating it on alerts:manage would mean an operator cannot
	// check an expression before proposing.
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

	// Captured Kubernetes events ride events:read rather than a permission of their own: they ARE
	// events.
	"GET /api/v1/k8s-events": {permission: authz.PermEventsRead},

	// Configuration export/import takes ONE permission for both routes, admin-only, and no read/write
	// split.
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

	// The ONE anyOf row, and the only route in this table whose authorization does not end here.
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

// authResultLabel maps an authn error to metrics.AuthRequests's closed result label set.
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

// authenticate resolves s.authenticator against r and stores the result on the request context for
// authorize (and handlers) to read via SubjectFrom; it NEVER writes a response and NEVER wraps w --
// every 401/403 decision lives in authorize.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mode := s.authenticator.Mode()
		subject, err := s.authenticator.Authenticate(r)

		switch {
		case err == nil:
			subject = s.resolveRoles(r.Context(), subject)
			// anonymousAuthenticator.Authenticate never fails by construction (NewAnonymous, authn.go).
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

		// Hand the resolved subject back UP to the recoverer as well.
		if h := subjectHolderFrom(r.Context()); h != nil {
			h.subject = subject
		}

		next.ServeHTTP(w, r.WithContext(contextWithSubject(r.Context(), subject)))
	})
}

// resolveRoles fills in subject.Roles for a subject authenticate just resolved; anonymous subjects
// are returned unchanged.
func (s *Server) resolveRoles(ctx context.Context, subject authz.Subject) authz.Subject { //nolint:gocritic // Subject is a value type by design
	if subject.Kind == authz.SubjectAnonymous {
		return subject
	}
	if s.roles != nil {
		roles, err := s.roles.RolesFor(ctx, subject)
		switch {
		case err != nil:
			/* Fail CLOSED. An unreadable role store is not evidence that this subject holds
			   auth.defaultRole -- it is no evidence at all, and handing out the default role on a
			   database blip grants permissions the subject may not have. No roles means every
			   permission check refuses, which is the honest answer to "we cannot tell". */
			slog.Error("httpapi: resolve roles failed, refusing rather than granting the default role",
				"subject_kind", subject.Kind, "error", err)
			subject.Roles = nil
			return subject
		case len(roles) > 0:
			subject.Roles = roles
			return subject
		}
	}
	// No role store, or a subject the store knows nothing about: the configured default.
	subject.Roles = s.defaultRoles()
	return subject
}

// defaultRoles is the config-derived fallback resolveRoles uses when no role_bindings resolution is
// available or possible.
func (s *Server) defaultRoles() []string {
	if s.cfg.Auth.DefaultRole == "" {
		return nil
	}
	return []string{s.cfg.Auth.DefaultRole}
}

// authorize is the sole 401/403 decision point in the chain; it looks up the matched route in
// routeTable.
func (s *Server) authorize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		subject, _ := SubjectFrom(r.Context())

		rule, ok := routeRuleFor(r)
		if !ok {
			// Structurally prevented for every real route by TestEveryAPIRouteHasAPermissionDecision.
			s.recordAudit(r, subject, auditOutcomeDenied, nil)
			writeProblem(w, http.StatusForbidden, "permission denied", "no permission decision for this route")
			return
		}

		if !rule.public {
			if subject.Kind == "" {
				/* A refused credential is a security event and left no trace: metrics counted it,
				   the audit log did not, so a token-guessing or brute-force attempt was invisible
				   to the one surface an operator investigates with. The subject is empty by
				   definition here — what is worth recording is the route and the outcome. */
				s.recordAudit(r, subject, auditOutcomeDenied, nil)
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

		// Mutations, plus the privileged READS: see auditedReads.
		if s.audit != nil && (isMutatingMethod(r.Method) || s.isAuditedRead(r)) {
			s.auditMutation(w, r, subject, next)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// maybeMintCSRFCookie closes header mode's CSRF gap (I-2); a no-op once the cookie exists
// (including for local/oidc sessions, which already got one from login/callback and never hit the
// missing-cookie branch here).
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

// write401 writes the RFC 7807 "authentication required" denial every unauthenticated access to a
// non-public route gets.
func write401(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeProblem(w, http.StatusUnauthorized, "authentication required", "")
}

// csrfHeaderName and csrfCookieName are the double-submit pair SECURITY.md §12 requires for
// cookie-authenticated mutations; giving it its own fixed name (rather than deriving it from
// auth.session.cookieName) keeps it simple and independent of that config value.
const (
	csrfHeaderName = "X-CSRF-Token" //nolint:gosec // G101: header/cookie NAME, not a credential value
	csrfCookieName = "csrf"         //nolint:gosec // G101: cookie NAME, not a credential value
)

// isMutatingMethod reports whether method is one CSRF protection applies to.
func isMutatingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// csrfOK reports whether r passes the double-submit CSRF check for cookie-authenticated mutations;
// I-2 review carry-forward: the exemption is keyed on subject.Kind.
func (s *Server) csrfOK(r *http.Request, subject authz.Subject) bool { //nolint:gocritic // Subject is a value type by design
	if !isMutatingMethod(r.Method) {
		return true
	}
	switch subject.Kind {
	case authz.SubjectToken, "":
		return true
	case authz.SubjectAnonymous:
		/* Anonymous mode has no token to double-submit, but it is NOT unprotected.
		   The exemption used to be unconditional, and anonymous.role may be operator or admin, so
		   any page an operator's browser happened to visit could POST into a console that was
		   deliberately kept off the internet: a CORS simple request (text/plain body, no custom
		   header) triggers no preflight, so the browser sent it and the console executed it. The
		   attacker never sees the response, but the write lands — targets, check definitions, alert
		   rules, RBAC roles, API tokens. Origin/Sec-Fetch-Site is the check that fits: a browser
		   always sends them cross-site, and a non-browser client (curl, a script) sends neither and
		   is unaffected. */
		if s.cfg.Auth.Mode == "anonymous" {
			return sameOriginRequest(r)
		}
	case authz.SubjectUser:
		// Falls through to the double-submit pair check below.
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

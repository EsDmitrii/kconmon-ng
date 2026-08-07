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
// itself, and the pre-login version/config probes).
type routeRule struct {
	permission authz.Permission
	public     bool
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

	// One permission gates the WHOLE socket, every multiplexed topic
	// included -- ws.Hub never receives an authz.Subject, so its subscribe
	// path (hub.go's topicAllowed/subscribe) decides on the topic NAME
	// alone and cannot be per-topic-authorized from here. Consequence, made
	// explicit by M4 Task 2 (M3 follow-up #10): a custom role holding only
	// runs:read cannot open the socket to watch its own run's progress and
	// must poll GET /api/v1/runs/{id} instead; conversely events:read alone
	// already covers run:{id} topics. Lowering this row to runs:read would
	// hand every run watcher the "live" events stream too -- a real
	// widening. Splitting it properly is a hub change (subject-aware
	// subscribe), not a routeTable change. Pinned by
	// TestWSRequiresEventsReadEvenForRunWatching and documented in
	// SECURITY.md §10.2.
	"GET /ws": {permission: authz.PermEventsRead},
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
			if !s.policy.Can(subject, rule.permission) {
				s.metrics.AuthzDenied.WithLabelValues(string(rule.permission)).Inc()
				s.recordAudit(r, subject, auditOutcomeDenied, nil)
				writeProblem(w, http.StatusForbidden, "permission denied", "missing permission: "+string(rule.permission))
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

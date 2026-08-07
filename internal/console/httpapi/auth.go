package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// RoleResolver maps a Subject's identity+groups to role names via
// role_bindings (store.RoleStore.ListBindingsForSubject is the production
// implementation, wired by cmd/console). nil means database.mode=disabled:
// every non-anonymous subject falls back to auth.defaultRole
// (resolveRoles, middleware_auth.go).
type RoleResolver interface {
	RolesFor(ctx context.Context, s authz.Subject) ([]string, error) //nolint:gocritic // Subject is a value type by design
}

// csrfTokenBytes is 256 bits of crypto/rand, the same size session.go's
// sessionIDBytes and oidc.go's oidcRandomBytes already use for a
// browser-facing random value.
const csrfTokenBytes = 32

// csrfRandRead is crypto/rand.Read, indirected through a package-level var
// so a test can stub a failure and exercise setCSRFCookie's error path
// (M-5: a csrf-mint failure on login/oidc-callback must abort the
// just-created session, not be silently swallowed) without touching the
// process's real entropy source.
var csrfRandRead = rand.Read

// newCSRFToken mints a fresh random CSRF token, base64url-encoded (the same
// "N raw bytes -> base64url" convention authn's session ids and OIDC state
// already use). No server-side record is kept anywhere: the double-submit
// pattern's whole security property is that the cookie and the header must
// match, and a cross-site attacker can read neither.
func newCSRFToken() (string, error) {
	buf := make([]byte, csrfTokenBytes)
	if _, err := csrfRandRead(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// setSessionCookie sets the session cookie under the CONFIGURED name
// (s.cfg.Auth.Session.CookieName) -- deliberately never
// authn.OIDCSessionCookieName's hardcoded constant, so an operator's
// non-default auth.session.cookieName is honored by every cookie this
// package mints (Task 15 review carry-forward). __Host- cookie rules
// (task-16-brief.md): Secure from config, HttpOnly, SameSite=Lax, Path=/,
// and no Domain is ever set here -- Config.validateAuth already refuses a
// __Host--prefixed name with secure:false.
func (s *Server) setSessionCookie(w http.ResponseWriter, sessionID string) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: HttpOnly+SameSite are set; Secure is config-driven, and validateAuth refuses a __Host- name with secure:false
		Name:     s.cfg.Auth.Session.CookieName,
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.Auth.Session.Secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(s.cfg.Auth.Session.TTL.Seconds()),
	})
}

// clearSessionCookie expires the session cookie client-side. Logout also
// deletes the session server-side (instant revocation) before calling
// this -- this alone only stops the BROWSER from presenting it again.
func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: same attributes as setSessionCookie; Secure is config-driven by design
		Name:     s.cfg.Auth.Session.CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.Auth.Session.Secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// setCSRFCookie mints and sets the double-submit CSRF cookie alongside a
// freshly-created session (login, oidc callback). Deliberately NOT
// HttpOnly: the frontend must be able to read it and echo it back in
// X-CSRF-Token on every mutating request.
func (s *Server) setCSRFCookie(w http.ResponseWriter) error {
	token, err := newCSRFToken()
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: double-submit CSRF cookie is deliberately readable by the frontend (not HttpOnly); Secure is config-driven
		Name:     csrfCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: false,
		Secure:   s.cfg.Auth.Session.Secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(s.cfg.Auth.Session.TTL.Seconds()),
	})
	return nil
}

// clearCSRFCookie expires the CSRF cookie client-side, alongside logout's
// clearSessionCookie.
func (s *Server) clearCSRFCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: same attributes as setCSRFCookie; deliberately not HttpOnly, Secure config-driven
		Name:     csrfCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: false,
		Secure:   s.cfg.Auth.Session.Secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// authMeSubject is GET /api/v1/auth/me's "subject" field, task-16-brief.md
// verbatim: {"kind","id","displayName","groups","roles"}.
type authMeSubject struct {
	Kind        string   `json:"kind"`
	ID          string   `json:"id"`
	DisplayName string   `json:"displayName"`
	Groups      []string `json:"groups"`
	Roles       []string `json:"roles"`
}

// nonNilStrings returns v unchanged when non-nil, or an empty (never null)
// slice otherwise -- the same "frontend indexes into it" convention
// capabilities() and eventsResponse.Events already follow.
func nonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// handleAuthMe answers "who am I". The ROUTE is public (routeTable: no
// permission decision at all -- the UI must be able to call this before it
// knows whether it is logged in), but the HANDLER still requires a
// resolved identity to answer with: for auth.mode=anonymous that is always
// true (NewAnonymous's Authenticate never fails), so this endpoint returns
// the anonymous subject unconditionally there -- no special case needed.
// Every other mode 401s here, exactly like a protected route would, when
// authenticate could not resolve a subject (subject.Kind == "" is how it
// spells that; see middleware_auth.go's authenticate).
func (s *Server) handleAuthMe(w http.ResponseWriter, r *http.Request) {
	subject, _ := SubjectFrom(r.Context())
	if subject.Kind == "" {
		write401(w)
		return
	}
	writeJSON(w, map[string]any{
		"subject": authMeSubject{
			Kind:        string(subject.Kind),
			ID:          subject.ID,
			DisplayName: subject.DisplayName,
			Groups:      nonNilStrings(subject.Groups),
			Roles:       nonNilStrings(subject.Roles),
		},
		"permissions": s.policy.PermissionsFor(subject),
	})
}

// dummyPasswordHash is a fixed argon2id PHC hash with no real backing
// account, computed once at package init (I-3 review carry-forward: kill
// the login timing oracle). handleAuthLogin's not-found and disabled paths
// used to return before doing ANY argon2 work, while its own comment
// claimed indistinguishability from the wrong-password path -- which
// argon2id's ~tens-of-milliseconds cost makes trivially distinguishable by
// response timing alone. Both of those paths now run VerifyPassword
// against THIS hash (discarding the result) before answering, so all three
// failure paths -- unknown username, disabled account, wrong password --
// pay the identical argon2 cost. The "password" hashed into it is a fixed
// literal, never a secret, and this hash authenticates nothing.
var dummyPasswordHash = mustHashDummyPassword()

func mustHashDummyPassword() string {
	hash, err := authn.HashPassword("kconmon-ng-fixed-dummy-password-for-login-timing-safety")
	if err != nil {
		// rand.Read failing at package init means the process's entropy
		// source is broken -- every other consumer of crypto/rand in this
		// binary (session ids, CSRF tokens, OIDC state/PKCE) is equally
		// unusable at that point, so failing loudly now is more
		// diagnosable than limping on with a zero-value dummy hash, which
		// would make VerifyPassword fail to PARSE instead of doing real
		// argon2 work -- reopening exactly the timing gap this exists to
		// close.
		panic(fmt.Sprintf("httpapi: precompute dummy password hash: %v", err))
	}
	return hash
}

// authLoginRequest is POST /api/v1/auth/login's body.
type authLoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"` //nolint:gosec // G117: request field carrying a client-supplied plaintext password to verify, not a hardcoded credential
}

// handleAuthLogin verifies username/password against s.users (argon2id, via
// authn.VerifyPassword) and mints a session -- auth.mode=local only. Every
// other mode answers 404, not 401 or 503, so the frontend can
// feature-detect which login affordance to offer (task-16-brief.md:
// "local mode only; 404 in other modes so the UI can feature-detect").
func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Auth.Mode != "local" {
		writeProblem(w, http.StatusNotFound, "not found", "")
		return
	}
	if s.users == nil || s.sessions == nil {
		// Defensive: config says local mode, but cmd/console did not wire a
		// user/session store (e.g. database unreachable at boot). Distinct
		// from the mode mismatch above -- this is a real configuration
		// problem, not "wrong login flow".
		writeProblem(w, http.StatusServiceUnavailable, "local auth not available", "no user or session store wired")
		return
	}

	var req authLoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" || req.Password == "" {
		writeProblem(w, http.StatusBadRequest, "invalid request", `body must be JSON with non-empty "username" and "password"`)
		return
	}

	user, err := s.users.GetUserByUsername(r.Context(), req.Username)
	switch {
	case err == nil:
		// fall through to password verification below
	case errors.Is(err, store.ErrNotFound):
		// I-3: pay the same argon2 cost the real verification path below
		// would, before answering -- see dummyPasswordHash's doc comment.
		_, _ = authn.VerifyPassword(dummyPasswordHash, req.Password)
		s.metrics.AuthRequests.WithLabelValues("local", "invalid").Inc()
		writeProblem(w, http.StatusUnauthorized, "invalid credentials", "")
		return
	default:
		s.metrics.AuthRequests.WithLabelValues("local", "error").Inc()
		writeProblem(w, http.StatusServiceUnavailable, "local auth unavailable", "")
		return
	}

	// Disabled and wrong-password both collapse to the same "invalid
	// credentials" response, the same metric outcome, AND -- now that both
	// pay the same argon2 cost as the not-found path above (I-3) -- the
	// same response timing. Same don't-narrow-the-oracle reasoning as
	// csrfOK/authResultLabel and token.go's authenticateToken: a login
	// attempt must not be usable to distinguish "no such user" from "that
	// user is disabled" from "wrong password", by status, body, OR timing.
	if user.Disabled {
		_, _ = authn.VerifyPassword(dummyPasswordHash, req.Password)
		s.metrics.AuthRequests.WithLabelValues("local", "invalid").Inc()
		writeProblem(w, http.StatusUnauthorized, "invalid credentials", "")
		return
	}
	if ok, verr := authn.VerifyPassword(user.PasswordHash, req.Password); verr != nil || !ok {
		s.metrics.AuthRequests.WithLabelValues("local", "invalid").Inc()
		writeProblem(w, http.StatusUnauthorized, "invalid credentials", "")
		return
	}

	sessionID, err := s.sessions.Create(r.Context(), authn.Session{
		Username:    user.Username,
		DisplayName: user.DisplayName,
	})
	if err != nil {
		slog.Warn("httpapi: create session on login failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "login failed", "")
		return
	}

	s.metrics.AuthRequests.WithLabelValues("local", "ok").Inc()
	s.setSessionCookie(w, sessionID)
	if cerr := s.setCSRFCookie(w); cerr != nil {
		// M-5: do not leave a session whose browser has no csrf cookie --
		// every subsequent mutation, including logout itself, would 403
		// forever (csrfOK requires the pair for a SubjectUser). Undo the
		// session both server-side (Delete) and client-side (clear the
		// cookie already queued above) and fail the login outright.
		slog.Error("httpapi: mint csrf cookie on login failed, aborting session", "error", cerr)
		if delErr := s.sessions.Delete(r.Context(), sessionID); delErr != nil {
			slog.Warn("httpapi: delete session after csrf mint failure failed", "error", delErr)
		}
		s.clearSessionCookie(w)
		writeProblem(w, http.StatusInternalServerError, "login failed", "")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAuthLogout deletes the session server-side -- instant revocation,
// regardless of the KV entry's own TTL (authn.SessionStore.Delete's doc
// comment) -- and clears both cookies client-side. Idempotent and
// mode-agnostic: safe to call with no session cookie at all (anonymous or
// header mode, or an already-logged-out browser), and it always answers
// 204 rather than distinguishing "there was nothing to log out of".
func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if s.sessions != nil {
		if cookie, err := r.Cookie(s.cfg.Auth.Session.CookieName); err == nil && cookie.Value != "" {
			if delErr := s.sessions.Delete(r.Context(), cookie.Value); delErr != nil {
				slog.Warn("httpapi: delete session on logout failed", "error", delErr)
			}
		}
	}
	s.clearSessionCookie(w)
	s.clearCSRFCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// oidcDefaultReturnTo is where GET /api/v1/auth/oidc/start sends the
// browser back to when the caller specifies no ?returnTo=.
const oidcDefaultReturnTo = "/"

// isSafeReturnTo mirrors authn.OIDCAuthenticator.AuthorizeURL's own
// same-origin-relative-path check (oidc.go's unexported isSafeReturnTo of
// the same name and behavior) so handleOIDCStart can validate returnTo
// BEFORE calling AuthorizeURL. Duplicated rather than exported from authn
// (I-4 keeps this package's OIDC seam to the narrow OIDCFlow interface,
// not authn internals): this is what lets M-10's error mapping tell "bad
// returnTo" (400) apart from "AuthorizeURL failed for some other reason"
// (500) reliably, without authn having to expose a distinguishing sentinel
// error through OIDCFlow.
func isSafeReturnTo(returnTo string) bool {
	if returnTo == "" || !strings.HasPrefix(returnTo, "/") || strings.HasPrefix(returnTo, "//") {
		return false
	}
	if strings.ContainsAny(returnTo, "\\") {
		return false
	}
	u, err := url.Parse(returnTo)
	if err != nil {
		return false
	}
	return u.Scheme == "" && u.Host == ""
}

// handleOIDCStart redirects to the IdP's authorization endpoint.
// auth.mode=oidc only; 404 otherwise, mirroring handleAuthLogin's
// feature-detection contract. M-10: only a caller-supplied bad returnTo is
// a 400 -- that is decided here, by isSafeReturnTo, before AuthorizeURL is
// even called, so any error AuthorizeURL itself returns (state mint / KV
// write failure) is unambiguously an infrastructure problem, answered 500
// with a generic detail (never the underlying error) and logged.
func (s *Server) handleOIDCStart(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Auth.Mode != "oidc" || s.oidc == nil {
		writeProblem(w, http.StatusNotFound, "not found", "")
		return
	}
	returnTo := r.URL.Query().Get("returnTo")
	if returnTo == "" {
		returnTo = oidcDefaultReturnTo
	}
	if !isSafeReturnTo(returnTo) {
		writeProblem(w, http.StatusBadRequest, "invalid oidc start request", "returnTo must be a same-origin relative path")
		return
	}
	authURL, err := s.oidc.AuthorizeURL(r.Context(), returnTo)
	if err != nil {
		slog.Error("httpapi: oidc authorize url failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "oidc start failed", "")
		return
	}
	//nolint:gosec // G710: authURL is built by AuthorizeURL from the operator-configured IdP endpoint; the only caller input (returnTo) was validated by isSafeReturnTo above and only rides inside the state stash
	http.Redirect(w, r, authURL, http.StatusFound)
}

// handleOIDCCallback consumes the IdP redirect (state + code), mints a
// session, and sends the browser on to the returnTo path AuthorizeURL
// validated and stashed. auth.mode=oidc only; 404 otherwise. Registered at
// config.OIDCCallbackPath in server.go -- never re-literalled here either.
func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Auth.Mode != "oidc" || s.oidc == nil {
		writeProblem(w, http.StatusNotFound, "not found", "")
		return
	}

	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")
	sessionID, returnTo, err := s.oidc.Callback(r.Context(), state, code)
	if err != nil {
		slog.Warn("httpapi: oidc callback failed", "error", err)
		s.metrics.AuthRequests.WithLabelValues("oidc", authResultLabel(err)).Inc()
		write401(w)
		return
	}

	s.metrics.AuthRequests.WithLabelValues("oidc", "ok").Inc()
	s.setSessionCookie(w, sessionID)
	if cerr := s.setCSRFCookie(w); cerr != nil {
		// M-5: same reasoning as handleAuthLogin's identical branch -- a
		// session with no matching csrf cookie would 403 on every mutation
		// forever, including logout. s.sessions here is the same store
		// s.oidc.Callback used to create sessionID (cmd/console wires both
		// from one *authn.SessionStore), so Delete against it actually
		// revokes what Callback just created; guarded nil-check because a
		// test double may exercise this handler with Deps.Sessions unset.
		slog.Error("httpapi: mint csrf cookie on oidc callback failed, aborting session", "error", cerr)
		if s.sessions != nil {
			if delErr := s.sessions.Delete(r.Context(), sessionID); delErr != nil {
				slog.Warn("httpapi: delete session after csrf mint failure failed", "error", delErr)
			}
		}
		s.clearSessionCookie(w)
		writeProblem(w, http.StatusInternalServerError, "login failed", "")
		return
	}
	// returnTo was validated by isSafeReturnTo before AuthorizeURL stashed
	// it, but it round-trips through the KV store between then and now --
	// re-check so a corrupted stash can never become an open redirect.
	if !isSafeReturnTo(returnTo) {
		returnTo = oidcDefaultReturnTo
	}
	//nolint:gosec // G710: returnTo is re-validated by isSafeReturnTo immediately above (same-origin relative path only)
	http.Redirect(w, r, returnTo, http.StatusFound)
}

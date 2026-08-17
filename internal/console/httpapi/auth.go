package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
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

// RoleResolver maps a Subject's identity+groups to role names via role_bindings
// (store.RoleStore.ListBindingsForSubject is the production implementation, wired by cmd/console).
type RoleResolver interface {
	RolesFor(ctx context.Context, s authz.Subject) ([]string, error) //nolint:gocritic // Subject is a value type by design
}

// csrfTokenBytes is 256 bits of crypto/rand, the same size session.go's
// sessionIDBytes and oidc.go's oidcRandomBytes already use for a
// browser-facing random value.
const csrfTokenBytes = 32

// csrfRandRead is crypto/rand.Read, indirected through a package-level var so a test can stub a
// failure and exercise setCSRFCookie's error path without touching the process's real entropy
// source.
var csrfRandRead = rand.Read

// newCSRFToken mints a fresh random CSRF token; no server-side record is kept anywhere: the
// double-submit pattern's whole security property is that the cookie and the header must match.
func newCSRFToken() (string, error) {
	buf := make([]byte, csrfTokenBytes)
	if _, err := csrfRandRead(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// setSessionCookie sets the session cookie under the CONFIGURED name
// (s.cfg.Auth.Session.CookieName).
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

// setCSRFCookie mints and sets the double-submit CSRF cookie alongside a freshly-created session
// (login, oidc callback); deliberately NOT HttpOnly: the frontend must be able to read it and echo
// it back in X-CSRF-Token on every mutating request.
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

// authMeSubject is GET /api/v1/auth/me's "subject" field.
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

// handleAuthMe answers "who am I"; the ROUTE is public (routeTable: no permission decision at all
// -- the UI must be able to call this before it knows whether it is logged in).
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

// dummyPasswordHash is a fixed argon2id PHC hash with no real backing account; the "password"
// hashed into it is a fixed literal.
var dummyPasswordHash = mustHashDummyPassword()

func mustHashDummyPassword() string {
	hash, err := authn.HashPassword("kconmon-ng-fixed-dummy-password-for-login-timing-safety")
	if err != nil {
		// rand.Read failing at package init means the process's entropy source is broken.
		panic(fmt.Sprintf("httpapi: precompute dummy password hash: %v", err))
	}
	return hash
}

// loginRateLimitDetail is the 429 body's detail for POST /api/v1/auth/login; it never says WHICH
// counter tripped (username or source IP).
const loginRateLimitDetail = "too many login attempts; retry shortly " +
	"(limit: console.rateLimit.loginPerMinute per username and per source address per minute)"

// authLoginRequest is POST /api/v1/auth/login's body.
type authLoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"` //nolint:gosec // G117: request field carrying a client-supplied plaintext password to verify, not a hardcoded credential
}

// handleAuthLogin verifies username/password against s.users (argon2id, via authn.VerifyPassword)
// and mints a session.
// sameOriginRequest reports whether this request came from the console's own origin.
//
// It is ws/conn.go's checkOrigin, deliberately identical: an ABSENT Origin passes, because a
// non-browser client (curl, a script, an agent) never sends one and this is a browser-CSRF defence,
// not an authentication step. A PRESENT Origin must match the Host the request was addressed to.
//
// Sec-Fetch-Site is honoured when the browser sent it, which is stricter and cheaper: "same-origin"
// passes, anything else (cross-site, same-site, none) is refused before the Origin is even parsed.
func sameOriginRequest(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return strings.EqualFold(site, "same-origin")
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Auth.Mode != "local" {
		writeProblem(w, http.StatusNotFound, "not found", "")
		return
	}
	if s.users == nil || s.sessions == nil {
		// Defensive: config says local mode, but cmd/console did not wire a user/session store (e.g.
		// database unreachable at boot).
		writeProblem(w, http.StatusServiceUnavailable, "local auth not available", "no user or session store wired")
		return
	}

	/* SAME-ORIGIN, and it is not belt-and-braces: this route is public, so authorize applies no CSRF
	   gate to it, and a login is a state change on the victim's browser. evil.example can post a
	   cross-site form with enctype=text/plain whose body parses as this JSON, and the 204's
	   Set-Cookie then replaces the victim's session with the ATTACKER's: everything the victim goes
	   on to write — annotations, incidents, maintenance windows — is written into the attacker's
	   account, and everything they read is read from it. The socket's own upgrade already refuses a
	   cross-origin request this way (ws/conn.go's checkOrigin); the shape is copied verbatim, an
	   ABSENT Origin included, since a non-browser client never sends one. */
	if !sameOriginRequest(r) {
		writeProblem(w, http.StatusForbidden, "cross-origin login refused",
			"a login must come from this console's own origin")
		return
	}
	/* And the enctype=text/plain form vector needs a content type it cannot set: a browser form can
	   only send text/plain, multipart/form-data or urlencoded, never application/json. */
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		writeProblem(w, http.StatusUnsupportedMediaType, "unsupported media type",
			"a login body must be sent as application/json")
		return
	}

	var req authLoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" || req.Password == "" {
		writeProblem(w, http.StatusBadRequest, "invalid request", `body must be JSON with non-empty "username" and "password"`)
		return
	}

	/* Two counters, two BUDGETS, and that is the fix rather than a nicety. Behind the chart's own
	   Ingress every request in the world arrives from the ingress-controller pod, so one address is
	   not one person: at a shared budget, six bogus attempts a minute locked out every user of the
	   console, correct password included, with no credentials needed to do it. The per-username
	   counter stays narrow (it is what protects an account); the per-address one is a wide net for a
	   single host hammering many usernames, and it counts the address the trusted-proxy
	   configuration says is really the client. */
	if !s.rateLimitAllow(r.Context(), rateLimitLogin, s.cfg.RateLimit.LoginPerMinute, loginUserRateLimitKey(req.Username)) ||
		!s.rateLimitAllow(r.Context(), rateLimitLogin, s.cfg.RateLimit.LoginPerMinute*loginIPBurstFactor,
			loginIPRateLimitKey(clientIP(r, s.trustedProxies))) {
		writeRateLimited(w, loginRateLimitDetail)
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

	// Same don't-narrow-the-oracle reasoning as csrfOK/authResultLabel and token.go's
	// authenticateToken.
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
		// do not leave a session whose browser has no csrf cookie -- every subsequent mutation, including
		// logout itself.
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

// handleAuthLogout deletes the session server-side -- instant revocation; idempotent and
// mode-agnostic: safe to call with no session cookie at all (anonymous or header mode, or an
// already-logged-out browser).
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

// oidcMaxReturnToBytes caps the caller-controlled string the state stash carries.
const oidcMaxReturnToBytes = 2048

// isSafeReturnTo mirrors authn.OIDCAuthenticator.AuthorizeURL's own same-origin-relative-path check
// (oidc.go's unexported isSafeReturnTo of the same name and behavior) so handleOIDCStart can
// validate returnTo BEFORE calling AuthorizeURL; duplicated rather than exported from authn (I-4
// keeps this package's OIDC seam to the narrow OIDCFlow interface, not authn internals).
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

/*
oidcStateCookie binds the CSRF state to the browser that STARTED the flow (RFC 6749 section 10.12).

Without it the callback accepts a state from ANY user agent: an attacker runs the flow to the point
of holding a valid state+code pair, then makes the victim's browser follow the callback URL, and the
victim is silently signed in as the attacker -- login CSRF. Everything the victim then records lands
in the attacker's account.

Path is the OIDC endpoints only, so the cookie never rides on an ordinary API call, and it is
cleared on the way out whatever the outcome.
*/
const oidcStateCookieName = "kconmon_oidc_state"

// Mirrors authn's own state TTL.
const oidcStateCookieMaxAge = 300

const oidcCookiePath = "/api/v1/auth/oidc"

func (s *Server) setOIDCStateCookie(w http.ResponseWriter, state string) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: HttpOnly+SameSite are set; Secure is config-driven, as for the session cookie
		Name:     oidcStateCookieName,
		Value:    state,
		Path:     oidcCookiePath,
		HttpOnly: true,
		Secure:   s.cfg.Auth.Session.Secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   oidcStateCookieMaxAge,
	})
}

func (s *Server) clearOIDCStateCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: same attributes as the setter, expired
		Name:     oidcStateCookieName,
		Value:    "",
		Path:     oidcCookiePath,
		HttpOnly: true,
		Secure:   s.cfg.Auth.Session.Secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// stateFromAuthorizeURL reads back the state AuthorizeURL minted. Read from the URL rather than
// returned separately so the authn seam stays the narrow OIDCFlow interface it was built as.
func stateFromAuthorizeURL(authURL string) string {
	u, err := url.Parse(authURL)
	if err != nil {
		return ""
	}
	return u.Query().Get("state")
}

// handleOIDCStart redirects to the IdP's authorization endpoint. auth.mode=oidc only; 404
// otherwise.
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
	/* The route is PUBLIC and unrate-limited, and every call stashes this string in the KV for the
	   state's lifetime. Uncapped, one unauthenticated request could park most of net/http's header
	   budget there; a path this console can actually route is far shorter than the cap. */
	if len(returnTo) > oidcMaxReturnToBytes {
		writeProblem(w, http.StatusBadRequest, "invalid oidc start request",
			fmt.Sprintf("returnTo must be at most %d bytes", oidcMaxReturnToBytes))
		return
	}
	authURL, err := s.oidc.AuthorizeURL(r.Context(), returnTo)
	if err != nil {
		slog.Error("httpapi: oidc authorize url failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "oidc start failed", "")
		return
	}
	state := stateFromAuthorizeURL(authURL)
	if state == "" {
		slog.Error("httpapi: oidc authorize url carried no state")
		writeProblem(w, http.StatusInternalServerError, "oidc start failed", "")
		return
	}
	s.setOIDCStateCookie(w, state)
	//nolint:gosec // G710: authURL is built by AuthorizeURL from the operator-configured IdP endpoint; the only caller input (returnTo) was validated by isSafeReturnTo above and only rides inside the state stash
	http.Redirect(w, r, authURL, http.StatusFound)
}

// handleOIDCCallback consumes the IdP redirect (state + code), mints a session.
func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Auth.Mode != "oidc" || s.oidc == nil {
		writeProblem(w, http.StatusNotFound, "not found", "")
		return
	}

	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")

	/* The state has to belong to THIS browser, not merely be a state the console minted. Compared in
	   constant time and consumed either way, so a failed attempt cannot be retried against the same
	   cookie. */
	bound, cookieErr := r.Cookie(oidcStateCookieName)
	s.clearOIDCStateCookie(w)
	if cookieErr != nil || bound.Value == "" || state == "" ||
		subtle.ConstantTimeCompare([]byte(bound.Value), []byte(state)) != 1 {
		slog.Warn("httpapi: oidc callback state did not come from this browser")
		s.metrics.AuthRequests.WithLabelValues("oidc", "invalid").Inc()
		write401(w)
		return
	}

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
		// same reasoning as handleAuthLogin's identical branch.
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

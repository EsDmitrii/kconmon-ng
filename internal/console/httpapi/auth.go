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
	"strconv"
	"strings"
	"sync"
	"time"

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
		writeUnauthenticated(w, r)
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
var loginRateLimitDetail = "too many login attempts; retry shortly (limit: console.rateLimit.loginPerMinute " +
	"per username, and loginPerMinute x " + strconv.Itoa(loginIPBurstFactor) + " per source address, per minute)"

// authLoginRequest is POST /api/v1/auth/login's body.
type authLoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"` //nolint:gosec // G117: request field carrying a client-supplied plaintext password to verify, not a hardcoded credential
}

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

// handleAuthLogin verifies username/password against s.users (argon2id, via authn.VerifyPassword)
// and mints a session.
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

	/* Same-origin only: the route is public, so authorize applies no CSRF gate, and a cross-site
	   text/plain form whose body parses as this JSON would swap the victim's session for the
	   attacker's (login CSRF). */
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
	// The per-username counter below is keyed on this caller-chosen string and lives for a minute.
	if len(req.Username) > maxLoginUsernameBytes {
		writeProblem(w, http.StatusBadRequest, "invalid request", "username is too long")
		return
	}

	/* The per-username budget protects an account; the wider per-address one (loginIPBurstFactor)
	   bounds one host spraying usernames. Neither charges the trusted proxy, whose budget every user
	   behind the ingress would share. */
	clientAddr, _ := requestAddrs(r, s.trustedProxies)
	if !s.rateLimitSpend(r.Context(), rateLimitLogin, s.cfg.RateLimit.LoginPerMinute, nil, loginUserRateLimitKey(req.Username)) ||
		!s.rateLimitSpend(r.Context(), rateLimitLogin, s.cfg.RateLimit.LoginPerMinute*loginIPBurstFactor, nil,
			loginIPRateLimitKey(clientAddr)) {
		writeRateLimited(w, loginRateLimitDetail)
		return
	}

	// No user can be named with a control character or bytes that are not UTF-8, and the database
	// refuses a NUL outright, which would read as the store being down.
	user, err := store.User{}, store.ErrNotFound
	if !invalidParamText(req.Username) {
		user, err = s.users.GetUserByUsername(r.Context(), req.Username)
	}
	switch {
	case err == nil:
		// fall through to password verification below
	case errors.Is(err, store.ErrNotFound):
		// Same argon2 cost as a real verify, so the timing does not reveal whether the username exists.
		_, _ = authn.VerifyPassword(dummyPasswordHash, req.Password)
		s.metrics.AuthRequests.WithLabelValues("local", "invalid").Inc()
		writeProblem(w, http.StatusUnauthorized, "invalid credentials", "")
		return
	default:
		s.metrics.AuthRequests.WithLabelValues("local", "error").Inc()
		writeProblem(w, http.StatusServiceUnavailable, "local auth unavailable", "")
		return
	}

	// Same answer and cost as a wrong password, so a disabled account is not revealed.
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
	s.upgradePasswordHash(r.Context(), &user, req.Password)

	sessionID, err := s.sessions.Create(r.Context(), authn.Session{
		Username:      user.Username,
		DisplayName:   user.DisplayName,
		PasswordStamp: authn.SessionStamp(user.PasswordHash, user.SessionEpoch),
	})
	if err != nil {
		slog.Warn("httpapi: create session on login failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "login failed", "")
		return
	}

	s.metrics.AuthRequests.WithLabelValues("local", "ok").Inc()
	s.setSessionCookie(w, sessionID)
	if cerr := s.setCSRFCookie(w); cerr != nil {
		// A session without a csrf cookie could make no mutation, logout included.
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

// passwordRehasher is a user store that replaces a password hash only while it is still oldHash.
type passwordRehasher interface {
	RehashUserPassword(ctx context.Context, id, oldHash, newHash string) (bool, error)
}

// The store must keep satisfying it: upgradePasswordHash finds it by type assertion and would
// otherwise fall back to the read-compare-write path without a word.
var _ passwordRehasher = (*store.DB)(nil)

// upgradePasswordHash rewrites a hash made with other argon2 parameters at the current ones, after a
// login verified it. Until then a wrong guess on that account takes a different time than one on an
// unknown username, which checks the dummy hash. The salt is kept, so the password stamp and every
// session stay valid. A failure only means the next login tries again.
func (s *Server) upgradePasswordHash(ctx context.Context, user *store.User, plain string) {
	if s.userAdmin == nil || !authn.NeedsRehash(user.PasswordHash) {
		return
	}
	upgraded, err := authn.RehashPassword(user.PasswordHash, plain)
	if err != nil {
		slog.Warn("httpapi: upgrade password hash on login failed", "error", err)
		return
	}
	// An admin reset landing after the verify must not be overwritten with the old password.
	if cas, ok := s.userAdmin.(passwordRehasher); ok {
		if _, err = cas.RehashUserPassword(ctx, user.ID, user.PasswordHash, upgraded); err != nil {
			slog.Warn("httpapi: upgrade password hash on login failed", "error", err)
		}
		return
	}
	current, err := s.users.GetUserByUsername(ctx, user.Username)
	if err != nil || current.ID != user.ID || current.PasswordHash != user.PasswordHash {
		return
	}
	if err = s.userAdmin.UpdateUserPassword(ctx, user.ID, upgraded); err != nil {
		slog.Warn("httpapi: upgrade password hash on login failed", "error", err)
	}
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

// oidcMaxReturnToBytes caps the caller-controlled string the sealed state carries.
const oidcMaxReturnToBytes = 2048

// sharedAddressHintOnce logs, once per process, the likely cause of a sign-in 429 that hits every
// user at once.
var sharedAddressHintOnce sync.Once

// oidcStartRateLimitDetail is the 429 detail for GET /api/v1/auth/oidc/start.
var oidcStartRateLimitDetail = "too many sign-in attempts from this address; retry shortly " +
	"(limit: console.rateLimit.loginPerMinute x " + strconv.Itoa(loginIPBurstFactor) + " per source address per minute)"

// oidcCallbackRateLimitDetail is the 429 detail for GET /api/v1/auth/oidc/callback.
var oidcCallbackRateLimitDetail = "too many sign-in callbacks from this address; retry shortly " +
	"(limit: console.rateLimit.loginPerMinute x " + strconv.Itoa(loginIPBurstFactor) + " per source address per minute)"

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

const oidcCookiePath = "/api/v1/auth/oidc"

func (s *Server) setOIDCStateCookie(w http.ResponseWriter, state string) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: HttpOnly+SameSite are set; Secure is config-driven, as for the session cookie
		Name:     oidcStateCookieName,
		Value:    state,
		Path:     oidcCookiePath,
		HttpOnly: true,
		Secure:   s.cfg.Auth.Session.Secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(authn.OIDCStateTTL / time.Second),
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
	if !authn.IsSafeReturnTo(returnTo) {
		writeProblem(w, http.StatusBadRequest, "invalid oidc start request", "returnTo must be a same-origin relative path")
		return
	}
	/* This string travels sealed inside the state, which rides in the IdP's URL and in the state
	   cookie; uncapped, it would outgrow what a browser keeps in one cookie. A path this console can
	   actually route is far shorter than the cap. */
	if len(returnTo) > oidcMaxReturnToBytes {
		writeProblem(w, http.StatusBadRequest, "invalid oidc start request",
			fmt.Sprintf("returnTo must be at most %d bytes", oidcMaxReturnToBytes))
		return
	}
	// A start stores nothing (the state is sealed, authn's oidcState), so this is login's wide address
	// net and no more; like login's, it charges no budget the whole ingress shares.
	addr, _ := requestAddrs(r, s.trustedProxies)
	if !s.rateLimitSpend(r.Context(), rateLimitLogin, s.cfg.RateLimit.LoginPerMinute*loginIPBurstFactor, nil,
		oidcStartIPRateLimitKey(addr)) {
		if len(s.trustedProxies) == 0 {
			sharedAddressHintOnce.Do(func() {
				slog.Warn("httpapi: one client address spent its sign-in budget and no trusted proxies are configured; "+
					"behind an Ingress or a NAT every browser shares that address: list the proxy networks in "+
					"clientAddress.trustedProxyCIDRs", "address", addr)
			})
		}
		writeRateLimited(w, oidcStartRateLimitDetail)
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
	//nolint:gosec // G710: authURL is built by AuthorizeURL from the operator-configured IdP endpoint; the only caller input (returnTo) was validated by authn.IsSafeReturnTo above and only rides sealed inside the state
	http.Redirect(w, r, authURL, http.StatusFound)
}

// handleOIDCCallback consumes the IdP redirect (state + code), mints a session.
func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Auth.Mode != "oidc" || s.oidc == nil {
		writeProblem(w, http.StatusNotFound, "not found", "")
		return
	}

	// A refused callback writes an audit row, so it is bounded per address like the start.
	addr, _ := requestAddrs(r, s.trustedProxies)
	if !s.rateLimitSpend(r.Context(), rateLimitLogin, s.cfg.RateLimit.LoginPerMinute*loginIPBurstFactor, nil,
		oidcCallbackIPRateLimitKey(addr)) {
		writeRateLimited(w, oidcCallbackRateLimitDetail)
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
		s.recordAudit(r, authz.Subject{}, auditOutcomeError, emptyDetail)
		write401(w)
		return
	}

	sessionID, returnTo, err := s.oidc.Callback(r.Context(), state, code)
	if err != nil {
		slog.Warn("httpapi: oidc callback failed", "error", err)
		s.metrics.AuthRequests.WithLabelValues("oidc", authResultLabel(err)).Inc()
		s.recordAudit(r, authz.Subject{}, auditOutcomeError, emptyDetail)
		write401(w)
		return
	}

	// Both OIDC routes are GETs, which authorize does not audit; a local login is recorded, so the
	// sign-in is recorded here, as the identity it just signed in.
	signedIn := s.sessionSubject(r.Context(), sessionID)
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
		s.recordAudit(r, signedIn, auditOutcomeError, emptyDetail)
		writeProblem(w, http.StatusInternalServerError, "login failed", "")
		return
	}
	s.recordAudit(r, signedIn, auditOutcomeAllowed, emptyDetail)
	// returnTo was validated before AuthorizeURL sealed it into the state, which came back through
	// the browser: re-check so no state can ever become an open redirect. The leading-slash test is
	// spelled out at the redirect so static analysis sees the guard, not only IsSafeReturnTo.
	if returnTo != "" && returnTo[0] == '/' && (len(returnTo) == 1 || (returnTo[1] != '/' && returnTo[1] != '\\')) &&
		authn.IsSafeReturnTo(returnTo) {
		//nolint:gosec // G710: the condition above admits only a same-origin relative path
		http.Redirect(w, r, returnTo, http.StatusFound)
		return
	}
	http.Redirect(w, r, oidcDefaultReturnTo, http.StatusFound)
}

// sessionSubject is the identity a session the OIDC flow just minted belongs to, read back for the
// sign-in's audit row; empty when it cannot be read.
func (s *Server) sessionSubject(ctx context.Context, sessionID string) authz.Subject {
	if s.sessions == nil {
		return authz.Subject{}
	}
	sess, ok, err := s.sessions.Get(ctx, sessionID)
	if err != nil || !ok {
		return authz.Subject{}
	}
	return authz.Subject{Kind: authz.SubjectUser, ID: sess.Username, DisplayName: sess.DisplayName}
}

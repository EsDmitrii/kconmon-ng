package authn

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/config"
)

// OIDCSessionCookieName is config.defaults' own auth.session.cookieName
// default ("__Host-kconmon_session"). NewOIDC's original signature
// (task-15-brief.md, verbatim) carried no config.SessionConfig, so
// OIDCAuthenticator.Authenticate read this hardcoded constant regardless of
// an operator's configured auth.session.cookieName -- a non-default cookie
// name silently broke oidc-mode session lookup (httpapi's boot-time WARN,
// removed once this was fixed). Task 18 threads the configured name through
// NewOIDC's cookieName parameter instead; this constant now serves only as
// NewOIDC's fallback when cookieName is passed empty (e.g. a hand-built
// OIDCAuthenticator in a test), so it stays exported and matches the
// config-level default exactly.
const OIDCSessionCookieName = "__Host-kconmon_session"

// oidcStateKeyPrefix namespaces the PKCE verifier + returnTo pair AuthorizeURL
// stores in cache.KV, exactly as the brief specifies: "oidcstate:{state}".
const oidcStateKeyPrefix = "oidcstate:"

// oidcStateTTL is the brief's fixed 5-minute window a state/verifier pair
// survives in the KV before Get simply misses -- an abandoned login attempt
// (state minted, browser never returns) self-cleans without an explicit
// sweep, the same way SessionStore leans on its own KV TTL.
const oidcStateTTL = 5 * time.Minute

// oidcRandomBytes is 256 bits, the size the brief specifies for BOTH the CSRF
// state ("256 bits of crypto/rand") and the PKCE verifier ("32 random bytes
// -> base64url verifier") -- one constant, since the two values share the
// same shape even though they serve different purposes.
const oidcRandomBytes = 32

// accessTokenRefreshMargin is how far ahead of Session.AccessExpiry
// Authenticate proactively refreshes the access token, so an in-flight
// request never races the token's actual expiry at the IdP.
const accessTokenRefreshMargin = 2 * time.Minute

// oidcState is what AuthorizeURL stores under oidcstate:{state} and Callback
// consumes exactly once: the PKCE verifier (never sent to the browser, per
// Decision 10) and the returnTo path AuthorizeURL already validated.
type oidcState struct {
	Verifier string `json:"verifier"`
	ReturnTo string `json:"returnTo"`
}

// OIDCAuthenticator implements the confidential-client authorization-code
// flow with PKCE (SECURITY.md §10.1). Every token stays server-side: the
// browser only ever receives the __Host- session cookie (Decision 10) --
// AuthorizeURL and Callback never hand a verifier, code, or token back to
// their caller for anything other than the redirect URL / session id.
type OIDCAuthenticator struct {
	oauth2Config  oauth2.Config
	verifier      *oidc.IDTokenVerifier
	usernameClaim string
	groupsClaim   string
	sessions      *SessionStore
	kv            cache.KV
	// cookieName is the configured auth.session.cookieName (Task 18):
	// Authenticate reads the session cookie under THIS name, never the
	// OIDCSessionCookieName constant directly, so it agrees with
	// httpapi's setSessionCookie/clearSessionCookie, which have always
	// written under the configured name.
	cookieName string

	// refreshMu guards refreshLocks, the per-session-id lock table
	// maybeRefresh uses to serialize concurrent lazy refreshes of the same
	// session -- see maybeRefresh's doc comment.
	refreshMu    sync.Mutex
	refreshLocks map[string]*inflightRefresh
}

// inflightRefresh is one session id's refresh serialization point: refCount
// tracks how many goroutines currently hold or are waiting on mu, so the
// entry can be removed from refreshLocks once nobody needs it any more
// instead of growing the map for the lifetime of the process.
type inflightRefresh struct {
	mu       sync.Mutex
	refCount int
}

// acquireRefreshLock returns the inflightRefresh for sessionID, creating one
// on first use, and bumps its reference count under refreshMu. Callers must
// pair this with releaseRefreshLock once they are done (typically via
// defer), and must NOT hold the returned lock's mu yet -- that happens
// separately, after acquireRefreshLock returns.
func (a *OIDCAuthenticator) acquireRefreshLock(sessionID string) *inflightRefresh {
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()
	if a.refreshLocks == nil {
		a.refreshLocks = make(map[string]*inflightRefresh)
	}
	l, ok := a.refreshLocks[sessionID]
	if !ok {
		l = &inflightRefresh{}
		a.refreshLocks[sessionID] = l
	}
	l.refCount++
	return l
}

// releaseRefreshLock drops sessionID's reference count and deletes the entry
// from refreshLocks once nothing references it any more, so a long-lived
// process does not accumulate one lock per session id it has ever seen.
func (a *OIDCAuthenticator) releaseRefreshLock(sessionID string, l *inflightRefresh) {
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()
	l.refCount--
	if l.refCount == 0 {
		delete(a.refreshLocks, sessionID)
	}
}

// NewOIDC performs provider discovery against cfg.Issuer. Discovery failure
// is returned, not retried forever: an unreachable IdP at boot with
// auth.mode=oidc is an unusable console, and crashlooping with the discovery
// error in the log is more diagnosable than serving 500s.
//
// cookieName is the operator's configured auth.session.cookieName
// (task-18-brief.md carry-forward): Authenticate reads the session cookie
// under this name. An empty cookieName falls back to OIDCSessionCookieName
// -- the zero value a hand-built call site (a test, or a caller that has not
// been updated) would otherwise pass -- so this stays a strict superset of
// the pre-Task-18 hardcoded behavior, never a silent "no cookie ever
// matches" regression.
//
//nolint:gocritic // NewOIDC(ctx, cfg config.OIDCConfig, ...) is the task-15-brief.md interface shape (OIDCConfig by value, not by pointer); cookieName appended by Task 18.
func NewOIDC(ctx context.Context, cfg config.OIDCConfig, clientSecret string, sessions *SessionStore, kv cache.KV, cookieName string) (*OIDCAuthenticator, error) {
	if cookieName == "" {
		cookieName = OIDCSessionCookieName
	}
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("authn: oidc: discover %q: %w", cfg.Issuer, err)
	}

	usernameClaim := cfg.UsernameClaim
	if usernameClaim == "" {
		usernameClaim = "preferred_username"
	}
	groupsClaim := cfg.GroupsClaim
	if groupsClaim == "" {
		groupsClaim = "groups"
	}
	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = []string{"openid", "profile", "email", "groups"}
	}

	// AuthStyle is pinned to client_secret_basic (RFC 6749 §2.3.1) instead
	// of left at oauth2's default AuthStyleAutoDetect: go-oidc's
	// Provider.Endpoint() does not read the discovery doc's
	// token_endpoint_auth_methods_supported, so nothing pins the style for
	// us, and auto-detect's own probe-then-retry-the-other-way behavior
	// would otherwise cost a second token-endpoint round trip against any
	// IdP that rejects the first style it tries.
	endpoint := provider.Endpoint()
	endpoint.AuthStyle = oauth2.AuthStyleInHeader

	return &OIDCAuthenticator{
		oauth2Config: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: clientSecret,
			Endpoint:     endpoint,
			RedirectURL:  cfg.RedirectURL,
			Scopes:       scopes,
		},
		verifier:      provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		usernameClaim: usernameClaim,
		groupsClaim:   groupsClaim,
		sessions:      sessions,
		kv:            kv,
		cookieName:    cookieName,
	}, nil
}

func (a *OIDCAuthenticator) Mode() string { return "oidc" }

// AuthorizeURL mints a fresh 256-bit CSRF state and a 32-byte PKCE verifier,
// stores {verifier, returnTo} in the KV under oidcstate:{state} with a
// 5-minute TTL, and returns the IdP authorization URL carrying
// response_type=code, the S256 code_challenge derived from the verifier, and
// the configured scopes. returnTo is validated as a same-origin relative path
// BEFORE anything is stored -- an open redirect through the login flow is the
// classic bug this class of check exists to close, so it happens first, not
// as an afterthought once the state is already minted. No nonce is minted:
// nonce is REQUIRED only for the implicit/hybrid flows, where the ID token
// comes straight off the redirect with nothing else binding it to this
// request; here, PKCE's code_verifier/code_challenge pair already closes the
// authorization-code-injection attack a nonce exists to prevent.
func (a *OIDCAuthenticator) AuthorizeURL(ctx context.Context, returnTo string) (authURL string, err error) {
	if !isSafeReturnTo(returnTo) {
		return "", fmt.Errorf("authn: oidc: unsafe returnTo %q", returnTo)
	}

	state, err := randomURLSafeString(oidcRandomBytes)
	if err != nil {
		return "", fmt.Errorf("authn: oidc: generate state: %w", err)
	}
	verifier, err := randomURLSafeString(oidcRandomBytes)
	if err != nil {
		return "", fmt.Errorf("authn: oidc: generate verifier: %w", err)
	}

	data, err := json.Marshal(oidcState{Verifier: verifier, ReturnTo: returnTo})
	if err != nil {
		return "", fmt.Errorf("authn: oidc: marshal state: %w", err)
	}
	if err := a.kv.Set(ctx, oidcStateKey(state), data, oidcStateTTL); err != nil {
		return "", fmt.Errorf("authn: oidc: store state: %w", err)
	}

	return a.oauth2Config.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier)), nil
}

// Callback consumes state (deleting it from the KV on this call regardless of
// what happens afterward -- delete-on-first-use, so a replayed callback
// ordinarily fails once the first call's Delete has landed, even if the
// token exchange below fails and this call is retried by the caller),
// exchanges code for tokens using the stored PKCE verifier, verifies the ID
// token (issuer, audience, expiry -- go-oidc's IDTokenVerifier.Verify, not
// reimplemented here), and creates a session. An unknown or already-consumed
// state is answered with ErrInvalid before the token endpoint is ever
// contacted.
//
// Get-then-Delete against the KV here is NOT atomic (cache.KV has no
// compare-and-delete), so two callbacks racing on the exact same state can
// both pass the Get check before either Delete runs. The backstop for that
// narrow window is the IdP itself, not this code: an authorization code is
// single-use per RFC 6749 §4.1.2, so at most one of the two racing token
// exchanges below can ever succeed.
func (a *OIDCAuthenticator) Callback(ctx context.Context, state, code string) (sessionID, returnTo string, err error) {
	data, ok, err := a.kv.Get(ctx, oidcStateKey(state))
	if err != nil {
		return "", "", fmt.Errorf("authn: oidc: get state: %w", err)
	}
	if !ok {
		return "", "", fmt.Errorf("%w: unknown or already-consumed oidc state", ErrInvalid)
	}
	if delErr := a.kv.Delete(ctx, oidcStateKey(state)); delErr != nil {
		return "", "", fmt.Errorf("authn: oidc: delete state: %w", delErr)
	}

	var st oidcState
	if unmarshalErr := json.Unmarshal(data, &st); unmarshalErr != nil {
		return "", "", fmt.Errorf("authn: oidc: corrupted state value: %w", unmarshalErr)
	}

	token, err := a.oauth2Config.Exchange(ctx, code, oauth2.VerifierOption(st.Verifier))
	if err != nil {
		return "", "", fmt.Errorf("%w: token exchange: %v", ErrInvalid, err) //nolint:errorlint // ErrInvalid is the sentinel being minted here, not something to further unwrap
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return "", "", fmt.Errorf("%w: token response carried no id_token", ErrInvalid)
	}
	idToken, err := a.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return "", "", fmt.Errorf("%w: verify id token: %v", ErrInvalid, err) //nolint:errorlint // same as above
	}

	var claims map[string]any
	if claimsErr := idToken.Claims(&claims); claimsErr != nil {
		return "", "", fmt.Errorf("authn: oidc: decode id token claims: %w", claimsErr)
	}

	username := claimString(claims, a.usernameClaim)
	if username == "" {
		// A subject with no stable id is unusable -- sub is always present on
		// a verified ID token (go-oidc requires it), so this is the fallback
		// the brief specifies, not a second failure mode.
		username = idToken.Subject
	}
	if username == "" {
		return "", "", fmt.Errorf("%w: id token carries no usable subject identifier", ErrInvalid)
	}

	accessExpiry := token.Expiry
	if accessExpiry.IsZero() && token.RefreshToken != "" {
		// Some IdPs omit expires_in on the initial exchange while still
		// issuing a refresh token. Leaving AccessExpiry zero here would make
		// maybeRefresh's own IsZero check treat this session as never
		// needing revalidation -- but the lazy refresh call is the ONLY
		// IdP-side revocation check this design has, so a session that can
		// never trigger one can never be revoked short of its own TTL. Stamp
		// a synthetic near-term expiry so the refresh path still engages.
		accessExpiry = time.Now().Add(5 * time.Minute)
	}

	id, err := a.sessions.Create(ctx, Session{
		Username:     username,
		DisplayName:  username,
		Groups:       claimGroups(claims, a.groupsClaim),
		RefreshToken: token.RefreshToken,
		AccessExpiry: accessExpiry,
	})
	if err != nil {
		return "", "", fmt.Errorf("authn: oidc: create session: %w", err)
	}

	return id, st.ReturnTo, nil
}

// Authenticate resolves the session cookie into a Subject, refreshing the
// OIDC access token first when it is at or near expiry (see maybeRefresh).
func (a *OIDCAuthenticator) Authenticate(r *http.Request) (authz.Subject, error) {
	cookie, err := r.Cookie(a.cookieName)
	if err != nil || cookie.Value == "" {
		return authz.Subject{}, ErrNoCredentials
	}

	sess, ok, err := a.sessions.Get(r.Context(), cookie.Value)
	if err != nil {
		return authz.Subject{}, fmt.Errorf("authn: oidc: get session: %w", err)
	}
	if !ok {
		return authz.Subject{}, ErrNoCredentials
	}

	sess, err = a.maybeRefresh(r.Context(), sess)
	if err != nil {
		return authz.Subject{}, err
	}

	// ID carries the resolved username claim here, not a users.id UUID
	// (authz.go's Subject.ID doc): OIDC user provisioning does not exist
	// yet, so per-user (subject_kind='user') role bindings never resolve
	// for an OIDC subject -- only group bindings and defaultRole apply.
	return authz.Subject{
		Kind:        authz.SubjectUser,
		ID:          sess.Username,
		DisplayName: sess.DisplayName,
		Groups:      sess.Groups,
	}, nil
}

// maybeRefresh is the server-side, lazy refresh SECURITY.md requires: a
// session with no AccessExpiry recorded (never tracked one, e.g. an IdP that
// issued no refresh token) or comfortably ahead of it is returned unchanged,
// with no request to the IdP at all. A session at or within
// accessTokenRefreshMargin of AccessExpiry, and holding a refresh token, is
// refreshed and re-persisted under its existing session id (via the KV
// directly, not SessionStore.Create, which would mint a NEW id the
// browser's cookie does not carry). A refresh that the IdP rejects
// (revoked/expired refresh token, 400 invalid_grant) means this session can
// never be extended again, so it is invalidated outright -- ErrExpired,
// never a 500 -- rather than left to ride out its own ExpiresAt.
//
// Concurrency guarantee: this is NOT "exactly one token-endpoint call" for
// the process as a whole -- different sessions refresh independently and
// concurrently. What it guarantees is per-session-id serialization: every
// concurrent Authenticate call sharing the same session id contends on the
// same inflightRefresh lock (acquireRefreshLock/releaseRefreshLock above),
// so only one of them ever calls the IdP's token endpoint for that session
// at a time. Everyone else blocks on the lock, then -- inside the critical
// section -- re-reads the session from the store before doing anything
// else; if the winner already refreshed it (AccessExpiry now clear of the
// margin, or the session gone because the winner's refresh was rejected),
// the loser returns that outcome directly with no second token call. This
// is what closes the original bug: parallel requests sharing a cookie used
// to each call TokenSource with the SAME refresh token, which under IdP
// refresh-token rotation + reuse detection (Auth0's default, Keycloak's
// revoke-on-reuse) gets the whole grant family revoked, and even without
// rotation left persistSession as last-write-wins over an already-consumed
// token.
//
//nolint:gocritic // Session is passed by value throughout this package (session.go's Create/Get/Refresh); consistency over a local micro-optimization.
func (a *OIDCAuthenticator) maybeRefresh(ctx context.Context, sess Session) (Session, error) {
	if sess.AccessExpiry.IsZero() || sess.RefreshToken == "" {
		return sess, nil
	}
	if time.Now().Add(accessTokenRefreshMargin).Before(sess.AccessExpiry) {
		return sess, nil
	}

	lock := a.acquireRefreshLock(sess.ID)
	lock.mu.Lock()
	defer func() {
		lock.mu.Unlock()
		a.releaseRefreshLock(sess.ID, lock)
	}()

	// Re-read: whoever held the lock before us may have already refreshed
	// (or invalidated) this exact session while we were waiting for it.
	current, ok, err := a.sessions.Get(ctx, sess.ID)
	if err != nil {
		return Session{}, fmt.Errorf("authn: oidc: reread session before refresh: %w", err)
	}
	if !ok {
		// A prior holder's refresh was rejected by the IdP and deleted the
		// session -- there is nothing left to extend.
		return Session{}, ErrExpired
	}
	if current.AccessExpiry.IsZero() || time.Now().Add(accessTokenRefreshMargin).Before(current.AccessExpiry) {
		// A prior holder already refreshed it past the margin; nothing left
		// for us to do, and no second call to the token endpoint.
		return current, nil
	}

	refreshed, err := a.oauth2Config.TokenSource(ctx, &oauth2.Token{RefreshToken: current.RefreshToken}).Token()
	if err != nil {
		if delErr := a.sessions.Delete(ctx, current.ID); delErr != nil {
			slog.Warn("authn: oidc: delete session after failed refresh", "session_id", current.ID, "error", delErr) //nolint:gosec // G706: current.ID is a server-generated base64url(32 random bytes) session id (session.go's newSessionID), logged as a structured slog field, not interpolated into anything rendered
		}
		return Session{}, ErrExpired
	}

	current.RefreshToken = refreshed.RefreshToken
	current.AccessExpiry = refreshed.Expiry
	if current.AccessExpiry.IsZero() && current.RefreshToken != "" {
		// Same guard as Callback's: an IdP omitting expires_in on the
		// REFRESH response would otherwise persist a zero AccessExpiry and
		// permanently disable revalidation for the rest of the session.
		current.AccessExpiry = time.Now().Add(5 * time.Minute)
	}
	if err := a.persistSession(ctx, current); err != nil {
		return Session{}, fmt.Errorf("authn: oidc: persist refreshed session: %w", err)
	}
	return current, nil
}

// persistSession rewrites sess in place under its existing KV key, mirroring
// SessionStore.Create's own marshal-and-Set (including the same floor-1s KV
// TTL derivation from sess.ExpiresAt), without minting a new session id --
// see maybeRefresh's doc comment for why that distinction matters here.
//
//nolint:gocritic // Session is passed by value throughout this package; see maybeRefresh's nolint above.
func (a *OIDCAuthenticator) persistSession(ctx context.Context, sess Session) error {
	data, err := json.Marshal(sess) //nolint:gosec // G117: server-side session record persisted to the KV store; RefreshToken lives there by design and is never sent to clients
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	ttl := time.Until(sess.ExpiresAt)
	if ttl < time.Second {
		ttl = time.Second
	}
	return a.kv.Set(ctx, sessionKey(sess.ID), data, ttl)
}

func oidcStateKey(state string) string {
	return oidcStateKeyPrefix + state
}

// randomURLSafeString draws n bytes of crypto/rand and returns their
// base64url (RawURLEncoding, no padding) encoding -- the same "N raw bytes"
// convention session.go's newSessionID and token.go's EncodeToken already
// use, applied here to both the CSRF state and the PKCE verifier.
func randomURLSafeString(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// isSafeReturnTo reports whether returnTo is safe to redirect the browser to
// after login: a same-origin relative path, never an absolute URL (which
// could carry an attacker-controlled scheme or host) and never a
// scheme-relative "//host/..." path (which browsers resolve against
// whatever host follows the slashes, exactly like an absolute URL). A
// backslash is rejected too: some browsers normalize a leading "/\" into
// "//", the same scheme-relative bypass by another spelling.
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

// claimString returns claims[key] as a string, or "" when the key is absent
// or not a string.
func claimString(claims map[string]any, key string) string {
	s, _ := claims[key].(string)
	return s
}

// claimGroups reads claims[key] as either a JSON array of strings (the
// common case) or a single bare string -- some IdPs put a lone group
// membership straight in the claim rather than a one-element array -- and
// normalizes both into a []string. An absent claim, or one of any other
// shape, returns nil: the brief's "groups scope is requested but not
// required" -- a subject with no groups is not an error here, it is a
// subject only defaultRole will apply to, decided downstream in the
// authorize middleware, not this package.
func claimGroups(claims map[string]any, key string) []string {
	switch v := claims[key].(type) {
	case []any:
		groups := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				groups = append(groups, s)
			}
		}
		return groups
	case string:
		if v == "" {
			return nil
		}
		return []string{v}
	default:
		return nil
	}
}

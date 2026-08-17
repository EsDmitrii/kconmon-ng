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

// OIDCSessionCookieName is config.defaults' own auth.session.cookieName default
// ("__Host-kconmon_session").
const OIDCSessionCookieName = "__Host-kconmon_session"

// oidcStateKeyPrefix namespaces the PKCE verifier + returnTo pair AuthorizeURL stores in cache.KV.
const oidcStateKeyPrefix = "oidcstate:"

// oidcStateTTL is the fixed 5-minute window a state/verifier pair survives in the KV before Get
// simply misses.
const oidcStateTTL = 5 * time.Minute

// oidcRandomBytes is 256 bits, the size specifies for BOTH the CSRF state ("256 bits of
// crypto/rand") and the PKCE verifier ("32 random bytes -> base64url verifier").
const oidcRandomBytes = 32

// accessTokenRefreshMargin is how far ahead of Session.AccessExpiry
// Authenticate proactively refreshes the access token, so an in-flight
// request never races the token's actual expiry at the IdP.
const accessTokenRefreshMargin = 2 * time.Minute

// oidcState is what AuthorizeURL stores under oidcstate:{state} and Callback consumes exactly once.
type oidcState struct {
	Verifier string `json:"verifier"`
	ReturnTo string `json:"returnTo"`
}

// OIDCAuthenticator implements the confidential-client authorization-code flow with PKCE
// (SECURITY.md §10.1); every token stays server-side: the browser only ever receives the __Host-
// session cookie.
type OIDCAuthenticator struct {
	oauth2Config  oauth2.Config
	verifier      *oidc.IDTokenVerifier
	usernameClaim string
	groupsClaim   string
	sessions      *SessionStore
	kv            cache.KV
	// cookieName is the configured auth.session.cookieName: Authenticate reads the session cookie
	// under THIS name.
	cookieName string

	// refreshMu guards refreshLocks, the per-session-id lock table
	// maybeRefresh uses to serialize concurrent lazy refreshes of the same
	// session -- see maybeRefresh's doc comment.
	refreshMu    sync.Mutex
	refreshLocks map[string]*inflightRefresh
}

// inflightRefresh is one session id's refresh serialization point.
type inflightRefresh struct {
	mu       sync.Mutex
	refCount int
}

// acquireRefreshLock returns the inflightRefresh for sessionID, creating one on first use; callers
// must pair this with releaseRefreshLock once they are done (typically via defer).
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

// NewOIDC performs provider discovery against cfg.Issuer; an empty cookieName falls back to
// OIDCSessionCookieName.
//
//nolint:gocritic // NewOIDC(ctx, cfg config.OIDCConfig, ...)
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

	// AuthStyle is pinned to client_secret_basic (RFC 6749 §2.3.1) instead of left at oauth2's default
	// AuthStyleAutoDetect.
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

// AuthorizeURL mints a fresh 256-bit CSRF state and a 32-byte PKCE verifier; no nonce is minted:
// nonce is REQUIRED only for the implicit/hybrid flows.
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

// Callback consumes state (deleting it from the KV on this call regardless of what happens
// afterward -- delete-on-first-use, so a replayed callback ordinarily fails once the first call's
// Delete has landed, even if the token exchange below fails and this call is retried by the
// caller), exchanges code for tokens using the stored PKCE verifier, verifies the ID token (issuer,
// audience, expiry -- go-oidc's IDTokenVerifier.Verify, not reimplemented here), and creates a
// session.
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

	// sub, and ONLY sub. It used to fall back the other way — the configured username claim first,
	// sub only if that was missing — which made a mutable, reassignable string the key an RBAC
	// binding hangs off (identity.go; OIDC Core §5.7). A login that cannot produce a sub is refused
	// rather than quietly keyed on something weaker.
	if idToken.Subject == "" {
		return "", "", fmt.Errorf("%w: id token carries no sub claim", ErrInvalid)
	}
	if reservedIdentity(idToken.Subject) {
		return "", "", fmt.Errorf("%w: id token sub claims a reserved identity namespace", ErrInvalid)
	}
	identity := IdentityPrefixOIDC + idToken.Subject

	accessExpiry := token.Expiry
	if accessExpiry.IsZero() && token.RefreshToken != "" {
		// Leaving AccessExpiry zero here would make maybeRefresh's own IsZero check treat this session as
		// never needing revalidation.
		accessExpiry = time.Now().Add(5 * time.Minute)
	}

	id, err := a.sessions.Create(ctx, Session{
		Username:     identity,
		DisplayName:  a.displayName(claims, idToken.Subject),
		Groups:       claimGroups(claims, a.groupsClaim),
		RefreshToken: token.RefreshToken,
		AccessExpiry: accessExpiry,
	})
	if err != nil {
		return "", "", fmt.Errorf("authn: oidc: create session: %w", err)
	}

	return id, st.ReturnTo, nil
}

// displayName is what a HUMAN reading the audit log or the header menu sees, and it is deliberately
// the opposite trade from the identity above: the friendliest claim available, in the order a person
// would recognise themselves — the configured username claim, then name, then email — falling back
// to the sub the identity is already keyed on. None of it is load-bearing: nothing authorizes on a
// display name, so a claim changing here changes a label and nothing else.
func (a *OIDCAuthenticator) displayName(claims map[string]any, sub string) string {
	for _, claim := range []string{a.usernameClaim, "name", "email"} {
		if claim == "" {
			continue
		}
		if v := claimString(claims, claim); v != "" {
			return v
		}
	}
	return sub
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

	/* A session minted before identity became "oidc:"+sub carries a bare username, and Valkey keeps
	   it across the upgrade for the rest of its 12h TTL. Honouring it would keep the LEGACY bindings
	   granting — precisely what the boot warning tells the operator has stopped happening — and it
	   can never match the refresh path's subject check, so its groups would freeze too. It is not an
	   identity this build can resolve, so it ends here: the session is dropped and the browser is
	   sent back through the IdP, which mints the same person under their sub. */
	if !strings.HasPrefix(sess.Username, IdentityPrefixOIDC) {
		if delErr := a.sessions.Delete(r.Context(), sess.ID); delErr != nil {
			slog.Warn("authn: oidc: delete pre-upgrade session", "error", delErr)
		}
		return authz.Subject{}, ErrExpired
	}

	sess, err = a.maybeRefresh(r.Context(), sess)
	if err != nil {
		return authz.Subject{}, err
	}

	// ID is "oidc:"+sub (identity.go), not a users.id UUID (authz.go's Subject.ID doc).
	return authz.Subject{
		Kind:        authz.SubjectUser,
		ID:          sess.Username,
		DisplayName: sess.DisplayName,
		Groups:      sess.Groups,
	}, nil
}

// adoptRefreshedClaims re-reads group membership from the id_token a refresh returned.
//
// Without this a session's groups were whatever the IdP said at LOGIN and stayed that way for the
// session's whole life: revoke someone's group at the IdP and the console kept honouring the group
// bindings until they happened to log out. Refresh is the moment the IdP re-states who this is, so
// it is the moment to believe it.
//
// Everything here is best-effort ON PURPOSE. A provider that returns no id_token on refresh (many
// do not) or one that fails verification leaves the session exactly as it was rather than dropping
// the caller to no groups at all — an empty group list is a silent, total deauthorization, and
// inventing one out of a missing optional field would be worse than the staleness it fixes. The
// SUBJECT is never adopted from here: a refresh that came back for a different sub is not this
// session, and the session is dropped rather than re-pointed.
func (a *OIDCAuthenticator) adoptRefreshedClaims(ctx context.Context, sess *Session, refreshed *oauth2.Token) {
	rawIDToken, ok := refreshed.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return
	}
	idToken, err := a.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		slog.Warn("authn: oidc: refreshed id token failed verification; keeping the session's existing groups", "error", err)
		return
	}
	/* An id_token with NO sub cannot be shown to belong to this session, and the login path already
	   refuses one outright; adopting claims from it here would be the same trust through a side door. */
	if idToken.Subject == "" || IdentityPrefixOIDC+idToken.Subject != sess.Username {
		slog.Warn("authn: oidc: refreshed id token does not identify this session; keeping its existing groups")
		return
	}
	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		return
	}
	/* PRESENT, not merely truthy. A refresh id_token is often minimal — sub, iss, aud, exp and
	   nothing else — and reading an ABSENT groups claim as "no groups" would deauthorize the session
	   completely on a token that said nothing about groups at all. An explicit empty array is a
	   different statement and IS adopted: that is the IdP revoking membership. */
	if _, present := claims[a.groupsClaim]; !present {
		return
	}
	sess.Groups = claimGroups(claims, a.groupsClaim)
	if name := a.displayName(claims, idToken.Subject); name != "" {
		sess.DisplayName = name
	}
}

// maybeRefresh is the server-side, lazy refresh SECURITY.md requires.
//
//nolint:gocritic // Session is passed by value throughout this package (session.go's Create/Get/Refresh)
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
	a.adoptRefreshedClaims(ctx, &current, refreshed)
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

// persistSession rewrites sess in place under its existing KV key.
//
//nolint:gocritic // Session is passed by value throughout this package; see maybeRefresh's nolint above
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

// randomURLSafeString draws n bytes of crypto/rand and returns their base64url (RawURLEncoding, no
// padding) encoding.
func randomURLSafeString(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// isSafeReturnTo reports whether returnTo is safe to redirect the browser to after login: a
// same-origin relative path.
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

// claimGroups reads claims[key] as either a JSON array of strings (the common case) or a single
// bare string.
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

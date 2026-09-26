package authn

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
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

// oidcStateKeyPrefix namespaces the record Callback leaves for a state it has signed someone in with.
const oidcStateKeyPrefix = "oidcstate:"

// OIDCStateTTL is how long a started sign-in may take to come back; the state cookie lives as long.
const OIDCStateTTL = 5 * time.Minute

// oidcStateKeyInfo is the HKDF info the state-sealing key is derived from the client secret under.
const oidcStateKeyInfo = "kconmon-ng oidc state"

// oidcRandomBytes is 256 bits, the size specifies for BOTH the CSRF state ("256 bits of
// crypto/rand") and the PKCE verifier ("32 random bytes -> base64url verifier").
const oidcRandomBytes = 32

// oidcVerifierLen is the PKCE verifier's length: oidcRandomBytes in unpadded base64url.
const oidcVerifierLen = (oidcRandomBytes*8 + 5) / 6

// accessTokenRefreshMargin is how far ahead of Session.AccessExpiry
// Authenticate proactively refreshes the access token, so an in-flight
// request never races the token's actual expiry at the IdP.
const accessTokenRefreshMargin = 2 * time.Minute

// refreshTimeout bounds a lazy refresh, which runs detached from the request that triggered it.
const refreshTimeout = 15 * time.Second

// refreshLockTTL is the session write lock's lifetime during a refresh: longer than the refresh's
// own budget, so the lock cannot lapse under a holder that is still talking to the IdP.
const refreshLockTTL = refreshTimeout + 5*time.Second

// refreshLockPoll is how often a replica waiting on another replica's refresh re-reads the session.
const refreshLockPoll = 100 * time.Millisecond

/*
oidcState is what the state parameter carries, sealed (AES-GCM) under a key derived from the client
secret, which every replica holds. Starting a sign-in is public, so it stores nothing: a record per
start could only be bounded by budgets keyed on a client address, and behind a proxy that address is
the proxy's word.
*/
type oidcState struct {
	Verifier string
	ReturnTo string
	Expires  int64
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
	stateAEAD     cipher.AEAD
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

	stateAEAD, err := newStateAEAD(clientSecret)
	if err != nil {
		return nil, fmt.Errorf("authn: oidc: state key: %w", err)
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
		stateAEAD:     stateAEAD,
		kv:            kv,
		cookieName:    cookieName,
	}, nil
}

func newStateAEAD(clientSecret string) (cipher.AEAD, error) {
	key, err := hkdf.Key(sha256.New, []byte(clientSecret), nil, oidcStateKeyInfo, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (a *OIDCAuthenticator) Mode() string { return "oidc" }

// AuthorizeURL mints a 32-byte PKCE verifier and seals it, with returnTo and an expiry, into the
// state; no nonce is minted: nonce is REQUIRED only for the implicit/hybrid flows.
func (a *OIDCAuthenticator) AuthorizeURL(_ context.Context, returnTo string) (authURL string, err error) {
	if !IsSafeReturnTo(returnTo) {
		return "", fmt.Errorf("authn: oidc: unsafe returnTo %q", returnTo)
	}

	verifier, err := randomURLSafeString(oidcRandomBytes)
	if err != nil {
		return "", fmt.Errorf("authn: oidc: generate verifier: %w", err)
	}
	state, err := a.sealState(oidcState{Verifier: verifier, ReturnTo: returnTo, Expires: time.Now().Add(OIDCStateTTL).Unix()})
	if err != nil {
		return "", err
	}

	return a.oauth2Config.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier)), nil
}

// sealState is st as a state parameter, base64url: a random nonce, then sealed the expiry (8 bytes,
// big-endian Unix seconds), the verifier and returnTo, unescaped, so that the longest returnTo the
// console accepts still fits in the state cookie.
func (a *OIDCAuthenticator) sealState(st oidcState) (string, error) {
	if len(st.Verifier) != oidcVerifierLen {
		return "", fmt.Errorf("authn: oidc: verifier is %d characters, want %d", len(st.Verifier), oidcVerifierLen)
	}
	data := binary.BigEndian.AppendUint64(nil, uint64(st.Expires)) //nolint:gosec // G115: a Unix time, read back the same way
	data = append(append(data, st.Verifier...), st.ReturnTo...)
	nonce := make([]byte, a.stateAEAD.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("authn: oidc: generate state nonce: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(a.stateAEAD.Seal(nonce, nonce, data, nil)), nil
}

// openState reads back a state sealState made and has not expired; anything else is ErrInvalid.
func (a *OIDCAuthenticator) openState(state string) (oidcState, error) {
	sealed, err := base64.RawURLEncoding.DecodeString(state)
	if err != nil || len(sealed) < a.stateAEAD.NonceSize() {
		return oidcState{}, fmt.Errorf("%w: malformed oidc state", ErrInvalid)
	}
	nonce, ciphertext := sealed[:a.stateAEAD.NonceSize()], sealed[a.stateAEAD.NonceSize():]
	data, err := a.stateAEAD.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return oidcState{}, fmt.Errorf("%w: oidc state was not issued by this console", ErrInvalid)
	}
	if len(data) < 8+oidcVerifierLen {
		return oidcState{}, fmt.Errorf("authn: oidc: corrupted state value of %d bytes", len(data))
	}
	st := oidcState{
		Expires:  int64(binary.BigEndian.Uint64(data)), //nolint:gosec // G115: written by sealState from an int64
		Verifier: string(data[8 : 8+oidcVerifierLen]),
		ReturnTo: string(data[8+oidcVerifierLen:]),
	}
	if time.Now().Unix() > st.Expires {
		return oidcState{}, fmt.Errorf("%w: oidc state expired", ErrInvalid)
	}
	return st, nil
}

// Callback opens state (sealState's), exchanges code for tokens using the PKCE verifier it carries,
// verifies the ID token (issuer, audience, expiry -- go-oidc's IDTokenVerifier.Verify, not
// reimplemented here), and creates a session. A state signs in once: the first exchange that
// succeeds records it until it expires, and a replay is refused before the token endpoint is asked.
func (a *OIDCAuthenticator) Callback(ctx context.Context, state, code string) (sessionID, returnTo string, err error) {
	st, err := a.openState(state)
	if err != nil {
		return "", "", err
	}
	consumed := oidcStateKey(state)
	switch _, used, getErr := a.kv.Get(ctx, consumed); {
	case getErr != nil:
		return "", "", fmt.Errorf("authn: oidc: get state: %w", getErr)
	case used:
		return "", "", fmt.Errorf("%w: oidc state already used", ErrInvalid)
	}

	token, err := a.oauth2Config.Exchange(ctx, code, oauth2.VerifierOption(st.Verifier))
	if err != nil {
		return "", "", fmt.Errorf("%w: token exchange: %v", ErrInvalid, err) //nolint:errorlint // ErrInvalid is the sentinel being minted here, not something to further unwrap
	}
	// Recorded only once the IdP has accepted a code, so a record costs a real sign-in.
	first, err := a.kv.SetNX(ctx, consumed, []byte{1}, max(time.Until(time.Unix(st.Expires, 0)), time.Second))
	if err != nil {
		return "", "", fmt.Errorf("authn: oidc: record state: %w", err)
	}
	if !first {
		return "", "", fmt.Errorf("%w: oidc state already used", ErrInvalid)
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

	// The identity is sub and only sub: a username claim is mutable and reassignable, and RBAC bindings
	// hang off this key (identity.go; OIDC Core §5.7). A login without a sub is refused.
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
		return authz.Subject{}, fmt.Errorf("authn: oidc: get session: %w: %w", ErrUnavailable, err)
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

	/* The mutex above serializes this process only. The session's KV write lock serializes every
	   replica sharing the store: two replicas posting the same refresh token to an IdP that rotates
	   it would have the second refused, and that refusal deletes the session. */
	deadline := time.Now().Add(refreshLockTTL)
	for {
		token, locked, err := a.sessions.lockSession(ctx, sess.ID, refreshLockTTL)
		if err != nil {
			return Session{}, fmt.Errorf("authn: oidc: %w: %w", ErrUnavailable, err)
		}
		if locked {
			defer a.sessions.unlockSession(ctx, sess.ID, token)
			return a.refreshLocked(ctx, sess.ID)
		}

		current, ok, err := a.sessions.Get(ctx, sess.ID)
		if err != nil {
			return Session{}, fmt.Errorf("authn: oidc: reread session while another replica refreshes it: %w: %w", ErrUnavailable, err)
		}
		if !ok {
			return Session{}, ErrExpired
		}
		// Refreshed by the holder, or still usable while the holder works: either way, serve it.
		if current.AccessExpiry.IsZero() || time.Now().Before(current.AccessExpiry) {
			return current, nil
		}
		if time.Now().After(deadline) {
			return Session{}, errors.New("authn: oidc: another replica is still refreshing this session")
		}
		select {
		case <-ctx.Done():
			return Session{}, fmt.Errorf("authn: oidc: wait for another replica's refresh: %w", ctx.Err())
		case <-time.After(refreshLockPoll):
		}
	}
}

// refreshLocked is the refresh itself; the caller holds the session's write lock.
func (a *OIDCAuthenticator) refreshLocked(ctx context.Context, sessionID string) (Session, error) {
	// Re-read: whoever held the lock before us may have already refreshed
	// (or invalidated) this exact session while we were waiting for it.
	current, ok, err := a.sessions.Get(ctx, sessionID)
	if err != nil {
		return Session{}, fmt.Errorf("authn: oidc: reread session before refresh: %w: %w", ErrUnavailable, err)
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

	// Detached from the request: a client that goes away mid-refresh, or the WebSocket revalidator's
	// short budget, must not leave a rotated refresh token unsaved or read as the IdP's refusal.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
	defer cancel()
	refreshed, err := a.oauth2Config.TokenSource(ctx, &oauth2.Token{RefreshToken: current.RefreshToken}).Token()
	if err != nil {
		if !refreshRefused(err) {
			// The IdP is unreachable or failing: no verdict on the grant. Serve what the session
			// already proves and try again on the next request.
			slog.Warn("authn: oidc: refresh failed, keeping the session", "error", err)
			if time.Now().Before(current.AccessExpiry) {
				return current, nil
			}
			return Session{}, fmt.Errorf("authn: oidc: refresh: %w", err)
		}
		if delErr := a.sessions.Delete(ctx, current.ID); delErr != nil {
			slog.Warn("authn: oidc: delete session after failed refresh", "error", delErr)
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
	stored, err := a.sessions.rewrite(ctx, current)
	if err != nil {
		return Session{}, fmt.Errorf("authn: oidc: persist refreshed session: %w", err)
	}
	if !stored {
		// Logged out (or expired) while the IdP was answering: that stays the outcome.
		return Session{}, ErrExpired
	}
	return current, nil
}

// refreshRefused reports whether a failed refresh is the IdP's answer about the grant itself:
// invalid_grant, or any other 4xx from the token endpoint. Transport errors, timeouts, 5xx and the
// throttling statuses 408 and 429 are not.
func refreshRefused(err error) bool {
	re, ok := errors.AsType[*oauth2.RetrieveError](err)
	if !ok {
		return false
	}
	if re.ErrorCode == "invalid_grant" {
		return true
	}
	if re.Response == nil {
		return false
	}
	code := re.Response.StatusCode
	if code == http.StatusRequestTimeout || code == http.StatusTooManyRequests {
		return false
	}
	return code >= 400 && code < 500
}

func oidcStateKey(state string) string {
	sum := sha256.Sum256([]byte(state))
	return oidcStateKeyPrefix + hex.EncodeToString(sum[:])
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

// IsSafeReturnTo reports whether returnTo is safe to redirect the browser to after login: a
// same-origin relative path. "//host" and "/\\host" are protocol-relative to a browser, so the
// second character is checked explicitly.
func IsSafeReturnTo(returnTo string) bool {
	if returnTo == "" || returnTo[0] != '/' {
		return false
	}
	if len(returnTo) > 1 && (returnTo[1] == '/' || returnTo[1] == '\\') {
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

// SafeReturnTo returns returnTo when IsSafeReturnTo accepts it, and fallback otherwise.
func SafeReturnTo(returnTo, fallback string) string {
	if IsSafeReturnTo(returnTo) {
		return returnTo
	}
	return fallback
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

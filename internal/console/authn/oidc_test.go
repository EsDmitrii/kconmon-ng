package authn_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/config"
)

const (
	testClientID     = "console-client"
	testClientSecret = "console-secret"
	testKeyID        = "test-key-1"
)

// sharedTestRSAKey is generated once for the whole file.
var (
	sharedTestRSAKeyOnce sync.Once
	sharedTestRSAKeyVal  *rsa.PrivateKey
)

func sharedTestRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	sharedTestRSAKeyOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate test RSA key: %v", err)
		}
		sharedTestRSAKeyVal = key
	})
	return sharedTestRSAKeyVal
}

// fakeIDP is a hand-written OpenID Connect provider double.
type fakeIDP struct {
	t   *testing.T
	key *rsa.PrivateKey
	srv *httptest.Server

	mu               sync.Mutex
	pendingCode      string
	pendingChallenge string
	claims           map[string]any
	refreshClaims    map[string]any
	refreshToken     string
	failRefresh      bool
	refreshStatus    int // non-zero: the refresh grant answers this status instead
	// rotateRefresh makes every accepted refresh grant mint a new refresh token and refuse the old one
	// from then on (Keycloak with revokeRefreshToken, Auth0/Okta rotation); refreshDelay holds the
	// response after the grant is accepted, and refreshAccepted is closed on the first acceptance.
	rotateRefresh   bool
	refreshDelay    time.Duration
	refreshAccepted chan struct{}
	refreshSeq      int

	tokenRequests atomic.Int32
}

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	f := &fakeIDP{t: t, key: sharedTestRSAKey(t), refreshToken: "initial-refresh-token"}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", f.serveDiscovery)
	mux.HandleFunc("/keys", f.serveKeys)
	mux.HandleFunc("/authorize", f.serveAuthorize)
	mux.HandleFunc("/token", f.serveToken)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)

	f.claims = f.defaultClaims()
	return f
}

func (f *fakeIDP) issuer() string { return f.srv.URL }

func (f *fakeIDP) defaultClaims() map[string]any {
	now := time.Now()
	return map[string]any{
		"iss":                f.issuer(),
		"aud":                testClientID,
		"sub":                "user-sub-1",
		"iat":                now.Unix(),
		"exp":                now.Add(time.Hour).Unix(),
		"preferred_username": "alice",
		"groups":             []string{"admins", "sre"},
	}
}

// setClaims replaces the claim set the NEXT successful authorization_code exchange will sign into
// the ID token.
func (f *fakeIDP) setClaims(claims map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims = claims
}

// setRefreshClaims makes the refresh grant return an id_token of its own, which a real provider is
// free to do and which is the only way a long-lived session ever hears that its groups changed.
func (f *fakeIDP) setRefreshClaims(claims map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshClaims = claims
}

func (f *fakeIDP) setFailRefresh(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failRefresh = fail
}

// setRotatingRefresh turns on refresh token rotation with reuse refusal and returns a channel closed
// when the first refresh grant is accepted, before its delayed response is written.
func (f *fakeIDP) setRotatingRefresh(delay time.Duration) <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rotateRefresh = true
	f.refreshDelay = delay
	f.refreshAccepted = make(chan struct{})
	return f.refreshAccepted
}

// currentRefreshToken is the one refresh token the IdP accepts right now.
func (f *fakeIDP) currentRefreshToken() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshToken
}

// setRefreshStatus makes the refresh grant answer status with an OAuth2 error body; 0 restores it.
func (f *fakeIDP) setRefreshStatus(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshStatus = status
}

func (f *fakeIDP) serveDiscovery(w http.ResponseWriter, _ *http.Request) {
	doc := map[string]any{
		"issuer":                                f.issuer(),
		"authorization_endpoint":                f.issuer() + "/authorize",
		"token_endpoint":                        f.issuer() + "/token",
		"jwks_uri":                              f.issuer() + "/keys",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		// Pins the auth style an operator reading this discovery doc would expect.
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic"},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(doc)
}

func (f *fakeIDP) serveKeys(w http.ResponseWriter, _ *http.Request) {
	pub := f.key.PublicKey
	jwk := map[string]any{
		"kty": "RSA",
		"kid": testKeyID,
		"use": "sig",
		"alg": "RS256",
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{jwk}})
}

func (f *fakeIDP) serveAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	code := fmt.Sprintf("code-%d", time.Now().UnixNano())
	f.mu.Lock()
	f.pendingCode = code
	f.pendingChallenge = q.Get("code_challenge")
	f.mu.Unlock()

	loc, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	v := loc.Query()
	v.Set("code", code)
	v.Set("state", q.Get("state"))
	loc.RawQuery = v.Encode()
	http.Redirect(w, r, loc.String(), http.StatusFound)
}

func (f *fakeIDP) serveToken(w http.ResponseWriter, r *http.Request) {
	f.tokenRequests.Add(1)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	if !f.authenticateClient(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client"})
		return
	}

	switch r.FormValue("grant_type") {
	case "authorization_code":
		f.serveAuthCodeGrant(w, r)
	case "refresh_token":
		f.serveRefreshGrant(w, r)
	default:
		writeTokenError(w, "unsupported_grant_type")
	}
}

// authenticateClient verifies client_id + client_secret the way a real token endpoint pinned to
// client_secret_basic would.
func (f *fakeIDP) authenticateClient(r *http.Request) bool {
	if id, secret, ok := r.BasicAuth(); ok {
		return id == testClientID && secret == testClientSecret
	}
	id, secret := r.FormValue("client_id"), r.FormValue("client_secret")
	if id == "" && secret == "" {
		return false
	}
	return id == testClientID && secret == testClientSecret
}

func (f *fakeIDP) serveAuthCodeGrant(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	wantCode, wantChallenge, claims := f.pendingCode, f.pendingChallenge, f.claims
	f.mu.Unlock()

	code := r.FormValue("code")
	verifier := r.FormValue("code_verifier")
	sum := sha256.Sum256([]byte(verifier))
	gotChallenge := base64.RawURLEncoding.EncodeToString(sum[:])

	if code == "" || code != wantCode || gotChallenge != wantChallenge {
		writeTokenError(w, "invalid_grant")
		return
	}

	idToken, err := signRS256(f.key, testKeyID, claims)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	f.mu.Lock()
	refreshToken := f.refreshToken
	f.mu.Unlock()

	writeTokenJSON(w, map[string]any{
		"access_token":  "access-" + wantCode,
		"token_type":    "Bearer",
		"refresh_token": refreshToken,
		"expires_in":    3600,
		"id_token":      idToken,
	})
}

func (f *fakeIDP) serveRefreshGrant(w http.ResponseWriter, r *http.Request) {
	got := r.FormValue("refresh_token")
	// The check and the rotation are one step, as at a real IdP: of two grants racing on the same
	// token, exactly one is accepted.
	f.mu.Lock()
	fail, want, claims, status := f.failRefresh, f.refreshToken, f.refreshClaims, f.refreshStatus
	accepted := status == 0 && !fail && got != "" && got == want
	rotated, delay := "", time.Duration(0)
	if accepted && f.rotateRefresh {
		f.refreshSeq++
		rotated = fmt.Sprintf("rotated-refresh-token-%d", f.refreshSeq)
		f.refreshToken = rotated
		delay = f.refreshDelay
		if f.refreshAccepted != nil {
			close(f.refreshAccepted)
			f.refreshAccepted = nil
		}
	}
	f.mu.Unlock()

	if status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "temporarily_unavailable"})
		return
	}
	if !accepted {
		writeTokenError(w, "invalid_grant")
		return
	}
	time.Sleep(delay)

	// No "refresh_token" key in the response unless rotation is on.
	body := map[string]any{
		"access_token": "refreshed-access-token",
		"token_type":   "Bearer",
		"expires_in":   3600,
	}
	if rotated != "" {
		body["refresh_token"] = rotated
	}
	// An id_token on the refresh response is OPTIONAL, so it is opt-in here: the default path stays
	// the provider that returns none, which is the common one.
	if claims != nil {
		idToken, err := signRS256(f.key, testKeyID, claims)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		body["id_token"] = idToken
	}
	writeTokenJSON(w, body)
}

func writeTokenJSON(w http.ResponseWriter, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func writeTokenError(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

// signRS256 hand-rolls a compact RS256 JWS; no JOSE library is used deliberately -- go-oidc/oauth2
// do not depend on one beyond go-jose.
func signRS256(key *rsa.PrivateKey, kid string, claims map[string]any) (string, error) {
	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("marshal header: %w", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshal claims: %w", err)
	}

	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	hashed := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hashed[:])
	if err != nil {
		return "", fmt.Errorf("sign: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// newOIDCFixture wires an OIDCAuthenticator against idp with a fresh
// SessionStore/KV pair, returning the KV too so tests can assert directly on
// what the authenticator stores in it.
func newOIDCFixture(t *testing.T, idp *fakeIDP) (*authn.OIDCAuthenticator, *authn.SessionStore, *cache.InProcessKV) {
	t.Helper()
	return newOIDCFixtureWithCookieName(t, idp, authn.OIDCSessionCookieName)
}

// newOIDCFixtureWithCookieName is newOIDCFixture with an explicit cookieName, so a test can pin the
// carry-forward fix.
func newOIDCFixtureWithCookieName(t *testing.T, idp *fakeIDP, cookieName string) (*authn.OIDCAuthenticator, *authn.SessionStore, *cache.InProcessKV) {
	t.Helper()
	kv := cache.NewInProcessKV()
	t.Cleanup(kv.Close)
	sessions := authn.NewSessionStore(kv, time.Hour, 0)

	cfg := config.OIDCConfig{
		Issuer:        idp.issuer(),
		ClientID:      testClientID,
		RedirectURL:   "http://console.example" + config.OIDCCallbackPath,
		Scopes:        []string{"openid", "profile", "email", "groups"},
		UsernameClaim: "preferred_username",
		GroupsClaim:   "groups",
	}
	a, err := authn.NewOIDC(context.Background(), cfg, testClientSecret, sessions, kv, cookieName)
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	return a, sessions, kv
}

// testReturnTo is the returnTo every fixture in this file drives through AuthorizeURL.
const testReturnTo = "/dashboard"

// authorizeAndRedirect drives AuthorizeURL.
func authorizeAndRedirect(t *testing.T, a *authn.OIDCAuthenticator) (state, code string) {
	t.Helper()
	authURL, err := a.AuthorizeURL(context.Background(), testReturnTo)
	if err != nil {
		t.Fatalf("AuthorizeURL: %v", err)
	}

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Get(authURL) //nolint:noctx // test helper; the request has no meaningful deadline to bound
	if err != nil {
		t.Fatalf("GET authorize url: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("expected 302 from the fake IdP's /authorize, got %d", resp.StatusCode)
	}

	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect Location: %v", err)
	}
	return loc.Query().Get("state"), loc.Query().Get("code")
}

func cookieRequest(sessionID string) *http.Request {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: authn.OIDCSessionCookieName, Value: sessionID})
	return r
}

func TestOIDCAuthorizeURLEmitsCodeFlowWithPKCEAndScopes(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	a, _, kv := newOIDCFixture(t, idp)

	rawURL, err := a.AuthorizeURL(context.Background(), "/dashboard")
	if err != nil {
		t.Fatalf("AuthorizeURL: %v", err)
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse AuthorizeURL output: %v", err)
	}
	q := u.Query()

	if got := q.Get("response_type"); got != "code" {
		t.Errorf("response_type = %q, want %q", got, "code")
	}
	if got := q.Get("code_challenge_method"); got != "S256" {
		t.Errorf("code_challenge_method = %q, want %q", got, "S256")
	}
	if q.Get("code_challenge") == "" {
		t.Error("expected a non-empty code_challenge")
	}
	state := q.Get("state")
	if state == "" {
		t.Fatal("expected a non-empty state")
	}
	for _, scope := range []string{"openid", "profile", "email", "groups"} {
		if !strings.Contains(q.Get("scope"), scope) {
			t.Errorf("scope %q missing from %q", scope, q.Get("scope"))
		}
	}

	// The verifier and returnTo travel sealed in the state; starting a sign-in stores nothing.
	if _, ok, err := kv.Get(context.Background(), "oidcstate:"+state); err != nil || ok {
		t.Fatalf("oidcstate:%s in the KV: ok=%v err=%v, want nothing stored", state, ok, err)
	}
}

func TestOIDCFullRoundTripAuthenticatesAndYieldsExpectedSubject(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	a, _, _ := newOIDCFixture(t, idp)

	state, code := authorizeAndRedirect(t, a)

	sessionID, returnTo, err := a.Callback(context.Background(), state, code)
	if err != nil {
		t.Fatalf("Callback: %v", err)
	}
	if returnTo != "/dashboard" {
		t.Errorf("returnTo = %q, want %q", returnTo, "/dashboard")
	}
	if sessionID == "" {
		t.Fatal("expected a non-empty session id")
	}

	subject, err := a.Authenticate(cookieRequest(sessionID))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	// The ID is the SUB, namespaced — not "alice". preferred_username is a display concern and a
	// reassignable one (OIDC Core §5.7); an RBAC binding may not hang off it.
	want := authz.Subject{
		Kind:        authz.SubjectUser,
		ID:          "oidc:user-sub-1",
		DisplayName: "alice",
		Groups:      []string{"admins", "sre"},
	}
	if subject.Kind != want.Kind || subject.ID != want.ID || subject.DisplayName != want.DisplayName {
		t.Errorf("subject = %+v, want %+v", subject, want)
	}
	if strings.Join(subject.Groups, ",") != strings.Join(want.Groups, ",") {
		t.Errorf("subject.Groups = %v, want %v", subject.Groups, want.Groups)
	}
}

// Before the fix, an operator setting a non-default auth.session.cookieName broke session lookup
// for every oidc-mode request.
func TestOIDCAuthenticateHonorsConfiguredCookieName(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	const customCookieName = "my_nondefault_session_cookie"
	a, _, _ := newOIDCFixtureWithCookieName(t, idp, customCookieName)

	state, code := authorizeAndRedirect(t, a)
	sessionID, _, err := a.Callback(context.Background(), state, code)
	if err != nil {
		t.Fatalf("Callback: %v", err)
	}

	// A request carrying the session under the OLD hardcoded default name
	// must NOT authenticate: that would mean the configured name is being
	// ignored again.
	if _, err := a.Authenticate(cookieRequest(sessionID)); !errors.Is(err, authn.ErrNoCredentials) {
		t.Errorf("Authenticate with the session under %q = %v, want ErrNoCredentials (cookieName=%q must be honored)",
			authn.OIDCSessionCookieName, err, customCookieName)
	}

	// The same session, under the CONFIGURED name, must authenticate.
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: customCookieName, Value: sessionID})
	if _, err := a.Authenticate(r); err != nil {
		t.Errorf("Authenticate with the session under the configured cookie name %q: %v", customCookieName, err)
	}
}

func TestOIDCCallbackReplayedStateFails(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	a, _, _ := newOIDCFixture(t, idp)

	state, code := authorizeAndRedirect(t, a)

	if _, _, err := a.Callback(context.Background(), state, code); err != nil {
		t.Fatalf("first Callback: %v", err)
	}

	_, _, err := a.Callback(context.Background(), state, code)
	if err == nil {
		t.Fatal("expected the replayed callback to fail")
	}
	if !errors.Is(err, authn.ErrInvalid) {
		t.Errorf("expected ErrInvalid, got: %v", err)
	}
}

func TestOIDCCallbackUnknownStateFailsWithoutTouchingTokenEndpoint(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	a, _, _ := newOIDCFixture(t, idp)

	_, _, err := a.Callback(context.Background(), "never-issued-state", "some-code")
	if err == nil {
		t.Fatal("expected an unknown state to fail")
	}
	if !errors.Is(err, authn.ErrInvalid) {
		t.Errorf("expected ErrInvalid, got: %v", err)
	}
	if got := idp.tokenRequests.Load(); got != 0 {
		t.Errorf("expected zero token requests for an unknown state, got %d", got)
	}
}

func TestOIDCAuthorizeURLRejectsUnsafeReturnTo(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	a, _, _ := newOIDCFixture(t, idp)

	for _, returnTo := range []string{
		"https://evil.example",
		"//evil.example",
		"javascript:alert(1)",
		"/\\evil.example",
	} {
		if _, err := a.AuthorizeURL(context.Background(), returnTo); err == nil {
			t.Errorf("AuthorizeURL(%q): expected an error, got nil", returnTo)
		}
	}
}

func TestOIDCCallbackRejectsWrongAudience(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	a, _, _ := newOIDCFixture(t, idp)

	claims := idp.defaultClaims()
	claims["aud"] = "some-other-client"
	idp.setClaims(claims)

	state, code := authorizeAndRedirect(t, a)
	_, _, err := a.Callback(context.Background(), state, code)
	if !errors.Is(err, authn.ErrInvalid) {
		t.Errorf("expected ErrInvalid for a wrong audience, got: %v", err)
	}
}

func TestOIDCCallbackRejectsWrongIssuer(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	a, _, _ := newOIDCFixture(t, idp)

	claims := idp.defaultClaims()
	claims["iss"] = "https://not-the-real-issuer.example"
	idp.setClaims(claims)

	state, code := authorizeAndRedirect(t, a)
	_, _, err := a.Callback(context.Background(), state, code)
	if !errors.Is(err, authn.ErrInvalid) {
		t.Errorf("expected ErrInvalid for a wrong issuer, got: %v", err)
	}
}

func TestOIDCCallbackRejectsExpiredIDToken(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	a, _, _ := newOIDCFixture(t, idp)

	claims := idp.defaultClaims()
	claims["exp"] = time.Now().Add(-time.Hour).Unix()
	idp.setClaims(claims)

	state, code := authorizeAndRedirect(t, a)
	_, _, err := a.Callback(context.Background(), state, code)
	if !errors.Is(err, authn.ErrInvalid) {
		t.Errorf("expected ErrInvalid for an expired id token, got: %v", err)
	}
}

func TestOIDCCallbackGroupsAsBareStringYieldsOneElementList(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	a, _, _ := newOIDCFixture(t, idp)

	claims := idp.defaultClaims()
	claims["groups"] = "solo-group"
	idp.setClaims(claims)

	state, code := authorizeAndRedirect(t, a)
	sessionID, _, err := a.Callback(context.Background(), state, code)
	if err != nil {
		t.Fatalf("Callback: %v", err)
	}

	subject, err := a.Authenticate(cookieRequest(sessionID))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if want := []string{"solo-group"}; strings.Join(subject.Groups, ",") != strings.Join(want, ",") {
		t.Errorf("subject.Groups = %v, want %v", subject.Groups, want)
	}
}

func TestOIDCCallbackGroupsClaimAbsentYieldsNoGroups(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	a, _, _ := newOIDCFixture(t, idp)

	claims := idp.defaultClaims()
	delete(claims, "groups")
	idp.setClaims(claims)

	state, code := authorizeAndRedirect(t, a)
	sessionID, _, err := a.Callback(context.Background(), state, code)
	if err != nil {
		t.Fatalf("Callback: %v", err)
	}

	subject, err := a.Authenticate(cookieRequest(sessionID))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if len(subject.Groups) != 0 {
		t.Errorf("subject.Groups = %v, want empty", subject.Groups)
	}
	// OIDCAuthenticator.Authenticate never resolves Roles itself.
	if len(subject.Roles) != 0 {
		t.Errorf("subject.Roles = %v, want empty (role resolution happens downstream)", subject.Roles)
	}
}

func TestOIDCCallbackGroupsClaimNonArrayNonStringYieldsNilGroups(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	a, _, _ := newOIDCFixture(t, idp)

	claims := idp.defaultClaims()
	claims["groups"] = 42 // neither a JSON array nor a bare string
	idp.setClaims(claims)

	state, code := authorizeAndRedirect(t, a)
	sessionID, _, err := a.Callback(context.Background(), state, code)
	if err != nil {
		t.Fatalf("Callback: %v", err)
	}

	subject, err := a.Authenticate(cookieRequest(sessionID))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if subject.Groups != nil {
		t.Errorf("subject.Groups = %v, want nil", subject.Groups)
	}
}

/* ── the identity a binding hangs off ────────────────────────────────────── */

/*
 * OIDC Core §5.7 says email and preferred_username MUST NOT be used as unique identifiers, and
 * Grafana's CVE-2023-3128 (CVSS 9.4) is what ignoring that costs: identity keyed on a reassignable
 * claim means taking a leaver's address inherits their roles. The console keys on sub, namespaced,
 * and the tests below are the ones that would go red if anyone reintroduced the friendlier claim.
 */

func TestOIDCCallbackRefusesAnIDTokenWithNoSub(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	a, _, _ := newOIDCFixture(t, idp)

	claims := idp.defaultClaims()
	delete(claims, "sub")
	// preferred_username is still there, and used to be enough. It is not any more: a login that
	// cannot produce a stable identifier is refused rather than keyed on a mutable one.
	idp.setClaims(claims)

	state, code := authorizeAndRedirect(t, a)
	_, _, err := a.Callback(context.Background(), state, code)
	if !errors.Is(err, authn.ErrInvalid) {
		t.Errorf("expected ErrInvalid for an id token with no sub, got: %v", err)
	}
}

func TestOIDCCallbackRefusesASubInsideAReservedNamespace(t *testing.T) {
	t.Parallel()
	for _, sub := range []string{"local:0a5f", "oidc:someone-else", "token:0a5f", "header:svc"} {
		t.Run(sub, func(t *testing.T) {
			t.Parallel()
			idp := newFakeIDP(t)
			a, _, _ := newOIDCFixture(t, idp)

			claims := idp.defaultClaims()
			claims["sub"] = sub
			idp.setClaims(claims)

			state, code := authorizeAndRedirect(t, a)
			// An issuer minting sub = "local:<uuid>" would otherwise be handed every binding that
			// local user holds — the prefix scheme turned inside out.
			if _, _, err := a.Callback(context.Background(), state, code); !errors.Is(err, authn.ErrInvalid) {
				t.Errorf("expected ErrInvalid for sub %q, got: %v", sub, err)
			}
		})
	}
}

func TestOIDCCallbackDisplayNameFallsBackFromUsernameToNameToEmailToSub(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		strip []string
		want  string
	}{
		{name: "preferred_username wins", strip: nil, want: "alice"},
		{name: "then name", strip: []string{"preferred_username"}, want: "Alice Liddell"},
		{name: "then email", strip: []string{"preferred_username", "name"}, want: "alice@example.test"},
		{name: "and finally the sub itself", strip: []string{"preferred_username", "name", "email"}, want: "user-sub-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			idp := newFakeIDP(t)
			a, _, _ := newOIDCFixture(t, idp)

			claims := idp.defaultClaims()
			claims["name"] = "Alice Liddell"
			claims["email"] = "alice@example.test"
			for _, key := range tc.strip {
				delete(claims, key)
			}
			idp.setClaims(claims)

			state, code := authorizeAndRedirect(t, a)
			sessionID, _, err := a.Callback(context.Background(), state, code)
			if err != nil {
				t.Fatalf("Callback: %v", err)
			}
			subject, err := a.Authenticate(cookieRequest(sessionID))
			if err != nil {
				t.Fatalf("Authenticate: %v", err)
			}
			if subject.DisplayName != tc.want {
				t.Errorf("DisplayName = %q, want %q", subject.DisplayName, tc.want)
			}
			// Whatever the label ends up being, the ID never moves.
			if subject.ID != "oidc:user-sub-1" {
				t.Errorf("subject.ID = %q, want %q", subject.ID, "oidc:user-sub-1")
			}
		})
	}
}

/* ── group membership across a refresh ───────────────────────────────────── */

func TestOIDCRefreshAdoptsTheGroupsTheIdPNowReports(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	a, sessions, _ := newOIDCFixture(t, idp)

	// The IdP drops this person from admins. Until the refresh path re-read the id_token, the
	// console kept honouring every admins binding for the rest of the session's life.
	demoted := idp.defaultClaims()
	demoted["groups"] = []string{"sre"}
	idp.setRefreshClaims(demoted)

	id, err := sessions.Create(context.Background(), authn.Session{
		Username:     "oidc:user-sub-1",
		DisplayName:  "alice",
		Groups:       []string{"admins", "sre"},
		RefreshToken: "initial-refresh-token",
		AccessExpiry: time.Now().Add(30 * time.Second), // inside the refresh margin
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	subject, err := a.Authenticate(cookieRequest(id))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got := strings.Join(subject.Groups, ","); got != "sre" {
		t.Errorf("subject.Groups = %v, want [sre] — the refresh must adopt the IdP's current membership", subject.Groups)
	}
}

func TestOIDCRefreshWithoutAnIDTokenKeepsTheGroupsItHad(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	a, sessions, _ := newOIDCFixture(t, idp)

	// No setRefreshClaims: the fake IdP returns no id_token on refresh, which is what most
	// providers do. Reading that as "no groups" would be a silent, total deauthorization.
	id, err := sessions.Create(context.Background(), authn.Session{
		Username:     "oidc:user-sub-1",
		DisplayName:  "alice",
		Groups:       []string{"admins", "sre"},
		RefreshToken: "initial-refresh-token",
		AccessExpiry: time.Now().Add(30 * time.Second),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	subject, err := a.Authenticate(cookieRequest(id))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got := strings.Join(subject.Groups, ","); got != "admins,sre" {
		t.Errorf("subject.Groups = %v, want the session's own [admins sre]", subject.Groups)
	}
}

func TestOIDCRefreshForADifferentSubjectIsNotAdopted(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	a, sessions, _ := newOIDCFixture(t, idp)

	// A token endpoint answering with someone ELSE's id_token is not this session being updated;
	// it is a session being re-pointed, and the groups it carries are not this person's.
	other := idp.defaultClaims()
	other["sub"] = "user-sub-2"
	other["groups"] = []string{"platform-admins"}
	idp.setRefreshClaims(other)

	id, err := sessions.Create(context.Background(), authn.Session{
		Username:     "oidc:user-sub-1",
		DisplayName:  "alice",
		Groups:       []string{"sre"},
		RefreshToken: "initial-refresh-token",
		AccessExpiry: time.Now().Add(30 * time.Second),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	subject, err := a.Authenticate(cookieRequest(id))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got := strings.Join(subject.Groups, ","); got != "sre" {
		t.Errorf("subject.Groups = %v, want [sre] — another subject's claims must not be adopted", subject.Groups)
	}
	if subject.ID != "oidc:user-sub-1" {
		t.Errorf("subject.ID = %q, want it unchanged", subject.ID)
	}
}

// Sessions outlive a deployment: they live in Valkey with a 12h TTL and nothing revalidates their
// shape at boot. One minted before identity became "oidc:"+sub carries a bare username, and
// honouring it would keep the LEGACY bindings granting for the rest of that TTL — exactly what the
// console tells the operator at boot has stopped happening.
func TestOIDCAuthenticateRefusesAPreUpgradeSessionAndDropsIt(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	a, sessions, _ := newOIDCFixture(t, idp)

	id, err := sessions.Create(context.Background(), authn.Session{
		Username:    "alice", // the pre-upgrade shape: the username claim, unprefixed
		DisplayName: "alice",
		Groups:      []string{"admins"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := a.Authenticate(cookieRequest(id)); !errors.Is(err, authn.ErrExpired) {
		t.Fatalf("Authenticate with a pre-upgrade session = %v, want ErrExpired", err)
	}
	// And it is GONE, so the next request cannot be answered from it either.
	if _, ok, err := sessions.Get(context.Background(), id); err != nil || ok {
		t.Errorf("session still present after refusal: ok=%v err=%v", ok, err)
	}
}

func TestOIDCRefreshKeepsGroupsWhenTheRefreshedTokenOmitsTheClaim(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	a, sessions, _ := newOIDCFixture(t, idp)

	/* A refresh id_token is often MINIMAL — sub, iss, aud, exp and nothing else. Reading an absent
	   groups claim as "no groups" would deauthorize the session completely on a token that said
	   nothing about groups at all. */
	minimal := idp.defaultClaims()
	delete(minimal, "groups")
	idp.setRefreshClaims(minimal)

	id, err := sessions.Create(context.Background(), authn.Session{
		Username:     "oidc:user-sub-1",
		DisplayName:  "alice",
		Groups:       []string{"admins", "sre"},
		RefreshToken: "initial-refresh-token",
		AccessExpiry: time.Now().Add(30 * time.Second),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	subject, err := a.Authenticate(cookieRequest(id))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got := strings.Join(subject.Groups, ","); got != "admins,sre" {
		t.Errorf("subject.Groups = %v, want them untouched by a token that never mentioned groups", subject.Groups)
	}
}

func TestOIDCRefreshAdoptsAnEXPLICITLYEmptyGroupsClaim(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	a, sessions, _ := newOIDCFixture(t, idp)

	// "groups": [] is a STATEMENT — the IdP saying this person is in none — and it is adopted, unlike
	// the absent claim above.
	revoked := idp.defaultClaims()
	revoked["groups"] = []string{}
	idp.setRefreshClaims(revoked)

	id, err := sessions.Create(context.Background(), authn.Session{
		Username:     "oidc:user-sub-1",
		DisplayName:  "alice",
		Groups:       []string{"admins"},
		RefreshToken: "initial-refresh-token",
		AccessExpiry: time.Now().Add(30 * time.Second),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	subject, err := a.Authenticate(cookieRequest(id))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if len(subject.Groups) != 0 {
		t.Errorf("subject.Groups = %v, want empty — the IdP said so explicitly", subject.Groups)
	}
}

func TestOIDCAuthenticateRefreshesNearExpirySessionExactlyOnce(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	a, sessions, _ := newOIDCFixture(t, idp)

	id, err := sessions.Create(context.Background(), authn.Session{
		Username:     "oidc:user-sub-1",
		DisplayName:  "alice",
		RefreshToken: "initial-refresh-token",
		AccessExpiry: time.Now().Add(30 * time.Second), // within accessTokenRefreshMargin
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	subject, err := a.Authenticate(cookieRequest(id))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if subject.ID != "oidc:user-sub-1" {
		t.Errorf("subject.ID = %q, want %q", subject.ID, "oidc:user-sub-1")
	}
	if got := idp.tokenRequests.Load(); got != 1 {
		t.Errorf("expected exactly one token request for the refresh, got %d", got)
	}

	sess, ok, err := sessions.Get(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("expected the session to still be alive after refresh, ok=%v err=%v", ok, err)
	}
	if !sess.AccessExpiry.After(time.Now().Add(time.Minute)) {
		t.Errorf("expected AccessExpiry to have moved forward, got %v", sess.AccessExpiry)
	}
	// The fake IdP's refresh response carries no refresh_token, so the
	// original one must be preserved (oauth2's own "don't overwrite with
	// empty" behavior -- see fakeIDP.serveRefreshGrant).
	if sess.RefreshToken != "initial-refresh-token" {
		t.Errorf("expected the refresh token to be preserved, got %q", sess.RefreshToken)
	}

	// A second Authenticate call within the request should not refresh
	// again -- AccessExpiry now sits comfortably past the margin.
	if _, err := a.Authenticate(cookieRequest(id)); err != nil {
		t.Fatalf("second Authenticate: %v", err)
	}
	if got := idp.tokenRequests.Load(); got != 1 {
		t.Errorf("expected still exactly one token request after a second Authenticate, got %d", got)
	}
}

// TestOIDCAuthenticateConcurrentRefreshesSingleFlight is I1's regression test; before
// maybeRefresh's per-session-id locking.
func TestOIDCAuthenticateConcurrentRefreshesSingleFlight(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	a, sessions, _ := newOIDCFixture(t, idp)

	id, err := sessions.Create(context.Background(), authn.Session{
		Username:     "oidc:user-sub-1",
		DisplayName:  "alice",
		RefreshToken: "initial-refresh-token",
		AccessExpiry: time.Now().Add(30 * time.Second), // within accessTokenRefreshMargin
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	const n = 25
	var ready, start sync.WaitGroup
	ready.Add(n)
	start.Add(1)
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ready.Done()
			start.Wait()
			_, authErr := a.Authenticate(cookieRequest(id))
			errs[i] = authErr
		}(i)
	}
	ready.Wait() // all goroutines parked at start.Wait() before any of them run
	start.Done()
	wg.Wait()

	for i, authErr := range errs {
		if authErr != nil {
			t.Errorf("Authenticate[%d]: %v", i, authErr)
		}
	}
	if got := idp.tokenRequests.Load(); got != 1 {
		t.Errorf("expected exactly one token request across %d concurrent Authenticate calls, got %d", n, got)
	}

	sess, ok, err := sessions.Get(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("expected the session to still be alive, ok=%v err=%v", ok, err)
	}
	if !sess.AccessExpiry.After(time.Now().Add(time.Minute)) {
		t.Errorf("expected AccessExpiry to have moved forward, got %v", sess.AccessExpiry)
	}
}

func TestOIDCAuthenticateInvalidatesSessionWhenRefreshFails(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	idp.setFailRefresh(true)
	a, sessions, _ := newOIDCFixture(t, idp)

	id, err := sessions.Create(context.Background(), authn.Session{
		Username:     "oidc:user-sub-1",
		DisplayName:  "alice",
		RefreshToken: "initial-refresh-token",
		AccessExpiry: time.Now().Add(30 * time.Second),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err = a.Authenticate(cookieRequest(id))
	if !errors.Is(err, authn.ErrExpired) {
		t.Errorf("expected ErrExpired, got: %v", err)
	}

	_, ok, err := sessions.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get after failed refresh: %v", err)
	}
	if ok {
		t.Error("expected the session to be invalidated after a failed refresh")
	}
}

// newNearExpirySession stores an OIDC session whose access token expires at accessExpiry.
func newNearExpirySession(t *testing.T, sessions *authn.SessionStore, accessExpiry time.Time) string {
	t.Helper()
	id, err := sessions.Create(context.Background(), authn.Session{
		Username:     "oidc:user-sub-1",
		DisplayName:  "alice",
		RefreshToken: "initial-refresh-token",
		AccessExpiry: accessExpiry,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return id
}

// Only the IdP refusing the grant ends a session. An IdP that is down or answers 5xx is no verdict
// on the refresh token: the session keeps serving until its access token actually expires, and the
// next refresh after the IdP recovers extends it.
func TestOIDCRefreshThatFailsAtTheIdPServerKeepsTheSession(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			testOIDCRefreshKeepsTheSessionOn(t, status)
		})
	}
}

func testOIDCRefreshKeepsTheSessionOn(t *testing.T, status int) {
	t.Helper()
	idp := newFakeIDP(t)
	idp.setRefreshStatus(status)
	a, sessions, _ := newOIDCFixture(t, idp)
	id := newNearExpirySession(t, sessions, time.Now().Add(30*time.Second))

	subject, err := a.Authenticate(cookieRequest(id))
	if err != nil {
		t.Fatalf("Authenticate with the IdP answering %d and the access token still valid: %v", status, err)
	}
	if subject.ID != "oidc:user-sub-1" {
		t.Errorf("subject = %q, want oidc:user-sub-1", subject.ID)
	}
	if _, ok, _ := sessions.Get(context.Background(), id); !ok {
		t.Fatalf("a %d from the token endpoint deleted the session", status)
	}

	idp.setRefreshStatus(0)
	if _, err := a.Authenticate(cookieRequest(id)); err != nil {
		t.Fatalf("Authenticate after the IdP recovered: %v", err)
	}
	sess, _, _ := sessions.Get(context.Background(), id)
	if !sess.AccessExpiry.After(time.Now().Add(time.Minute)) {
		t.Errorf("AccessExpiry = %v, want it refreshed once the IdP recovered", sess.AccessExpiry)
	}
}

func TestOIDCRefreshFailureAfterTheAccessTokenExpiredRefusesButKeepsTheSession(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	idp.setRefreshStatus(http.StatusBadGateway)
	a, sessions, _ := newOIDCFixture(t, idp)
	id := newNearExpirySession(t, sessions, time.Now().Add(-time.Minute))

	_, err := a.Authenticate(cookieRequest(id))
	if err == nil {
		t.Fatal("an expired access token with no refresh authenticated")
	}
	if errors.Is(err, authn.ErrExpired) {
		t.Errorf("err = %v, want a transient error rather than ErrExpired", err)
	}
	if _, ok, _ := sessions.Get(context.Background(), id); !ok {
		t.Fatal("a 502 from the token endpoint deleted the session")
	}

	idp.setRefreshStatus(0)
	if _, err := a.Authenticate(cookieRequest(id)); err != nil {
		t.Fatalf("Authenticate after the IdP recovered: %v", err)
	}
}

// The refresh outlives the request that triggered it: a browser that navigates away mid-refresh (or
// the WebSocket revalidator's short budget running out) must not cost the user their session.
func TestOIDCRefreshOnACanceledRequestKeepsTheSession(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	a, sessions, _ := newOIDCFixture(t, idp)
	id := newNearExpirySession(t, sessions, time.Now().Add(30*time.Second))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = a.Authenticate(cookieRequest(id).WithContext(ctx))

	if _, ok, _ := sessions.Get(context.Background(), id); !ok {
		t.Fatal("a canceled request deleted a session whose refresh token is valid")
	}
	if _, err := a.Authenticate(cookieRequest(id)); err != nil {
		t.Fatalf("Authenticate on a live request afterwards: %v", err)
	}
}

// A 4xx from the token endpoint is the IdP's answer about this grant, like invalid_grant.
func TestOIDCRefreshRefusedWithA4xxDeletesTheSession(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	idp.setRefreshStatus(http.StatusUnauthorized)
	a, sessions, _ := newOIDCFixture(t, idp)
	id := newNearExpirySession(t, sessions, time.Now().Add(30*time.Second))

	if _, err := a.Authenticate(cookieRequest(id)); !errors.Is(err, authn.ErrExpired) {
		t.Errorf("err = %v, want ErrExpired", err)
	}
	if _, ok, _ := sessions.Get(context.Background(), id); ok {
		t.Error("a 401 from the token endpoint left the session in place")
	}
}

// failingSessionDeleteKV refuses to delete session records.
type failingSessionDeleteKV struct {
	*cache.InProcessKV
}

func (kv failingSessionDeleteKV) Delete(ctx context.Context, key string) error {
	if strings.HasPrefix(key, "sess:") {
		return errors.New("kv unavailable")
	}
	return kv.InProcessKV.Delete(ctx, key)
}

// The session id is the bearer cookie value; the warning about a session that outlived a refused
// refresh must not carry it.
func TestOIDCRefusedRefreshDeleteFailureWarningOmitsTheSessionID(t *testing.T) {
	logs := captureLogs(t)
	idp := newFakeIDP(t)
	idp.setFailRefresh(true)
	inner := cache.NewInProcessKV()
	t.Cleanup(inner.Close)
	kv := failingSessionDeleteKV{InProcessKV: inner}
	sessions := authn.NewSessionStore(kv, time.Hour, 0)
	cfg := config.OIDCConfig{
		Issuer:        idp.issuer(),
		ClientID:      testClientID,
		RedirectURL:   "http://console.example" + config.OIDCCallbackPath,
		Scopes:        []string{"openid"},
		UsernameClaim: "preferred_username",
		GroupsClaim:   "groups",
	}
	a, err := authn.NewOIDC(context.Background(), cfg, testClientSecret, sessions, kv, authn.OIDCSessionCookieName)
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	id := newNearExpirySession(t, sessions, time.Now().Add(30*time.Second))

	if _, err := a.Authenticate(cookieRequest(id)); !errors.Is(err, authn.ErrExpired) {
		t.Fatalf("err = %v, want ErrExpired", err)
	}
	out := logs.String()
	if !strings.Contains(out, "delete session after failed refresh") {
		t.Fatalf("no warning for the failed delete:\n%s", out)
	}
	if strings.Contains(out, id) {
		t.Errorf("the warning carries the session id:\n%s", out)
	}
}

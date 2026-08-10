package authn_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// waitFor polls cond until it holds, or fails the test.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// makeToken returns a wire-format token ("kcm_<43-char base64url>") built from a deterministic
// 32-byte secret.
func makeToken(seed byte) (wire string, hash []byte) {
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = seed + byte(i)
	}
	sum := sha256.Sum256(secret)
	return "kcm_" + base64.RawURLEncoding.EncodeToString(secret), sum[:]
}

// fakeTokenStore is a minimal in-memory authn.TokenStore double, keyed by
// the raw hash bytes (as a string map key).
type fakeTokenStore struct {
	mu       sync.Mutex
	byHash   map[string]store.Token
	err      error // when set, returned verbatim from GetTokenByHash
	touchErr error // when set, returned verbatim from TouchTokenLastUsed (and NOT counted as a successful touch)
	touched  map[string]int
	attempts map[string]int // every TouchTokenLastUsed call, success or failure -- lets a test observe a FAILED attempt landing
}

func newFakeTokenStore(tokens map[string]store.Token) *fakeTokenStore {
	return &fakeTokenStore{byHash: tokens, touched: make(map[string]int), attempts: make(map[string]int)}
}

func (f *fakeTokenStore) GetTokenByHash(_ context.Context, hash []byte) (store.Token, error) {
	if f.err != nil {
		return store.Token{}, f.err
	}
	tok, ok := f.byHash[string(hash)]
	if !ok {
		return store.Token{}, store.ErrNotFound
	}
	return tok, nil
}

func (f *fakeTokenStore) TouchTokenLastUsed(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts[id]++
	if f.touchErr != nil {
		return f.touchErr
	}
	f.touched[id]++
	return nil
}

func (f *fakeTokenStore) touchCount(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.touched[id]
}

func (f *fakeTokenStore) attemptCount(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts[id]
}

func (f *fakeTokenStore) setTouchErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touchErr = err
}

// spyUserStore is an authn.UserStore double for WithOwnerDisabledCheck tests.
type spyUserStore struct {
	mu      sync.Mutex
	byID    map[string]store.User
	err     error // when set, returned verbatim from GetUserByID regardless of id
	idCalls []string
}

func (f *spyUserStore) GetUserByID(_ context.Context, id string) (store.User, error) {
	f.mu.Lock()
	f.idCalls = append(f.idCalls, id)
	f.mu.Unlock()
	if f.err != nil {
		return store.User{}, f.err
	}
	u, ok := f.byID[id]
	if !ok {
		return store.User{}, store.ErrNotFound
	}
	return u, nil
}

func (f *spyUserStore) GetUserByUsername(context.Context, string) (store.User, error) {
	return store.User{}, store.ErrNotFound
}

func (f *spyUserStore) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.idCalls)
}

// TestTokenFallbackOwnerDisabledCheckDisabledOwnerIsErrDisabled proves the core of I-2 / the
// PAT-disable-fix.
func TestTokenFallbackOwnerDisabledCheckDisabledOwnerIsErrDisabled(t *testing.T) {
	t.Parallel()

	const ownerID = "11111111-1111-1111-1111-111111111111"
	wire, hash := makeToken(10)
	ts := newFakeTokenStore(map[string]store.Token{string(hash): {ID: "tok-disabled-owner", Owner: ownerID}})
	users := &spyUserStore{byID: map[string]store.User{ownerID: {ID: ownerID, Disabled: true}}}
	inner := &spyInner{err: authn.ErrNoCredentials, mode: "anonymous"}
	a := authn.NewTokenFallback(ts, inner, authn.WithOwnerDisabledCheck(users))

	subject, err := a.Authenticate(bearerRequest(wire))
	if !errors.Is(err, authn.ErrDisabled) {
		t.Fatalf("Authenticate: err = %v, want ErrDisabled", err)
	}
	if !reflect.DeepEqual(subject, authz.Subject{}) {
		t.Errorf("Authenticate: subject = %+v on a disabled-owner error, want zero value (not a successful auth)", subject)
	}
	if len(inner.calls) != 0 {
		t.Errorf("inner.calls = %d, want 0 (a disabled owner must be rejected outright, never given a second, silent chance via inner)", len(inner.calls))
	}
}

// TestTokenFallbackOwnerDisabledCheckEnabledOwnerSucceeds is the positive
// counterpart: a live, enabled owner authenticates exactly as before.
func TestTokenFallbackOwnerDisabledCheckEnabledOwnerSucceeds(t *testing.T) {
	t.Parallel()

	const ownerID = "22222222-2222-2222-2222-222222222222"
	wire, hash := makeToken(11)
	ts := newFakeTokenStore(map[string]store.Token{string(hash): {ID: "tok-enabled-owner", Name: "ci", Owner: ownerID}})
	users := &spyUserStore{byID: map[string]store.User{ownerID: {ID: ownerID, Disabled: false}}}
	inner := &spyInner{err: authn.ErrNoCredentials, mode: "anonymous"}
	a := authn.NewTokenFallback(ts, inner, authn.WithOwnerDisabledCheck(users))

	subject, err := a.Authenticate(bearerRequest(wire))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	want := authz.Subject{Kind: authz.SubjectToken, ID: "tok-enabled-owner", DisplayName: "ci"}
	if !reflect.DeepEqual(subject, want) {
		t.Errorf("got %+v, want %+v", subject, want)
	}
}

// TestTokenFallbackOwnerDisabledCheckUnknownOwnerUUIDSucceeds covers a token whose owner is a
// well-formed UUID that names no row in users at all; GetUserByID's ErrNotFound must be treated as
// "allow", not propagated as a hard failure.
func TestTokenFallbackOwnerDisabledCheckUnknownOwnerUUIDSucceeds(t *testing.T) {
	t.Parallel()

	const ownerID = "33333333-3333-3333-3333-333333333333"
	wire, hash := makeToken(12)
	ts := newFakeTokenStore(map[string]store.Token{string(hash): {ID: "tok-unknown-owner", Owner: ownerID}})
	users := &spyUserStore{byID: map[string]store.User{}} // no rows at all
	inner := &spyInner{err: authn.ErrNoCredentials, mode: "anonymous"}
	a := authn.NewTokenFallback(ts, inner, authn.WithOwnerDisabledCheck(users))

	_, err := a.Authenticate(bearerRequest(wire))
	if err != nil {
		t.Fatalf("Authenticate: %v, want success (unknown owner UUID must not block auth)", err)
	}
}

// TestTokenFallbackOwnerDisabledCheckSystemOwnerSkipsLookupEntirely covers the "system" owner (and,
// by the same non-UUID logic, empty/legacy data -- but NOT a token id: token ids are canonical
// UUIDs, exactly like a users.id, so a token-minted-token's owner does NOT take this
// skip-the-lookup path -- see
// TestTokenFallbackOwnerDisabledCheckParentTokenOwnerUUIDAllowsThroughErrNotFound below for that
// case instead): the check must not even call GetUserByID for "system", proven here with a spy that
// would otherwise report the owner as disabled if it were ever looked up.
func TestTokenFallbackOwnerDisabledCheckSystemOwnerSkipsLookupEntirely(t *testing.T) {
	t.Parallel()

	wire, hash := makeToken(13)
	ts := newFakeTokenStore(map[string]store.Token{string(hash): {ID: "tok-system-owner", Owner: "system"}})
	users := &spyUserStore{err: errors.New("must never be called for a non-UUID owner")}
	inner := &spyInner{err: authn.ErrNoCredentials, mode: "anonymous"}
	a := authn.NewTokenFallback(ts, inner, authn.WithOwnerDisabledCheck(users))

	_, err := a.Authenticate(bearerRequest(wire))
	if err != nil {
		t.Fatalf("Authenticate: %v, want success", err)
	}
	if got := users.callCount(); got != 0 {
		t.Errorf("GetUserByID call count = %d, want 0 (a non-UUID owner must skip the lookup entirely)", got)
	}
}

// TestTokenFallbackOwnerDisabledCheckStoreErrorPropagates proves the check fails closed on an infra
// error from GetUserByID.
func TestTokenFallbackOwnerDisabledCheckStoreErrorPropagates(t *testing.T) {
	t.Parallel()

	const ownerID = "44444444-4444-4444-4444-444444444444"
	wire, hash := makeToken(14)
	ts := newFakeTokenStore(map[string]store.Token{string(hash): {ID: "tok-store-error", Owner: ownerID}})
	boom := errors.New("boom: store unavailable")
	users := &spyUserStore{err: boom}
	inner := &spyInner{err: authn.ErrNoCredentials, mode: "anonymous"}
	a := authn.NewTokenFallback(ts, inner, authn.WithOwnerDisabledCheck(users))

	_, err := a.Authenticate(bearerRequest(wire))
	if err == nil {
		t.Fatal("Authenticate: err = nil, want the propagated store error")
	}
	if errors.Is(err, authn.ErrInvalid) {
		t.Errorf("Authenticate: err = %v, must NOT be ErrInvalid (a store outage is not the same as a bad token)", err)
	}
	if !errors.Is(err, boom) {
		t.Errorf("Authenticate: err = %v, want it to wrap %v", err, boom)
	}
}

// TestTokenFallbackOwnerDisabledCheckParentTokenOwnerUUIDAllowsThroughErrNotFound pins the
// transitive-gap scenario that motivated the mint-time owner inheritance in httpapi/tokens.go's
// handleTokensCreate.
func TestTokenFallbackOwnerDisabledCheckParentTokenOwnerUUIDAllowsThroughErrNotFound(t *testing.T) {
	t.Parallel()

	const parentTokenID = "55555555-5555-5555-5555-555555555555" // shaped exactly like a users.id
	wire, hash := makeToken(15)
	ts := newFakeTokenStore(map[string]store.Token{string(hash): {ID: "tok-child", Owner: parentTokenID}})
	users := &spyUserStore{byID: map[string]store.User{}} // no users row named parentTokenID
	inner := &spyInner{err: authn.ErrNoCredentials, mode: "anonymous"}
	a := authn.NewTokenFallback(ts, inner, authn.WithOwnerDisabledCheck(users))

	_, err := a.Authenticate(bearerRequest(wire))
	if err != nil {
		t.Fatalf("Authenticate: %v, want success (a parent-token-id owner is a UUID, reaches ErrNotFound, and allows)", err)
	}
	if got := users.callCount(); got != 1 {
		t.Errorf("GetUserByID call count = %d, want 1 (a token-id owner IS a UUID and must reach the real lookup, unlike the \"system\" owner case)", got)
	}
}

// spyInner is a hand-written Authenticator double that records every
// request it is called with (by pointer identity, to prove pass-through is
// untouched -- not a clone) and returns a canned result.
type spyInner struct {
	calls   []*http.Request
	subject authz.Subject
	err     error
	mode    string
}

func (s *spyInner) Authenticate(r *http.Request) (authz.Subject, error) {
	s.calls = append(s.calls, r)
	return s.subject, s.err
}

func (s *spyInner) Mode() string { return s.mode }

func bearerRequest(token string) *http.Request {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func TestTokenFallbackValidTokenResolvesSubjectWithoutConsultingInner(t *testing.T) {
	t.Parallel()

	wire, hash := makeToken(1)
	ts := newFakeTokenStore(map[string]store.Token{
		string(hash): {ID: "tok-1", Name: "ci pipeline"},
	})
	inner := &spyInner{err: authn.ErrNoCredentials, mode: "anonymous"}
	a := authn.NewTokenFallback(ts, inner)

	subject, err := a.Authenticate(bearerRequest(wire))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	want := authz.Subject{Kind: authz.SubjectToken, ID: "tok-1", DisplayName: "ci pipeline"}
	if !reflect.DeepEqual(subject, want) {
		t.Errorf("got %+v, want %+v", subject, want)
	}
	if len(inner.calls) != 0 {
		t.Errorf("expected inner not to be consulted for a resolved token, got %d calls", len(inner.calls))
	}
	// TouchTokenLastUsed now runs on a detached goroutine (I-1: it must never run synchronously on the
	// request path).
	waitFor(t, "TouchTokenLastUsed to be called once", func() bool { return ts.touchCount("tok-1") == 1 })
}

func TestTokenFallbackUnknownRevokedExpiredAllErrInvalidIndistinguishably(t *testing.T) {
	t.Parallel()

	revokedAt := time.Now().Add(-time.Hour)
	expiredAt := time.Now().Add(-time.Minute)

	wireRevoked, hashRevoked := makeToken(2)
	wireExpired, hashExpired := makeToken(3)
	wireUnknown, _ := makeToken(4) // never registered in the store

	ts := newFakeTokenStore(map[string]store.Token{
		string(hashRevoked): {ID: "tok-revoked", RevokedAt: &revokedAt},
		string(hashExpired): {ID: "tok-expired", ExpiresAt: &expiredAt},
	})
	inner := &spyInner{err: authn.ErrNoCredentials, mode: "anonymous"}
	a := authn.NewTokenFallback(ts, inner)

	cases := map[string]string{
		"revoked": wireRevoked,
		"expired": wireExpired,
		"unknown": wireUnknown,
	}
	for name, wire := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := a.Authenticate(bearerRequest(wire))
			if !errors.Is(err, authn.ErrInvalid) {
				t.Errorf("got %v, want ErrInvalid", err)
			}
			// Indistinguishable: none of these ever surface as ErrNoCredentials
			// (which would incorrectly fall through to inner) or ErrDisabled.
			if errors.Is(err, authn.ErrNoCredentials) {
				t.Errorf("must not be ErrNoCredentials, got %v", err)
			}
		})
	}
}

func TestTokenFallbackMalformedPrefixFallsThroughToInner(t *testing.T) {
	t.Parallel()

	ts := newFakeTokenStore(nil)
	innerSubject := authz.Subject{Kind: authz.SubjectAnonymous, ID: "anonymous", Roles: []string{"viewer"}}
	inner := &spyInner{subject: innerSubject, mode: "anonymous"}
	a := authn.NewTokenFallback(ts, inner)

	requests := map[string]*http.Request{
		"no Authorization header at all":  httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil),
		"wrong scheme":                    authorizationRequest("Basic dXNlcjpwYXNz"),
		"bearer without kcm_ prefix":      authorizationRequest("Bearer not-a-kcm-token-at-all"),
		"bearer with kcm_ but too short":  authorizationRequest("Bearer kcm_short"),
		"bearer with kcm_ but bad base64": authorizationRequest("Bearer kcm_" + string(make([]byte, 43))),
	}

	for name, r := range requests {
		t.Run(name, func(t *testing.T) {
			subject, err := a.Authenticate(r)
			if err != nil {
				t.Fatalf("Authenticate: %v", err)
			}
			if !reflect.DeepEqual(subject, innerSubject) {
				t.Errorf("got %+v, want inner's subject %+v", subject, innerSubject)
			}
		})
	}
}

func authorizationRequest(value string) *http.Request {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	r.Header.Set("Authorization", value)
	return r
}

// TestTokenFallbackPassesCookieCarryingRequestToInnerUntouched proves a request with no bearer
// token (here, one carrying only a session cookie) is handed to inner as the EXACT same
// *http.Request.
func TestTokenFallbackPassesCookieCarryingRequestToInnerUntouched(t *testing.T) {
	t.Parallel()

	ts := newFakeTokenStore(nil)
	innerSubject := authz.Subject{Kind: authz.SubjectUser, ID: "some-uuid", DisplayName: "Alice"}
	inner := &spyInner{subject: innerSubject, mode: "local"}
	a := authn.NewTokenFallback(ts, inner)

	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: "kconmon_session", Value: "some-session-id"})

	subject, err := a.Authenticate(r)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !reflect.DeepEqual(subject, innerSubject) {
		t.Errorf("got %+v, want %+v", subject, innerSubject)
	}
	if len(inner.calls) != 1 || inner.calls[0] != r {
		t.Fatalf("expected inner.Authenticate to be called exactly once with the SAME *http.Request, got %d calls", len(inner.calls))
	}
}

func TestTokenFallbackModeDelegatesToInner(t *testing.T) {
	t.Parallel()
	inner := &spyInner{mode: "header"}
	a := authn.NewTokenFallback(newFakeTokenStore(nil), inner)
	if got := a.Mode(); got != "header" {
		t.Errorf("Mode() = %q, want %q (a token capability layered on top is not a fifth auth.mode)", got, "header")
	}
}

func TestTokenFallbackTouchIsDebouncedPerToken(t *testing.T) {
	t.Parallel()

	wire, hash := makeToken(5)
	ts := newFakeTokenStore(map[string]store.Token{string(hash): {ID: "tok-debounce"}})
	inner := &spyInner{err: authn.ErrNoCredentials, mode: "anonymous"}
	a := authn.NewTokenFallback(ts, inner)

	// The first request's touch must be allowed to actually land (and update the debounce bookkeeping,
	// which -- since I-1 -- only happens on a SUCCESSFUL write) before the next two requests can be a
	// meaningful test of the debounce window at all.
	if _, err := a.Authenticate(bearerRequest(wire)); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	waitFor(t, "the first touch to land", func() bool { return ts.touchCount("tok-debounce") == 1 })

	for range 2 {
		if _, err := a.Authenticate(bearerRequest(wire)); err != nil {
			t.Fatalf("Authenticate: %v", err)
		}
	}

	// Give any (incorrectly fired) second touch a brief moment to land on its detached goroutine, then
	// assert it never does.
	time.Sleep(50 * time.Millisecond)
	if got := ts.touchCount("tok-debounce"); got != 1 {
		t.Errorf("expected 3 requests within the 60s debounce window to touch last-used exactly once, got %d", got)
	}
}

// TestTokenFallbackFailedTouchStillDebounces pins the debounce-on-ATTEMPT policy; the alternative
// (advance bookkeeping only on success) re-opens the storm the debounce exists to prevent.
func TestTokenFallbackFailedTouchStillDebounces(t *testing.T) {
	t.Parallel()

	wire, hash := makeToken(6)
	ts := newFakeTokenStore(map[string]store.Token{string(hash): {ID: "tok-retry"}})
	ts.setTouchErr(errors.New("boom: store unavailable"))
	inner := &spyInner{err: authn.ErrNoCredentials, mode: "anonymous"}
	a := authn.NewTokenFallback(ts, inner)

	if _, err := a.Authenticate(bearerRequest(wire)); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	waitFor(t, "the failed touch attempt to land", func() bool { return ts.attemptCount("tok-retry") == 1 })
	if got := ts.touchCount("tok-retry"); got != 0 {
		t.Fatalf("a failed touch must not count as a successful one, got touchCount=%d", got)
	}

	// The write path recovers, but we are still inside the debounce window:
	// authenticating again must NOT dispatch another write — the failed
	// attempt above already consumed this window.
	ts.setTouchErr(nil)
	if _, err := a.Authenticate(bearerRequest(wire)); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := ts.attemptCount("tok-retry"); got != 1 {
		t.Errorf("a failed touch must still debounce: want 1 attempt within the window, got %d", got)
	}
	if got := ts.touchCount("tok-retry"); got != 0 {
		t.Errorf("no successful touch expected within the debounced window, got %d", got)
	}
}

func TestTokenFallbackBearerSchemeIsCaseInsensitive(t *testing.T) {
	t.Parallel()

	wire, hash := makeToken(7)
	ts := newFakeTokenStore(map[string]store.Token{string(hash): {ID: "tok-case", Name: "ci"}})
	inner := &spyInner{err: authn.ErrNoCredentials, mode: "anonymous"}
	a := authn.NewTokenFallback(ts, inner)

	want := authz.Subject{Kind: authz.SubjectToken, ID: "tok-case", DisplayName: "ci"}

	schemes := map[string]string{
		"lowercase bearer": "bearer " + wire,
		"uppercase BEARER": "BEARER " + wire,
	}
	for name, header := range schemes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			subject, err := a.Authenticate(authorizationRequest(header))
			if err != nil {
				t.Fatalf("Authenticate: %v", err)
			}
			if !reflect.DeepEqual(subject, want) {
				t.Errorf("got %+v, want %+v", subject, want)
			}
		})
	}
}

// TestEncodeTokenHashTokenSecretRoundTrip proves I-3's exported round trip: EncodeToken renders a
// raw secret into wire form.
func TestEncodeTokenHashTokenSecretRoundTrip(t *testing.T) {
	t.Parallel()

	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = byte(i * 7)
	}

	wire := authn.EncodeToken(secret)

	wantHashHex := authn.HashTokenSecret(secret)
	sum := sha256.Sum256(secret)
	if wantHashHex != hex.EncodeToString(sum[:]) {
		t.Fatalf("HashTokenSecret(secret) = %q, want hex(sha256(secret)) = %q", wantHashHex, hex.EncodeToString(sum[:]))
	}

	// Recover the secret the way authenticateToken does: strip the "kcm_"
	// prefix and base64url-decode.
	const prefix = "kcm_"
	if len(wire) <= len(prefix) || wire[:len(prefix)] != prefix {
		t.Fatalf("EncodeToken(secret) = %q, want it to start with %q", wire, prefix)
	}
	recovered, err := base64.RawURLEncoding.DecodeString(wire[len(prefix):])
	if err != nil {
		t.Fatalf("decode EncodeToken's output: %v", err)
	}
	if !reflect.DeepEqual(recovered, secret) {
		t.Fatalf("recovered secret %x != original secret %x", recovered, secret)
	}

	if got := authn.HashTokenSecret(recovered); got != wantHashHex {
		t.Errorf("HashTokenSecret(recovered) = %q, want %q (must match what a creation endpoint hashed and stored)", got, wantHashHex)
	}
}

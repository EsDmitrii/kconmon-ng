package authn_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/config"
)

// countingKV counts the records written through it.
type countingKV struct {
	cache.KV
	writes atomic.Int32
}

func (k *countingKV) Set(ctx context.Context, key string, val []byte, ttl time.Duration) error {
	k.writes.Add(1)
	return k.KV.Set(ctx, key, val, ttl)
}

func (k *countingKV) SetNX(ctx context.Context, key string, val []byte, ttl time.Duration) (bool, error) {
	k.writes.Add(1)
	return k.KV.SetNX(ctx, key, val, ttl)
}

// newStateFixture is an authenticator whose state records, if any, land in kv, apart from its sessions.
func newStateFixture(t *testing.T, idp *fakeIDP, kv cache.KV, clientSecret string) *authn.OIDCAuthenticator {
	t.Helper()
	sessionKV := cache.NewInProcessKV()
	t.Cleanup(sessionKV.Close)
	cfg := config.OIDCConfig{
		Issuer:      idp.issuer(),
		ClientID:    testClientID,
		RedirectURL: "http://console.example" + config.OIDCCallbackPath,
	}
	a, err := authn.NewOIDC(context.Background(), cfg, clientSecret, authn.NewSessionStore(sessionKV, time.Hour, 0), kv,
		authn.OIDCSessionCookieName)
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	return a
}

/*
GET /api/v1/auth/oidc/start is public, and it used to park a record per call in the KV. Budgets keyed
on the client address bounded that only as far as the address could be believed, and the budget of
the trusted proxy that stood in for the rest became one counter every user of the ingress shared.
Starting a sign-in now stores nothing, so there is nothing for any budget to protect.
*/
func TestOIDCSignInStartStoresNothing(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	kv := &countingKV{KV: cache.NewInProcessKV()}
	t.Cleanup(kv.KV.(*cache.InProcessKV).Close)
	a := newStateFixture(t, idp, kv, testClientSecret)

	for i := range 200 {
		if _, err := a.AuthorizeURL(context.Background(), testReturnTo); err != nil {
			t.Fatalf("AuthorizeURL %d: %v", i, err)
		}
	}
	if got := kv.writes.Load(); got != 0 {
		t.Fatalf("200 sign-in starts wrote %d records, want 0", got)
	}
}

// Replicas share the client secret, not a record: a sign-in started on one completes on another.
func TestOIDCSignInStartedOnOneReplicaCompletesOnAnother(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	kvA, kvB := cache.NewInProcessKV(), cache.NewInProcessKV()
	t.Cleanup(kvA.Close)
	t.Cleanup(kvB.Close)
	started, finishing := newStateFixture(t, idp, kvA, testClientSecret), newStateFixture(t, idp, kvB, testClientSecret)

	state, code := authorizeAndRedirect(t, started)
	sessionID, returnTo, err := finishing.Callback(context.Background(), state, code)
	if err != nil {
		t.Fatalf("Callback on the other replica: %v", err)
	}
	if sessionID == "" || returnTo != testReturnTo {
		t.Fatalf("Callback = (%q, %q), want a session and %q", sessionID, returnTo, testReturnTo)
	}
}

// A state is only as good as the key it was sealed with: one minted elsewhere, or altered on the way,
// is refused before the token endpoint is asked anything.
func TestOIDCCallbackRefusesAStateThisConsoleDidNotSeal(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	kv := cache.NewInProcessKV()
	t.Cleanup(kv.Close)
	a := newStateFixture(t, idp, kv, testClientSecret)
	foreign := newStateFixture(t, idp, kv, "another-console-secret")

	foreignState, code := authorizeAndRedirect(t, foreign)
	ownState, _ := authorizeAndRedirect(t, a)
	altered := []byte(ownState)
	altered[len(altered)/2] ^= 'A' ^ 'B'
	for name, state := range map[string]string{
		"sealed under another secret": foreignState,
		"altered":                     string(altered),
		"truncated":                   ownState[:len(ownState)/2],
	} {
		if _, _, err := a.Callback(context.Background(), state, code); !errors.Is(err, authn.ErrInvalid) {
			t.Errorf("%s: Callback err = %v, want ErrInvalid", name, err)
		}
	}
	if got := idp.tokenRequests.Load(); got != 0 {
		t.Errorf("token requests = %d, want 0", got)
	}
}

// The state is also the state cookie's value, so the longest return path the console accepts (2048
// bytes, however many characters JSON would escape) still has to fit in one cookie.
func TestOIDCStateForTheLongestReturnPathFitsInACookie(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	kv := cache.NewInProcessKV()
	t.Cleanup(kv.Close)
	a := newStateFixture(t, idp, kv, testClientSecret)

	returnTo := "/" + strings.Repeat(`"`, 2047)
	authURL, err := a.AuthorizeURL(context.Background(), returnTo)
	if err != nil {
		t.Fatalf("AuthorizeURL: %v", err)
	}
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	if state := u.Query().Get("state"); len(state) > 3800 {
		t.Fatalf("state for a %d-byte return path is %d bytes, more than a cookie holds with its attributes",
			len(returnTo), len(state))
	}
}

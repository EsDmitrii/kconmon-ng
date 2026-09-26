package authn_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/config"
)

/*
With console.replicas > 1 every replica reads and writes the same session records in Valkey. The
refresh used to be serialized per process only: two replicas refreshing one session posted the same
refresh token, and an IdP that rotates refresh tokens refused the second, which deleted the shared
session and signed the user out. The idle touch rewrote the whole record from a copy it had just
read, so it could put a rotated-away refresh token back.
*/

// newOIDCReplica is one console replica: its own authenticator and SessionStore over the shared kv.
func newOIDCReplica(t *testing.T, idp *fakeIDP, kv cache.KV, idle time.Duration) (*authn.OIDCAuthenticator, *authn.SessionStore) {
	t.Helper()
	sessions := authn.NewSessionStore(kv, time.Hour, idle)
	cfg := config.OIDCConfig{
		Issuer:        idp.issuer(),
		ClientID:      testClientID,
		RedirectURL:   "http://console.example" + config.OIDCCallbackPath,
		Scopes:        []string{"openid", "profile", "email", "groups"},
		UsernameClaim: "preferred_username",
		GroupsClaim:   "groups",
	}
	a, err := authn.NewOIDC(context.Background(), cfg, testClientSecret, sessions, kv, authn.OIDCSessionCookieName)
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	return a, sessions
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestOIDCRefreshAcrossReplicasPostsTheRotatingTokenOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		accessLeft time.Duration
	}{
		{"access token still valid", 30 * time.Second},
		{"access token already expired", -time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			idp := newFakeIDP(t)
			accepted := idp.setRotatingRefresh(300 * time.Millisecond)
			kv := cache.NewInProcessKV()
			t.Cleanup(kv.Close)
			a, sessions := newOIDCReplica(t, idp, kv, 0)
			b, _ := newOIDCReplica(t, idp, kv, 0)
			id := newNearExpirySession(t, sessions, time.Now().Add(tc.accessLeft))

			errA := make(chan error, 1)
			go func() {
				_, err := a.Authenticate(cookieRequest(id))
				errA <- err
			}()
			waitClosed(t, accepted, "replica A's refresh to reach the IdP")

			if _, err := b.Authenticate(cookieRequest(id)); err != nil {
				t.Errorf("replica B while replica A was refreshing: %v", err)
			}
			if err := <-errA; err != nil {
				t.Errorf("replica A: %v", err)
			}
			if got := idp.tokenRequests.Load(); got != 1 {
				t.Errorf("two replicas posted %d refresh grants for one session, want 1", got)
			}
			sess, ok, err := sessions.Get(context.Background(), id)
			if err != nil || !ok {
				t.Fatalf("the session is gone after two replicas refreshed it (ok=%v err=%v): the user was signed out", ok, err)
			}
			if want := idp.currentRefreshToken(); sess.RefreshToken != want {
				t.Errorf("stored refresh token = %q, want the one the IdP accepts now (%q)", sess.RefreshToken, want)
			}
		})
	}
}

// holdingKV holds the first write to a session record, once armed, until release is closed.
type holdingKV struct {
	cache.KV
	mu      sync.Mutex
	armed   bool
	writing chan struct{}
	release chan struct{}
}

func newHoldingKV(kv cache.KV) *holdingKV {
	return &holdingKV{KV: kv, armed: true, writing: make(chan struct{}), release: make(chan struct{})}
}

func (h *holdingKV) hold(key string) {
	if !strings.HasPrefix(key, "sess:") {
		return
	}
	h.mu.Lock()
	armed := h.armed
	h.armed = false
	h.mu.Unlock()
	if armed {
		close(h.writing)
		<-h.release
	}
}

func (h *holdingKV) Set(ctx context.Context, key string, val []byte, ttl time.Duration) error {
	h.hold(key)
	return h.KV.Set(ctx, key, val, ttl)
}

func (h *holdingKV) SetXX(ctx context.Context, key string, val []byte, ttl time.Duration) (bool, error) {
	h.hold(key)
	return h.KV.SetXX(ctx, key, val, ttl)
}

// storeStaleSeenSession stores an OIDC session whose idle touch is due and whose access token is
// within the refresh margin.
func storeStaleSeenSession(t *testing.T, kv cache.KV) string {
	t.Helper()
	now := time.Now()
	sess := authn.Session{
		ID:           "replica-shared-session",
		Username:     "oidc:user-sub-1",
		DisplayName:  "alice",
		IssuedAt:     now.Add(-20 * time.Minute),
		ExpiresAt:    now.Add(time.Hour),
		LastSeenAt:   now.Add(-10 * time.Minute),
		RefreshToken: "initial-refresh-token",
		AccessExpiry: now.Add(30 * time.Second),
	}
	data, err := json.Marshal(sess)
	if err != nil {
		t.Fatal(err)
	}
	if err := kv.Set(context.Background(), "sess:"+sess.ID, data, time.Hour); err != nil {
		t.Fatal(err)
	}
	return sess.ID
}

func TestOIDCIdleTouchCannotWriteBackARotatedRefreshToken(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	idp.setRotatingRefresh(0)
	kv := cache.NewInProcessKV()
	t.Cleanup(kv.Close)
	held := newHoldingKV(kv)
	a, sessions := newOIDCReplica(t, idp, kv, time.Hour)
	b, _ := newOIDCReplica(t, idp, held, time.Hour)
	id := storeStaleSeenSession(t, kv)

	errB := make(chan error, 1)
	go func() {
		_, err := b.Authenticate(cookieRequest(id))
		errB <- err
	}()
	// Replica B's idle touch has read the record and is about to write it back.
	waitClosed(t, held.writing, "replica B's idle touch")

	if _, err := a.Authenticate(cookieRequest(id)); err != nil {
		t.Errorf("replica A: %v", err)
	}
	close(held.release)
	if err := <-errB; err != nil {
		t.Errorf("replica B: %v", err)
	}

	sess, ok, err := sessions.Get(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("the session is gone (ok=%v err=%v): the user was signed out", ok, err)
	}
	if want := idp.currentRefreshToken(); sess.RefreshToken != want {
		t.Errorf("stored refresh token = %q, want the one the IdP accepts now (%q): the idle touch wrote a stale record back", sess.RefreshToken, want)
	}
	if _, err := a.Authenticate(cookieRequest(id)); err != nil {
		t.Errorf("Authenticate after both replicas were done: %v", err)
	}
}

// A logout that lands while a refresh is at the IdP stays a logout.
func TestOIDCRefreshDoesNotResurrectASessionLoggedOutMidRefresh(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	accepted := idp.setRotatingRefresh(300 * time.Millisecond)
	kv := cache.NewInProcessKV()
	t.Cleanup(kv.Close)
	a, sessions := newOIDCReplica(t, idp, kv, 0)
	id := newNearExpirySession(t, sessions, time.Now().Add(30*time.Second))

	errA := make(chan error, 1)
	go func() {
		_, err := a.Authenticate(cookieRequest(id))
		errA <- err
	}()
	waitClosed(t, accepted, "the refresh to reach the IdP")
	if err := sessions.Delete(context.Background(), id); err != nil {
		t.Fatalf("logout: %v", err)
	}

	if err := <-errA; !errors.Is(err, authn.ErrExpired) {
		t.Errorf("Authenticate racing a logout = %v, want ErrExpired", err)
	}
	if _, ok, _ := sessions.Get(context.Background(), id); ok {
		t.Error("the refresh wrote a logged-out session back into the store")
	}
}

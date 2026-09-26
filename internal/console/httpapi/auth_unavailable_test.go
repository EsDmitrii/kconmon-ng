package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// flakyUserStore answers like fakeUserStore until down is set, then fails every lookup the way a
// restarting Postgres does.
type flakyUserStore struct {
	fakeUserStore
	down atomic.Bool
}

var errStoreDown = errors.New("dial tcp 10.0.0.5:5432: connect: connection refused")

func (f *flakyUserStore) GetUserByUsername(ctx context.Context, username string) (store.User, error) {
	if f.down.Load() {
		return store.User{}, errStoreDown
	}
	return f.fakeUserStore.GetUserByUsername(ctx, username)
}

func (f *flakyUserStore) GetUserByID(ctx context.Context, id string) (store.User, error) {
	if f.down.Load() {
		return store.User{}, errStoreDown
	}
	return f.fakeUserStore.GetUserByID(ctx, id)
}

// A store blip is not a signed-out user: a 401 sends the SPA to the login page and API clients into
// a re-login, so a one-second Postgres restart would bounce everyone. It must answer 503.
func TestStoreOutageDuringAuthenticationIs503Not401(t *testing.T) {
	users := &flakyUserStore{fakeUserStore: fakeUserStore{users: map[string]store.User{
		"alice": {ID: "u-1", Username: "alice", DisplayName: "Alice"},
	}}}
	kv := cache.NewInProcessKV()
	t.Cleanup(kv.Close)
	sessions := authn.NewSessionStore(kv, time.Hour, 0)
	cfg := authTestConfig("local")
	cfg.Auth.DefaultRole = "viewer"
	reg := prometheus.NewRegistry()
	s := NewServer(Deps{
		Config: cfg, Metrics: metrics.New(cfg.MetricsPrefix, reg), PromRegistry: reg,
		UI:            http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("spa")) }),
		Users:         users,
		Sessions:      sessions,
		Authenticator: authn.NewLocal(users, sessions, cfg.Auth.Session.CookieName),
		Policy:        authz.NewPolicy(nil),
	})
	sid, err := sessions.Create(context.Background(), authn.Session{Username: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	withSession := func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: cfg.Auth.Session.CookieName, Value: sid})
	}

	if w := doRequest(t, s, http.MethodGet, "/api/v1/auth/me", nil, withSession); w.Code != http.StatusOK {
		t.Fatalf("healthy store: /auth/me = %d, want 200: %s", w.Code, w.Body)
	}

	users.down.Store(true)
	for _, path := range []string{"/api/v1/auth/me", "/api/v1/events", "/api/v1/matrix"} {
		w := doRequest(t, s, http.MethodGet, path, nil, withSession)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("store down: GET %s = %d, want 503: %s", path, w.Code, w.Body)
			continue
		}
		if w.Header().Get("Retry-After") == "" {
			t.Errorf("store down: GET %s has no Retry-After", path)
		}
		var p problem
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil || p.Title != "authentication unavailable" {
			t.Errorf("store down: GET %s body = %s, want the authentication unavailable problem", path, w.Body)
		}
	}

	// No credentials at all is still a plain 401 while the store is down.
	if w := doRequest(t, s, http.MethodGet, "/api/v1/events", nil, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("store down, no cookie: /api/v1/events = %d, want 401", w.Code)
	}

	users.down.Store(false)
	if w := doRequest(t, s, http.MethodGet, "/api/v1/auth/me", nil, withSession); w.Code != http.StatusOK {
		t.Errorf("store back: /auth/me = %d, want 200 with the same session: %s", w.Code, w.Body)
	}
}

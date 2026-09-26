package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// DeleteUserGuarded mirrors the store: under the same lock as UpdateUserGuarded, guard first, and
// the user and their direct bindings go together.
func (f *fakeUserAdmin) DeleteUserGuarded(ctx context.Context, id string, guard func(context.Context) error) error {
	f.guardMu.Lock()
	defer f.guardMu.Unlock()
	if guard != nil {
		if err := guard(ctx); err != nil {
			return err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.users[id]; !ok {
		return store.ErrNotFound
	}
	delete(f.users, id)
	delete(f.roles, id)
	return nil
}

func TestUsersDeleteRemovesTheUserAndRevokesTheirTokens(t *testing.T) {
	admin := newFakeUserAdmin(store.User{ID: "u-admin", Username: "root"}, store.User{ID: "u-bob", Username: "bob"})
	admin.roles["u-bob"] = "viewer"
	tokens := newFakeTokenStore()
	seedToken(tokens, "00000000-0000-4000-8000-0000000000b1", "u-bob")
	seedToken(tokens, "00000000-0000-4000-8000-0000000000b2", "00000000-0000-4000-8000-0000000000b1")
	seedToken(tokens, "00000000-0000-4000-8000-0000000000a1", "u-admin")
	s := newUsersServer(t, admin, rolesByID{"u-admin": {"admin"}})
	s.tokens = tokens

	w := doRequest(t, s, http.MethodDelete, "/api/v1/users/u-bob", nil, mutateWithCSRF)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s, want 204", w.Code, w.Body)
	}
	if _, ok := admin.users["u-bob"]; ok {
		t.Fatal("bob is still there")
	}
	if _, ok := admin.roles["u-bob"]; ok {
		t.Fatal("bob's binding outlived him")
	}
	for _, id := range []string{"00000000-0000-4000-8000-0000000000b1", "00000000-0000-4000-8000-0000000000b2"} {
		if !tokenRevoked(tokens, id) {
			t.Errorf("bob's token %s is still active after the delete", id)
		}
	}
	if tokenRevoked(tokens, "00000000-0000-4000-8000-0000000000a1") {
		t.Error("another user's token was revoked")
	}
	if w := doRequest(t, s, http.MethodDelete, "/api/v1/users/u-bob", nil, mutateWithCSRF); w.Code != http.StatusNotFound {
		t.Fatalf("deleting a deleted user = %d, want 404", w.Code)
	}
	// The username is free again.
	body := `{"username":"bob","password":"a long enough secret","role":"viewer"}`
	if w := doRequest(t, s, http.MethodPost, "/api/v1/users", strings.NewReader(body), mutateWithCSRF); w.Code != http.StatusCreated {
		t.Fatalf("recreating bob = %d %s, want 201", w.Code, w.Body)
	}
}

func TestUsersDeleteRefusesTheLastAdmin(t *testing.T) {
	admin := newFakeUserAdmin(store.User{ID: "u-admin", Username: "root"}, store.User{ID: "u-2", Username: "ops"})
	s := newUsersServer(t, admin, rolesByID{"u-admin": {"admin"}, "u-2": {"operator"}})

	w := doRequest(t, s, http.MethodDelete, "/api/v1/users/u-admin", nil, mutateWithCSRF)
	if w.Code != http.StatusConflict {
		t.Fatalf("deleting the last admin = %d %s, want 409", w.Code, w.Body)
	}
	if _, ok := admin.users["u-admin"]; !ok {
		t.Fatal("the last admin was deleted anyway")
	}
}

// Deleting a user whose tokens cannot be revoked would leave the tokens working: a token whose owner
// row is gone passes the owner check by design.
func TestUsersDeleteRefusedWhenTheTokensCannotBeRevoked(t *testing.T) {
	admin := newFakeUserAdmin(store.User{ID: "u-admin", Username: "root"}, store.User{ID: "u-bob", Username: "bob"})
	tokens := newFakeTokenStore()
	seedToken(tokens, "00000000-0000-4000-8000-0000000000b1", "u-bob")
	s := newUsersServer(t, admin, rolesByID{"u-admin": {"admin"}})
	s.tokens = failingRevokes{tokens}

	w := doRequest(t, s, http.MethodDelete, "/api/v1/users/u-bob", nil, mutateWithCSRF)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("delete with the token store down = %d %s, want 502", w.Code, w.Body)
	}
	if _, ok := admin.users["u-bob"]; !ok {
		t.Fatal("bob was deleted with his token still active")
	}
}

func TestUsersDeleteNeedsUsersManage(t *testing.T) {
	admin := newFakeUserAdmin(store.User{ID: "u-admin", Username: "root"}, store.User{ID: "u-bob", Username: "bob"})
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u-bob"}}
	s := newAuthzServer(t, authr, authz.NewPolicy(nil), Deps{UserAdmin: admin, Roles: rolesByID{"u-bob": {"operator"}}})

	if w := doRequest(t, s, http.MethodDelete, "/api/v1/users/u-admin", nil, mutateWithCSRF); w.Code != http.StatusForbidden {
		t.Fatalf("delete without users:manage = %d, want 403", w.Code)
	}
}

// A session of the deleted user ends, and does not pass to a new account that reuses the username.
func TestUsersDeleteEndsTheUsersSessions(t *testing.T) {
	hash, err := authn.HashPassword("bob's old secret value")
	if err != nil {
		t.Fatal(err)
	}
	admin := newFakeUserAdmin(
		store.User{ID: "u-admin", Username: "root"},
		store.User{ID: "u-bob", Username: "bob", PasswordHash: hash},
	)
	kv := cache.NewInProcessKV()
	t.Cleanup(kv.Close)
	sessions := authn.NewSessionStore(kv, time.Hour, 0)
	cfg := authTestConfig("local")
	cfg.Auth.DefaultRole = "viewer"
	reg := prometheus.NewRegistry()
	s := NewServer(Deps{
		Config: cfg, Metrics: metrics.New(cfg.MetricsPrefix, reg), PromRegistry: reg,
		UI:            http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("spa")) }),
		Users:         admin,
		UserAdmin:     admin,
		Sessions:      sessions,
		Authenticator: authn.NewLocal(admin, sessions, cfg.Auth.Session.CookieName),
		Policy:        authz.NewPolicy(nil),
		Roles:         rolesByID{"u-admin": {"admin"}},
	})
	sid, err := sessions.Create(context.Background(), authn.Session{Username: "bob", PasswordStamp: authn.SessionStamp(hash, 0)})
	if err != nil {
		t.Fatal(err)
	}
	asBob := func(r *http.Request) { r.AddCookie(&http.Cookie{Name: cfg.Auth.Session.CookieName, Value: sid}) }
	if w := doRequest(t, s, http.MethodGet, "/api/v1/auth/me", nil, asBob); w.Code != http.StatusOK {
		t.Fatalf("bob before the delete = %d %s, want 200", w.Code, w.Body)
	}

	if err := admin.DeleteUserGuarded(context.Background(), "u-bob", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.CreateUserWithRole(context.Background(), "bob", mustHash(t, "a different secret"), "Bob 2", "viewer"); err != nil {
		t.Fatal(err)
	}
	if w := doRequest(t, s, http.MethodGet, "/api/v1/auth/me", nil, asBob); w.Code != http.StatusUnauthorized {
		t.Fatalf("the deleted bob's session = %d %s, want 401", w.Code, w.Body)
	}
}

func mustHash(t *testing.T, p string) string {
	t.Helper()
	h, err := authn.HashPassword(p)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

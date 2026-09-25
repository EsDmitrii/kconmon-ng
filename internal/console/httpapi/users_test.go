package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/config"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

type fakeUserAdmin struct {
	users map[string]store.User // by id
	roles map[string]string     // id -> role bound at create
}

func newFakeUserAdmin(users ...store.User) *fakeUserAdmin {
	f := &fakeUserAdmin{users: map[string]store.User{}, roles: map[string]string{}}
	for _, u := range users {
		f.users[u.ID] = u
	}
	return f
}

func (f *fakeUserAdmin) ListUsers(context.Context) ([]store.User, error) {
	out := make([]store.User, 0, len(f.users))
	for _, u := range f.users {
		u.PasswordHash = ""
		out = append(out, u)
	}
	return out, nil
}

func (f *fakeUserAdmin) GetUserByID(_ context.Context, id string) (store.User, error) {
	u, ok := f.users[id]
	if !ok {
		return store.User{}, store.ErrNotFound
	}
	return u, nil
}

func (f *fakeUserAdmin) GetUserByUsername(_ context.Context, name string) (store.User, error) {
	for _, u := range f.users {
		if u.Username == name {
			return u, nil
		}
	}
	return store.User{}, store.ErrNotFound
}

func (f *fakeUserAdmin) CreateUserWithRole(_ context.Context, username, hash, display, role string) (store.User, error) {
	for _, u := range f.users {
		if u.Username == username {
			return store.User{}, store.ErrAlreadyExists
		}
	}
	u := store.User{ID: "u-" + username, Username: username, PasswordHash: hash, DisplayName: display, CreatedAt: time.Unix(0, 0)}
	f.users[u.ID], f.roles[u.ID] = u, role
	return u, nil
}

func (f *fakeUserAdmin) UpdateUserPassword(_ context.Context, id, hash string) error {
	u, ok := f.users[id]
	if !ok {
		return store.ErrNotFound
	}
	u.PasswordHash = hash
	f.users[id] = u
	return nil
}

func (f *fakeUserAdmin) SetUserDisabled(_ context.Context, id string, disabled bool) error {
	u, ok := f.users[id]
	if !ok {
		return store.ErrNotFound
	}
	u.Disabled = disabled
	f.users[id] = u
	return nil
}

func (f *fakeUserAdmin) SetUserRole(_ context.Context, id, role string) error {
	if _, ok := f.users[id]; !ok {
		return store.ErrNotFound
	}
	f.roles[id] = role
	return nil
}

// rolesByID resolves roles per subject id, so the last-admin guard sees who is an admin.
type rolesByID map[string][]string

func (r rolesByID) RolesFor(_ context.Context, s authz.Subject) ([]string, error) { //nolint:gocritic // matches the seam
	return r[s.ID], nil
}

func newUsersServer(t *testing.T, admin *fakeUserAdmin, roles rolesByID) *Server {
	t.Helper()
	policy := authz.NewPolicy(nil)
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u-admin"}}
	return newAuthzServer(t, authr, policy, Deps{UserAdmin: admin, Roles: roles})
}

func TestUsersCreateBindsTheRoleAndNeverEchoesThePassword(t *testing.T) {
	admin := newFakeUserAdmin(store.User{ID: "u-admin", Username: "root"})
	s := newUsersServer(t, admin, rolesByID{"u-admin": {"admin"}})

	body := `{"username":"bob","displayName":"Bob","password":"a long enough secret","role":"operator"}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/users", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "argon2") {
		t.Fatalf("response leaks the password or its hash: %s", w.Body)
	}
	if admin.roles["u-bob"] != "operator" {
		t.Fatalf("role bound = %q, want operator", admin.roles["u-bob"])
	}
	if ok, _ := authn.VerifyPassword(admin.users["u-bob"].PasswordHash, "a long enough secret"); !ok {
		t.Fatal("stored hash does not verify the submitted password")
	}
}

func TestUsersCreateRefusesBadInput(t *testing.T) {
	s := newUsersServer(t, newFakeUserAdmin(store.User{ID: "u-admin", Username: "root"}), rolesByID{"u-admin": {"admin"}})
	for name, body := range map[string]string{
		"short password": `{"username":"bob","password":"short","role":"viewer"}`,
		"unknown role":   `{"username":"bob","password":"a long enough secret","role":"superuser"}`,
		"bad username":   `{"username":"bob smith","password":"a long enough secret","role":"viewer"}`,
		"unknown field":  `{"username":"bob","password":"a long enough secret","role":"viewer","admin":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			w := doRequest(t, s, http.MethodPost, "/api/v1/users", strings.NewReader(body), mutateWithCSRF)
			if w.Code != http.StatusBadRequest && w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("%s = %d %s, want 400 or 422", name, w.Code, w.Body)
			}
		})
	}
	dup := `{"username":"root","password":"a long enough secret","role":"viewer"}`
	if w := doRequest(t, s, http.MethodPost, "/api/v1/users", strings.NewReader(dup), mutateWithCSRF); w.Code != http.StatusConflict {
		t.Fatalf("duplicate username = %d, want 409", w.Code)
	}
}

func TestUsersListShowsResolvedRoles(t *testing.T) {
	admin := newFakeUserAdmin(
		store.User{ID: "u-admin", Username: "root", PasswordHash: "argon2id$x"},
		store.User{ID: "u-2", Username: "ops"},
	)
	s := newUsersServer(t, admin, rolesByID{"u-admin": {"admin"}, "u-2": {"operator"}})
	w := doRequest(t, s, http.MethodGet, "/api/v1/users", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d %s", w.Code, w.Body)
	}
	var got struct {
		Users []struct {
			Username string   `json:"username"`
			Roles    []string `json:"roles"`
		} `json:"users"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Users) != 2 || strings.Contains(w.Body.String(), "argon2id") {
		t.Fatalf("list = %s", w.Body)
	}
}

// The last user who can manage users cannot be disabled: that is a console nobody can administer.
func TestUsersRefusesToDisableTheLastAdmin(t *testing.T) {
	admin := newFakeUserAdmin(store.User{ID: "u-admin", Username: "root"}, store.User{ID: "u-2", Username: "ops"})
	s := newUsersServer(t, admin, rolesByID{"u-admin": {"admin"}, "u-2": {"operator"}})

	w := doRequest(t, s, http.MethodPatch, "/api/v1/users/u-admin", strings.NewReader(`{"disabled":true}`), mutateWithCSRF)
	if w.Code != http.StatusConflict {
		t.Fatalf("disabling the last admin = %d %s, want 409", w.Code, w.Body)
	}
	if admin.users["u-admin"].Disabled {
		t.Fatal("the last admin was disabled anyway")
	}
	// A non-admin can be disabled, and re-enabled.
	for _, v := range []string{"true", "false"} {
		w = doRequest(t, s, http.MethodPatch, "/api/v1/users/u-2", strings.NewReader(`{"disabled":`+v+`}`), mutateWithCSRF)
		if w.Code != http.StatusOK {
			t.Fatalf("disabled=%s on a non-admin = %d %s", v, w.Code, w.Body)
		}
	}
}

func TestUsersRoleChangeAndDemotionGuard(t *testing.T) {
	admin := newFakeUserAdmin(store.User{ID: "u-admin", Username: "root"}, store.User{ID: "u-2", Username: "ops"})
	s := newUsersServer(t, admin, rolesByID{"u-admin": {"admin"}, "u-2": {"viewer"}})

	w := doRequest(t, s, http.MethodPatch, "/api/v1/users/u-2", strings.NewReader(`{"role":"operator"}`), mutateWithCSRF)
	if w.Code != http.StatusOK || admin.roles["u-2"] != "operator" {
		t.Fatalf("role change = %d %s, bound %q; want 200 and operator", w.Code, w.Body, admin.roles["u-2"])
	}
	// Demoting the only admin is the same lock-out as disabling them.
	w = doRequest(t, s, http.MethodPatch, "/api/v1/users/u-admin", strings.NewReader(`{"role":"viewer"}`), mutateWithCSRF)
	if w.Code != http.StatusConflict {
		t.Fatalf("demoting the last admin = %d %s, want 409", w.Code, w.Body)
	}
	if w := doRequest(t, s, http.MethodPatch, "/api/v1/users/u-2", strings.NewReader(`{}`), mutateWithCSRF); w.Code != http.StatusBadRequest {
		t.Fatalf("an empty patch = %d, want 400", w.Code)
	}
	if w := doRequest(t, s, http.MethodPatch, "/api/v1/users/u-2", strings.NewReader(`{"role":"nope"}`), mutateWithCSRF); w.Code != http.StatusBadRequest {
		t.Fatalf("an unknown role = %d, want 400", w.Code)
	}
}

func TestUsersPasswordResetStoresANewHash(t *testing.T) {
	admin := newFakeUserAdmin(store.User{ID: "u-admin", Username: "root"}, store.User{ID: "u-2", Username: "ops", PasswordHash: "old"})
	s := newUsersServer(t, admin, rolesByID{"u-admin": {"admin"}})
	w := doRequest(t, s, http.MethodPost, "/api/v1/users/u-2/password",
		strings.NewReader(`{"password":"a brand new secret"}`), mutateWithCSRF)
	if w.Code != http.StatusNoContent {
		t.Fatalf("reset = %d %s, want 204", w.Code, w.Body)
	}
	if ok, _ := authn.VerifyPassword(admin.users["u-2"].PasswordHash, "a brand new secret"); !ok {
		t.Fatal("reset did not store a hash of the new password")
	}
	if w := doRequest(t, s, http.MethodPost, "/api/v1/users/nobody/password",
		strings.NewReader(`{"password":"a brand new secret"}`), mutateWithCSRF); w.Code != http.StatusNotFound {
		t.Fatalf("reset of an unknown user = %d, want 404", w.Code)
	}
}

// Outside auth.mode=local the identity provider owns the accounts: even an admin gets 404, not an
// empty list that suggests users could be managed here. (Anonymous mode never gets that far: its
// viewer role lacks users:manage and authorize answers 403.)
func TestUsersRoutesAreLocalModeOnly(t *testing.T) {
	s := newUsersServer(t, newFakeUserAdmin(store.User{ID: "u-admin", Username: "root"}), rolesByID{"u-admin": {"admin"}})
	s.cfg.Auth.Mode = "oidc"
	if w := doRequest(t, s, http.MethodGet, "/api/v1/users", nil, nil); w.Code != http.StatusNotFound {
		t.Fatalf("users outside local mode = %d, want 404", w.Code)
	}
	if w := doRequest(t, newTestServer(t), http.MethodGet, "/api/v1/users", nil, nil); w.Code != http.StatusForbidden {
		t.Fatalf("anonymous users list = %d, want 403", w.Code)
	}
}

func TestAuthPasswordChangeKeepsTheCallerSignedInAndStalesOtherSessions(t *testing.T) {
	hash, err := authn.HashPassword("the current secret")
	if err != nil {
		t.Fatal(err)
	}
	alice := store.User{ID: "u-1", Username: "alice", PasswordHash: hash, DisplayName: "Alice"}
	admin := newFakeUserAdmin(alice)
	s := newLocalAuthServer(t) // local mode, real session store, fakeUserStore with alice
	s.userAdmin = admin
	s.users = admin // the password check and the stamp read the same rows the update writes

	ctx := context.Background()
	mine, _ := s.sessions.Create(ctx, authn.Session{Username: "alice", PasswordStamp: authn.PasswordStamp(hash)})
	other, _ := s.sessions.Create(ctx, authn.Session{Username: "alice", PasswordStamp: authn.PasswordStamp(hash)})

	w := doRequest(t, s, http.MethodPost, "/api/v1/auth/password",
		strings.NewReader(`{"currentPassword":"the current secret","newPassword":"the next long secret"}`),
		func(r *http.Request) {
			r.Header.Set("Content-Type", "application/json")
			r.AddCookie(&http.Cookie{Name: s.cfg.Auth.Session.CookieName, Value: mine})
		})
	if w.Code != http.StatusNoContent {
		t.Fatalf("change = %d %s, want 204", w.Code, w.Body)
	}
	var reissued string
	for _, c := range w.Result().Cookies() {
		if c.Name == s.cfg.Auth.Session.CookieName {
			reissued = c.Value
		}
	}
	if reissued == "" || reissued == mine {
		t.Fatalf("the caller must get a fresh session cookie, got %q", reissued)
	}
	if _, ok, _ := s.sessions.Get(ctx, mine); ok {
		t.Error("the caller's old session still exists")
	}
	local := authn.NewLocal(admin, s.sessions, s.cfg.Auth.Session.CookieName)
	withCookie := func(id string) *http.Request {
		r, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
		r.AddCookie(&http.Cookie{Name: s.cfg.Auth.Session.CookieName, Value: id})
		return r
	}
	if _, err := local.Authenticate(withCookie(other)); err == nil {
		t.Error("another session opened with the old password still authenticates")
	}
	if _, err := local.Authenticate(withCookie(reissued)); err != nil {
		t.Errorf("the reissued session does not authenticate: %v", err)
	}

	// Wrong current password: 401, nothing changes.
	w = doRequest(t, s, http.MethodPost, "/api/v1/auth/password",
		strings.NewReader(`{"currentPassword":"wrong","newPassword":"another long secret"}`),
		func(r *http.Request) {
			r.Header.Set("Content-Type", "application/json")
			r.AddCookie(&http.Cookie{Name: s.cfg.Auth.Session.CookieName, Value: reissued})
		})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong current password = %d, want 401", w.Code)
	}
}

// A password change verifies the current password with argon2id, exactly like a login, so it spends
// login's per-username budget: otherwise a stolen session could guess the password, and burn 64MiB
// of the pod's memory per guess, without limit.
func TestAuthPasswordChangeSharesTheLoginRateLimit(t *testing.T) {
	kv := cache.NewInProcessKV()
	t.Cleanup(kv.Close)
	ts, _ := newLoginRateLimitServer(t, config.RateLimitConfig{LoginPerMinute: 2}, kv)
	s := ts.srv
	hash, err := authn.HashPassword("the current secret")
	if err != nil {
		t.Fatal(err)
	}
	admin := newFakeUserAdmin(store.User{ID: "u-1", Username: "alice", PasswordHash: hash})
	s.userAdmin, s.users = admin, admin
	sid, err := s.sessions.Create(context.Background(), authn.Session{Username: "alice", PasswordStamp: authn.PasswordStamp(hash)})
	if err != nil {
		t.Fatal(err)
	}
	attempt := func() int {
		return doRequest(t, s, http.MethodPost, "/api/v1/auth/password",
			strings.NewReader(`{"currentPassword":"a wrong guess","newPassword":"the next long secret"}`),
			func(r *http.Request) {
				r.Header.Set("Content-Type", "application/json")
				r.AddCookie(&http.Cookie{Name: s.cfg.Auth.Session.CookieName, Value: sid})
			}).Code
	}
	for i := range 2 {
		if code := attempt(); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i, code)
		}
	}
	if code := attempt(); code != http.StatusTooManyRequests {
		t.Fatalf("third attempt = %d, want 429", code)
	}
}

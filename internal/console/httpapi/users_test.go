package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/config"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

type fakeUserAdmin struct {
	guardMu sync.Mutex
	mu      sync.Mutex
	users   map[string]store.User // by id
	roles   map[string]string     // id -> role bound at create
}

func newFakeUserAdmin(users ...store.User) *fakeUserAdmin {
	f := &fakeUserAdmin{users: map[string]store.User{}, roles: map[string]string{}}
	for _, u := range users {
		f.users[u.ID] = u
	}
	return f
}

func (f *fakeUserAdmin) ListUsers(context.Context) ([]store.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.User, 0, len(f.users))
	for _, u := range f.users {
		u.PasswordHash = ""
		out = append(out, u)
	}
	return out, nil
}

func (f *fakeUserAdmin) GetUserByID(_ context.Context, id string) (store.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return store.User{}, store.ErrNotFound
	}
	return u, nil
}

func (f *fakeUserAdmin) GetUserByUsername(_ context.Context, name string) (store.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.users {
		if u.Username == name {
			return u, nil
		}
	}
	return store.User{}, store.ErrNotFound
}

func (f *fakeUserAdmin) CreateUserWithRole(_ context.Context, username, hash, display, role string) (store.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return store.ErrNotFound
	}
	u.PasswordHash = hash
	f.users[id] = u
	return nil
}

// UpdateUserGuarded mirrors the store: guardMu stands in for the advisory lock, the guard runs under
// it, and nothing is written when the guard refuses.
func (f *fakeUserAdmin) UpdateUserGuarded(ctx context.Context, id string, change store.UserChange, guard func(context.Context) error) error {
	f.guardMu.Lock()
	defer f.guardMu.Unlock()
	if guard != nil {
		if err := guard(ctx); err != nil {
			return err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return store.ErrNotFound
	}
	if change.Disabled != nil {
		u.Disabled = *change.Disabled
		f.users[id] = u
	}
	if change.Role != nil {
		f.roles[id] = *change.Role
	}
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

// A session whose stamp no longer matches the stored hash (an admin reset it) is signed out
// everywhere, the password change included.
func TestAuthPasswordChangeRefusesAStaleSession(t *testing.T) {
	old, err := authn.HashPassword("the old secret")
	if err != nil {
		t.Fatal(err)
	}
	current, err := authn.HashPassword("the reset secret")
	if err != nil {
		t.Fatal(err)
	}
	admin := newFakeUserAdmin(store.User{ID: "u-1", Username: "alice", PasswordHash: current})
	s := newLocalAuthServer(t)
	s.userAdmin, s.users = admin, admin
	stale, _ := s.sessions.Create(context.Background(), authn.Session{Username: "alice", PasswordStamp: authn.PasswordStamp(old)})

	w := doRequest(t, s, http.MethodPost, "/api/v1/auth/password",
		strings.NewReader(`{"currentPassword":"the reset secret","newPassword":"one more long secret"}`),
		func(r *http.Request) {
			r.Header.Set("Content-Type", "application/json")
			r.AddCookie(&http.Cookie{Name: s.cfg.Auth.Session.CookieName, Value: stale})
		})
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "not signed in") {
		t.Fatalf("stale session = %d %s, want 401 not signed in", w.Code, w.Body)
	}
	if admin.users["u-1"].PasswordHash != current {
		t.Fatal("a stale session changed the password")
	}
}

// The guard works in both directions: another enabled admin makes a disable or demotion fine, and a
// disabled one does not count.
func TestUsersLastAdminGuardCountsOnlyEnabledAdmins(t *testing.T) {
	roles := rolesByID{"u-admin": {"admin"}, "u-2": {"admin"}}
	twoAdmins := func(secondDisabled bool) (*Server, *fakeUserAdmin) {
		admin := newFakeUserAdmin(store.User{ID: "u-admin", Username: "root"},
			store.User{ID: "u-2", Username: "second", Disabled: secondDisabled})
		return newUsersServer(t, admin, roles), admin
	}

	s, _ := twoAdmins(false)
	if w := doRequest(t, s, http.MethodPatch, "/api/v1/users/u-2", strings.NewReader(`{"role":"viewer"}`), mutateWithCSRF); w.Code != http.StatusOK {
		t.Fatalf("demoting one of two enabled admins = %d %s, want 200", w.Code, w.Body)
	}
	s, _ = twoAdmins(false)
	if w := doRequest(t, s, http.MethodPatch, "/api/v1/users/u-2", strings.NewReader(`{"disabled":true}`), mutateWithCSRF); w.Code != http.StatusOK {
		t.Fatalf("disabling one of two enabled admins = %d %s, want 200", w.Code, w.Body)
	}
	s, admin := twoAdmins(true)
	if w := doRequest(t, s, http.MethodPatch, "/api/v1/users/u-admin", strings.NewReader(`{"disabled":true}`), mutateWithCSRF); w.Code != http.StatusConflict {
		t.Fatalf("disabling the only enabled admin (the other one is disabled) = %d %s, want 409", w.Code, w.Body)
	}
	if admin.users["u-admin"].Disabled {
		t.Fatal("the only enabled admin was disabled anyway")
	}
}

// Passwords are capped at 256 bytes on every route that hashes one.
func TestPasswordsOver256BytesAreRefusedEverywhere(t *testing.T) {
	long := strings.Repeat("x", 257)
	admin := newFakeUserAdmin(store.User{ID: "u-admin", Username: "root"}, store.User{ID: "u-2", Username: "ops", PasswordHash: "old"})
	s := newUsersServer(t, admin, rolesByID{"u-admin": {"admin"}})

	if w := doRequest(t, s, http.MethodPost, "/api/v1/users",
		strings.NewReader(`{"username":"bob","password":"`+long+`","role":"viewer"}`), mutateWithCSRF); w.Code != http.StatusBadRequest {
		t.Fatalf("create with a 257-byte password = %d, want 400", w.Code)
	}
	if w := doRequest(t, s, http.MethodPost, "/api/v1/users/u-2/password",
		strings.NewReader(`{"password":"`+long+`"}`), mutateWithCSRF); w.Code != http.StatusBadRequest {
		t.Fatalf("reset to a 257-byte password = %d, want 400", w.Code)
	}

	hash, err := authn.HashPassword("the current secret")
	if err != nil {
		t.Fatal(err)
	}
	alice := newFakeUserAdmin(store.User{ID: "u-1", Username: "alice", PasswordHash: hash})
	ls := newLocalAuthServer(t)
	ls.userAdmin, ls.users = alice, alice
	sid, _ := ls.sessions.Create(context.Background(), authn.Session{Username: "alice", PasswordStamp: authn.PasswordStamp(hash)})
	w := doRequest(t, ls, http.MethodPost, "/api/v1/auth/password",
		strings.NewReader(`{"currentPassword":"the current secret","newPassword":"`+long+`"}`),
		func(r *http.Request) {
			r.Header.Set("Content-Type", "application/json")
			r.AddCookie(&http.Cookie{Name: ls.cfg.Auth.Session.CookieName, Value: sid})
		})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("own change to a 257-byte password = %d, want 400", w.Code)
	}
}

// The display name is echoed into every session and /auth/me, so it is bounded like the username.
func TestUsersCreateRefusesAnUnstorableDisplayName(t *testing.T) {
	s := newUsersServer(t, newFakeUserAdmin(store.User{ID: "u-admin", Username: "root"}), rolesByID{"u-admin": {"admin"}})
	for name, display := range map[string]string{
		"too long":     strings.Repeat("d", 129),
		"NUL":          `bob\u0000`,
		"control char": `bob\u001b[31m`,
	} {
		t.Run(name, func(t *testing.T) {
			body := `{"username":"bob","displayName":"` + display + `","password":"a long enough secret","role":"viewer"}`
			if w := doRequest(t, s, http.MethodPost, "/api/v1/users", strings.NewReader(body), mutateWithCSRF); w.Code != http.StatusBadRequest {
				t.Fatalf("displayName %s = %d %s, want 400", name, w.Code, w.Body)
			}
		})
	}
	ok := `{"username":"bob","displayName":"Bob Smith (ops)","password":"a long enough secret","role":"viewer"}`
	if w := doRequest(t, s, http.MethodPost, "/api/v1/users", strings.NewReader(ok), mutateWithCSRF); w.Code != http.StatusCreated {
		t.Fatalf("an ordinary displayName = %d %s, want 201", w.Code, w.Body)
	}
}

// flakyUserAdmin fails the PATCH write the way a rolled-back store transaction does: nothing lands.
type flakyUserAdmin struct {
	*fakeUserAdmin
}

func (f *flakyUserAdmin) UpdateUserGuarded(context.Context, string, store.UserChange, func(context.Context) error) error {
	return errors.New("store down")
}

// A PATCH carrying both role and disabled is one store call, so a failed write answers 502 and
// leaves the user as it was. The store transaction itself is covered by
// TestUpdateUserGuardedIsAllOrNothing (store, integration).
func TestUsersPatchIsAllOrNothing(t *testing.T) {
	base := newFakeUserAdmin(store.User{ID: "u-admin", Username: "root"}, store.User{ID: "u-2", Username: "ops"})
	base.roles["u-2"] = "viewer"
	admin := &flakyUserAdmin{fakeUserAdmin: base}
	policy := authz.NewPolicy(nil)
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u-admin"}}
	s := newAuthzServer(t, authr, policy, Deps{UserAdmin: admin, Roles: rolesByID{"u-admin": {"admin"}, "u-2": {"viewer"}}})

	w := doRequest(t, s, http.MethodPatch, "/api/v1/users/u-2",
		strings.NewReader(`{"role":"operator","disabled":true}`), mutateWithCSRF)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("patch = %d %s, want 502", w.Code, w.Body)
	}
	if base.roles["u-2"] != "viewer" || base.users["u-2"].Disabled {
		t.Fatalf("half the patch landed: role %q, disabled %v", base.roles["u-2"], base.users["u-2"].Disabled)
	}
}

// rolesThenDown answers the first lookup (the caller's own, in the auth middleware) and fails
// every one after it.
type rolesThenDown struct {
	rolesByID
	calls *int
}

func (r rolesThenDown) RolesFor(ctx context.Context, s authz.Subject) ([]string, error) { //nolint:gocritic // matches the seam
	*r.calls++
	if *r.calls > 1 {
		return nil, errors.New("role store down")
	}
	return r.rolesByID.RolesFor(ctx, s)
}

// The last-admin guard fails closed: an unreadable role store is no evidence that the target is not
// the last one who can manage users.
func TestUsersLastAdminGuardFailsClosedOnARoleStoreError(t *testing.T) {
	admin := newFakeUserAdmin(store.User{ID: "u-admin", Username: "root"}, store.User{ID: "u-2", Username: "ops"})
	s := newUsersServer(t, admin, rolesByID{"u-admin": {"admin"}})
	s.roles = rolesThenDown{rolesByID: rolesByID{"u-admin": {"admin"}}, calls: new(int)}

	w := doRequest(t, s, http.MethodPatch, "/api/v1/users/u-admin", strings.NewReader(`{"disabled":true}`), mutateWithCSRF)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("disabling the only admin while the role store is down = %d %s, want 502", w.Code, w.Body)
	}
	if admin.users["u-admin"].Disabled {
		t.Fatal("the only admin was disabled because the guard could not read their roles")
	}
}

// overlappingChecks makes the last-admin checks of two concurrent PATCHes overlap: ListUsers waits
// briefly for a second caller, so unless the check and the write are serialized both checks see
// the other admin still enabled.
type overlappingChecks struct {
	*fakeUserAdmin
	entered atomic.Int32
	both    chan struct{}
}

func (o *overlappingChecks) ListUsers(ctx context.Context) ([]store.User, error) {
	users, err := o.fakeUserAdmin.ListUsers(ctx)
	if o.entered.Add(1) == 2 {
		close(o.both)
	}
	select {
	case <-o.both:
	case <-time.After(300 * time.Millisecond):
	}
	return users, err
}

// Two admins disabling each other at the same moment must not both succeed: the check and the
// write run under one lock, so the second check sees the first write.
func TestUsersConcurrentDisablesCannotRemoveTheLastAdmin(t *testing.T) {
	base := newFakeUserAdmin(store.User{ID: "u-admin", Username: "root"}, store.User{ID: "u-2", Username: "second"})
	admin := &overlappingChecks{fakeUserAdmin: base, both: make(chan struct{})}
	policy := authz.NewPolicy(nil)
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u-admin"}}
	s := newAuthzServer(t, authr, policy, Deps{UserAdmin: admin, Roles: rolesByID{"u-admin": {"admin"}, "u-2": {"admin"}}})

	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i, target := range []string{"u-admin", "u-2"} {
		wg.Go(func() {
			w := doRequest(t, s, http.MethodPatch, "/api/v1/users/"+target, strings.NewReader(`{"disabled":true}`), mutateWithCSRF)
			codes[i] = w.Code
		})
	}
	wg.Wait()

	if base.users["u-admin"].Disabled && base.users["u-2"].Disabled {
		t.Fatalf("both admins were disabled (statuses %v): nobody can manage users any more", codes)
	}
	slices.Sort(codes)
	if codes[0] != http.StatusOK || codes[1] != http.StatusConflict {
		t.Errorf("statuses = %v, want one 200 and one 409", codes)
	}
}

// A user who was disabled and enabled again signs in to a session that authenticates: login stamps
// the session with the user's current epoch, the one every request compares.
func TestLoginAfterAReEnableOpensAWorkingSession(t *testing.T) {
	hash, err := authn.HashPassword("s3cret! long enough")
	if err != nil {
		t.Fatal(err)
	}
	users := fakeUserStore{users: map[string]store.User{
		"alice": {ID: "u-1", Username: "alice", PasswordHash: hash, SessionEpoch: 2},
	}}
	s := newLocalAuthServer(t)
	s.users = users

	w := doRequest(t, s, http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(`{"username":"alice","password":"s3cret! long enough"}`), nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("login = %d %s, want 204", w.Code, w.Body)
	}
	var id string
	for _, c := range w.Result().Cookies() {
		if c.Name == s.cfg.Auth.Session.CookieName {
			id = c.Value
		}
	}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: s.cfg.Auth.Session.CookieName, Value: id})
	if _, err := authn.NewLocal(users, s.sessions, s.cfg.Auth.Session.CookieName).Authenticate(r); err != nil {
		t.Fatalf("the session login opened does not authenticate: %v", err)
	}
}

// The last-admin guard reads custom roles from the store, as the RBAC guard does, not from the
// policy cache: another replica may have just dropped users:manage from "ops" while this replica's
// policy still grants it. Counting u-2 (bound to ops) as a holder on that stale answer let the only
// real admin be disabled, or demoted to ops itself, and nobody could manage users any more.
func TestUsersLastAdminGuardReadsCustomRolesFromTheStore(t *testing.T) {
	for _, patch := range []string{`{"disabled":true}`, `{"role":"ops"}`} {
		admin := newFakeUserAdmin(store.User{ID: "u-admin", Username: "root"}, store.User{ID: "u-2", Username: "ops"})
		rbac := newFakeRoleAdmin()
		if _, err := rbac.UpsertRole(context.Background(), "ops", []string{"events:read"}); err != nil {
			t.Fatal(err)
		}
		stale := authz.NewPolicy(map[string][]authz.Permission{"ops": {authz.PermUsersManage}})
		authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u-admin"}}
		s := newAuthzServer(t, authr, stale, Deps{UserAdmin: admin, RBAC: rbac,
			Roles: rolesByID{"u-admin": {"admin"}, "u-2": {"ops"}}})

		w := doRequest(t, s, http.MethodPatch, "/api/v1/users/u-admin", strings.NewReader(patch), mutateWithCSRF)
		if w.Code != http.StatusConflict {
			t.Errorf("PATCH %s on the only admin while the policy cache still grants ops users:manage = %d %s, want 409",
				patch, w.Code, w.Body)
		}
		if admin.users["u-admin"].Disabled || admin.roles["u-admin"] != "" {
			t.Errorf("PATCH %s landed anyway: disabled %v, role %q", patch, admin.users["u-admin"].Disabled, admin.roles["u-admin"])
		}
	}
}

// failsAfterFirstLookup answers the first role lookup for id and fails every later one, the shape of a
// database blip between two back-to-back queries.
type failsAfterFirstLookup struct {
	rolesByID
	id    string
	mu    sync.Mutex
	calls int
}

func (f *failsAfterFirstLookup) RolesFor(ctx context.Context, s authz.Subject) ([]string, error) { //nolint:gocritic // matches the seam
	if s.ID == f.id {
		f.mu.Lock()
		f.calls++
		n := f.calls
		f.mu.Unlock()
		if n > 1 {
			return nil, errors.New("role store down")
		}
	}
	return f.rolesByID.RolesFor(ctx, s)
}

// The guard reads the target's roles once. A second lookup that failed used to be narrowed to no
// roles at all, so the only admin counted as a non-admin and was disabled.
func TestUsersLastAdminGuardDoesNotReadTheTargetsRolesTwice(t *testing.T) {
	admin := newFakeUserAdmin(store.User{ID: "u-admin", Username: "root"}, store.User{ID: "u-2", Username: "ops"})
	roles := &failsAfterFirstLookup{rolesByID: rolesByID{"tok-1": {"admin"}, "u-admin": {"admin"}}, id: "u-admin"}
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectToken, ID: "tok-1"}}
	s := newAuthzServer(t, authr, authz.NewPolicy(nil), Deps{UserAdmin: admin, Roles: roles})

	w := doRequest(t, s, http.MethodPatch, "/api/v1/users/u-admin", strings.NewReader(`{"disabled":true}`), mutateWithCSRF)
	if w.Code != http.StatusConflict {
		t.Fatalf("disabling the only admin = %d %s, want 409", w.Code, w.Body)
	}
	if admin.users["u-admin"].Disabled {
		t.Fatal("the only admin was disabled")
	}
}

// failingRevokes is a token store whose revocations fail.
type failingRevokes struct{ *fakeTokenStore }

func (failingRevokes) RevokeToken(context.Context, string) error { return errors.New("database down") }

func seedToken(f *fakeTokenStore, id, owner string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens[id] = store.Token{ID: id, Name: id, Owner: owner, CreatedAt: time.Now()}
}

func tokenRevoked(f *fakeTokenStore, id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokens[id].RevokedAt != nil
}

// Disabling a user ends their sessions, and it has to end their API tokens as well: a token minted by
// whoever had taken the account over must not work again once the account is re-enabled.
func TestUsersDisableRevokesTheUsersTokensAndReEnableDoesNotReviveThem(t *testing.T) {
	admin := newFakeUserAdmin(store.User{ID: "u-admin", Username: "root"}, store.User{ID: "u-bob", Username: "bob"})
	tokens := newFakeTokenStore()
	seedToken(tokens, "00000000-0000-4000-8000-0000000000b1", "u-bob")
	// Minted by bob's token before owners were inherited: owned through that token.
	seedToken(tokens, "00000000-0000-4000-8000-0000000000b2", "00000000-0000-4000-8000-0000000000b1")
	seedToken(tokens, "00000000-0000-4000-8000-0000000000a1", "u-admin")
	s := newUsersServer(t, admin, rolesByID{"u-admin": {"admin"}})
	s.tokens = tokens

	w := doRequest(t, s, http.MethodPatch, "/api/v1/users/u-bob", strings.NewReader(`{"disabled":true}`), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("disable = %d %s, want 200", w.Code, w.Body)
	}
	for _, id := range []string{"00000000-0000-4000-8000-0000000000b1", "00000000-0000-4000-8000-0000000000b2"} {
		if !tokenRevoked(tokens, id) {
			t.Errorf("bob's token %s is still active after the disable", id)
		}
	}
	if tokenRevoked(tokens, "00000000-0000-4000-8000-0000000000a1") {
		t.Error("another user's token was revoked")
	}

	// One that slipped past the disable (minted in the same instant) goes on the re-enable.
	seedToken(tokens, "00000000-0000-4000-8000-0000000000b3", "u-bob")
	w = doRequest(t, s, http.MethodPatch, "/api/v1/users/u-bob", strings.NewReader(`{"disabled":false}`), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("re-enable = %d %s, want 200", w.Code, w.Body)
	}
	if !tokenRevoked(tokens, "00000000-0000-4000-8000-0000000000b3") {
		t.Error("a token minted before the re-enable works again")
	}

	// An enabled user's role change leaves their tokens alone.
	seedToken(tokens, "00000000-0000-4000-8000-0000000000b4", "u-bob")
	w = doRequest(t, s, http.MethodPatch, "/api/v1/users/u-bob", strings.NewReader(`{"disabled":false,"role":"operator"}`), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("role change = %d %s, want 200", w.Code, w.Body)
	}
	if tokenRevoked(tokens, "00000000-0000-4000-8000-0000000000b4") {
		t.Error("a PATCH on an enabled user revoked their token")
	}
}

// A re-enable that cannot revoke the old tokens leaves the user disabled rather than reviving them.
func TestUsersReEnableRefusedWhenTheOldTokensCannotBeRevoked(t *testing.T) {
	admin := newFakeUserAdmin(store.User{ID: "u-admin", Username: "root"}, store.User{ID: "u-bob", Username: "bob", Disabled: true})
	tokens := newFakeTokenStore()
	seedToken(tokens, "00000000-0000-4000-8000-0000000000b1", "u-bob")
	s := newUsersServer(t, admin, rolesByID{"u-admin": {"admin"}})
	s.tokens = failingRevokes{tokens}

	w := doRequest(t, s, http.MethodPatch, "/api/v1/users/u-bob", strings.NewReader(`{"disabled":false}`), mutateWithCSRF)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("re-enable with the token store down = %d %s, want 502", w.Code, w.Body)
	}
	if !admin.users["u-bob"].Disabled {
		t.Fatal("bob was re-enabled with a token from before the disable still active")
	}
}

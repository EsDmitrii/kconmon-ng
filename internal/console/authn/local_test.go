package authn_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

const localCookieName = "kconmon_session"

// fakeUserStore is a minimal in-memory authn.UserStore double, keyed by username.
type fakeUserStore struct {
	users map[string]store.User
	err   error // when set, returned verbatim for every call regardless of username
}

func (f *fakeUserStore) GetUserByUsername(_ context.Context, username string) (store.User, error) {
	if f.err != nil {
		return store.User{}, f.err
	}
	u, ok := f.users[username]
	if !ok {
		return store.User{}, store.ErrNotFound
	}
	return u, nil
}

func (f *fakeUserStore) GetUserByID(_ context.Context, id string) (store.User, error) {
	if f.err != nil {
		return store.User{}, f.err
	}
	for _, u := range f.users {
		if u.ID == id {
			return u, nil
		}
	}
	return store.User{}, store.ErrNotFound
}

func newLocalFixture(t *testing.T, users map[string]store.User) (authn.Authenticator, *authn.SessionStore) {
	t.Helper()
	kv := cache.NewInProcessKV()
	t.Cleanup(kv.Close)
	sessions := authn.NewSessionStore(kv, time.Hour, 0)
	a := authn.NewLocal(&fakeUserStore{users: users}, sessions, localCookieName)
	return a, sessions
}

func requestWithCookie(value string) *http.Request {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	if value != "" {
		r.AddCookie(&http.Cookie{Name: localCookieName, Value: value})
	}
	return r
}

// TestLocalAuthenticateValidSessionResolvesSubjectIdentity proves what a valid session actually
// resolves to here; Subject.Roles is asserted implicitly (via reflect.DeepEqual against a
// zero-value/nil Roles field) but that is NOT this test proving anything about role resolution.
func TestLocalAuthenticateValidSessionResolvesSubjectIdentity(t *testing.T) {
	t.Parallel()

	const aliceID = "11111111-1111-1111-1111-111111111111"
	a, sessions := newLocalFixture(t, map[string]store.User{
		"alice": {ID: aliceID, Username: "alice", DisplayName: "Alice Example"},
	})

	id, err := sessions.Create(context.Background(), authn.Session{Username: "alice", Groups: []string{"sre", "on-call"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	subject, err := a.Authenticate(requestWithCookie(id))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	// Subject.ID must be the users.id UUID -- never the username -- so a
	// downstream RoleStore.ListBindingsForSubject lookup (subject_kind=
	// 'user', subject_id=users.id) resolves the right bindings/roles.
	want := authz.Subject{
		Kind:        authz.SubjectUser,
		ID:          aliceID,
		DisplayName: "Alice Example",
		Groups:      []string{"sre", "on-call"},
	}
	if !reflect.DeepEqual(subject, want) {
		t.Errorf("got %+v, want %+v", subject, want)
	}
}

func TestLocalAuthenticateNoCookieIsNoCredentials(t *testing.T) {
	t.Parallel()
	a, _ := newLocalFixture(t, nil)

	_, err := a.Authenticate(requestWithCookie(""))
	if !errors.Is(err, authn.ErrNoCredentials) {
		t.Fatalf("got %v, want ErrNoCredentials", err)
	}
}

func TestLocalAuthenticateUnknownSessionIDIsNoCredentialsNotInvalid(t *testing.T) {
	t.Parallel()
	a, _ := newLocalFixture(t, nil)

	_, err := a.Authenticate(requestWithCookie("this-session-id-was-never-issued"))
	if !errors.Is(err, authn.ErrNoCredentials) {
		t.Fatalf("got %v, want ErrNoCredentials (a stale cookie must re-prompt login, not read as an attack)", err)
	}
	if errors.Is(err, authn.ErrInvalid) {
		t.Fatalf("an unknown session id must NOT be ErrInvalid, got %v", err)
	}
}

func TestLocalAuthenticateDisabledUserIsErrDisabled(t *testing.T) {
	t.Parallel()

	a, sessions := newLocalFixture(t, map[string]store.User{
		"bob": {ID: "22222222-2222-2222-2222-222222222222", Username: "bob", Disabled: true},
	})
	id, err := sessions.Create(context.Background(), authn.Session{Username: "bob"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err = a.Authenticate(requestWithCookie(id))
	if !errors.Is(err, authn.ErrDisabled) {
		t.Fatalf("got %v, want ErrDisabled", err)
	}
}

// TestLocalAuthenticateDeletedUserBehindLiveSessionIsNoCredentials is not an explicitly required
// test-list case.
func TestLocalAuthenticateDeletedUserBehindLiveSessionIsNoCredentials(t *testing.T) {
	t.Parallel()

	a, sessions := newLocalFixture(t, map[string]store.User{}) // no users at all
	id, err := sessions.Create(context.Background(), authn.Session{Username: "ghost"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err = a.Authenticate(requestWithCookie(id))
	if !errors.Is(err, authn.ErrNoCredentials) {
		t.Fatalf("got %v, want ErrNoCredentials", err)
	}
}

func TestLocalAuthenticateModeIsLocal(t *testing.T) {
	t.Parallel()
	a, _ := newLocalFixture(t, nil)
	if got := a.Mode(); got != "local" {
		t.Errorf("Mode() = %q, want %q", got, "local")
	}
}

// A session opened with the old password must stop authenticating once the password changes; a
// session from before 2.5.0 (no stamp) keeps working until it expires.
func TestLocalAuthenticatorRejectsASessionOpenedWithAnOldPassword(t *testing.T) {
	oldHash, err := authn.HashPassword("old password 123")
	if err != nil {
		t.Fatal(err)
	}
	newHash, err := authn.HashPassword("new password 456")
	if err != nil {
		t.Fatal(err)
	}
	sessions := authn.NewSessionStore(cache.NewInProcessKV(), time.Hour, 0)
	ctx := context.Background()
	stamped, err := sessions.Create(ctx, authn.Session{Username: "alice", PasswordStamp: authn.PasswordStamp(oldHash)})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := sessions.Create(ctx, authn.Session{Username: "alice"})
	if err != nil {
		t.Fatal(err)
	}

	users := &mapUsers{m: map[string]store.User{"alice": {ID: "u-1", Username: "alice", PasswordHash: oldHash}}}
	a := authn.NewLocal(users, sessions, "kconmon_session")
	req := func(id string) *http.Request {
		r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
		r.AddCookie(&http.Cookie{Name: "kconmon_session", Value: id})
		return r
	}
	if _, err := a.Authenticate(req(stamped)); err != nil {
		t.Fatalf("stamped session before the change: %v", err)
	}

	users.m["alice"] = store.User{ID: "u-1", Username: "alice", PasswordHash: newHash}
	if _, err := a.Authenticate(req(stamped)); !errors.Is(err, authn.ErrNoCredentials) {
		t.Fatalf("stamped session after the change = %v, want ErrNoCredentials", err)
	}
	if _, err := a.Authenticate(req(legacy)); err != nil {
		t.Fatalf("a pre-2.5.0 session without a stamp must keep working: %v", err)
	}
}

type mapUsers struct{ m map[string]store.User }

func (u *mapUsers) GetUserByUsername(_ context.Context, name string) (store.User, error) {
	usr, ok := u.m[name]
	if !ok {
		return store.User{}, store.ErrNotFound
	}
	return usr, nil
}

func (u *mapUsers) GetUserByID(_ context.Context, id string) (store.User, error) {
	for _, usr := range u.m {
		if usr.ID == id {
			return usr, nil
		}
	}
	return store.User{}, store.ErrNotFound
}

package httpapi

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/crypto/argon2"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// otherParamsPHC is a hash at argon2 parameters this build does not write, cheap enough for a test.
func otherParamsPHC(plain string) string {
	salt := []byte("0123456789ABCDEF")
	key := argon2.IDKey([]byte(plain), salt, 1, 8*1024, 1, 32)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=1,p=1$%s$%s", argon2.Version, 8*1024,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

// casUserAdmin adds the store's compare-and-swap rehash to the fake.
type casUserAdmin struct {
	*fakeUserAdmin
	calls int
}

func (c *casUserAdmin) RehashUserPassword(_ context.Context, id, oldHash, newHash string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	u, ok := c.users[id]
	if !ok || u.PasswordHash != oldHash {
		return false, nil
	}
	u.PasswordHash = newHash
	c.users[id] = u
	return true, nil
}

func newRehashLoginServer(t *testing.T, admin UserAdmin, users authn.UserStore) *Server {
	t.Helper()
	cfg := authTestConfig("local")
	reg := prometheus.NewRegistry()
	return NewServer(Deps{
		Config: cfg, Metrics: metrics.New(cfg.MetricsPrefix, reg), PromRegistry: reg,
		UI:            http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("spa")) }),
		Users:         users,
		UserAdmin:     admin,
		Sessions:      authn.NewSessionStore(cache.NewInProcessKV(), time.Hour, 0),
		Authenticator: fakeAuthenticator{err: authn.ErrNoCredentials, mode: "local"},
	})
}

func login(t *testing.T, s *Server, password string) int {
	t.Helper()
	body := `{"username":"alice","password":"` + password + `"}`
	return doRequest(t, s, http.MethodPost, "/api/v1/auth/login", strings.NewReader(body), nil).Code
}

// An account hashed at other argon2 parameters (every pre-2.5.0 account) costs a wrong guess a
// different time than an unknown username does. Its first successful login upgrades it, with the
// salt kept so no session of that user ends; a failed one leaves it alone.
func TestLoginUpgradesAHashMadeWithOtherParameters(t *testing.T) {
	const password = "correct horse battery staple"
	for _, withCAS := range []bool{false, true} {
		t.Run(fmt.Sprintf("store compare-and-swap=%v", withCAS), func(t *testing.T) {
			legacy := otherParamsPHC(password)
			base := newFakeUserAdmin(store.User{ID: "u-1", Username: "alice", PasswordHash: legacy, DisplayName: "Alice"})
			var admin UserAdmin = base
			cas := &casUserAdmin{fakeUserAdmin: base}
			if withCAS {
				admin = cas
			}
			s := newRehashLoginServer(t, admin, base)

			if code := login(t, s, "wrong guess, long enough"); code != http.StatusUnauthorized {
				t.Fatalf("wrong password = %d, want 401", code)
			}
			if got := base.users["u-1"].PasswordHash; got != legacy {
				t.Fatal("a failed login rewrote the stored hash")
			}

			if code := login(t, s, password); code != http.StatusNoContent {
				t.Fatalf("login = %d, want 204", code)
			}
			upgraded := base.users["u-1"].PasswordHash
			if authn.NeedsRehash(upgraded) {
				t.Fatalf("stored hash still at the old parameters after a successful login: %s", upgraded)
			}
			if ok, _ := authn.VerifyPassword(upgraded, password); !ok {
				t.Fatal("the upgraded hash does not verify the password")
			}
			if authn.PasswordStamp(upgraded) != authn.PasswordStamp(legacy) {
				t.Error("the upgrade changed the password stamp, which signs every session of the user out")
			}
			if withCAS && cas.calls != 1 {
				t.Errorf("RehashUserPassword calls = %d, want 1", cas.calls)
			}
		})
	}
}

// resetDuringLogin is a user store whose admin reset lands between the login's read of the old hash
// and the upgrade: the upgrade must not put the old password back.
type resetDuringLogin struct {
	*fakeUserAdmin
	stale store.User
}

func (r *resetDuringLogin) GetUserByUsername(ctx context.Context, name string) (store.User, error) {
	if name == r.stale.Username && r.stale.ID != "" {
		u := r.stale
		r.stale = store.User{}
		return u, nil
	}
	return r.fakeUserAdmin.GetUserByUsername(ctx, name)
}

func TestLoginUpgradeNeverUndoesAConcurrentPasswordReset(t *testing.T) {
	const password = "correct horse battery staple"
	legacy := otherParamsPHC(password)
	reset, err := authn.HashPassword("the admin's new password")
	if err != nil {
		t.Fatal(err)
	}
	for _, withCAS := range []bool{false, true} {
		t.Run(fmt.Sprintf("store compare-and-swap=%v", withCAS), func(t *testing.T) {
			base := newFakeUserAdmin(store.User{ID: "u-1", Username: "alice", PasswordHash: reset})
			users := &resetDuringLogin{fakeUserAdmin: base, stale: store.User{ID: "u-1", Username: "alice", PasswordHash: legacy}}
			var admin UserAdmin = base
			if withCAS {
				admin = &casUserAdmin{fakeUserAdmin: base}
			}
			s := newRehashLoginServer(t, admin, users)

			login(t, s, password)
			if got := base.users["u-1"].PasswordHash; got != reset {
				t.Fatalf("the login's upgrade overwrote an admin's password reset: stored %s", got)
			}
		})
	}
}

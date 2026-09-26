package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// roleDeletedFirst is a user admin whose guarded write loses the lock to a concurrent role delete.
type roleDeletedFirst struct {
	*fakeUserAdmin
	rbac *fakeRoleAdmin
	role string
}

func (f roleDeletedFirst) UpdateUserGuarded(ctx context.Context, id string, change store.UserChange, guard func(context.Context) error) error {
	_ = f.rbac.DeleteRole(ctx, f.role)
	return f.fakeUserAdmin.UpdateUserGuarded(ctx, id, change, guard)
}

/*
PATCH /api/v1/users/{id} checked the role outside the lock the role delete takes. A delete landing
in between committed a binding to a role that no longer exists, and it grants again the moment a role
of that name is re-created, through POST /api/v1/import included.
*/
func TestUsersPatchRechecksTheRoleUnderTheLock(t *testing.T) {
	admin := newFakeUserAdmin(
		store.User{ID: "u-admin", Username: "root"},
		store.User{ID: "u-bob", Username: "bob"},
	)
	admin.roles["u-bob"] = "viewer"
	rbac := newFakeRoleAdmin()
	if _, err := rbac.UpsertRole(t.Context(), "ops", []string{string(authz.PermRunsRead)}); err != nil {
		t.Fatal(err)
	}
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u-admin"}}
	s := newAuthzServer(t, authr, authz.NewPolicy(nil), Deps{
		UserAdmin: roleDeletedFirst{fakeUserAdmin: admin, rbac: rbac, role: "ops"},
		Roles:     rolesByID{"u-admin": {"admin"}},
		RBAC:      rbac,
	})

	w := doRequest(t, s, http.MethodPatch, "/api/v1/users/u-bob", strings.NewReader(`{"role":"ops"}`), mutateWithCSRF)
	if w.Code != http.StatusBadRequest && w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PATCH to a role deleted meanwhile = %d, want it refused: %s", w.Code, w.Body)
	}
	if got := admin.roles["u-bob"]; got != "viewer" {
		t.Fatalf("bob is bound to %q, a role that no longer exists", got)
	}
}

// guardedCreator is a user admin with the store's guarded create, losing the lock to a role delete.
type guardedCreator struct {
	*fakeUserAdmin
	rbac *fakeRoleAdmin
	role string
}

func (f guardedCreator) CreateUserWithRoleGuarded(ctx context.Context, username, hash, display, role string, guard func(context.Context) error) (store.User, error) {
	_ = f.rbac.DeleteRole(ctx, f.role)
	f.guardMu.Lock()
	defer f.guardMu.Unlock()
	if err := guard(ctx); err != nil {
		return store.User{}, err
	}
	return f.CreateUserWithRole(ctx, username, hash, display, role)
}

// POST /api/v1/users makes the same check under the same lock, when the store offers it.
func TestUsersCreateRechecksTheRoleUnderTheLock(t *testing.T) {
	admin := newFakeUserAdmin(store.User{ID: "u-admin", Username: "root"})
	rbac := newFakeRoleAdmin()
	if _, err := rbac.UpsertRole(t.Context(), "ops", []string{string(authz.PermRunsRead)}); err != nil {
		t.Fatal(err)
	}
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u-admin"}}
	s := newAuthzServer(t, authr, authz.NewPolicy(nil), Deps{
		UserAdmin: guardedCreator{fakeUserAdmin: admin, rbac: rbac, role: "ops"},
		Roles:     rolesByID{"u-admin": {"admin"}},
		RBAC:      rbac,
	})

	body := `{"username":"carol","password":"a long enough secret","role":"ops"}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/users", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusBadRequest && w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create with a role deleted meanwhile = %d, want it refused: %s", w.Code, w.Body)
	}
	if _, err := admin.GetUserByUsername(t.Context(), "carol"); err == nil {
		t.Fatal("carol was created bound to a role that no longer exists")
	}
}

package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// A user who holds users:manage only through auth.defaultRole loses it the moment any binding resolves
// for them, so creating one can take users:manage from the last holder as surely as deleting one.
func TestRBACBindingCreateKeepsTheLastUsersManageHolder(t *testing.T) {
	users := newFakeUserAdmin(store.User{ID: "u-admin", Username: "root"}, store.User{ID: "u-bob", Username: "bob"})
	rbac := newFakeRoleAdmin()
	mustBind(t, rbac, "viewer", "u-bob")
	s := newLastAdminRBACServer(t, users, rbac, nil)
	s.cfg.Auth.DefaultRole = "admin"

	for _, role := range []string{"viewer", "operator"} {
		w := doRequest(t, s, http.MethodPost, "/api/v1/rbac/bindings",
			strings.NewReader(`{"roleName":"`+role+`","subjectKind":"user","subjectId":"u-admin"}`), mutateWithCSRF)
		assertLastAdminRefusal(t, "binding the only default-role admin to "+role, w.Code, w.Body.String())
	}
	if len(rbac.bindings) != 1 {
		t.Fatalf("a refused binding was written anyway: %v", rbac.bindings)
	}

	if w := doRequest(t, s, http.MethodPost, "/api/v1/rbac/bindings",
		strings.NewReader(`{"roleName":"admin","subjectKind":"user","subjectId":"u-bob"}`), mutateWithCSRF); w.Code != http.StatusOK {
		t.Fatalf("binding bob to admin = %d %s, want 200", w.Code, w.Body)
	}
	if w := doRequest(t, s, http.MethodPost, "/api/v1/rbac/bindings",
		strings.NewReader(`{"roleName":"viewer","subjectKind":"user","subjectId":"u-admin"}`), mutateWithCSRF); w.Code != http.StatusOK {
		t.Fatalf("binding root to viewer once bob holds admin = %d %s, want 200", w.Code, w.Body)
	}
}

// racingRoleAdmin lets a concurrent writer's commit land at the one moment a handler cannot see it
// unless it checks under the lock: after the handler's own reads, before its guarded write.
type racingRoleAdmin struct {
	*fakeRoleAdmin
	beforeWrite func()
}

func (r *racingRoleAdmin) DeleteRoleGuarded(ctx context.Context, name string, guard func(context.Context) error) error {
	r.beforeWrite()
	return r.fakeRoleAdmin.DeleteRoleGuarded(ctx, name, guard)
}

func (r *racingRoleAdmin) CreateBinding(ctx context.Context, roleName, subjectKind, subjectID string) (store.RoleBinding, error) {
	r.beforeWrite()
	return r.fakeRoleAdmin.CreateBinding(ctx, roleName, subjectKind, subjectID)
}

func (r *racingRoleAdmin) CreateBindingGuarded(ctx context.Context, roleName, subjectKind, subjectID string, guard func(context.Context) error) (store.RoleBinding, error) {
	r.beforeWrite()
	return r.fakeRoleAdmin.CreateBindingGuarded(ctx, roleName, subjectKind, subjectID, guard)
}

// role_bindings has no foreign key to roles, so a binding that lands while its role is deleted
// outlives it and grants again the day a role of that name is re-created.
func TestRBACRoleDeleteSeesABindingCreatedMeanwhile(t *testing.T) {
	rbac := newFakeRoleAdmin()
	if _, err := rbac.UpsertRole(context.Background(), "ops", []string{"topology:read"}); err != nil {
		t.Fatal(err)
	}
	racing := &racingRoleAdmin{fakeRoleAdmin: rbac, beforeWrite: func() {
		if _, err := rbac.CreateBinding(context.Background(), "ops", "user", "u-carol"); err != nil {
			t.Error(err)
		}
	}}
	s := newRBACTestServer(t, racing)

	w := doRequest(t, s, http.MethodDelete, "/api/v1/rbac/roles/ops", nil, mutateWithCSRF)
	if w.Code != http.StatusConflict {
		t.Fatalf("delete while a binding landed = %d %s, want 409 role in use", w.Code, w.Body)
	}
	if _, ok := rbac.roles["ops"]; !ok {
		t.Fatal("the role was deleted under a binding that references it")
	}
}

func TestRBACBindingCreateSeesTheRoleDeletedMeanwhile(t *testing.T) {
	rbac := newFakeRoleAdmin()
	if _, err := rbac.UpsertRole(context.Background(), "ops", []string{"topology:read"}); err != nil {
		t.Fatal(err)
	}
	racing := &racingRoleAdmin{fakeRoleAdmin: rbac, beforeWrite: func() {
		if err := rbac.DeleteRole(context.Background(), "ops"); err != nil {
			t.Error(err)
		}
	}}
	s := newRBACTestServer(t, racing)

	w := doRequest(t, s, http.MethodPost, "/api/v1/rbac/bindings",
		strings.NewReader(`{"roleName":"ops","subjectKind":"user","subjectId":"u-carol"}`), mutateWithCSRF)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("binding to a role deleted meanwhile = %d %s, want 422 unknown role", w.Code, w.Body)
	}
	if len(rbac.bindings) != 0 {
		t.Fatalf("a binding to a deleted role was stored: %v", rbac.bindings)
	}
}

// interfaceOnlyRoleAdmin has exactly RoleAdmin's method set, as *store.DB has in production, and
// counts plain CreateBinding calls: a binding written there is outside the lock the guard ran under.
type interfaceOnlyRoleAdmin struct {
	RoleAdmin
	unlockedCreates int
}

func (a *interfaceOnlyRoleAdmin) CreateBinding(ctx context.Context, roleName, subjectKind, subjectID string) (store.RoleBinding, error) {
	a.unlockedCreates++
	return a.RoleAdmin.CreateBinding(ctx, roleName, subjectKind, subjectID)
}

func TestRBACBindingCreateWritesUnderTheGuardLock(t *testing.T) {
	rbac := &interfaceOnlyRoleAdmin{RoleAdmin: newFakeRoleAdmin()}
	s := newRBACTestServer(t, rbac)

	w := doRequest(t, s, http.MethodPost, "/api/v1/rbac/bindings",
		strings.NewReader(`{"roleName":"viewer","subjectKind":"user","subjectId":"u-carol"}`), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("create binding = %d %s, want 200", w.Code, w.Body)
	}
	if rbac.unlockedCreates != 0 {
		t.Fatalf("the binding was written by the plain CreateBinding %d time(s), outside the guard's lock", rbac.unlockedCreates)
	}
}

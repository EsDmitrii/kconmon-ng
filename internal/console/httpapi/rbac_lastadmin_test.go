package httpapi

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// UpsertRoleGuarded, DeleteRoleGuarded, DeleteBindingGuarded and CreateBindingGuarded mirror the store: guardMu stands in
// for the advisory lock, the guard runs under it, and nothing is written when the guard refuses.
func (f *fakeRoleAdmin) UpsertRoleGuarded(ctx context.Context, name string, permissions []string, guard func(context.Context) error) (store.Role, error) {
	f.guardMu.Lock()
	defer f.guardMu.Unlock()
	if guard != nil {
		if err := guard(ctx); err != nil {
			return store.Role{}, err
		}
	}
	return f.UpsertRole(ctx, name, permissions)
}

func (f *fakeRoleAdmin) DeleteRoleGuarded(ctx context.Context, name string, guard func(context.Context) error) error {
	f.guardMu.Lock()
	defer f.guardMu.Unlock()
	if guard != nil {
		if err := guard(ctx); err != nil {
			return err
		}
	}
	return f.DeleteRole(ctx, name)
}

func (f *fakeRoleAdmin) DeleteBindingGuarded(ctx context.Context, id int64, guard func(context.Context) error) error {
	f.guardMu.Lock()
	defer f.guardMu.Unlock()
	if guard != nil {
		if err := guard(ctx); err != nil {
			return err
		}
	}
	return f.DeleteBinding(ctx, id)
}

func (f *fakeRoleAdmin) CreateBindingGuarded(ctx context.Context, roleName, subjectKind, subjectID string, guard func(context.Context) error) (store.RoleBinding, error) {
	f.guardMu.Lock()
	defer f.guardMu.Unlock()
	if guard != nil {
		if err := guard(ctx); err != nil {
			return store.RoleBinding{}, err
		}
	}
	return f.CreateBinding(ctx, roleName, subjectKind, subjectID)
}

// bindingRoles resolves a subject's roles from the fake RBAC table, as the store-backed resolver
// does from role_bindings.
type bindingRoles struct{ f *fakeRoleAdmin }

func (r bindingRoles) RolesFor(ctx context.Context, s authz.Subject) ([]string, error) { //nolint:gocritic // matches the seam
	bindings, err := r.f.ListBindings(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, b := range bindings {
		if b.SubjectKind == string(s.Kind) && b.SubjectID == s.ID {
			out = append(out, b.RoleName)
		}
	}
	return out, nil
}

// newLastAdminRBACServer runs auth.mode=local with local users, so an RBAC write can take
// users:manage from the last user who holds it. The caller is u-admin; custom holds the custom
// roles the caller's own request is authorized against.
func newLastAdminRBACServer(t *testing.T, users UserAdmin, rbac *fakeRoleAdmin, custom map[string][]authz.Permission) *Server {
	t.Helper()
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u-admin"}}
	return newAuthzServer(t, authr, authz.NewPolicy(custom), Deps{UserAdmin: users, RBAC: rbac, Roles: bindingRoles{rbac}})
}

func mustBind(t *testing.T, rbac *fakeRoleAdmin, role, userID string) store.RoleBinding {
	t.Helper()
	b, err := rbac.CreateBinding(context.Background(), role, "user", userID)
	if err != nil {
		t.Fatalf("seed binding %s -> %s: %v", role, userID, err)
	}
	return b
}

func bindingPath(b store.RoleBinding) string {
	return "/api/v1/rbac/bindings/" + strconv.FormatInt(b.ID, 10)
}

func assertLastAdminRefusal(t *testing.T, what string, code int, body string) {
	t.Helper()
	if code != http.StatusConflict || !strings.Contains(body, "last administrator") {
		t.Fatalf("%s = %d %s, want 409 last administrator", what, code, body)
	}
}

// Deleting the only admin's binding would leave nobody who can manage users; the users API refuses
// the same outcome, and so does the RBAC API. Other bindings stay deletable.
func TestRBACBindingDeleteKeepsTheLastUsersManageHolder(t *testing.T) {
	users := newFakeUserAdmin(store.User{ID: "u-admin", Username: "root"}, store.User{ID: "u-bob", Username: "bob"})
	rbac := newFakeRoleAdmin()
	adminBinding := mustBind(t, rbac, "admin", "u-admin")
	bobBinding := mustBind(t, rbac, "operator", "u-bob")
	s := newLastAdminRBACServer(t, users, rbac, nil)

	w := doRequest(t, s, http.MethodDelete, bindingPath(adminBinding), nil, mutateWithCSRF)
	assertLastAdminRefusal(t, "deleting the only admin's binding", w.Code, w.Body.String())
	if _, ok := rbac.bindings[adminBinding.ID]; !ok {
		t.Fatal("the refused delete removed the binding anyway")
	}

	if w := doRequest(t, s, http.MethodDelete, bindingPath(bobBinding), nil, mutateWithCSRF); w.Code != http.StatusNoContent {
		t.Fatalf("deleting a binding that grants no users:manage = %d %s, want 204", w.Code, w.Body)
	}
}

// With a second enabled admin the delete goes through; a disabled one does not count.
func TestRBACBindingDeleteCountsOnlyEnabledHolders(t *testing.T) {
	users := newFakeUserAdmin(store.User{ID: "u-admin", Username: "root"},
		store.User{ID: "u-2", Username: "second"}, store.User{ID: "u-off", Username: "off", Disabled: true})
	rbac := newFakeRoleAdmin()
	adminBinding := mustBind(t, rbac, "admin", "u-admin")
	mustBind(t, rbac, "admin", "u-off")
	secondBinding := mustBind(t, rbac, "admin", "u-2")
	s := newLastAdminRBACServer(t, users, rbac, nil)

	if w := doRequest(t, s, http.MethodDelete, bindingPath(secondBinding), nil, mutateWithCSRF); w.Code != http.StatusNoContent {
		t.Fatalf("deleting one of two admins' bindings = %d %s, want 204", w.Code, w.Body)
	}
	w := doRequest(t, s, http.MethodDelete, bindingPath(adminBinding), nil, mutateWithCSRF)
	assertLastAdminRefusal(t, "deleting the last enabled admin's binding", w.Code, w.Body.String())
}

// A custom role that is the only source of users:manage cannot lose it by an upsert; an upsert that
// keeps it goes through.
func TestRBACRoleUpsertKeepsTheLastUsersManageHolder(t *testing.T) {
	users := newFakeUserAdmin(store.User{ID: "u-admin", Username: "root"})
	rbac := newFakeRoleAdmin()
	if _, err := rbac.UpsertRole(context.Background(), "ops", []string{"users:manage", "rbac:manage"}); err != nil {
		t.Fatal(err)
	}
	mustBind(t, rbac, "ops", "u-admin")
	s := newLastAdminRBACServer(t, users, rbac, map[string][]authz.Permission{"ops": {authz.PermUsersManage, authz.PermRBACManage}})

	w := doRequest(t, s, http.MethodPost, "/api/v1/rbac/roles",
		strings.NewReader(`{"name":"ops","permissions":["rbac:manage"]}`), mutateWithCSRF)
	assertLastAdminRefusal(t, "dropping users:manage from the only role that grants it", w.Code, w.Body.String())
	if got := rbac.roles["ops"].Permissions; !slices.Contains(got, "users:manage") {
		t.Fatalf("the refused upsert was written anyway: ops = %v", got)
	}

	if w := doRequest(t, s, http.MethodPost, "/api/v1/rbac/roles",
		strings.NewReader(`{"name":"ops","permissions":["users:manage"]}`), mutateWithCSRF); w.Code != http.StatusOK {
		t.Fatalf("an upsert that keeps users:manage = %d %s, want 200", w.Code, w.Body)
	}
}

// A user without bindings gets auth.defaultRole. When that is a custom role holding users:manage,
// deleting the role (no binding references it) would lock everyone out.
func TestRBACRoleDeleteKeepsTheDefaultRoleThatGrantsUsersManage(t *testing.T) {
	users := newFakeUserAdmin(store.User{ID: "u-admin", Username: "root"})
	rbac := newFakeRoleAdmin()
	if _, err := rbac.UpsertRole(context.Background(), "ops", []string{"users:manage", "rbac:manage"}); err != nil {
		t.Fatal(err)
	}
	s := newLastAdminRBACServer(t, users, rbac, map[string][]authz.Permission{"ops": {authz.PermUsersManage, authz.PermRBACManage}})
	s.cfg.Auth.DefaultRole = "ops"

	w := doRequest(t, s, http.MethodDelete, "/api/v1/rbac/roles/ops", nil, mutateWithCSRF)
	assertLastAdminRefusal(t, "deleting the default role that grants users:manage", w.Code, w.Body.String())
	if _, ok := rbac.roles["ops"]; !ok {
		t.Fatal("the refused delete removed the role anyway")
	}
}

// usersDown fails the user listing the guard needs.
type usersDown struct{ *fakeUserAdmin }

func (usersDown) ListUsers(context.Context) ([]store.User, error) {
	return nil, errors.New("users table unreachable")
}

// The guard fails closed: a delete it cannot check is refused with the users API's 502.
func TestRBACLastAdminGuardFailsClosed(t *testing.T) {
	rbac := newFakeRoleAdmin()
	adminBinding := mustBind(t, rbac, "admin", "u-admin")
	s := newLastAdminRBACServer(t, usersDown{newFakeUserAdmin(store.User{ID: "u-admin", Username: "root"})}, rbac, nil)

	w := doRequest(t, s, http.MethodDelete, bindingPath(adminBinding), nil, mutateWithCSRF)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("delete while the users table is unreadable = %d %s, want 502", w.Code, w.Body)
	}
	if _, ok := rbac.bindings[adminBinding.ID]; !ok {
		t.Fatal("a delete the guard could not check was written anyway")
	}
}

// Two admins deleting each other's bindings at once must not both succeed: the check and the write
// run under one lock, so the second check sees the first delete.
func TestRBACConcurrentBindingDeletesCannotRemoveTheLastAdmin(t *testing.T) {
	base := newFakeUserAdmin(store.User{ID: "u-admin", Username: "root"}, store.User{ID: "u-2", Username: "second"})
	users := &overlappingChecks{fakeUserAdmin: base, both: make(chan struct{})}
	rbac := newFakeRoleAdmin()
	first := mustBind(t, rbac, "admin", "u-admin")
	second := mustBind(t, rbac, "admin", "u-2")
	s := newLastAdminRBACServer(t, users, rbac, nil)

	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i, b := range []store.RoleBinding{first, second} {
		wg.Go(func() {
			codes[i] = doRequest(t, s, http.MethodDelete, bindingPath(b), nil, mutateWithCSRF).Code
		})
	}
	wg.Wait()

	if len(rbac.bindings) == 0 {
		t.Fatalf("both admin bindings were deleted (statuses %v): nobody can manage users any more", codes)
	}
	slices.Sort(codes)
	if codes[0] != http.StatusNoContent || codes[1] != http.StatusConflict {
		t.Errorf("statuses = %v, want one 204 and one 409", codes)
	}
}

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// fakeRoleAdmin is a RoleAdmin test double: an in-memory roles/bindings
// table, mutex-guarded for -race.
type fakeRoleAdmin struct {
	mu       sync.Mutex
	roles    map[string]store.Role
	bindings map[int64]store.RoleBinding
	nextID   int64
	listErr  error
}

func newFakeRoleAdmin() *fakeRoleAdmin {
	return &fakeRoleAdmin{roles: map[string]store.Role{}, bindings: map[int64]store.RoleBinding{}}
}

func (f *fakeRoleAdmin) ListRoles(context.Context) ([]store.Role, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]store.Role, 0, len(f.roles))
	for _, r := range f.roles {
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeRoleAdmin) UpsertRole(_ context.Context, name string, permissions []string) (store.Role, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := store.Role{Name: name, Permissions: permissions, CreatedAt: time.Now()}
	f.roles[name] = r
	return r, nil
}

func (f *fakeRoleAdmin) DeleteRole(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.roles[name]; !ok {
		return store.ErrNotFound
	}
	delete(f.roles, name)
	return nil
}

func (f *fakeRoleAdmin) ListBindings(context.Context) ([]store.RoleBinding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.RoleBinding, 0, len(f.bindings))
	for _, b := range f.bindings {
		out = append(out, b)
	}
	return out, nil
}

func (f *fakeRoleAdmin) CreateBinding(_ context.Context, roleName, subjectKind, subjectID string) (store.RoleBinding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range f.bindings {
		if b.RoleName == roleName && b.SubjectKind == subjectKind && b.SubjectID == subjectID {
			return store.RoleBinding{}, store.ErrAlreadyExists
		}
	}
	f.nextID++
	b := store.RoleBinding{ID: f.nextID, RoleName: roleName, SubjectKind: subjectKind, SubjectID: subjectID, CreatedAt: time.Now()}
	f.bindings[b.ID] = b
	return b, nil
}

func (f *fakeRoleAdmin) DeleteBinding(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.bindings[id]; !ok {
		return store.ErrNotFound
	}
	delete(f.bindings, id)
	return nil
}

// newRBACTestServer wires a Server holding rbac:manage (via role "tester")
// and the given RoleAdmin (nil is a legal, deliberate 503 case).
func newRBACTestServer(t *testing.T, roleAdmin RoleAdmin) *Server {
	t.Helper()
	policy := authz.NewPolicy(map[string][]authz.Permission{"tester": {authz.PermRBACManage}})
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u1"}}
	return newAuthzServer(t, authr, policy, Deps{Roles: fakeRoleResolver{roles: []string{"tester"}}, RBAC: roleAdmin})
}

func TestRBACPermissionsListsAllPermissionsWithoutAStore(t *testing.T) {
	s := newRBACTestServer(t, nil) // no RoleAdmin wired at all
	w := doRequest(t, s, http.MethodGet, "/api/v1/rbac/permissions", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 (permissions is DB-independent): %s", w.Code, w.Body)
	}
	var body struct {
		Permissions []string `json:"permissions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Permissions) != len(authz.AllPermissions) {
		t.Errorf("permissions = %v, want the full authz.AllPermissions list", body.Permissions)
	}
}

func TestRBACRolesWithoutStoreReturns503(t *testing.T) {
	s := newRBACTestServer(t, nil)
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/rbac/roles"},
		{http.MethodPost, "/api/v1/rbac/roles"},
		{http.MethodDelete, "/api/v1/rbac/roles/custom-1"},
		{http.MethodGet, "/api/v1/rbac/bindings"},
		{http.MethodPost, "/api/v1/rbac/bindings"},
		{http.MethodDelete, "/api/v1/rbac/bindings/1"},
	} {
		var body func(*http.Request)
		if isMutatingMethod(c.method) {
			body = mutateWithCSRF
		}
		w := doRequest(t, s, c.method, c.path, strings.NewReader(`{}`), body)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s without a RoleAdmin = %d, want 503", c.method, c.path, w.Code)
		}
	}
}

func TestRBACRolesCreateRejectsBuiltinName(t *testing.T) {
	s := newRBACTestServer(t, newFakeRoleAdmin())
	for _, name := range []string{"viewer", "operator", "alert-editor", "admin"} {
		body := `{"name":"` + name + `","permissions":[]}`
		w := doRequest(t, s, http.MethodPost, "/api/v1/rbac/roles", strings.NewReader(body), mutateWithCSRF)
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("create role named %q = %d, want 422", name, w.Code)
		}
	}
}

func TestRBACRolesCreateRejectsUnknownPermission(t *testing.T) {
	s := newRBACTestServer(t, newFakeRoleAdmin())
	w := doRequest(t, s, http.MethodPost, "/api/v1/rbac/roles",
		strings.NewReader(`{"name":"custom-1","permissions":["bogus:perm"]}`), mutateWithCSRF)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", w.Code, w.Body)
	}
}

func TestRBACRolesCreateAndList(t *testing.T) {
	s := newRBACTestServer(t, newFakeRoleAdmin())
	w := doRequest(t, s, http.MethodPost, "/api/v1/rbac/roles",
		strings.NewReader(`{"name":"custom-1","permissions":["topology:read","matrix:read"]}`), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("create status %d: %s", w.Code, w.Body)
	}

	w = doRequest(t, s, http.MethodGet, "/api/v1/rbac/roles", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list status %d: %s", w.Code, w.Body)
	}
	var body struct {
		Roles []roleResponse `json:"roles"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Roles) != 1 || body.Roles[0].Name != "custom-1" {
		t.Fatalf("roles = %+v, want one role named custom-1", body.Roles)
	}
}

func TestRBACRolesDeleteNotFound(t *testing.T) {
	s := newRBACTestServer(t, newFakeRoleAdmin())
	w := doRequest(t, s, http.MethodDelete, "/api/v1/rbac/roles/does-not-exist", nil, mutateWithCSRF)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404: %s", w.Code, w.Body)
	}
}

// TestRBACRolesDeleteBlockedWhileBound is the brief's guard rail: a role
// may not be deleted while bindings reference it (409); once the binding
// is gone, delete succeeds.
func TestRBACRolesDeleteBlockedWhileBound(t *testing.T) {
	roleAdmin := newFakeRoleAdmin()
	s := newRBACTestServer(t, roleAdmin)

	if w := doRequest(t, s, http.MethodPost, "/api/v1/rbac/roles",
		strings.NewReader(`{"name":"custom-1","permissions":[]}`), mutateWithCSRF); w.Code != http.StatusOK {
		t.Fatalf("create role status %d: %s", w.Code, w.Body)
	}
	w := doRequest(t, s, http.MethodPost, "/api/v1/rbac/bindings",
		strings.NewReader(`{"roleName":"custom-1","subjectKind":"user","subjectId":"u2"}`), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("create binding status %d: %s", w.Code, w.Body)
	}
	var binding bindingResponse
	if err := json.Unmarshal(w.Body.Bytes(), &binding); err != nil {
		t.Fatalf("decode binding: %v", err)
	}

	w = doRequest(t, s, http.MethodDelete, "/api/v1/rbac/roles/custom-1", nil, mutateWithCSRF)
	if w.Code != http.StatusConflict {
		t.Fatalf("delete role while bound = %d, want 409: %s", w.Code, w.Body)
	}

	idPath := "/api/v1/rbac/bindings/" + strconv.FormatInt(binding.ID, 10)
	w = doRequest(t, s, http.MethodDelete, idPath, nil, mutateWithCSRF)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete binding status %d: %s", w.Code, w.Body)
	}

	w = doRequest(t, s, http.MethodDelete, "/api/v1/rbac/roles/custom-1", nil, mutateWithCSRF)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete role after unbinding = %d, want 204: %s", w.Code, w.Body)
	}
}

func TestRBACBindingsCreateRejectsUnknownRole(t *testing.T) {
	s := newRBACTestServer(t, newFakeRoleAdmin())
	w := doRequest(t, s, http.MethodPost, "/api/v1/rbac/bindings",
		strings.NewReader(`{"roleName":"no-such-role","subjectKind":"user","subjectId":"u2"}`), mutateWithCSRF)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", w.Code, w.Body)
	}
}

func TestRBACBindingsCreateAcceptsBuiltinRole(t *testing.T) {
	s := newRBACTestServer(t, newFakeRoleAdmin())
	w := doRequest(t, s, http.MethodPost, "/api/v1/rbac/bindings",
		strings.NewReader(`{"roleName":"viewer","subjectKind":"group","subjectId":"platform-team"}`), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 (viewer is a built-in role): %s", w.Code, w.Body)
	}
}

func TestRBACBindingsCreateDuplicateReturns409(t *testing.T) {
	s := newRBACTestServer(t, newFakeRoleAdmin())
	body := `{"roleName":"viewer","subjectKind":"user","subjectId":"u2"}`
	if w := doRequest(t, s, http.MethodPost, "/api/v1/rbac/bindings", strings.NewReader(body), mutateWithCSRF); w.Code != http.StatusOK {
		t.Fatalf("first create status %d: %s", w.Code, w.Body)
	}
	w := doRequest(t, s, http.MethodPost, "/api/v1/rbac/bindings", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate create status %d, want 409: %s", w.Code, w.Body)
	}
}

func TestRBACBindingsDeleteNotFound(t *testing.T) {
	s := newRBACTestServer(t, newFakeRoleAdmin())
	w := doRequest(t, s, http.MethodDelete, "/api/v1/rbac/bindings/999", nil, mutateWithCSRF)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404: %s", w.Code, w.Body)
	}
}

func TestRBACBindingsDeleteInvalidIDReturns400(t *testing.T) {
	s := newRBACTestServer(t, newFakeRoleAdmin())
	w := doRequest(t, s, http.MethodDelete, "/api/v1/rbac/bindings/not-a-number", nil, mutateWithCSRF)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", w.Code, w.Body)
	}
}

func TestRBACRolesListStoreErrorReturns502(t *testing.T) {
	roleAdmin := newFakeRoleAdmin()
	roleAdmin.listErr = errors.New("pq: connection reset, dsn=postgres://secret")
	s := newRBACTestServer(t, roleAdmin)
	w := doRequest(t, s, http.MethodGet, "/api/v1/rbac/roles", nil, nil)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "secret") {
		t.Errorf("driver error text leaked into response: %s", w.Body)
	}
}

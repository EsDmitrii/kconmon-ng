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
	return newRBACTestServerWithAudit(t, roleAdmin, nil)
}

// newRBACTestServerWithAudit is newRBACTestServer with somewhere for the audit trail to land.
func newRBACTestServerWithAudit(t *testing.T, roleAdmin RoleAdmin, audit Auditor) *Server {
	t.Helper()
	policy := authz.NewPolicy(map[string][]authz.Permission{"tester": {authz.PermRBACManage}})
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u1"}}
	deps := Deps{Roles: fakeRoleResolver{roles: []string{"tester"}}, RBAC: roleAdmin}
	if audit != nil {
		deps.Audit = audit
	}
	return newAuthzServer(t, authr, policy, deps)
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

/*
A permission list is a SET, and the stored row has to be one too.

Nothing bounded the array a role could carry: every element only had to name a known permission,
and repeats were kept verbatim. One POST inside the 16 MiB body limit therefore stored a
multi-million-element text[] that every GET /api/v1/rbac/roles, every export, and every authz
snapshot rebuild had to materialize from then on. The client asked for a set; it gets that set back,
and the row is capped at len(authz.AllPermissions) by construction.
*/
func TestRBACRolesCreateStoresEachPermissionOnce(t *testing.T) {
	admin := newFakeRoleAdmin()
	s := newRBACTestServer(t, admin)

	// The shape that did the damage: one valid permission, repeated.
	repeated := make([]string, 0, 5000)
	for range 5000 {
		repeated = append(repeated, `"topology:read"`)
	}
	body := `{"name":"custom-1","permissions":[` + strings.Join(repeated, ",") + `,"matrix:read"]}`

	w := doRequest(t, s, http.MethodPost, "/api/v1/rbac/roles", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("create status %d: %s", w.Code, w.Body)
	}

	// What the STORE was handed, not what the response happened to echo.
	stored, err := admin.ListRoles(context.Background())
	if err != nil {
		t.Fatalf("list roles: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored %d roles, want 1", len(stored))
	}
	got := stored[0].Permissions
	want := []string{"topology:read", "matrix:read"}
	if len(got) != len(want) {
		t.Fatalf("stored %d permissions, want %d (%v): the repeats went into the row verbatim",
			len(got), len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("permission %d = %q, want %q (first-seen order)", i, got[i], want[i])
		}
	}
}

func TestRBACRolesDeleteNotFound(t *testing.T) {
	s := newRBACTestServer(t, newFakeRoleAdmin())
	w := doRequest(t, s, http.MethodDelete, "/api/v1/rbac/roles/does-not-exist", nil, mutateWithCSRF)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404: %s", w.Code, w.Body)
	}
}

// TestRBACRolesDeleteBlockedWhileBound is the guard rail: a role may not be deleted while bindings
// reference it (409).
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

/* ── what the audit log says about a grant and a revocation ──────────────── */

// A binding IS the grant. Its creation is already described by the request body, but a DELETE
// carries no body at all: before the handler read the row first, the audit trail for revoking
// someone's admin said "binding 1 was deleted" and the row that would have explained it was gone.
func TestRBACBindingDeleteAuditNamesTheRoleAndSubjectItRevoked(t *testing.T) {
	fs := &fakeAuditStore{}
	roleAdmin := newFakeRoleAdmin()
	binding, err := roleAdmin.CreateBinding(context.Background(), "admin", "group", "platform-sre")
	if err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	s := newRBACTestServerWithAudit(t, roleAdmin, fs)

	w := doRequest(t, s, http.MethodDelete, "/api/v1/rbac/bindings/"+strconv.FormatInt(binding.ID, 10), nil, mutateWithCSRF)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status %d, want 204: %s", w.Code, w.Body)
	}

	entries := waitForOneAuditEntry(t, fs)
	detail := string(entries[0].Detail)
	for _, want := range []string{`"roleName":"admin"`, `"subjectKind":"group"`, `"subjectId":"platform-sre"`} {
		if !strings.Contains(detail, want) {
			t.Errorf("audit detail = %s, want it to carry %s", detail, want)
		}
	}
}

func TestRBACBindingCreateAuditCarriesTheNewBindingID(t *testing.T) {
	fs := &fakeAuditStore{}
	roleAdmin := newFakeRoleAdmin()
	s := newRBACTestServerWithAudit(t, roleAdmin, fs)

	body := `{"roleName":"admin","subjectKind":"user","subjectId":"oidc:user-sub-1"}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/rbac/bindings", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", w.Code, w.Body)
	}

	entries := waitForOneAuditEntry(t, fs)
	detail := string(entries[0].Detail)
	// The id is the handle the eventual revocation will be recorded under; without it the two
	// halves of a grant's life cannot be joined up.
	if !strings.Contains(detail, `"bindingId":1`) {
		t.Errorf("audit detail = %s, want the created binding's id", detail)
	}
	if !strings.Contains(detail, `"subjectId":"oidc:user-sub-1"`) {
		t.Errorf("audit detail = %s, want the request's own subjectId preserved", detail)
	}
}

// createdAt is what makes a binding list reviewable — "who holds admin, and since when".
func TestRBACBindingsListCarriesCreatedAt(t *testing.T) {
	roleAdmin := newFakeRoleAdmin()
	if _, err := roleAdmin.CreateBinding(context.Background(), "admin", "user", "oidc:user-sub-1"); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	s := newRBACTestServer(t, roleAdmin)

	w := doRequest(t, s, http.MethodGet, "/api/v1/rbac/bindings", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", w.Code, w.Body)
	}
	var body struct {
		Bindings []struct {
			CreatedAt time.Time `json:"createdAt"`
		} `json:"bindings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Bindings) != 1 {
		t.Fatalf("bindings = %d, want 1", len(body.Bindings))
	}
	if body.Bindings[0].CreatedAt.IsZero() {
		t.Error("createdAt is zero — the column is stored and must be handed out")
	}
}

/*
 * A role whose name carries "/" can never be deleted.
 *
 * The name IS the delete route's path segment: /api/v1/rbac/roles/team/ops carries an extra segment
 * and matches no route, and the percent-encoded spelling routes on RawPath so the store is handed
 * the still-escaped "team%2Fops". The row was permanent -- listed, exported, rendered on the RBAC
 * page, removable only by direct SQL -- and if it carried rbac:manage it could only be neutralised.
 */
func TestRBACRolesCreateRejectsASlashInTheName(t *testing.T) {
	s := newRBACTestServer(t, newFakeRoleAdmin())
	w := doRequest(t, s, http.MethodPost, "/api/v1/rbac/roles",
		strings.NewReader(`{"name":"team/ops","permissions":[]}`), mutateWithCSRF)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create role named \"team/ops\" = %d, want 422: %s", w.Code, w.Body)
	}

	list := doRequest(t, s, http.MethodGet, "/api/v1/rbac/roles", nil, nil)
	if strings.Contains(list.Body.String(), "team/ops") {
		t.Errorf("the refused role was stored anyway: %s", list.Body)
	}
}

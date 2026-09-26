package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// RoleAdmin is the subset of store.RoleStore the RBAC admin API (GET/POST/DELETE
// /api/v1/rbac/roles[/{name}], /api/v1/rbac/bindings[/{id}]) needs.
type RoleAdmin interface {
	ListRoles(ctx context.Context) ([]store.Role, error)
	UpsertRole(ctx context.Context, name string, permissions []string) (store.Role, error)
	DeleteRole(ctx context.Context, name string) error
	ListBindings(ctx context.Context) ([]store.RoleBinding, error)
	CreateBinding(ctx context.Context, roleName, subjectKind, subjectID string) (store.RoleBinding, error)
	DeleteBinding(ctx context.Context, id int64) error
	// The *Guarded writes run guard and the write under the lock store.DB.UpdateUserGuarded takes, so
	// the last-admin check of the users API and of these routes cannot pass on a stale read.
	UpsertRoleGuarded(ctx context.Context, name string, permissions []string, guard func(context.Context) error) (store.Role, error)
	DeleteRoleGuarded(ctx context.Context, name string, guard func(context.Context) error) error
	DeleteBindingGuarded(ctx context.Context, id int64, guard func(context.Context) error) error
	CreateBindingGuarded(ctx context.Context, roleName, subjectKind, subjectID string, guard func(context.Context) error) (store.RoleBinding, error)
}

var _ RoleAdmin = (*store.DB)(nil)

// RBACChangedTopic is the bus topic an RBAC write publishes on so every replica re-reads the roles
// table at once: authz.Policy caches custom roles on a timer, and a revoked permission must not keep
// working until the next refresh. It is not a WebSocket topic.
const RBACChangedTopic = "rbac-changed"

const rbacUnavailableDetail = databaseKnob + " to enable /api/v1/rbac/roles and /api/v1/rbac/bindings"

// rbacUnavailable answers 503 and reports true when s.roleAdmin is nil
// (no database configured). GET /api/v1/rbac/permissions never calls this --
// it serves the static authz.AllPermissions list and needs no store at all.
func (s *Server) rbacUnavailable(w http.ResponseWriter) bool {
	if s.roleAdmin == nil {
		writeProblem(w, http.StatusServiceUnavailable, "rbac admin not available", rbacUnavailableDetail)
		return true
	}
	return false
}

// handleRBACPermissions serves the static authz.AllPermissions list so the UI can build a role
// editor without hardcoding permission strings.
func (s *Server) handleRBACPermissions(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"permissions": authz.AllPermissions})
}

// roleResponse is one role's shape in GET/POST /api/v1/rbac/roles.
type roleResponse struct {
	Name        string   `json:"name"`
	Permissions []string `json:"permissions"`
}

func roleResponseFrom(r *store.Role) roleResponse {
	return roleResponse{Name: r.Name, Permissions: nonNilStrings(r.Permissions)}
}

func (s *Server) handleRBACRolesList(w http.ResponseWriter, r *http.Request) {
	if s.rbacUnavailable(w) {
		return
	}
	roles, err := s.roleAdmin.ListRoles(r.Context())
	if err != nil {
		slog.Error("list roles failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "rbac unavailable", "failed to list roles")
		return
	}
	out := make([]roleResponse, 0, len(roles))
	for i := range roles {
		out = append(out, roleResponseFrom(&roles[i]))
	}
	writeJSON(w, map[string]any{"roles": out})
}

type roleRequest struct {
	Name        string   `json:"name"`
	Permissions []string `json:"permissions"`
}

// validPermission reports whether p names a permission this build knows --
// authz.AllPermissions is the closed set (authz.go's own doc comment).
func validPermission(p string) bool {
	for _, known := range authz.AllPermissions {
		if string(known) == p {
			return true
		}
	}
	return false
}

// sanitizeRolePermissions validates and deduplicates a role's permission list, returning the cleaned
// slice and the first unknown permission ("" when all are known). A permission list is a set, and
// deduping bounds the stored array by len(authz.AllPermissions); the create route and the import
// both use it.
func sanitizeRolePermissions(in []string) (out []string, unknown string) {
	// Non-nil even when empty: pgx turns a nil slice into a NULL the store refuses.
	out = make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, p := range in {
		if !validPermission(p) {
			return nil, p
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out, ""
}

// nameMaxLen bounds the custom role and API token names this package validates, mirroring
// store.nameMaxLen, the CHECK targets and check definitions carry. A name is display text and an
// index key, and a role's is part of every binding that references it.
const nameMaxLen = 63

/*
roleNameProblem is why name cannot name a custom role, or "", for the create route and the import
alike. The name is the DELETE route's path segment: a blank one, or one carrying "/", could never be
addressed there again, and neither could one the route refuses for its characters.
*/
func roleNameProblem(name string) string {
	switch {
	case strings.TrimSpace(name) == "":
		return "role: name must not be empty"
	case invalidParamText(name):
		return "role: name must be valid UTF-8 without control characters"
	case strings.Contains(name, "/"):
		return `role: name may not contain "/" — the name addresses the role in its own API path`
	case len(name) > nameMaxLen:
		return fmt.Sprintf("role: name is %d bytes, limit is %d", len(name), nameMaxLen)
	}
	return ""
}

// subjectIDMaxLen bounds a binding's subject identifier: an OIDC `sub`, an email, a UUID or a group
// name. 255 is the widest of those with room to spare, and well inside what a btree index accepts.
const subjectIDMaxLen = 255

// handleRBACRolesCreate upserts a custom role; a custom role may not be named like a built-in.
func (s *Server) handleRBACRolesCreate(w http.ResponseWriter, r *http.Request) {
	if s.rbacUnavailable(w) {
		return
	}
	var req roleRequest
	// Deliberately LENIENT (no DisallowUnknownFields): the audit middleware
	// captures the mutation body and scrubs any non-allow-listed key (password,
	// token, query, ...) rather than the handler rejecting it, so a body that
	// carries an unexpected key is tolerated and redacted, not 400'd
	// (TestAuditDetailAllowlistDropsSecrets).
	// `permissions == nil` is what pgx turns into a NULL the store refuses, and a 502 naming the
	// store is the wrong answer to a body that simply omitted a required field. `[]` still passes:
	// a role with no permissions is a legal, if useless, role.
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" || req.Permissions == nil {
		writeProblem(w, http.StatusBadRequest, "invalid request", `body must be JSON with a non-empty "name" and a "permissions" array`)
		return
	}
	if problem := roleNameProblem(req.Name); problem != "" {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid role", problem)
		return
	}
	if authz.IsBuiltinRole(req.Name) {
		writeProblem(w, http.StatusUnprocessableEntity, "reserved role name",
			"a custom role may not be named like a built-in role (viewer, operator, alert-editor, admin)")
		return
	}
	perms, badPerm := sanitizeRolePermissions(req.Permissions)
	if badPerm != "" {
		writeProblem(w, http.StatusUnprocessableEntity, "unknown permission", "unknown permission: "+badPerm)
		return
	}
	var guard func(context.Context) error
	if !slices.Contains(perms, string(authz.PermUsersManage)) {
		guard = s.rbacLastAdminGuard(rbacChange{role: req.Name, permissions: perms})
	}
	role, err := s.roleAdmin.UpsertRoleGuarded(r.Context(), req.Name, perms, guard)
	if writeLastAdminRefusal(w, err) {
		return
	}
	if err != nil {
		slog.Error("upsert role failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "rbac unavailable", "failed to save role")
		return
	}
	s.publishRBACChanged(r.Context())
	writeJSON(w, roleResponseFrom(&role))
}

// handleRBACRolesDelete removes a custom role; guard rail: a role may not be deleted while any
// binding still references it (409).
func (s *Server) handleRBACRolesDelete(w http.ResponseWriter, r *http.Request) {
	if s.rbacUnavailable(w) {
		return
	}
	name := chi.URLParam(r, "name")

	// The in-use check runs under the lock: role_bindings has no foreign key to roles, and a binding
	// created between an unlocked check and the delete would outlive the role and grant again once a
	// role of that name is re-created.
	lastAdmin := s.rbacLastAdminGuard(rbacChange{role: name, deleteRole: true})
	err := s.roleAdmin.DeleteRoleGuarded(r.Context(), name, func(ctx context.Context) error {
		bindings, err := s.roleAdmin.ListBindings(ctx)
		if err != nil {
			return rbacReadError{detail: "failed to check role bindings", err: err}
		}
		for i := range bindings {
			if bindings[i].RoleName == name {
				return errRoleInUse
			}
		}
		if lastAdmin != nil {
			return lastAdmin(ctx)
		}
		return nil
	})
	if writeLastAdminRefusal(w, err) || writeRBACGuardRefusal(w, err, name) {
		return
	}
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "not found", "no custom role named "+name)
			return
		}
		slog.Error("delete role failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "rbac unavailable", "failed to delete role")
		return
	}
	s.publishRBACChanged(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

/*
publishRBACChanged tells every replica to re-read the roles table now.

Best effort by design: the timed refresh is still there, so a bus outage costs latency on a
permission change and nothing else. Failing the write because the notification did not go out would
be worse — the role IS changed, and the caller needs to know that.
*/
func (s *Server) publishRBACChanged(ctx context.Context) {
	if s.kvBus == nil {
		return
	}
	if err := s.kvBus.Publish(ctx, RBACChangedTopic, cache.Message{Type: "rbac.changed"}); err != nil {
		slog.Warn("could not announce the RBAC change to the other replicas; "+
			"they will pick it up on their next refresh", "error", err)
	}
}

// bindingResponse is one binding's shape in GET/POST /api/v1/rbac/bindings.
//
// createdAt is carried because a grant's AGE is half of reviewing it: a reviewer looking at a list
// of who holds admin needs to see which of them was added last Tuesday. The row has always had the
// column; it simply was not being handed out.
type bindingResponse struct {
	ID          int64     `json:"id"`
	RoleName    string    `json:"roleName"`
	SubjectKind string    `json:"subjectKind"`
	SubjectID   string    `json:"subjectId"`
	CreatedAt   time.Time `json:"createdAt"`
}

func bindingResponseFrom(b *store.RoleBinding) bindingResponse {
	return bindingResponse{
		ID:          b.ID,
		RoleName:    b.RoleName,
		SubjectKind: b.SubjectKind,
		SubjectID:   b.SubjectID,
		CreatedAt:   b.CreatedAt,
	}
}

func (s *Server) handleRBACBindingsList(w http.ResponseWriter, r *http.Request) {
	if s.rbacUnavailable(w) {
		return
	}
	bindings, err := s.roleAdmin.ListBindings(r.Context())
	if err != nil {
		slog.Error("list bindings failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "rbac unavailable", "failed to list bindings")
		return
	}
	out := make([]bindingResponse, 0, len(bindings))
	for i := range bindings {
		out = append(out, bindingResponseFrom(&bindings[i]))
	}
	writeJSON(w, map[string]any{"bindings": out})
}

// validSubjectKinds is the set of subject kinds a binding can be CREATED with; the schema
// (migration 00002) also declares "token".
var validSubjectKinds = map[string]bool{"user": true, "group": true}

type bindingRequest struct {
	RoleName    string `json:"roleName"`
	SubjectKind string `json:"subjectKind"`
	SubjectID   string `json:"subjectId"`
}

// requireKnownRole is nil for a built-in role or an existing custom one, errUnknownRole for any other
// name, and an rbacReadError when the custom roles cannot be read. Without a role store only the
// built-ins exist.
func (s *Server) requireKnownRole(ctx context.Context, name string) error {
	if authz.IsBuiltinRole(name) {
		return nil
	}
	if s.roleAdmin == nil {
		return errUnknownRole
	}
	roles, err := s.roleAdmin.ListRoles(ctx)
	if err != nil {
		return rbacReadError{detail: "failed to read roles", err: err}
	}
	for i := range roles {
		if roles[i].Name == name {
			return nil
		}
	}
	return errUnknownRole
}

// handleRBACBindingsCreate binds a subject to a role.
func (s *Server) handleRBACBindingsCreate(w http.ResponseWriter, r *http.Request) {
	if s.rbacUnavailable(w) {
		return
	}
	var req bindingRequest
	// Deliberately LENIENT, same reason as handleRBACRolesCreate: the audit
	// middleware scrubs unexpected keys rather than the handler rejecting them.
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RoleName == "" || req.SubjectID == "" {
		writeProblem(w, http.StatusBadRequest, "invalid request", `body must be JSON with non-empty "roleName", "subjectKind", and "subjectId"`)
		return
	}
	// Bounded and free of control characters, like every other identifier: the id is listed, exported
	// and rendered, and PostgreSQL refuses a NUL.
	if rejectControlChars(w, "subjectId", req.SubjectID) || rejectControlChars(w, "roleName", req.RoleName) {
		return
	}
	if len(req.SubjectID) > subjectIDMaxLen {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid binding",
			fmt.Sprintf("subjectId is %d bytes, limit is %d", len(req.SubjectID), subjectIDMaxLen))
		return
	}
	if !validSubjectKinds[req.SubjectKind] {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid subject kind", `subjectKind must be "user" or "group"`)
		return
	}

	// Checked under the lock for the same reason the role delete checks bindings there; and a subject
	// that gains a binding loses auth.defaultRole, so the binding can also take users:manage away.
	lastAdmin := s.rbacLastAdminGuard(rbacChange{addBinding: &req})
	guard := func(ctx context.Context) error {
		if err := s.requireKnownRole(ctx, req.RoleName); err != nil {
			return err
		}
		if lastAdmin != nil {
			return lastAdmin(ctx)
		}
		return nil
	}
	binding, err := s.roleAdmin.CreateBindingGuarded(r.Context(), req.RoleName, req.SubjectKind, req.SubjectID, guard)
	if writeLastAdminRefusal(w, err) || writeRBACGuardRefusal(w, err, req.RoleName) {
		return
	}
	if err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			writeProblem(w, http.StatusConflict, "binding already exists", "")
			return
		}
		slog.Error("create binding failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "rbac unavailable", "failed to create binding")
		return
	}
	// The request body already tells the audit log WHAT was asked for; the id tells it which row
	// now exists, which is the only handle the delete below will be identified by.
	setAuditResult(r, map[string]any{"bindingId": binding.ID})
	writeJSON(w, bindingResponseFrom(&binding))
}

func (s *Server) handleRBACBindingsDelete(w http.ResponseWriter, r *http.Request) {
	if s.rbacUnavailable(w) {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid id", "id must be an integer")
		return
	}

	// Read before deleting: a DELETE has no body, and the audit row must say which role was revoked
	// from whom.
	bindings, err := s.roleAdmin.ListBindings(r.Context())
	if err != nil {
		slog.Error("list bindings failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "rbac unavailable", "failed to look up the binding")
		return
	}
	var doomed *store.RoleBinding
	for i := range bindings {
		if bindings[i].ID == id {
			doomed = &bindings[i]
			break
		}
	}
	if doomed == nil {
		writeProblem(w, http.StatusNotFound, "not found", "no binding with that id")
		return
	}

	err = s.roleAdmin.DeleteBindingGuarded(r.Context(), id, s.rbacLastAdminGuard(rbacChange{dropBinding: id}))
	if writeLastAdminRefusal(w, err) {
		return
	}
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "not found", "no binding with that id")
			return
		}
		slog.Error("delete binding failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "rbac unavailable", "failed to delete binding")
		return
	}
	setAuditResult(r, map[string]any{
		"bindingId":   doomed.ID,
		"roleName":    doomed.RoleName,
		"subjectKind": doomed.SubjectKind,
		"subjectId":   doomed.SubjectID,
	})
	w.WriteHeader(http.StatusNoContent)
}

// rbacChange is the RBAC write the last-admin guard checks: a binding going away (dropBinding) or
// being created (addBinding), or a custom role getting new permissions or being deleted.
// pendingRoles are role writes the store has not seen yet (an import's dry run), laid over its roles.
type rbacChange struct {
	dropBinding  int64
	addBinding   *bindingRequest
	role         string
	permissions  []string
	deleteRole   bool
	pendingRoles map[string][]authz.Permission
}

// The RBAC guards' own refusals, beside errLastAdmin.
var (
	errUnknownRole = errors.New("unknown role")
	errRoleInUse   = errors.New("role in use")
)

// rbacReadError is an RBAC guard that could not read what it checks; detail is the 502's detail.
type rbacReadError struct {
	detail string
	err    error
}

func (e rbacReadError) Error() string { return e.detail + ": " + e.err.Error() }
func (e rbacReadError) Unwrap() error { return e.err }

// writeRBACGuardRefusal answers the refusals of the role-delete and binding-create guards, and reports
// whether it did.
func writeRBACGuardRefusal(w http.ResponseWriter, err error, roleName string) bool {
	var readErr rbacReadError
	switch {
	case errors.Is(err, errUnknownRole):
		writeProblem(w, http.StatusUnprocessableEntity, "unknown role", "no built-in or custom role named "+roleName)
		return true
	case errors.Is(err, errRoleInUse):
		writeProblem(w, http.StatusConflict, "role in use", "role is still referenced by one or more bindings; delete those first")
		return true
	case errors.As(err, &readErr):
		slog.Error("rbac guard read failed", "error", readErr.err)
		writeProblem(w, http.StatusBadGateway, "rbac unavailable", readErr.detail)
		return true
	}
	return false
}

// rbacLastAdminGuard returns the guard for change: errLastAdmin when change would leave no enabled
// local user holding users:manage, lastAdminCheckError when that cannot be read. Nil outside
// auth.mode=local, where the users API and the permission it gates do not exist.
func (s *Server) rbacLastAdminGuard(change rbacChange) func(context.Context) error {
	if s.cfg.Auth.Mode != "local" || s.userAdmin == nil {
		return nil
	}
	return func(ctx context.Context) error {
		before, after, err := s.usersManageHolders(ctx, change)
		if err != nil {
			return lastAdminCheckError{err}
		}
		if before > 0 && after == 0 {
			return errLastAdmin
		}
		return nil
	}
}

// usersManageHolders counts the enabled local users holding users:manage now and after change. Roles
// and bindings come from the store, not from the policy cache another replica's write may not have
// refreshed yet; a user with no binding gets auth.defaultRole, as in resolveRoles.
func (s *Server) usersManageHolders(ctx context.Context, change rbacChange) (before, after int, err error) {
	users, err := s.userAdmin.ListUsers(ctx)
	if err != nil {
		return 0, 0, err
	}
	roles, err := s.roleAdmin.ListRoles(ctx)
	if err != nil {
		return 0, 0, err
	}
	bindings, err := s.roleAdmin.ListBindings(ctx)
	if err != nil {
		return 0, 0, err
	}

	custom := customRolePermissions(roles)
	maps.Copy(custom, change.pendingRoles)
	changed := maps.Clone(custom)
	switch {
	case change.role != "" && change.deleteRole:
		delete(changed, change.role)
	case change.role != "":
		changed[change.role] = asPermissions(change.permissions)
	}
	policyBefore, policyAfter := authz.NewPolicy(custom), authz.NewPolicy(changed)

	rolesBefore := map[string][]string{}
	rolesAfter := map[string][]string{}
	for i := range bindings {
		b := &bindings[i]
		if b.SubjectKind != string(authz.SubjectUser) {
			continue
		}
		rolesBefore[b.SubjectID] = append(rolesBefore[b.SubjectID], b.RoleName)
		if b.ID != change.dropBinding {
			rolesAfter[b.SubjectID] = append(rolesAfter[b.SubjectID], b.RoleName)
		}
	}
	if b := change.addBinding; b != nil && b.SubjectKind == string(authz.SubjectUser) {
		rolesAfter[b.SubjectID] = append(rolesAfter[b.SubjectID], b.RoleName)
	}
	for i := range users {
		if users[i].Disabled {
			continue
		}
		if s.holdsUsersManage(policyBefore, rolesBefore[users[i].ID]) {
			before++
		}
		if s.holdsUsersManage(policyAfter, rolesAfter[users[i].ID]) {
			after++
		}
	}
	return before, after, nil
}

func (s *Server) holdsUsersManage(policy *authz.Policy, roles []string) bool {
	if len(roles) == 0 {
		roles = s.defaultRoles()
	}
	return policy.Can(authz.Subject{Kind: authz.SubjectUser, Roles: roles}, authz.PermUsersManage)
}

// customRolePermissions is the custom-role map authz.NewPolicy takes, built from the stored roles.
func customRolePermissions(roles []store.Role) map[string][]authz.Permission {
	custom := make(map[string][]authz.Permission, len(roles))
	for i := range roles {
		custom[roles[i].Name] = asPermissions(roles[i].Permissions)
	}
	return custom
}

func asPermissions(perms []string) []authz.Permission {
	out := make([]authz.Permission, len(perms))
	for i, p := range perms {
		out[i] = authz.Permission(p)
	}
	return out
}

// writeLastAdminRefusal answers a refused or unchecked guarded RBAC write the way PATCH
// /api/v1/users answers the same outcomes, and reports whether it did.
func writeLastAdminRefusal(w http.ResponseWriter, err error) bool {
	var checkErr lastAdminCheckError
	switch {
	case errors.Is(err, errLastAdmin):
		writeProblem(w, http.StatusConflict, "last administrator",
			"this change leaves no enabled user who can manage users; grant users:manage to someone else first")
		return true
	case errors.As(err, &checkErr):
		slog.Error("last-admin check failed", "error", checkErr.err)
		writeProblem(w, http.StatusBadGateway, "users unavailable", "failed to read users")
		return true
	}
	return false
}

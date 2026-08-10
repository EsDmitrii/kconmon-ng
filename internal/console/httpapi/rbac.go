package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
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
}

// rbacUnavailable answers 503 and reports true when s.roleAdmin is nil
// (database.mode=disabled). GET /api/v1/rbac/permissions never calls this --
// it serves the static authz.AllPermissions list and needs no store at all.
func (s *Server) rbacUnavailable(w http.ResponseWriter) bool {
	if s.roleAdmin == nil {
		writeProblem(w, http.StatusServiceUnavailable, "rbac admin not available",
			"set console.database.mode in the console config (Helm: console.database.mode) to enable /api/v1/rbac/roles and /api/v1/rbac/bindings")
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

// handleRBACRolesCreate upserts a custom role; guard rails, verbatim: a custom role may not be
// named like a built-in.
func (s *Server) handleRBACRolesCreate(w http.ResponseWriter, r *http.Request) {
	if s.rbacUnavailable(w) {
		return
	}
	var req roleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		writeProblem(w, http.StatusBadRequest, "invalid request", `body must be JSON with a non-empty "name" and a "permissions" array`)
		return
	}
	if authz.IsBuiltinRole(req.Name) {
		writeProblem(w, http.StatusUnprocessableEntity, "reserved role name",
			"a custom role may not be named like a built-in role (viewer, operator, alert-editor, admin)")
		return
	}
	for _, p := range req.Permissions {
		if !validPermission(p) {
			writeProblem(w, http.StatusUnprocessableEntity, "unknown permission", "unknown permission: "+p)
			return
		}
	}
	role, err := s.roleAdmin.UpsertRole(r.Context(), req.Name, req.Permissions)
	if err != nil {
		slog.Error("upsert role failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "rbac unavailable", "failed to save role")
		return
	}
	writeJSON(w, roleResponseFrom(&role))
}

// handleRBACRolesDelete removes a custom role; guard rail: a role may not be deleted while any
// binding still references it (409).
func (s *Server) handleRBACRolesDelete(w http.ResponseWriter, r *http.Request) {
	if s.rbacUnavailable(w) {
		return
	}
	name := chi.URLParam(r, "name")

	bindings, err := s.roleAdmin.ListBindings(r.Context())
	if err != nil {
		slog.Error("list bindings failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "rbac unavailable", "failed to check role bindings")
		return
	}
	for i := range bindings {
		if bindings[i].RoleName == name {
			writeProblem(w, http.StatusConflict, "role in use", "role is still referenced by one or more bindings; delete those first")
			return
		}
	}

	if err := s.roleAdmin.DeleteRole(r.Context(), name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "not found", "no custom role named "+name)
			return
		}
		slog.Error("delete role failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "rbac unavailable", "failed to delete role")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// bindingResponse is one binding's shape in GET/POST /api/v1/rbac/bindings.
type bindingResponse struct {
	ID          int64  `json:"id"`
	RoleName    string `json:"roleName"`
	SubjectKind string `json:"subjectKind"`
	SubjectID   string `json:"subjectId"`
}

func bindingResponseFrom(b *store.RoleBinding) bindingResponse {
	return bindingResponse{ID: b.ID, RoleName: b.RoleName, SubjectKind: b.SubjectKind, SubjectID: b.SubjectID}
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

// roleKnown reports whether name is a built-in role or an existing custom
// role -- the check handleRBACBindingsCreate's "unknown role" guard rail
// needs.
func (s *Server) roleKnown(ctx context.Context, name string) (bool, error) {
	if authz.IsBuiltinRole(name) {
		return true, nil
	}
	roles, err := s.roleAdmin.ListRoles(ctx)
	if err != nil {
		return false, err
	}
	for i := range roles {
		if roles[i].Name == name {
			return true, nil
		}
	}
	return false, nil
}

// handleRBACBindingsCreate binds a subject to a role.
func (s *Server) handleRBACBindingsCreate(w http.ResponseWriter, r *http.Request) {
	if s.rbacUnavailable(w) {
		return
	}
	var req bindingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RoleName == "" || req.SubjectID == "" {
		writeProblem(w, http.StatusBadRequest, "invalid request", `body must be JSON with non-empty "roleName", "subjectKind", and "subjectId"`)
		return
	}
	if !validSubjectKinds[req.SubjectKind] {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid subject kind", `subjectKind must be "user" or "group" ("token" bindings are declared in the schema but not yet resolved by anything — storing one would silently grant nothing)`)
		return
	}

	known, err := s.roleKnown(r.Context(), req.RoleName)
	if err != nil {
		slog.Error("check role existence failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "rbac unavailable", "failed to validate role")
		return
	}
	if !known {
		writeProblem(w, http.StatusUnprocessableEntity, "unknown role", "no built-in or custom role named "+req.RoleName)
		return
	}

	binding, err := s.roleAdmin.CreateBinding(r.Context(), req.RoleName, req.SubjectKind, req.SubjectID)
	if err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			writeProblem(w, http.StatusConflict, "binding already exists", "")
			return
		}
		slog.Error("create binding failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "rbac unavailable", "failed to create binding")
		return
	}
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
	if err := s.roleAdmin.DeleteBinding(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "not found", "no binding with that id")
			return
		}
		slog.Error("delete binding failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "rbac unavailable", "failed to delete binding")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

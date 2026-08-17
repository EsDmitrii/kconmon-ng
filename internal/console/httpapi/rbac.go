package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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
}

/*
RBACChangedTopic is the bus topic an RBAC write publishes on so every replica re-reads the roles
table at once.

The permission SET of a custom role is cached in authz.Policy and refreshed on a timer. Without this
kick, POST /api/v1/rbac/roles answered 200 with the new set while every request kept being
authorized against the old one until the next refresh — a revoked permission still working for up to
a minute, with the API and the UI both showing it revoked. For a revocation that is the wrong
direction to be wrong in.

It is NOT a WebSocket topic: nothing in a browser subscribes to it.
*/
const RBACChangedTopic = "rbac-changed"

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

/*
sanitizeRolePermissions validates AND deduplicates a role's permission list, returning the cleaned
slice and the first unknown permission it found ("" when they were all known).

Every element has to name a permission from authz.AllPermissions, but nothing said each one could
appear only once, and a permission list is a SET everywhere it is read — authz walks it looking for
a match, so a repeat changes no decision. That made the stored array the only unbounded thing an
RBAC write could produce: one request carrying the same valid permission a million times over (well
inside the 16 MiB body limit) stored a million-element text[], and from then on every
GET /api/v1/rbac/roles, every export, and every authz snapshot rebuild materialized it. Deduping
caps the row at len(authz.AllPermissions) by construction, and the client sees no difference: it
asked for a set and gets the same set back.

It lives here, shared, because POST /api/v1/rbac/roles is NOT the only way a role is written:
POST /api/v1/import writes roles too, and it applied neither this nor the name bound. Same table,
same rebuild, one guard.
*/
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

// handleRBACRolesCreate upserts a custom role; guard rails, verbatim: a custom role may not be
// named like a built-in.
// roleNameMaxLen bounds a custom role name, mirroring the 63 targets and check definitions carry.
// The name is display text, an index key, and part of every binding that references it.
const roleNameMaxLen = 63

// subjectIDMaxLen bounds a binding's subject identifier: an OIDC `sub`, an email, a UUID or a group
// name. 255 is the widest of those with room to spare, and well inside what a btree index accepts.
const subjectIDMaxLen = 255

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
	/* A BOUND, like every sibling resource carries (target 63, webhook 64, alert rule 63).
	   Nothing limited this: a 200 000-character role name was accepted and stored, and a 3 KB one
	   answered 502 "rbac unavailable" because PostgreSQL refused the btree index entry — a client's
	   own input reported as an outage of the RBAC backend. */
	if rejectControlChars(w, "name", req.Name) {
		return
	}
	if len(req.Name) > roleNameMaxLen {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid role",
			fmt.Sprintf("role: name is %d bytes, limit is %d", len(req.Name), roleNameMaxLen))
		return
	}
	if authz.IsBuiltinRole(req.Name) {
		writeProblem(w, http.StatusUnprocessableEntity, "reserved role name",
			"a custom role may not be named like a built-in role (viewer, operator, alert-editor, admin)")
		return
	}
	/* The name is the DELETE route's path segment, so a "/" in it makes the role undeletable.
	   DELETE /api/v1/rbac/roles/team/ops carries an extra segment and matches no route (404 "no such
	   API route"); the percent-encoded form routes on RawPath, so chi.URLParam hands the store the
	   still-escaped "team%2Fops" and that names no role either. The row then lives forever — in the
	   role list, in every export bundle, on the RBAC page — removable only by direct SQL, and if it
	   was minted with rbac:manage it can only be neutralised, never withdrawn. */
	if strings.Contains(req.Name, "/") {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid role",
			`role: name may not contain "/" — the name addresses the role in its own API path`)
		return
	}
	perms, badPerm := sanitizeRolePermissions(req.Permissions)
	if badPerm != "" {
		writeProblem(w, http.StatusUnprocessableEntity, "unknown permission", "unknown permission: "+badPerm)
		return
	}
	role, err := s.roleAdmin.UpsertRole(r.Context(), req.Name, perms)
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
	// Deliberately LENIENT, same reason as handleRBACRolesCreate: the audit
	// middleware scrubs unexpected keys rather than the handler rejecting them.
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RoleName == "" || req.SubjectID == "" {
		writeProblem(w, http.StatusBadRequest, "invalid request", `body must be JSON with non-empty "roleName", "subjectKind", and "subjectId"`)
		return
	}
	/* BOUNDED and free of control characters, like every sibling resource's identifier.

	   Nothing checked this field. An id of any length was accepted and stored, and then came back in
	   every GET /api/v1/rbac/bindings, in the rbac section of every export, and rendered on the RBAC
	   page — the same page-widening a token name and a role name are bounded to prevent. Past the
	   btree index limit (~2.7 KB) PostgreSQL refused the row and the handler reported it as
	   502 "rbac unavailable": a client's own input told the operator the RBAC backend was down. A NUL
	   byte did the same at any length, because PostgreSQL cannot store one in text.

	   255 bytes comfortably fits an OIDC `sub`, an email, a UUID or a group DN. */
	if rejectControlChars(w, "subjectId", req.SubjectID) || rejectControlChars(w, "roleName", req.RoleName) {
		return
	}
	if len(req.SubjectID) > subjectIDMaxLen {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid binding",
			fmt.Sprintf("subjectId is %d bytes, limit is %d", len(req.SubjectID), subjectIDMaxLen))
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

	/* Read the binding BEFORE deleting it. A DELETE carries no body, so without this the audit row
	   for the single most sensitive operation in the RBAC surface said only "binding 41 was
	   deleted" — and once the row is gone, nothing on earth can say afterwards which role that
	   was or whose. Reviewing a revocation means seeing what was revoked. */
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

	if err := s.roleAdmin.DeleteBinding(r.Context(), id); err != nil {
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

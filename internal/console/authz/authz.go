// Package authz is the Console's permission model (SECURITY.md §10.2); built-in roles are
// compiled-in constants rather than seeded database rows.
package authz

import "sync/atomic"

// Permission is a fine-grained action string. The set is CLOSED: adding one
// means adding a constant here, which is what keeps the authz_denied_total
// metric's label bounded.
type Permission string

const (
	PermTopologyRead  Permission = "topology:read"
	PermMatrixRead    Permission = "matrix:read"
	PermEventsRead    Permission = "events:read"
	PermPromQLQuery   Permission = "promql:query"
	PermRunsRead      Permission = "runs:read"
	PermRunsCreate    Permission = "runs:create"
	PermAuditRead     Permission = "audit:read"
	PermRBACManage    Permission = "rbac:manage"
	PermTokensManage  Permission = "tokens:manage"
	PermSettingsWrite Permission = "settings:write"

	// Read is split from write because the Targets page must be viewable by a role that cannot mutate
	// the fleet's probe configuration.
	PermTargetsRead    Permission = "targets:read"
	PermTargetsWrite   Permission = "targets:write"
	PermChecksRead     Permission = "checks:read"
	PermChecksWrite    Permission = "checks:write"
	PermSchedulesWrite Permission = "schedules:write"

	// (MTR Explorer + annotations). mtr:read is TELEMETRY -- path history the fleet already recorded.
	PermMTRRead          Permission = "mtr:read"
	PermAnnotationsRead  Permission = "annotations:read"
	PermAnnotationsWrite Permission = "annotations:write"

	// K8s events carry no permission of their own -- they are events.
	PermIncidentsRead    Permission = "incidents:read"
	PermIncidentsWrite   Permission = "incidents:write"
	PermMaintenanceRead  Permission = "maintenance:read"
	PermMaintenanceWrite Permission = "maintenance:write"
	PermWebhooksManage   Permission = "webhooks:manage"

	// (console-managed Prometheus alert rules); it also gates the PREVIEW route, which persists
	// nothing.
	PermAlertsRead   Permission = "alerts:read"
	PermAlertsManage Permission = "alerts:manage"
)

// AllPermissions is every permission this build knows, in a stable order; a new Permission constant
// must be added here too.
var AllPermissions = []Permission{
	PermTopologyRead,
	PermMatrixRead,
	PermEventsRead,
	PermPromQLQuery,
	PermRunsRead,
	PermRunsCreate,
	PermAuditRead,
	PermRBACManage,
	PermTokensManage,
	PermSettingsWrite,
	PermTargetsRead,
	PermTargetsWrite,
	PermChecksRead,
	PermChecksWrite,
	PermSchedulesWrite,
	PermMTRRead,
	PermAnnotationsRead,
	PermAnnotationsWrite,
	PermIncidentsRead,
	PermIncidentsWrite,
	PermMaintenanceRead,
	PermMaintenanceWrite,
	PermWebhooksManage,
	PermAlertsRead,
	PermAlertsManage,
}

// SubjectKind is how a request was authenticated.
type SubjectKind string

const (
	SubjectAnonymous SubjectKind = "anonymous"
	SubjectUser      SubjectKind = "user"
	SubjectToken     SubjectKind = "token"
)

// Subject is the authenticated identity of one request. Built by authn, consumed
// by the authorize middleware, recorded by the auditor. It is a VALUE: no
// pointers, no mutation after construction, no database handle.
type Subject struct {
	Kind SubjectKind
	// ID's meaning is MODE-SCOPED, not a single uniform shape -- and since auth.mode is a single.
	ID          string
	DisplayName string
	Groups      []string // OIDC/header groups; RBAC binding subjects
	Roles       []string // resolved role names (built-in or custom)
}

// Policy answers permission questions for a set of roles; the role→permission map lives behind an
// atomic.Pointer.
type Policy struct {
	roles atomic.Pointer[map[string]map[Permission]struct{}]
}

// NewPolicy builds a Policy from the built-in roles plus custom roles a RoleSource supplied; this
// guarantees a database row can never silently widen or weaken viewer.
func NewPolicy(custom map[string][]Permission) *Policy {
	p := &Policy{}
	p.Reload(custom)
	return p
}

// Reload atomically replaces the built-in-plus-custom role map Can and PermissionsFor read; safe to
// call concurrently with itself and with Can/PermissionsFor from any number of goroutines.
func (p *Policy) Reload(custom map[string][]Permission) {
	roles := make(map[string]map[Permission]struct{}, len(builtinRoles)+len(custom))
	for name, perms := range builtinRoles {
		roles[name] = permSet(perms)
	}
	for name, perms := range custom {
		if _, isBuiltin := builtinRoles[name]; isBuiltin {
			continue
		}
		roles[name] = permSet(perms)
	}
	p.roles.Store(&roles)
}

// Can reports whether s holds perm through any of its bound roles. A subject
// with no roles, or with only unknown role names, is always denied — there
// is no default-allow path.
func (p *Policy) Can(s Subject, perm Permission) bool { //nolint:gocritic // Subject is a value type by design (package doc); no pointers
	roles := *p.roles.Load()
	for _, role := range s.Roles {
		if perms, ok := roles[role]; ok {
			if _, ok := perms[perm]; ok {
				return true
			}
		}
	}
	return false
}

// PermissionsFor returns the union of permissions s holds across its bound
// roles, in AllPermissions order.
func (p *Policy) PermissionsFor(s Subject) []Permission { //nolint:gocritic // Subject is a value type by design (package doc); no pointers
	roles := *p.roles.Load()
	held := make(map[Permission]struct{})
	for _, role := range s.Roles {
		for perm := range roles[role] {
			held[perm] = struct{}{}
		}
	}
	out := make([]Permission, 0, len(held))
	for _, perm := range AllPermissions {
		if _, ok := held[perm]; ok {
			out = append(out, perm)
		}
	}
	return out
}

// permSet turns a permission slice into a lookup set; called only from Reload, which builds an
// entire new roles map before Policy.roles.Store ever publishes.
func permSet(perms []Permission) map[Permission]struct{} {
	set := make(map[Permission]struct{}, len(perms))
	for _, perm := range perms {
		set[perm] = struct{}{}
	}
	return set
}

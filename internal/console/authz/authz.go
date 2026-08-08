// Package authz is the Console's permission model (SECURITY.md §10.2). Built-in
// roles are compiled-in constants rather than seeded database rows, so RBAC is
// fully functional with database.mode=disabled — which is the default, and the
// state the whole M1/M2 surface runs in (Decision 7).
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

	// M4 (targets/checks/schedules). Read is split from write because the
	// Targets page must be viewable by a role that cannot mutate the
	// fleet's probe configuration: a write here is the authority to make N
	// agents send traffic to an operator-chosen address, the
	// highest-blast-radius action in the product. Granted to operator and
	// admin only -- never to viewer, which is the anonymous default
	// (Decision 3, roles.go).
	PermTargetsRead    Permission = "targets:read"
	PermTargetsWrite   Permission = "targets:write"
	PermChecksRead     Permission = "checks:read"
	PermChecksWrite    Permission = "checks:write"
	PermSchedulesWrite Permission = "schedules:write"

	// M5 (MTR Explorer + annotations). mtr:read is TELEMETRY -- path
	// history the fleet already recorded -- so the viewer role holds it,
	// unlike the targets/checks CONFIGURATION reads above (M5 Plan
	// Decision 11); launching a new trace stays behind runs:create.
	// Annotations split read from write like every other resource: a note
	// pinned to a chart is visible to anyone who can see the chart, but
	// writing one is an operator statement about the fleet's history.
	PermMTRRead          Permission = "mtr:read"
	PermAnnotationsRead  Permission = "annotations:read"
	PermAnnotationsWrite Permission = "annotations:write"

	// M6 (Investigation + incidents). incidents/maintenance follow the M5
	// telemetry-vs-statement split (Plan Decision 8): reading what happened
	// -- an incident's record, an active maintenance window -- is context
	// every role needs; writing one is an operator statement. webhooks are
	// CREDENTIAL-ADJACENT (each endpoint carries an HMAC secret), so they
	// take the rbac:manage/tokens:manage shape instead: one combined manage
	// permission, held by admin alone via AllPermissions. K8s events carry
	// no permission of their own -- they are events, and events:read
	// already answers who may read the fleet's history.
	PermIncidentsRead    Permission = "incidents:read"
	PermIncidentsWrite   Permission = "incidents:write"
	PermMaintenanceRead  Permission = "maintenance:read"
	PermMaintenanceWrite Permission = "maintenance:write"
	PermWebhooksManage   Permission = "webhooks:manage"

	// M7 (console-managed Prometheus alert rules). The pair follows the
	// incidents/maintenance groove exactly, not webhooks' single combined
	// permission: an alert rule carries no secret, so nothing here is
	// credential-adjacent.
	//
	// alerts:read is TELEMETRY. What it opens is the rule list, the expression
	// the console rendered from it, and the set Prometheus is currently firing
	// -- context on the very charts every role already reads, and the Overview
	// card that shows it is on the landing page. Every built-in role holds it,
	// viewer (the anonymous default) included. It also gates the PREVIEW route,
	// which persists nothing: previewing is asking what a draft expression
	// would match right now, which is a read of Prometheus, not a change to
	// anything.
	//
	// alerts:manage is the STATEMENT half: creating, editing, deleting and
	// force-syncing a rule are all "this fleet should page someone when X",
	// which is an operator statement about the fleet in the same class as
	// opening an incident. It stops at operator and admin, exactly where
	// incidents:write does.
	PermAlertsRead   Permission = "alerts:read"
	PermAlertsManage Permission = "alerts:manage"
)

// AllPermissions is every permission this build knows, in a stable order.
// Used by the admin role, by the /api/v1/rbac/permissions endpoint, and by the
// metric's label-set assertion. A new Permission constant must be added here
// too — TestAdminHoldsEveryPermission pins the list so that omission fails.
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
	// ID's meaning is MODE-SCOPED, not a single uniform shape -- and since
	// auth.mode is a single, mutually-exclusive config choice (local /
	// header / oidc / anonymous never run at once), these namespaces never
	// have to coexist or be told apart at runtime by ID's shape alone:
	//   - local mode: the users.id UUID (canonical text form) -- never the
	//     username. store.RoleStore.ListBindingsForSubject pins subject_id,
	//     for subject_kind='user', to this same UUID form.
	//   - OIDC mode: CURRENTLY the resolved username claim (oidc.go's
	//     usernameClaim), not a users.id UUID -- there is no OIDC user
	//     provisioning yet, pending a later milestone. Consequence: a
	//     per-user (subject_kind='user') role binding never resolves for an
	//     OIDC subject, since a username string never matches a UUID and
	//     the lookup fails closed -- OIDC subjects only ever get group
	//     bindings plus defaultRole.
	//   - header mode: the forwarded username, verbatim, exactly as the
	//     trusted proxy's configured user header carried it -- there is no
	//     users.id to resolve to, since header mode never queries a
	//     UserStore.
	//   - token (PAT) auth: the token's own id (the api_tokens.id UUID),
	//     matching role_bindings.subject_id for subject_kind='token'.
	//   - anonymous mode: the fixed literal string "anonymous".
	ID          string
	DisplayName string
	Groups      []string // OIDC/header groups; RBAC binding subjects
	Roles       []string // resolved role names (built-in or custom)
}

// Policy answers permission questions for a set of roles. Built at boot from
// the built-ins plus any custom roles a RoleSource supplied, and re-buildable
// afterward via Reload (task-18-brief.md: a changed custom-role PERMISSION
// SET should not require a console restart). The role→permission map lives
// behind an atomic.Pointer, swapped as a whole by Reload rather than mutated
// in place, so Can/PermissionsFor never observe a half-written map and need
// no lock of their own — cmd/console's 60s refresh ticker (main.go) is the
// only production Reload caller, running concurrently with every request's
// Can/PermissionsFor call.
type Policy struct {
	roles atomic.Pointer[map[string]map[Permission]struct{}]
}

// NewPolicy builds a Policy from the built-in roles plus custom roles a
// RoleSource supplied (nil or empty when database.mode=disabled, or before
// Task 12 wires a store). Equivalent to an empty Policy immediately followed
// by Reload(custom).
//
// A custom role whose name collides with a built-in role name is rejected:
// it is silently dropped and the built-in definition is kept as-is. This
// guarantees a database row can never silently widen or weaken viewer,
// operator, alert-editor, or admin. Reload applies the same rule on every
// later call.
func NewPolicy(custom map[string][]Permission) *Policy {
	p := &Policy{}
	p.Reload(custom)
	return p
}

// Reload atomically replaces the built-in-plus-custom role map Can and
// PermissionsFor read, from a freshly supplied custom role set. Safe to call
// concurrently with itself and with Can/PermissionsFor from any number of
// goroutines: the new map is built in full before the swap, so a concurrent
// reader either sees the entire old map or the entire new one, never a mix.
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

// permSet turns a permission slice into a lookup set. Called only from
// Reload, which builds an entire new roles map before Policy.roles.Store
// ever publishes it, so the resulting maps are safe to read concurrently
// without further synchronization once published.
func permSet(perms []Permission) map[Permission]struct{} {
	set := make(map[Permission]struct{}, len(perms))
	for _, perm := range perms {
		set[perm] = struct{}{}
	}
	return set
}

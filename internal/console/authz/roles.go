package authz

// builtinRoles are the compiled-in role→permission sets (SECURITY.md §10.2).
// These are NOT database rows: RBAC must work with database.mode=disabled,
// which is the default and the state the whole M1/M2 surface runs in
// (Decision 7). The roles table (Task 12) only ever adds custom roles
// alongside these; it never redefines them — see NewPolicy.
var builtinRoles = map[string][]Permission{
	// viewer holds EXACTLY the permissions the M1/M2 endpoints require. This
	// is what makes auth.mode=anonymous + auth.anonymous.role=viewer (the
	// defaults) byte-identical to the pre-M3 surface — the degraded-state
	// guarantee.
	"viewer": {
		PermTopologyRead,
		PermMatrixRead,
		PermEventsRead,
		PermPromQLQuery,
		PermRunsRead,
	},

	// operator adds the ability to trigger diagnostic runs.
	"operator": {
		PermTopologyRead,
		PermMatrixRead,
		PermEventsRead,
		PermPromQLQuery,
		PermRunsRead,
		PermRunsCreate,
	},

	// alert-editor has no alerting permissions yet — those land in M7. The
	// role exists now, identical to operator, so bindings written today
	// (e.g. via the roles table in Task 12) stay valid once alerting
	// permissions are added; it is not a lie, it is a placeholder with an
	// honest, currently-equal permission set.
	"alert-editor": {
		PermTopologyRead,
		PermMatrixRead,
		PermEventsRead,
		PermPromQLQuery,
		PermRunsRead,
		PermRunsCreate,
	},

	// admin holds every permission this build knows.
	"admin": AllPermissions,
}

// IsBuiltinRole reports whether name is one of the compiled-in role names
// (viewer/operator/alert-editor/admin). Used by internal/console/config to
// validate auth.defaultRole: a typo there must fail config validation loudly
// at boot, rather than silently granting nothing to every authenticated
// subject (which presents as "auth is broken", not "config is wrong").
func IsBuiltinRole(name string) bool {
	_, ok := builtinRoles[name]
	return ok
}

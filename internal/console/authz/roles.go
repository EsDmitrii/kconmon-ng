package authz

// builtinRoles are the compiled-in role→permission sets (SECURITY.md §10.2).
// These are NOT database rows: RBAC must work with database.mode=disabled,
// which is the default and the state the whole M1/M2 surface runs in
// (Decision 7). The roles table (Task 12) only ever adds custom roles
// alongside these; it never redefines them — see NewPolicy.
var builtinRoles = map[string][]Permission{
	// viewer is read-only TELEMETRY: the M1/M2 surface plus M5's mtr:read
	// and annotations:read (Plan Decision 11 — path history and chart notes
	// are things the fleet already recorded, the same class as events and
	// runs). What viewer must NEVER gain is CONFIGURATION authority — the
	// M4 targets/checks/schedules permissions — because viewer is what
	// auth.mode=anonymous defaults to.
	"viewer": {
		PermTopologyRead,
		PermMatrixRead,
		PermEventsRead,
		PermPromQLQuery,
		PermRunsRead,
		PermMTRRead,
		PermAnnotationsRead,
	},

	// operator adds the ability to trigger diagnostic runs, plus M4's
	// targets/checks/schedules authority (Decision 3). The five M4
	// permissions stop here and at admin: viewer must not gain them, since
	// viewer is what auth.anonymous.role defaults to, and alert-editor is an
	// M7 alerting placeholder with no reason to reconfigure the fleet's
	// probes.
	"operator": {
		PermTopologyRead,
		PermMatrixRead,
		PermEventsRead,
		PermPromQLQuery,
		PermRunsRead,
		PermRunsCreate,
		PermTargetsRead,
		PermTargetsWrite,
		PermChecksRead,
		PermChecksWrite,
		PermSchedulesWrite,
		PermMTRRead,
		PermAnnotationsRead,
		// annotations:write stops at operator and admin: a note pinned to
		// the fleet's history is an operator statement, not a viewer one.
		PermAnnotationsWrite,
	},

	// alert-editor has no alerting permissions yet — those land in M7. The
	// role exists now so bindings written today (e.g. via the roles table in
	// Task 12) stay valid once alerting permissions are added; it is not a
	// lie, it is a placeholder with an honest permission set. It was
	// identical to operator through M3; M4 deliberately did NOT give it the
	// targets/checks/schedules permissions (Decision 3), so the two roles
	// have diverged — an alert editor reconfiguring what the fleet probes is
	// not what the name promises.
	"alert-editor": {
		PermTopologyRead,
		PermMatrixRead,
		PermEventsRead,
		PermPromQLQuery,
		PermRunsRead,
		PermRunsCreate,
		// M5 telemetry reads, same reasoning as viewer's: an alert editor
		// reads charts, so it reads the notes and path history on them.
		// annotations:write deliberately absent — same line as the M4
		// configuration permissions above.
		PermMTRRead,
		PermAnnotationsRead,
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

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
		PermIncidentsRead,
		PermMaintenanceRead,
		// M7 telemetry read, same line again: the rules this console manages
		// and the alerts Prometheus is firing are context on the charts viewer
		// already sees, and the Overview card that lists them is the landing
		// page. alerts:manage is deliberately absent -- see the operator role.
		PermAlertsRead,
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
		PermIncidentsRead,
		PermMaintenanceRead,
		// incidents/maintenance writes are the same statement class as
		// annotations:write. webhooks:manage is deliberately ABSENT here --
		// admin-only, the tokens:manage/rbac:manage credential posture.
		PermIncidentsWrite,
		PermMaintenanceWrite,
		// M7's alerting pair, on the same telemetry-vs-statement split: the
		// read is context, and declaring "page someone when X" is an operator
		// statement about the fleet -- exactly incidents:write's class.
		PermAlertsRead,
		PermAlertsManage,
	},

	// alert-editor holds BOTH of M7's alerting permissions — the one
	// deliberate exception to the "statement-class writes stop at operator
	// and admin" groove, and the exception is the role's entire charter.
	//
	// The role existed as a placeholder from M3 through M6 (M4 deliberately
	// withheld the targets/checks/schedules permissions from it, Decision 3)
	// precisely so that, when alerting landed, an on-call engineer could be
	// delegated rule editing WITHOUT operator's wider fleet authority. A
	// role named alert-editor that cannot edit an alert rule would be a
	// promise the console breaks on first click; renaming the builtin
	// instead would break every existing role_binding row and
	// auth.anonymous.role reference that names it. So the name means what
	// it says: full alerts:read + alerts:manage, and nothing else beyond
	// the telemetry-read context below.
	//
	// Decided by the M7 coordinator over the uniform-groove alternative;
	// pinned by TestM7AlertPermissionsFollowTheIncidentsPosture, so
	// narrowing it back has to happen in that test's diff, consciously.
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
		// M6 telemetry reads, same line: incidents and maintenance windows
		// are context on the charts an alert editor works with; the writes
		// stay with operator/admin.
		PermIncidentsRead,
		PermMaintenanceRead,
		// M7's pair — see this role's doc comment for why manage lands here
		// despite the operator/admin groove: alerting IS this role's charter.
		PermAlertsRead,
		PermAlertsManage,
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

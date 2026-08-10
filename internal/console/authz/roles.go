package authz

// builtinRoles are the compiled-in role→permission sets (SECURITY.md §10.2); these are NOT database
// rows: RBAC must work with database.mode=disabled.
var builtinRoles = map[string][]Permission{
	// What viewer must NEVER gain is CONFIGURATION authority.
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
		// telemetry read, same line again: the rules this console manages and the alerts Prometheus is
		// firing are context on the charts viewer already sees.
		PermAlertsRead,
	},

	// operator adds the ability to trigger diagnostic runs, plus the targets/checks/schedules
	// authority.
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
		// the alerting pair, on the same telemetry-vs-statement split: the read is context.
		PermAlertsRead,
		PermAlertsManage,
	},

	// A role named alert-editor that cannot edit an alert rule would be a promise the console breaks
	// on first click.
	"alert-editor": {
		PermTopologyRead,
		PermMatrixRead,
		PermEventsRead,
		PermPromQLQuery,
		PermRunsRead,
		PermRunsCreate,
		// telemetry reads, same reasoning as viewer's: an alert editor reads charts, so it reads the
		// notes and path history on them.
		PermMTRRead,
		PermAnnotationsRead,
		// telemetry reads, same line: incidents and maintenance windows are context on the charts an
		// alert editor works with.
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
// (viewer/operator/alert-editor/admin); used by internal/console/config to validate
// auth.defaultRole.
func IsBuiltinRole(name string) bool {
	_, ok := builtinRoles[name]
	return ok
}

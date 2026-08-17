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
	"unicode"

	"github.com/EsDmitrii/kconmon-ng/internal/console/alerting"
	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// AlertRuleService is the subset of *store.DB the export/import routes need for alert_rules.
type AlertRuleService interface {
	store.AlertRuleReader
	store.AlertRuleStore
}

var _ AlertRuleService = (*store.DB)(nil)

// exportBundleVersion is the ONLY bundle version this build reads or writes.
const exportBundleVersion = 1

// exportUnavailableDetail is served whenever any config seam is unwired; the bundle is the WHOLE
// declarative configuration.
const exportUnavailableDetail = "configuration export/import reads every persisted config table and has no " +
	"in-memory fallback: set console.database.mode in the console config (Helm: console.database.mode) to " +
	"enable /api/v1/export and /api/v1/import"

// exportPageLimit is the page size the paged list seams are walked with; the cap exists so a
// pathological table cannot turn one request into an unbounded read.
const (
	exportPageLimit = 500
	exportMaxPages  = 20
)

// webhookImportNoSecretReason is the warning an endpoint that does not exist here yet carries; it
// is a WARNING and a SKIP, never an error and never a create.
const webhookImportNoSecretReason = "not imported: a bundle never carries webhook secrets and an endpoint cannot " +
	"be created without one -- create it with POST /api/v1/webhooks (secret required), then re-import to apply " +
	"the bundle's url, events and enabled flag"

// ---------------------------------------------------------------------------
// Bundle shape
// ---------------------------------------------------------------------------

// exportSchedule is a check schedule as a bundle carries it; importing them would assert a fire
// history (and a failure) the destination never had.
type exportSchedule struct {
	ID           string     `json:"id"`
	DefinitionID string     `json:"definitionId"`
	Kind         string     `json:"kind"`
	IntervalNs   int64      `json:"intervalNs"`
	RunAt        *time.Time `json:"runAt,omitempty"`
	Enabled      bool       `json:"enabled"`
}

// exportWebhook is an endpoint as a bundle carries it: name; there is no secret field and there
// never will be.
type exportWebhook struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	URL     string   `json:"url"`
	Events  []string `json:"events"`
	Enabled bool     `json:"enabled"`
	// HasSecret is INFORMATIONAL on import: it tells a human reading the bundle that the source
	// endpoint could sign its deliveries; it is always true for a stored row (the store refuses an
	// empty secret).
	HasSecret bool `json:"hasSecret"`
}

// exportAlertRule is an alert rule as a bundle carries it: the BUILDER half of store.AlertRule and
// only that half.
type exportAlertRule struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Kind         string          `json:"kind"`
	Params       json.RawMessage `json:"params"`
	Severity     string          `json:"severity"`
	ForNs        int64           `json:"forNs"`
	Labels       json.RawMessage `json:"labels"`
	Annotations  json.RawMessage `json:"annotations"`
	Enabled      bool            `json:"enabled"`
	RenderedExpr string          `json:"renderedExpr"`
}

// exportRole is a CUSTOM role as a bundle carries it: a name and the permission set it grants.
// Built-in roles are never exported — they are compiled in, identical on every build, and a bundle
// claiming to define "admin" would be a bundle claiming to redefine it.
type exportRole struct {
	Name        string   `json:"name"`
	Permissions []string `json:"permissions"`
}

// exportBinding is one grant as a bundle carries it. It is EXPORTED and never IMPORTED, for the
// same class of reason a webhook secret is never carried: a binding names a person, in the identity
// namespace of the SOURCE console's auth mode (authn/identity.go), and replaying it into another
// console would hand that role to whatever that string means there — or to nobody, silently. The
// bundle is the record of who held what; re-granting is a decision, not a restore.
type exportBinding struct {
	ID          int64     `json:"id"`
	RoleName    string    `json:"roleName"`
	SubjectKind string    `json:"subjectKind"`
	SubjectID   string    `json:"subjectId"`
	CreatedAt   time.Time `json:"createdAt"`
}

// exportRBAC is the bundle's access-control section.
//
// It is PRESENT only when the caller holds rbac:manage. Everything else in the bundle needs
// settings:write, and a grant list is strictly more sensitive than a target list: it names people
// and says what they can do. A custom role carrying settings:write without rbac:manage would
// otherwise read the whole access map through the export route.
type exportRBAC struct {
	Roles    []exportRole    `json:"roles"`
	Bindings []exportBinding `json:"bindings"`
}

// exportBundle is GET /api/v1/export's body and POST /api/v1/import's `bundle`.
type exportBundle struct {
	Version            int                   `json:"version"`
	ExportedAt         time.Time             `json:"exportedAt"`
	Targets            []targetResponse      `json:"targets"`
	CheckDefinitions   []definitionResponse  `json:"checkDefinitions"`
	CheckSchedules     []exportSchedule      `json:"checkSchedules"`
	AlertRules         []exportAlertRule     `json:"alertRules"`
	Webhooks           []exportWebhook       `json:"webhooks"`
	MaintenanceWindows []maintenanceResponse `json:"maintenanceWindows"`
	// RBAC is omitted rather than empty when the caller may not see it, so "absent" and "none
	// defined" stay distinguishable to whoever reads the file.
	RBAC *exportRBAC `json:"rbac,omitempty"`
}

// ---------------------------------------------------------------------------
// Import request/response shape
// ---------------------------------------------------------------------------

// importRequest is POST /api/v1/import's body.
type importRequest struct {
	DryRun bool          `json:"dryRun"`
	Bundle *exportBundle `json:"bundle"`
}

// importItemNote names ONE item and what happened to it; name is the item's natural key as a human
// reads it (a target/definition/rule/endpoint name, a "definition/kind" pair for a schedule, a
// "scope@start" pair for a window).
type importItemNote struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// importCollectionResult is one collection's outcome.
type importCollectionResult struct {
	Created  int              `json:"created"`
	Updated  int              `json:"updated"`
	Skipped  int              `json:"skipped"`
	Errors   []importItemNote `json:"errors"`
	Warnings []importItemNote `json:"warnings"`
}

// newImportCollectionResult keeps errors and warnings non-nil: the schema declares both as REQUIRED
// ARRAYS, and Go marshals a nil slice as null, which a strict client cannot iterate.
func newImportCollectionResult() importCollectionResult {
	return importCollectionResult{Errors: []importItemNote{}, Warnings: []importItemNote{}}
}

func (r *importCollectionResult) fail(name, reason string) {
	r.Errors = append(r.Errors, importItemNote{Name: name, Reason: reason})
}

func (r *importCollectionResult) warn(name, reason string) {
	r.Warnings = append(r.Warnings, importItemNote{Name: name, Reason: reason})
}

// counts renders this collection's three counters plus its error/warning tallies for the audit row.
func (r *importCollectionResult) counts() map[string]int {
	return map[string]int{
		"created": r.Created, "updated": r.Updated, "skipped": r.Skipped,
		"errors": len(r.Errors), "warnings": len(r.Warnings),
	}
}

// importResponse is POST /api/v1/import's body.
type importResponse struct {
	DryRun             bool                   `json:"dryRun"`
	Targets            importCollectionResult `json:"targets"`
	CheckDefinitions   importCollectionResult `json:"checkDefinitions"`
	CheckSchedules     importCollectionResult `json:"checkSchedules"`
	AlertRules         importCollectionResult `json:"alertRules"`
	Webhooks           importCollectionResult `json:"webhooks"`
	MaintenanceWindows importCollectionResult `json:"maintenanceWindows"`
	RBACRoles          importCollectionResult `json:"rbacRoles"`
	RBACBindings       importCollectionResult `json:"rbacBindings"`
}

// auditDetail renders the whole response as the import's audit row detail:
// the dryRun flag and every collection's counts, and nothing else.
func (r *importResponse) auditDetail() map[string]any {
	return map[string]any{
		"dryRun":             r.DryRun,
		"targets":            r.Targets.counts(),
		"checkDefinitions":   r.CheckDefinitions.counts(),
		"checkSchedules":     r.CheckSchedules.counts(),
		"alertRules":         r.AlertRules.counts(),
		"webhooks":           r.Webhooks.counts(),
		"maintenanceWindows": r.MaintenanceWindows.counts(),
		"rbacRoles":          r.RBACRoles.counts(),
		"rbacBindings":       r.RBACBindings.counts(),
	}
}

// ---------------------------------------------------------------------------
// Availability gate
// ---------------------------------------------------------------------------

// configUnavailable answers 503 and reports true unless EVERY config seam is
// wired. All-or-nothing on purpose: see exportUnavailableDetail.
func (s *Server) configUnavailable(w http.ResponseWriter) bool {
	if s.targets == nil || s.definitions == nil || s.schedules == nil ||
		s.alertRules == nil || s.webhooks == nil || s.maintenance == nil {
		writeProblem(w, http.StatusServiceUnavailable, "configuration export/import not available",
			exportUnavailableDetail)
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Export
// ---------------------------------------------------------------------------

// handleExport serves the whole declarative configuration as one versioned
// JSON bundle.
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	if s.configUnavailable(w) {
		return
	}
	bundle, err := s.buildExportBundle(r.Context())
	if err != nil {
		slog.Error("httpapi: export failed", "error", err) //nolint:gosec // G706: structured slog fields, not string-built log injection
		writeProblem(w, http.StatusBadGateway, "export unavailable", "failed to read the configuration to export")
		return
	}
	if s.mayManageRBAC(r) {
		if err := s.appendRBAC(r.Context(), &bundle); err != nil {
			slog.Error("httpapi: export rbac failed", "error", err) //nolint:gosec // G706: structured slog fields
			writeProblem(w, http.StatusBadGateway, "export unavailable", "failed to read the access control to export")
			return
		}
	}
	writeJSON(w, bundle)
}

// mayManageRBAC reports whether THIS request's caller holds rbac:manage — the gate on the bundle's
// access-control section, in both directions.
func (s *Server) mayManageRBAC(r *http.Request) bool {
	return s.callerCan(r, authz.PermRBACManage)
}

/*
callerCan answers "does the caller of THIS request hold p".

The import route is one permission at the door (settings:write), and for a long time that was the
whole story: every section of the bundle was then applied with no further question. That made the
route a way around every other permission in the system — the sharp example is webhooks, whose CRUD
routes require webhooks:manage: a bundle naming an existing endpoint rewrote its delivery URL, so a
subject with settings:write and nothing else could point every incident notification at a host of
their choosing, with the stored secret carried along to sign the deliveries. The same shape applied
to check definitions, targets, schedules and alert rules.

A section is now applied only if the caller could have written the same thing through that
section's own routes; otherwise it is skipped with a reason, which is exactly how the RBAC section
has always behaved.
*/
/*
importItemDetail is what a per-item failure tells the client.

publicValidationDetail only trims the "store: " prefix, so a driver error went out verbatim: a role
name carrying a NUL answered with `upsert role: ERROR: invalid byte sequence for encoding "UTF8":
0x00 (SQLSTATE 22021)` — PostgreSQL's own words, SQLSTATE included, in a response body. A validation
error is the client's own input described back to them and belongs there; anything else is this
console's internals and belongs in the log.
*/
func importItemDetail(err error) string {
	if isStoreValidationError(err) {
		return publicValidationDetail(err)
	}
	slog.Error("httpapi: import item failed", "error", err)
	return "could not be applied; see the console logs for the reason"
}

/*
isStoreValidationError reports whether err came from a store Validate rather than from the driver.

Every one of those messages is built by this project and starts with the package prefix followed by
the resource name — "store: target: ...", "store: definition: ...". A pgx error never carries it.
*/
func isStoreValidationError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrAlreadyExists) || errors.Is(err, store.ErrInUse) {
		return false
	}
	return strings.HasPrefix(err.Error(), "store: ")
}

// importAuthor is the caller as the SERVER sees them, for every imported row that records an author.
func importAuthor(r *http.Request) string {
	subject, _ := SubjectFrom(r.Context())
	return annotationAuthor(subject)
}

func (s *Server) callerCan(r *http.Request, p authz.Permission) bool {
	subject, ok := SubjectFrom(r.Context())
	return ok && s.policy != nil && s.policy.Can(subject, p)
}

// appendRBAC adds the access-control section, or leaves it absent.
func (s *Server) appendRBAC(ctx context.Context, bundle *exportBundle) error {
	if s.roleAdmin == nil {
		return nil
	}
	section := exportRBAC{Roles: []exportRole{}, Bindings: []exportBinding{}}

	roles, err := s.roleAdmin.ListRoles(ctx)
	if err != nil {
		return fmt.Errorf("list roles: %w", err)
	}
	for i := range roles {
		if authz.IsBuiltinRole(roles[i].Name) {
			continue
		}
		perms := roles[i].Permissions
		if perms == nil {
			perms = []string{}
		}
		section.Roles = append(section.Roles, exportRole{Name: roles[i].Name, Permissions: perms})
	}

	bindings, err := s.roleAdmin.ListBindings(ctx)
	if err != nil {
		return fmt.Errorf("list bindings: %w", err)
	}
	for i := range bindings {
		section.Bindings = append(section.Bindings, exportBinding{
			ID:          bindings[i].ID,
			RoleName:    bindings[i].RoleName,
			SubjectKind: bindings[i].SubjectKind,
			SubjectID:   bindings[i].SubjectID,
			CreatedAt:   bindings[i].CreatedAt,
		})
	}

	bundle.RBAC = &section
	return nil
}

func (s *Server) buildExportBundle(ctx context.Context) (exportBundle, error) {
	bundle := exportBundle{
		Version:    exportBundleVersion,
		ExportedAt: time.Now().UTC(),
		// Every collection is a non-nil empty slice so an empty console
		// exports [] rather than null: the bundle is round-tripped through
		// clients that iterate these, and null is not iterable.
		Targets:            []targetResponse{},
		CheckDefinitions:   []definitionResponse{},
		CheckSchedules:     []exportSchedule{},
		AlertRules:         []exportAlertRule{},
		Webhooks:           []exportWebhook{},
		MaintenanceWindows: []maintenanceResponse{},
	}

	targets, err := s.listAllTargets(ctx)
	if err != nil {
		return exportBundle{}, err
	}
	for i := range targets {
		bundle.Targets = append(bundle.Targets, targetResponseFrom(&targets[i]))
	}

	defs, err := s.listAllDefinitions(ctx)
	if err != nil {
		return exportBundle{}, err
	}
	for i := range defs {
		bundle.CheckDefinitions = append(bundle.CheckDefinitions, definitionResponseFrom(&defs[i]))
	}

	scheds, err := s.listAllSchedules(ctx)
	if err != nil {
		return exportBundle{}, err
	}
	for i := range scheds {
		bundle.CheckSchedules = append(bundle.CheckSchedules, exportScheduleFrom(&scheds[i]))
	}

	rules, err := s.alertRules.ListAlertRules(ctx, false)
	if err != nil {
		return exportBundle{}, fmt.Errorf("list alert rules: %w", err)
	}
	for i := range rules {
		bundle.AlertRules = append(bundle.AlertRules, exportAlertRuleFrom(&rules[i]))
	}

	hooks, err := s.webhooks.ListWebhooks(ctx)
	if err != nil {
		return exportBundle{}, fmt.Errorf("list webhooks: %w", err)
	}
	for i := range hooks {
		bundle.Webhooks = append(bundle.Webhooks, exportWebhookFrom(&hooks[i]))
	}

	// Only windows that have NOT ENDED.
	windows, err := s.listMaintenanceWindows(ctx, store.MaintenanceFilter{From: time.Now().UTC()})
	if err != nil {
		return exportBundle{}, err
	}
	for i := range windows {
		bundle.MaintenanceWindows = append(bundle.MaintenanceWindows, maintenanceResponseFrom(&windows[i]))
	}

	return bundle, nil
}

func exportScheduleFrom(s *store.Schedule) exportSchedule {
	return exportSchedule{
		ID: s.ID, DefinitionID: s.DefinitionID, Kind: s.Kind,
		IntervalNs: s.IntervalNs, RunAt: s.RunAt, Enabled: s.Enabled,
	}
}

func exportWebhookFrom(h *store.Webhook) exportWebhook {
	events := h.Events
	if events == nil {
		events = []string{}
	}
	return exportWebhook{
		ID: h.ID, Name: h.Name, URL: h.URL, Events: events,
		Enabled: h.Enabled, HasSecret: len(h.SecretEnc) > 0,
	}
}

func exportAlertRuleFrom(r *store.AlertRule) exportAlertRule {
	orEmptyObject := func(raw json.RawMessage) json.RawMessage {
		if len(raw) == 0 {
			return json.RawMessage(`{}`)
		}
		return raw
	}
	return exportAlertRule{
		ID: r.ID, Name: r.Name, Kind: r.Kind, Params: orEmptyObject(r.Params),
		Severity: r.Severity, ForNs: r.ForNs,
		Labels: orEmptyObject(r.Labels), Annotations: orEmptyObject(r.Annotations),
		Enabled: r.Enabled, RenderedExpr: r.RenderedExpr,
	}
}

// Paged reads The four keyset-paged seams are walked to exhaustion here rather than served one page
// at a time.

func (s *Server) listAllTargets(ctx context.Context) ([]store.Target, error) {
	var out []store.Target
	cursor := ""
	for page := 0; page < exportMaxPages; page++ {
		res, err := s.targets.ListTargets(ctx, store.TargetFilter{Cursor: cursor, Limit: exportPageLimit})
		if err != nil {
			return nil, fmt.Errorf("list targets: %w", err)
		}
		out = append(out, res.Targets...)
		if res.NextCursor == "" {
			return out, nil
		}
		cursor = res.NextCursor
	}
	return nil, errors.New("list targets: more pages than an export may read")
}

func (s *Server) listAllDefinitions(ctx context.Context) ([]store.Definition, error) {
	var out []store.Definition
	cursor := ""
	for page := 0; page < exportMaxPages; page++ {
		res, err := s.definitions.ListDefinitions(ctx, store.DefinitionFilter{Cursor: cursor, Limit: exportPageLimit})
		if err != nil {
			return nil, fmt.Errorf("list check definitions: %w", err)
		}
		out = append(out, res.Definitions...)
		if res.NextCursor == "" {
			return out, nil
		}
		cursor = res.NextCursor
	}
	return nil, errors.New("list check definitions: more pages than an export may read")
}

func (s *Server) listAllSchedules(ctx context.Context) ([]store.Schedule, error) {
	var out []store.Schedule
	cursor := ""
	for page := 0; page < exportMaxPages; page++ {
		res, err := s.schedules.ListSchedules(ctx, store.ScheduleFilter{Cursor: cursor, Limit: exportPageLimit})
		if err != nil {
			return nil, fmt.Errorf("list check schedules: %w", err)
		}
		out = append(out, res.Schedules...)
		if res.NextCursor == "" {
			return out, nil
		}
		cursor = res.NextCursor
	}
	return nil, errors.New("list check schedules: more pages than an export may read")
}

func (s *Server) listMaintenanceWindows(ctx context.Context, f store.MaintenanceFilter) ([]store.MaintenanceWindow, error) { //nolint:gocritic // hugeParam: mirrors the store signature
	var out []store.MaintenanceWindow
	f.Limit = exportPageLimit
	for page := 0; page < exportMaxPages; page++ {
		res, err := s.maintenance.ListMaintenanceWindows(ctx, f)
		if err != nil {
			return nil, fmt.Errorf("list maintenance windows: %w", err)
		}
		out = append(out, res.Windows...)
		if res.NextCursor == "" {
			return out, nil
		}
		f.Cursor = res.NextCursor
	}
	return nil, errors.New("list maintenance windows: more pages than an export may read")
}

// ---------------------------------------------------------------------------
// Import
// ---------------------------------------------------------------------------

// handleImport merges a bundle into this console's configuration; for CONFIG RECONCILIATION that
// beats all-or-nothing.
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	if s.configUnavailable(w) {
		return
	}

	var req importRequest
	/* STRICT, and this is the dangerous one: a misspelled "dryRun" (dry_run, dry-run) decodes as
	   false and turns a PREVIEW into an APPLY -- the exact failure the flag exists to prevent. */
	if !decodeMutationBody(w, r, &req,
		`body must be JSON with an optional "dryRun" boolean and a "bundle" object in the shape of GET /api/v1/export`) {
		return
	}
	if req.Bundle == nil {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid import",
			`import: "bundle" is required -- POST the object GET /api/v1/export returned, under a "bundle" key`)
		return
	}
	if req.Bundle.Version != exportBundleVersion {
		writeProblem(w, http.StatusUnprocessableEntity, "unsupported bundle version",
			"import: bundle version "+strconv.Itoa(req.Bundle.Version)+" is not supported; this console reads version "+
				strconv.Itoa(exportBundleVersion)+" only")
		return
	}

	imp := &importer{
		server: s, ctx: r.Context(), dryRun: req.DryRun,
		mayManageRBAC:       s.mayManageRBAC(r),
		importedBy:          importAuthor(r),
		mayWriteTargets:     s.callerCan(r, authz.PermTargetsWrite),
		mayWriteChecks:      s.callerCan(r, authz.PermChecksWrite),
		mayWriteSchedules:   s.callerCan(r, authz.PermSchedulesWrite),
		mayWriteMaintenance: s.callerCan(r, authz.PermMaintenanceWrite),
		mayManageAlerts:     s.callerCan(r, authz.PermAlertsManage),
		mayManageWebhook:    s.callerCan(r, authz.PermWebhooksManage),
	}
	res, err := imp.run(req.Bundle)
	if err != nil {
		slog.Error("httpapi: import could not read current configuration", "error", err) //nolint:gosec // G706: structured slog fields
		writeProblem(w, http.StatusBadGateway, "import unavailable",
			"failed to read the current configuration the bundle would be merged into")
		return
	}

	setAuditResult(r, res.auditDetail())
	writeJSON(w, res)
}

// importer carries the one thing that makes this task more than six merge loops: the ID REMAP; a
// bundle's own cross-references (a definition's destinationTargetId, a schedule's definitionId) are
// therefore ids that will not exist in the destination.
type importer struct {
	server *Server
	ctx    context.Context
	dryRun bool
	// mayManageRBAC is the caller's rbac:manage, decided once at the door. The bundle's roles are
	// permission SETS: applying one with only settings:write would let a bundle mint a role carrying
	// rbac:manage, which is the access map editing itself through the config route.
	mayManageRBAC bool
	// importedBy is the AUTHENTICATED caller, as the server sees them; it is what every imported row
	// that records an author is attributed to.
	importedBy string
	/* The same rule, for every other section. The import route's own gate is settings:write, and
	   that used to be the ONLY check — so the route applied targets, check definitions, schedules,
	   alert rules and webhooks that the caller could not have written through their own routes.
	   Each section is now gated on the permission its CRUD routes require, and a section the caller
	   may not write is skipped with a reason rather than silently applied. */
	mayWriteTargets     bool
	mayWriteChecks      bool
	mayWriteSchedules   bool
	mayWriteMaintenance bool
	mayManageAlerts     bool
	mayManageWebhook    bool

	// targetIDs and defIDs are bundle id -> destination id; on a dry run the value for a would-be
	// create is the bundle's own id: nothing is written.
	targetIDs map[string]string
	defIDs    map[string]string

	// defNames is bundle definition id -> definition name, so a schedule --
	// which has no name of its own -- can be reported by the definition it
	// belongs to rather than by a UUID.
	defNames map[string]string
}

func (i *importer) run(bundle *exportBundle) (importResponse, error) {
	i.targetIDs = map[string]string{}
	i.defIDs = map[string]string{}
	i.defNames = map[string]string{}

	res := importResponse{
		DryRun:             i.dryRun,
		Targets:            newImportCollectionResult(),
		CheckDefinitions:   newImportCollectionResult(),
		CheckSchedules:     newImportCollectionResult(),
		AlertRules:         newImportCollectionResult(),
		Webhooks:           newImportCollectionResult(),
		MaintenanceWindows: newImportCollectionResult(),
		RBACRoles:          newImportCollectionResult(),
		RBACBindings:       newImportCollectionResult(),
	}

	// Dependency order, and the reason for it: a definition may point at a target and a schedule at a
	// definition.
	if err := i.section(i.mayWriteTargets, authz.PermTargetsWrite, len(bundle.Targets), &res.Targets,
		func() error { return i.importTargets(bundle.Targets, &res.Targets) },
		i.mapExistingTargets); err != nil {
		return importResponse{}, err
	}
	if err := i.section(i.mayWriteChecks, authz.PermChecksWrite, len(bundle.CheckDefinitions), &res.CheckDefinitions,
		func() error { return i.importDefinitions(bundle.CheckDefinitions, &res.CheckDefinitions) },
		i.mapExistingDefinitions); err != nil {
		return importResponse{}, err
	}
	/* schedules:write is its OWN permission, and this used to pass mayWriteChecks while WARNING about
	   schedules:write — a caller holding checks:write and not schedules:write had the section applied
	   under a message naming a permission nobody checked, and one holding schedules:write and not
	   checks:write was refused a section they were entitled to. */
	if err := i.section(i.mayWriteSchedules, authz.PermSchedulesWrite, len(bundle.CheckSchedules), &res.CheckSchedules,
		func() error { return i.importSchedules(bundle.CheckSchedules, &res.CheckSchedules) }, nil); err != nil {
		return importResponse{}, err
	}
	if err := i.section(i.mayManageAlerts, authz.PermAlertsManage, len(bundle.AlertRules), &res.AlertRules,
		func() error { return i.importAlertRules(bundle.AlertRules, &res.AlertRules) }, nil); err != nil {
		return importResponse{}, err
	}
	if err := i.section(i.mayManageWebhook, authz.PermWebhooksManage, len(bundle.Webhooks), &res.Webhooks,
		func() error { return i.importWebhooks(bundle.Webhooks, &res.Webhooks) }, nil); err != nil {
		return importResponse{}, err
	}
	/* Maintenance windows were the one section with NO gate: POST /api/v1/maintenance requires
	   maintenance:write, and the import created the same rows for a caller holding settings:write
	   alone — a window suppresses alerting for its scope, so writing one is exactly the kind of act
	   the permission exists to hold. */
	if err := i.section(i.mayWriteMaintenance, authz.PermMaintenanceWrite, len(bundle.MaintenanceWindows), &res.MaintenanceWindows,
		func() error { return i.importMaintenanceWindows(bundle.MaintenanceWindows, &res.MaintenanceWindows) }, nil); err != nil {
		return importResponse{}, err
	}
	if err := i.importRBAC(bundle.RBAC, &res.RBACRoles, &res.RBACBindings); err != nil {
		return importResponse{}, err
	}
	return res, nil
}

/*
section applies one bundle section if the caller may write it, and skips it with a reason if not.

An empty section is a no-op either way: reporting "you may not import webhooks" to a bundle that
carries none would be noise, and would leak which permissions the caller lacks for no purpose.
*/
func (i *importer) section(
	allowed bool, p authz.Permission, count int, res *importCollectionResult, apply, always func() error,
) error {
	/* `always` runs on EVERY path, and forgetting that was a real defect.

	   importTargets and importDefinitions end by recording identity mappings for every row this
	   console ALREADY has — that is what lets a bundle reference a target or a definition that
	   exists here under the same name but a different id, which docs/console-api.yaml promises in so
	   many words. Those mappings were built inside apply(), so an empty section (a bundle that
	   carries no targets) or one the caller may not write skipped them — and the NEXT section then
	   rejected every reference with "neither in the bundle nor in this console", which was simply
	   false. Reading a table to learn what is already here needs no permission; only WRITING does. */
	defer func() {
		if always != nil {
			_ = always()
		}
	}()
	if count == 0 {
		return nil
	}
	if !allowed {
		res.Skipped += count
		res.warn(string(p), sectionNoPermissionReason(p))
		return nil
	}
	return apply()
}

// --- access control --------------------------------------------------------

// sectionNoPermissionReason names the permission a section needs, for the skip warning.
func sectionNoPermissionReason(p authz.Permission) string {
	return "skipped: importing this section requires " + string(p) +
		", and this caller holds only settings:write — apply it through the section's own routes, " +
		"or have the permission granted"
}

// rbacImportNoPermissionReason is why a section was left alone.
const rbacImportNoPermissionReason = "skipped: importing access control requires rbac:manage, and this caller " +
	"holds only settings:write"

// rbacImportBindingReason is why a binding is NEVER applied, however authorised the caller is.
const rbacImportBindingReason = "not imported by design: a binding names a person in the SOURCE console's identity " +
	"namespace (\"oidc:<sub>\", a local user's UUID), and replaying it here would either grant the role to " +
	"whatever that string happens to mean on this console or silently grant it to nobody -- create the binding " +
	"with POST /api/v1/rbac/bindings, against an identity this console can resolve"

// importRBAC applies the bundle's custom ROLES and never its bindings.
func (i *importer) importRBAC(section *exportRBAC, roles, bindings *importCollectionResult) error {
	if section == nil || i.server.roleAdmin == nil {
		return nil
	}
	if !i.mayManageRBAC {
		// Counted rather than 403'd: the rest of the bundle is legitimately the caller's to apply, and
		// an import that silently dropped a section would be the worse failure.
		roles.Skipped += len(section.Roles)
		bindings.Skipped += len(section.Bindings)
		if len(section.Roles) > 0 || len(section.Bindings) > 0 {
			roles.warn("rbac", rbacImportNoPermissionReason)
		}
		return nil
	}

	existing, err := i.server.roleAdmin.ListRoles(i.ctx)
	if err != nil {
		return fmt.Errorf("list roles: %w", err)
	}
	known := make(map[string]bool, len(existing))
	for idx := range existing {
		known[existing[idx].Name] = true
	}

	for idx := range section.Roles {
		item := &section.Roles[idx]
		if authz.IsBuiltinRole(item.Name) {
			// A bundle does not get to redefine a compiled-in role; Policy.Reload ignores it anyway,
			// so writing the row would only create a lie in the table.
			roles.Skipped++
			roles.warn(item.Name, "skipped: built-in roles are compiled in and cannot be redefined by a bundle")
			continue
		}
		/* THE SAME NAME BOUND the create route applies. A bundle is not a trusted input just because
		   importing one is deliberate: it is a file, often produced by another console and edited by
		   hand on the way. Without this an imported role could carry a name of any length —
		   POST /api/v1/rbac/roles answers 422 past 63 bytes, and the row it refuses is the row the
		   import wrote. */
		/* The SAME name rules the create route applies, all of them.
		   An empty name produced a row DELETE /api/v1/rbac/roles/{name} can never address — the
		   pattern does not match an empty segment — so the only way to remove it was direct SQL. A
		   control character rendered into the RBAC page and every export from then on, and could not
		   be deleted either, because the path carrying it is now refused at the door. */
		if strings.TrimSpace(item.Name) == "" {
			roles.fail("(empty)", "role: name must not be empty")
			continue
		}
		if idx := strings.IndexFunc(item.Name, unicode.IsControl); idx >= 0 {
			roles.fail(item.Name, fmt.Sprintf("role: name contains a control character at byte %d", idx))
			continue
		}
		// A "/" makes the role undeletable for the same reason an empty name does: the name IS the
		// delete route's path segment, and neither spelling of it addresses the row.
		if strings.Contains(item.Name, "/") {
			roles.fail(item.Name, `role: name may not contain "/" — the name addresses the role in its own API path`)
			continue
		}
		if len(item.Name) > roleNameMaxLen {
			roles.fail(item.Name, fmt.Sprintf("role: name is %d bytes, limit is %d", len(item.Name), roleNameMaxLen))
			continue
		}
		/* Same closed set the create route enforces (a bundle from a NEWER build naming a permission
		   this one does not have would otherwise store a role granting a string nothing checks), and
		   the same dedup, which is what bounds the stored array. */
		perms, unknown := sanitizeRolePermissions(item.Permissions)
		if unknown != "" {
			roles.fail(item.Name, "unknown permission: "+unknown)
			continue
		}
		if i.dryRun {
			if known[item.Name] {
				roles.Updated++
			} else {
				roles.Created++
			}
			continue
		}
		if _, err := i.server.roleAdmin.UpsertRole(i.ctx, item.Name, perms); err != nil {
			roles.fail(item.Name, importItemDetail(err))
			continue
		}
		if known[item.Name] {
			roles.Updated++
		} else {
			roles.Created++
			known[item.Name] = true
		}
	}

	/* And the SAME KICK the direct route publishes. Without it an import that narrows a role's
	   permission set answered 200 with the new set while every replica went on authorizing against
	   the old one until the 60s refresh — a revoked permission still working, with the API and the
	   UI both showing it revoked. A bundle is a bulk edit of the access map; it is the last place
	   that window belongs. */
	if !i.dryRun && (roles.Created > 0 || roles.Updated > 0) {
		i.server.publishRBACChanged(i.ctx)
	}

	for idx := range section.Bindings {
		b := &section.Bindings[idx]
		bindings.Skipped++
		bindings.warn(b.RoleName+"="+b.SubjectID, rbacImportBindingReason)
	}
	return nil
}

// remember records a bundle id -> destination id mapping, skipping the empty
// bundle id a hand-written bundle may carry.
func remember(m map[string]string, bundleID, liveID string) {
	if bundleID == "" {
		return
	}
	m[bundleID] = liveID
}

// --- targets ---------------------------------------------------------------

func (i *importer) importTargets(items []targetResponse, res *importCollectionResult) error {
	existing, err := i.server.listAllTargets(i.ctx)
	if err != nil {
		return err
	}
	byName := make(map[string]store.Target, len(existing))
	byID := make(map[string]bool, len(existing))
	for idx := range existing {
		byName[existing[idx].Name] = existing[idx]
		byID[existing[idx].ID] = true
	}

	for idx := range items {
		item := &items[idx]
		in := store.TargetInput{Name: item.Name, Kind: item.Kind, Address: item.Address, Labels: item.Labels}
		if err := in.Validate(); err != nil {
			res.fail(item.Name, importItemDetail(err))
			continue
		}
		/* The IDENTITY MAPPING first, whatever the gate below decides.
		   A bundle from another console carries its own ids, and remember() is what lets a check
		   definition in that bundle reference a target this console already holds under the same
		   name. Failing the target BEFORE the mapping cascaded: the definition was then refused with
		   "neither in the bundle nor in this console" -- about a target sitting right there, which
		   the direct route would happily accept. Reading what this console already holds needs no
		   permission and no gate; only WRITING does. */
		cur, found := byName[item.Name]
		if found {
			remember(i.targetIDs, item.ID, cur.ID)
		}
		/* THE SAME REACHABILITY GATE the direct route applies.
		   POST /api/v1/targets answers 422 for an address outside config.checkers.external
		   .allowedCidrs, because no agent could ever probe it and every check against it would time
		   out with no explanation. A bundle went straight to the store and skipped that entirely, so
		   an import could plant such a target -- or, through the update branch, re-point an existing
		   endpoint at one -- which is precisely the failure the create-time guard exists to prevent.
		   This is the ResponseWriter-free half of refuseUnreachableTarget, the way overProjection is
		   the ResponseWriter-free half of enforceProjection, and it fails OPEN on an unknown or
		   unreadable allowlist for the same reason the direct route does. */
		if list, outside := i.server.targetOutsideAllowlist(i.ctx, item.Address); outside {
			res.fail(item.Name, "target: "+strconv.Quote(item.Address)+
				" is outside the addresses this fleet's agents may probe ("+strings.Join(list.raw, ", ")+
				"), so every check against it would time out")
			continue
		}
		if found {
			if i.dryRun {
				res.Updated++
				continue
			}
			row, err := i.server.targets.UpdateTarget(i.ctx, cur.ID, in)
			if err != nil {
				res.fail(item.Name, importItemDetail(err))
				continue
			}
			byName[item.Name] = row
			res.Updated++
			continue
		}
		if i.dryRun {
			remember(i.targetIDs, item.ID, item.ID)
			// A bundle that names the same target twice would otherwise be
			// predicted as two creates and applied as a create plus an
			// update; recording the would-be row keeps the two identical.
			byName[item.Name] = store.Target{ID: item.ID, Name: item.Name}
			res.Created++
			continue
		}
		row, err := i.server.targets.CreateTarget(i.ctx, in)
		if err != nil {
			res.fail(item.Name, importItemDetail(err))
			continue
		}
		remember(i.targetIDs, item.ID, row.ID)
		byName[row.Name] = row
		byID[row.ID] = true
		res.Created++
	}

	return nil
}

/*
mapExistingTargets records identity mappings for every target id this console already has.

Targets the bundle did not carry are still legitimate reference destinations for a definition it
did. It is a READ, so it runs whether or not the targets section was applied — see importer.section.
*/
func (i *importer) mapExistingTargets() error {
	existing, err := i.server.listAllTargets(i.ctx)
	if err != nil {
		return err
	}
	for idx := range existing {
		if _, mapped := i.targetIDs[existing[idx].ID]; !mapped {
			i.targetIDs[existing[idx].ID] = existing[idx].ID
		}
	}
	return nil
}

/*
overProjection is enforceProjection's answer without a ResponseWriter: the import reports per item,
not per request.

Fails OPEN on a topology error, and counts that in the same ProjectionGuardFailOpen metric the
direct route does — a bypassed guard has to be alertable wherever it is bypassed.
*/
func (i *importer) overProjection(in *store.DefinitionInput) (over bool, detail string) {
	if !in.Enabled {
		return false, ""
	}
	proj, err := i.server.projectDefinition(i.ctx, in)
	if err != nil {
		i.server.metrics.ProjectionGuardFailOpen.WithLabelValues().Inc()
		slog.Warn("httpapi: import projection guard could not read the topology, allowing the write", //nolint:gosec // G706: structured slog fields, not string-built log injection
			"definition", in.Name, "error", err)
		return false, ""
	}
	if !proj.OverLimit {
		return false, ""
	}
	return true, projectionDetail(in.SourceSelection, proj)
}

// --- check definitions -----------------------------------------------------

func (i *importer) importDefinitions(items []definitionResponse, res *importCollectionResult) error {
	existing, err := i.server.listAllDefinitions(i.ctx)
	if err != nil {
		return err
	}
	byName := make(map[string]store.Definition, len(existing))
	byID := make(map[string]bool, len(existing))
	for idx := range existing {
		byName[existing[idx].Name] = existing[idx]
		byID[existing[idx].ID] = true
	}

	for idx := range items {
		item := &items[idx]
		remember(i.defNames, item.ID, item.Name)

		targetID := item.DestinationTargetID
		if targetID != "" {
			live, ok := i.targetIDs[targetID]
			if !ok {
				res.fail(item.Name, "destination target "+strconv.Quote(targetID)+
					" is neither in the bundle nor in this console")
				continue
			}
			targetID = live
		}

		in := store.DefinitionInput{
			Name: item.Name, SourceSelection: item.SourceSelection,
			DestinationKind: item.DestinationKind, DestinationTargetID: targetID,
			DestinationAddress: item.DestinationAddress, CheckType: item.CheckType,
			Plane: item.Plane, Params: item.Params, Enabled: item.Enabled,
		}
		if err := in.Validate(); err != nil {
			res.fail(item.Name, importItemDetail(err))
			continue
		}
		/* THE PROJECTION CEILING, which the direct route enforces and this one used to walk past.

		   POST /api/v1/checks refuses an ENABLED definition whose source selection projects more
		   continuous external series than the per-definition ceiling allows — it is what stops one
		   definition from assigning a probe to every agent on a large fleet. The identical definition
		   arriving in a bundle was created and left enabled, so the bound was a property of one
		   route rather than of the table.

		   It fails OPEN when the topology cannot be read, for the same reason the direct route does:
		   a controller outage must not become a config-write outage. */
		if over, detail := i.overProjection(&in); over {
			res.fail(item.Name, detail)
			continue
		}

		if cur, found := byName[item.Name]; found {
			remember(i.defIDs, item.ID, cur.ID)
			if i.dryRun {
				res.Updated++
				continue
			}
			row, err := i.server.definitions.UpdateDefinition(i.ctx, cur.ID, in)
			if err != nil {
				res.fail(item.Name, importItemDetail(err))
				continue
			}
			byName[item.Name] = row
			res.Updated++
			continue
		}
		if i.dryRun {
			remember(i.defIDs, item.ID, item.ID)
			byName[item.Name] = store.Definition{ID: item.ID, Name: item.Name}
			res.Created++
			continue
		}
		row, err := i.server.definitions.CreateDefinition(i.ctx, in)
		if err != nil {
			res.fail(item.Name, importItemDetail(err))
			continue
		}
		remember(i.defIDs, item.ID, row.ID)
		byName[row.Name] = row
		byID[row.ID] = true
		res.Created++
	}

	return nil
}

// mapExistingDefinitions is mapExistingTargets for check definitions, and runs for the same reason:
// a schedule in the bundle may name a definition this console already has.
func (i *importer) mapExistingDefinitions() error {
	existing, err := i.server.listAllDefinitions(i.ctx)
	if err != nil {
		return err
	}
	for idx := range existing {
		if _, mapped := i.defIDs[existing[idx].ID]; !mapped {
			i.defIDs[existing[idx].ID] = existing[idx].ID
		}
		remember(i.defNames, existing[idx].ID, existing[idx].Name)
	}
	return nil
}

// --- check schedules -------------------------------------------------------

// scheduleLabel is a schedule's natural key as a human reads it.
func scheduleLabel(defName, kind string) string {
	if defName == "" {
		defName = "(unknown definition)"
	}
	return defName + "/" + kind
}

func (i *importer) importSchedules(items []exportSchedule, res *importCollectionResult) error {
	existing, err := i.server.listAllSchedules(i.ctx)
	if err != nil {
		return err
	}
	// byKey groups the destination's schedules by (definition id, kind) -- the natural key; a slice,
	// not a single value: the column has no unique constraint.
	byKey := map[string][]store.Schedule{}
	for idx := range existing {
		key := existing[idx].DefinitionID + "\x00" + existing[idx].Kind
		byKey[key] = append(byKey[key], existing[idx])
	}

	for idx := range items {
		item := &items[idx]
		defName := i.defNames[item.DefinitionID]
		label := scheduleLabel(defName, item.Kind)

		defID, ok := i.defIDs[item.DefinitionID]
		if !ok {
			res.fail(label, "check definition "+strconv.Quote(item.DefinitionID)+
				" is neither in the bundle nor in this console")
			continue
		}

		in := store.ScheduleInput{
			DefinitionID: defID, Kind: item.Kind,
			IntervalNs: clampScheduleInterval(item.Kind, item.IntervalNs),
			RunAt:      item.RunAt, Enabled: item.Enabled,
		}
		if err := in.Validate(); err != nil {
			res.fail(label, importItemDetail(err))
			continue
		}
		// nextFireAt is re-seeded, never imported: the bundle carries no
		// scheduler bookkeeping (exportSchedule's doc comment) and this is the
		// same seed POST /api/v1/schedules applies.
		in.NextFireAt = seedNextFireAt(&in)

		key := defID + "\x00" + item.Kind
		matches := byKey[key]
		switch {
		case len(matches) > 1:
			res.fail(label, fmt.Sprintf(
				"this console already has %d %q schedules on definition %q: the natural key (definition, kind) "+
					"identifies none of them, so the import will not guess which to overwrite -- delete the extras first",
				len(matches), item.Kind, defName))
		case len(matches) == 1:
			if i.dryRun {
				res.Updated++
				continue
			}
			if _, err := i.server.schedules.UpdateSchedule(i.ctx, matches[0].ID, in); err != nil {
				res.fail(label, importItemDetail(err))
				continue
			}
			res.Updated++
		default:
			if i.dryRun {
				byKey[key] = []store.Schedule{{ID: item.ID, DefinitionID: defID, Kind: item.Kind}}
				res.Created++
				continue
			}
			row, err := i.server.schedules.CreateSchedule(i.ctx, in)
			if err != nil {
				res.fail(label, importItemDetail(err))
				continue
			}
			byKey[key] = append(byKey[key], row)
			res.Created++
		}
	}
	return nil
}

// --- alert rules -----------------------------------------------------------

func (i *importer) importAlertRules(items []exportAlertRule, res *importCollectionResult) error {
	existing, err := i.server.alertRules.ListAlertRules(i.ctx, false)
	if err != nil {
		return fmt.Errorf("list alert rules: %w", err)
	}
	// lower(name), because that is exactly what migration 00007's alert_rules_name_lower_idx makes
	// unique.
	byName := make(map[string]store.AlertRule, len(existing))
	for idx := range existing {
		byName[strings.ToLower(existing[idx].Name)] = existing[idx]
	}

	for idx := range items {
		item := &items[idx]
		/* RE-RENDERED from the bundle's own kind/params, never copied from it.

		   renderedExpr is a DERIVED column: the reconciler builds the desired bundle from kind and
		   params and ignores the stored string entirely. Copying the caller's value therefore stored
		   an expression that no longer follows from its own builder fields — the console displayed
		   one PromQL expression, Prometheus evaluated another, and the rule reported syncStatus
		   "synced" the whole time, because from the reconciler's point of view nothing was wrong.
		   A bundle is a file; the rule's own fields are the only thing that decides its expression.

		   The metric prefix is this console's, which is the other half of it: a bundle from a console
		   publishing under a different prefix carried expressions that match nothing here. */
		expr, rerr := i.server.renderer().Render(alerting.Rule{
			Name: item.Name, Kind: item.Kind, Params: mustDecodeObjectMap(item.Params),
			Severity: item.Severity, ForNS: item.ForNs,
			Labels: mustDecodeStringMap(item.Labels), Annotations: mustDecodeStringMap(item.Annotations),
		})
		if rerr != nil {
			res.fail(item.Name, "cannot render an expression from these fields: "+rerr.Error())
			continue
		}
		in := store.AlertRuleInput{
			Name: item.Name, Kind: item.Kind, Params: item.Params,
			Severity: item.Severity, ForNs: item.ForNs,
			Labels: item.Labels, Annotations: item.Annotations,
			Enabled: item.Enabled, RenderedExpr: expr,
		}
		if err := in.Validate(); err != nil {
			res.fail(item.Name, importItemDetail(err))
			continue
		}
		key := strings.ToLower(item.Name)
		if cur, found := byName[key]; found {
			if i.dryRun {
				res.Updated++
				continue
			}
			row, err := i.server.alertRules.UpdateAlertRule(i.ctx, cur.ID, in)
			if err != nil {
				res.fail(item.Name, importItemDetail(err))
				continue
			}
			byName[key] = row
			res.Updated++
			continue
		}
		if i.dryRun {
			byName[key] = store.AlertRule{ID: item.ID, Name: item.Name}
			res.Created++
			continue
		}
		row, err := i.server.alertRules.CreateAlertRule(i.ctx, in)
		if err != nil {
			res.fail(item.Name, importItemDetail(err))
			continue
		}
		byName[key] = row
		res.Created++
	}
	return nil
}

/*
mustDecodeObjectMap / mustDecodeStringMap decode a bundle's JSONB-shaped field for the renderer.

A malformed one decodes to an EMPTY map rather than an error, deliberately: the renderer is asked
next, and it refuses a rule whose params do not carry what its kind needs — with a message naming
the kind and the field. Failing here instead would report "invalid JSON" for a field the rule may
not even use. store.AlertRuleInput.Validate still sees the raw bytes and has its own opinion.
*/
func mustDecodeObjectMap(raw json.RawMessage) map[string]any {
	m, err := decodeJSONObjectMap(raw)
	if err != nil {
		return map[string]any{}
	}
	return m
}

func mustDecodeStringMap(raw json.RawMessage) map[string]string {
	m, err := decodeJSONStringMap("", raw)
	if err != nil {
		return map[string]string{}
	}
	return m
}

// --- webhooks --------------------------------------------------------------

func (i *importer) importWebhooks(items []exportWebhook, res *importCollectionResult) error {
	existing, err := i.server.webhooks.ListWebhooks(i.ctx)
	if err != nil {
		return fmt.Errorf("list webhooks: %w", err)
	}
	byName := make(map[string]store.Webhook, len(existing))
	for idx := range existing {
		byName[existing[idx].Name] = existing[idx]
	}

	for idx := range items {
		item := &items[idx]
		cur, found := byName[item.Name]
		if !found {
			// The one asymmetry in the whole import, and it is the store's
			// rule, not this package's: see webhookImportNoSecretReason.
			res.Skipped++
			res.warn(item.Name, webhookImportNoSecretReason)
			continue
		}
		// The STORED ciphertext is carried through untouched.
		in := store.WebhookInput{
			Name: item.Name, URL: item.URL, Events: item.Events,
			SecretEnc: cur.SecretEnc, Enabled: item.Enabled,
		}
		if err := in.Validate(); err != nil {
			res.fail(item.Name, importItemDetail(err))
			continue
		}
		if i.dryRun {
			res.Updated++
			continue
		}
		row, err := i.server.webhooks.UpdateWebhook(i.ctx, cur.ID, in)
		if err != nil {
			res.fail(item.Name, importItemDetail(err))
			continue
		}
		byName[item.Name] = row
		res.Updated++
	}
	return nil
}

// --- maintenance windows ---------------------------------------------------

// maintenanceLabel names a window the way a human reads one: its scope (or
// "(global)" for the empty one, which is a real scope) and when it starts.
func maintenanceLabel(scope string, startAt time.Time) string {
	if scope == "" {
		scope = "(global)"
	}
	return scope + "@" + startAt.UTC().Format(time.RFC3339)
}

func maintenanceKey(scope string, startAt, endAt time.Time) string {
	return scope + "\x00" + startAt.UTC().Format(time.RFC3339Nano) + "\x00" + endAt.UTC().Format(time.RFC3339Nano)
}

func (i *importer) importMaintenanceWindows(items []maintenanceResponse, res *importCollectionResult) error {
	if len(items) == 0 {
		return nil
	}
	// The existence check is bounded by exactly the span the bundle covers,
	// rather than reading every window this console ever recorded: outside
	// that span there is nothing the bundle could duplicate.
	from, to := items[0].StartAt, items[0].EndAt
	for idx := range items {
		if items[idx].StartAt.Before(from) {
			from = items[idx].StartAt
		}
		if items[idx].EndAt.After(to) {
			to = items[idx].EndAt
		}
	}
	existing, err := i.server.listMaintenanceWindows(i.ctx, store.MaintenanceFilter{From: from, To: to})
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(existing))
	for idx := range existing {
		seen[maintenanceKey(existing[idx].Scope, existing[idx].StartAt, existing[idx].EndAt)] = true
	}

	for idx := range items {
		item := &items[idx]
		label := maintenanceLabel(item.Scope, item.StartAt)
		/* createdBy is THIS console's view of who did it, never the bundle's claim about it.
		   POST /api/v1/maintenance derives it from the authenticated subject and offers the client no
		   way to set it; the import copied the field straight out of the file, so a bundle could
		   assert that any name at all had opened a maintenance window here. Attribution that a caller
		   can choose is not attribution. The bundle's value is dropped, exactly as the direct route
		   would drop it. */
		in := store.MaintenanceInput{
			Scope: item.Scope, StartAt: item.StartAt, EndAt: item.EndAt,
			Reason: item.Reason, CreatedBy: i.importedBy,
		}
		if err := in.Validate(); err != nil {
			res.fail(label, importItemDetail(err))
			continue
		}
		key := maintenanceKey(item.Scope, item.StartAt, item.EndAt)
		if seen[key] {
			// SKIPPED, never updated: store.MaintenanceStore has no update by design.
			res.Skipped++
			continue
		}
		if i.dryRun {
			seen[key] = true
			res.Created++
			continue
		}
		if _, err := i.server.maintenance.CreateMaintenanceWindow(i.ctx, in); err != nil {
			res.fail(label, importItemDetail(err))
			continue
		}
		seen[key] = true
		res.Created++
	}
	return nil
}

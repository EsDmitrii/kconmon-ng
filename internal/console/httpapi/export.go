package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/alerting"
	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
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
	"in-memory fallback: " + databaseKnob + " to enable /api/v1/export and /api/v1/import"

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
// It is PRESENT only when the caller holds rbac:manage, as every section is present only for a caller
// holding its own read permission (exportSectionGates): a grant list names people and says what they
// can do, and a custom role carrying settings:write without rbac:manage would otherwise read the whole
// access map through the export route.
type exportRBAC struct {
	Roles    []exportRole    `json:"roles"`
	Bindings []exportBinding `json:"bindings"`
}

// exportBundle is GET /api/v1/export's body and POST /api/v1/import's `bundle`.
//
// A section the caller may not read through its own route is nil, which omitzero leaves out of the
// file, and is named in Omitted; a readable empty section is [], so "absent" and "none defined" stay
// distinguishable to whoever reads the file.
type exportBundle struct {
	Version            int                   `json:"version"`
	ExportedAt         time.Time             `json:"exportedAt"`
	Targets            []targetResponse      `json:"targets,omitzero"`
	CheckDefinitions   []definitionResponse  `json:"checkDefinitions,omitzero"`
	CheckSchedules     []exportSchedule      `json:"checkSchedules,omitzero"`
	AlertRules         []exportAlertRule     `json:"alertRules,omitzero"`
	Webhooks           []exportWebhook       `json:"webhooks,omitzero"`
	MaintenanceWindows []maintenanceResponse `json:"maintenanceWindows,omitzero"`
	RBAC               *exportRBAC           `json:"rbac,omitempty"`
	// Omitted names the sections withheld from this caller, in bundle order. Import ignores it.
	Omitted []string `json:"omitted,omitzero"`
}

// exportSectionGates are the bundle's sections and the permission each one's own read route
// requires; the export gives a section only to a caller who could read it there.
var exportSectionGates = []struct {
	name string
	perm authz.Permission
}{
	{"targets", authz.PermTargetsRead},
	{"checkDefinitions", authz.PermChecksRead},
	{"checkSchedules", authz.PermChecksRead},
	{"alertRules", authz.PermAlertsRead},
	{"webhooks", authz.PermWebhooksManage},
	{"maintenanceWindows", authz.PermMaintenanceRead},
	{"rbac", authz.PermRBACManage},
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
	Created int `json:"created"`
	Updated int `json:"updated"`
	// Unchanged counts rows that already match the bundle; they are not written.
	Unchanged int              `json:"unchanged"`
	Skipped   int              `json:"skipped"`
	Errors    []importItemNote `json:"errors"`
	Warnings  []importItemNote `json:"warnings"`
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
		"created": r.Created, "updated": r.Updated, "unchanged": r.Unchanged, "skipped": r.Skipped,
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
	readable := map[string]bool{}
	var omitted []string
	for _, gate := range exportSectionGates {
		readable[gate.name] = s.callerCan(r, gate.perm)
		if !readable[gate.name] {
			omitted = append(omitted, gate.name)
		}
	}
	bundle, err := s.buildExportBundle(r.Context(), readable)
	if err != nil {
		slog.Error("httpapi: export failed", "error", err) //nolint:gosec // G706: structured slog fields, not string-built log injection
		writeProblem(w, http.StatusBadGateway, "export unavailable", "failed to read the configuration to export")
		return
	}
	bundle.Omitted = omitted
	if readable["rbac"] {
		if err := s.appendRBAC(r.Context(), &bundle); err != nil {
			slog.Error("httpapi: export rbac failed", "error", err) //nolint:gosec // G706: structured slog fields
			writeProblem(w, http.StatusBadGateway, "export unavailable", "failed to read the access control to export")
			return
		}
	}
	writeJSON(w, bundle)
}

// importItemDetail is what a per-item failure tells the client: a validation error describes the
// client's own input back to them, anything else is this console's internals and goes to the log.
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

// callerCan reports whether the caller of this request holds p. The export and the import ask it per
// section, beyond the route's own gate: a section is read or written only by a caller who could do the
// same through that section's own routes.
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

// buildExportBundle reads the sections readable names; the others stay nil and are left out.
func (s *Server) buildExportBundle(ctx context.Context, readable map[string]bool) (exportBundle, error) {
	bundle := exportBundle{Version: exportBundleVersion, ExportedAt: time.Now().UTC()}

	// Every readable collection is a non-nil empty slice so an empty console exports [] rather than
	// null: the bundle is round-tripped through clients that iterate these, and null is not iterable.
	if readable["targets"] {
		targets, err := s.listAllTargets(ctx)
		if err != nil {
			return exportBundle{}, err
		}
		bundle.Targets = []targetResponse{}
		for i := range targets {
			bundle.Targets = append(bundle.Targets, targetResponseFrom(&targets[i]))
		}
	}

	if readable["checkDefinitions"] {
		defs, err := s.listAllDefinitions(ctx)
		if err != nil {
			return exportBundle{}, err
		}
		bundle.CheckDefinitions = []definitionResponse{}
		for i := range defs {
			bundle.CheckDefinitions = append(bundle.CheckDefinitions, definitionResponseFrom(&defs[i]))
		}
	}

	if readable["checkSchedules"] {
		scheds, err := s.listAllSchedules(ctx)
		if err != nil {
			return exportBundle{}, err
		}
		bundle.CheckSchedules = []exportSchedule{}
		for i := range scheds {
			bundle.CheckSchedules = append(bundle.CheckSchedules, exportScheduleFrom(&scheds[i]))
		}
	}

	if readable["alertRules"] {
		rules, err := s.alertRules.ListAlertRules(ctx, false)
		if err != nil {
			return exportBundle{}, fmt.Errorf("list alert rules: %w", err)
		}
		bundle.AlertRules = []exportAlertRule{}
		for i := range rules {
			bundle.AlertRules = append(bundle.AlertRules, exportAlertRuleFrom(&rules[i]))
		}
	}

	if readable["webhooks"] {
		hooks, err := s.webhooks.ListWebhooks(ctx)
		if err != nil {
			return exportBundle{}, fmt.Errorf("list webhooks: %w", err)
		}
		bundle.Webhooks = []exportWebhook{}
		for i := range hooks {
			bundle.Webhooks = append(bundle.Webhooks, exportWebhookFrom(&hooks[i]))
		}
	}

	if readable["maintenanceWindows"] {
		// Only windows that have NOT ENDED.
		windows, err := s.listMaintenanceWindows(ctx, store.MaintenanceFilter{From: time.Now().UTC()})
		if err != nil {
			return exportBundle{}, err
		}
		bundle.MaintenanceWindows = []maintenanceResponse{}
		for i := range windows {
			bundle.MaintenanceWindows = append(bundle.MaintenanceWindows, maintenanceResponseFrom(&windows[i]))
		}
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
	for range exportMaxPages {
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
	for range exportMaxPages {
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
	for range exportMaxPages {
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
	for range exportMaxPages {
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
		mayManageRBAC:       s.callerCan(r, authz.PermRBACManage),
		importedBy:          importAuthor(r),
		mayWriteTargets:     s.callerCan(r, authz.PermTargetsWrite),
		mayWriteChecks:      s.callerCan(r, authz.PermChecksWrite),
		mayWriteSchedules:   s.callerCan(r, authz.PermSchedulesWrite),
		mayWriteMaintenance: s.callerCan(r, authz.PermMaintenanceWrite),
		mayManageAlerts:     s.callerCan(r, authz.PermAlertsManage),
		mayManageWebhooks:   s.callerCan(r, authz.PermWebhooksManage),
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
	// The same rule for every other section: each is gated on the permission its own routes require,
	// beyond the import route's settings:write.
	mayWriteTargets     bool
	mayWriteChecks      bool
	mayWriteSchedules   bool
	mayWriteMaintenance bool
	mayManageAlerts     bool
	mayManageWebhooks   bool

	// targetIDs and defIDs are bundle id -> destination id; on a dry run the value for a would-be
	// create is the bundle's own id: nothing is written.
	targetIDs map[string]string
	defIDs    map[string]string

	// defShapes is definition id (as defIDs gives it) -> the check a schedule of it would run, as the
	// import leaves the definition: a schedule is refused when its kind cannot run that check.
	defShapes map[string]definitionShape

	// defNames is bundle definition id -> definition name, so a schedule --
	// which has no name of its own -- can be reported by the definition it
	// belongs to rather than by a UUID.
	defNames map[string]string

	// previewRoles holds, on a dry run, the roles the import would already have written, so each
	// later role's last-admin guard sees them as the real import's guard will.
	previewRoles map[string][]authz.Permission

	// previewTargets is previewRoles for targets: live (or, for a would-be create, bundle) id ->
	// the row a dry run would have written, so a definition is judged against it.
	previewTargets map[string]store.Target
	// bundleDefinitions are the definitions this import may write (the bundle's, when the caller holds
	// checks:write); a target edit does not answer for one the import will rewrite (rewrittenDefinitions).
	bundleDefinitions []definitionResponse
	// liveDefinitions caches liveDefinitionsByName.
	liveDefinitions map[string]store.Definition
	// topo and topoErr memoise projectionTopology once topoRead is set: every definition in the
	// bundle is judged against the same fleet, so a large bundle costs one controller read.
	topo     *controllerclient.Topology
	topoErr  error
	topoRead bool
}

// projectionTopology is Server.projectionTopology read once per import, error included.
func (i *importer) projectionTopology() (*controllerclient.Topology, error) {
	if !i.topoRead {
		i.topo, i.topoErr = i.server.projectionTopology(i.ctx)
		i.topoRead = true
	}
	return i.topo, i.topoErr
}

func (i *importer) run(bundle *exportBundle) (importResponse, error) {
	i.targetIDs = map[string]string{}
	i.defIDs = map[string]string{}
	i.defNames = map[string]string{}
	i.defShapes = map[string]definitionShape{}
	i.previewTargets = map[string]store.Target{}
	if i.mayWriteChecks {
		i.bundleDefinitions = bundle.CheckDefinitions
	}

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
	if err := i.section(i.mayWriteSchedules, authz.PermSchedulesWrite, len(bundle.CheckSchedules), &res.CheckSchedules,
		func() error { return i.importSchedules(bundle.CheckSchedules, &res.CheckSchedules) }, nil); err != nil {
		return importResponse{}, err
	}
	if err := i.section(i.mayManageAlerts, authz.PermAlertsManage, len(bundle.AlertRules), &res.AlertRules,
		func() error { return i.importAlertRules(bundle.AlertRules, &res.AlertRules) }, nil); err != nil {
		return importResponse{}, err
	}
	if err := i.section(i.mayManageWebhooks, authz.PermWebhooksManage, len(bundle.Webhooks), &res.Webhooks,
		func() error { return i.importWebhooks(bundle.Webhooks, &res.Webhooks) }, nil); err != nil {
		return importResponse{}, err
	}
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

always runs on every path, empty and skipped sections included: it maps the rows this console already
holds, which a later section may reference whether or not this one was written. Reading them needs
no permission, and a failed read fails the import like any other.
*/
func (i *importer) section(
	allowed bool, p authz.Permission, count int, res *importCollectionResult, apply, always func() error,
) (err error) {
	defer func() {
		if always == nil {
			return
		}
		if aerr := always(); err == nil {
			err = aerr
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
		", which this caller does not hold; apply it through the section's own routes, or have the permission granted"
}

// rbacImportNoPermissionReason is the skip warning of an access-control section.
var rbacImportNoPermissionReason = sectionNoPermissionReason(authz.PermRBACManage)

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
	stored := make(map[string][]string, len(existing))
	for idx := range existing {
		known[existing[idx].Name] = true
		stored[existing[idx].Name] = existing[idx].Permissions
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
		// The create route's name rules; the name is the DELETE route's path segment.
		if problem := roleNameProblem(item.Name); problem != "" {
			label := item.Name
			if strings.TrimSpace(label) == "" {
				label = "(empty)"
			}
			roles.fail(label, problem)
			continue
		}
		// The create route's closed permission set and dedup: a newer build's permission would grant a
		// string nothing here checks.
		perms, unknown := sanitizeRolePermissions(item.Permissions)
		if unknown != "" {
			roles.fail(item.Name, "unknown permission: "+unknown)
			continue
		}
		if cur, ok := stored[item.Name]; ok && sameStringSet(cur, perms) {
			roles.Unchanged++
			continue
		}
		// The same last-admin guard POST /api/v1/rbac/roles applies. A dry run has written none of the
		// bundle's earlier roles, so they are laid over the stored ones.
		var guard func(context.Context) error
		if !slices.Contains(perms, string(authz.PermUsersManage)) {
			guard = i.server.rbacLastAdminGuard(rbacChange{role: item.Name, permissions: perms, pendingRoles: i.previewRoles})
		}
		if i.dryRun {
			if guard != nil {
				if err := guard(i.ctx); err != nil {
					roles.fail(item.Name, importRoleDetail(err))
					continue
				}
			}
			if i.previewRoles == nil {
				i.previewRoles = map[string][]authz.Permission{}
			}
			i.previewRoles[item.Name] = asPermissions(perms)
			if known[item.Name] {
				roles.Updated++
			} else {
				roles.Created++
			}
			continue
		}
		if _, err := i.server.roleAdmin.UpsertRoleGuarded(i.ctx, item.Name, perms, guard); err != nil {
			roles.fail(item.Name, importRoleDetail(err))
			continue
		}
		if known[item.Name] {
			roles.Updated++
		} else {
			roles.Created++
			known[item.Name] = true
		}
	}

	// The same kick the direct route publishes, so no replica keeps authorizing against the old roles.
	if !i.dryRun && (roles.Created > 0 || roles.Updated > 0) {
		i.server.publishRBACChanged(i.ctx)
	}

	// Said once for the section: the reason is the same for every binding.
	if n := len(section.Bindings); n > 0 {
		bindings.Skipped += n
		bindings.warn(fmt.Sprintf("%d role bindings", n), rbacImportBindingReason)
	}
	return nil
}

// importRoleDetail is importItemDetail plus the last-admin guard's refusal, which is the caller's
// to act on and so is named rather than logged.
func importRoleDetail(err error) string {
	if errors.Is(err, errLastAdmin) {
		return "skipped: this change leaves no enabled user who can manage users; grant users:manage to someone else first"
	}
	return importItemDetail(err)
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
		// The identity mapping first, whatever the gates below decide: a definition in the bundle may
		// reference this target even when the target itself is refused.
		cur, found := byName[item.Name]
		if found {
			remember(i.targetIDs, item.ID, cur.ID)
		}
		// POST /api/v1/targets' reachability gate (refuseUnreachableTarget without a ResponseWriter),
		// failing open on an unknown or unreadable allowlist as it does.
		if list, outside := i.server.targetOutsideAllowlist(i.ctx, item.Address); outside {
			res.fail(item.Name, "target: "+strconv.Quote(item.Address)+
				" is outside the addresses this fleet's agents may probe ("+strings.Join(list.raw, ", ")+
				"), so every check against it would time out")
			continue
		}
		if found && cur.Kind == in.Kind && cur.Address == in.Address && sameJSON(cur.Labels, in.Labels) {
			if i.dryRun {
				i.previewTargets[cur.ID] = store.Target{ID: cur.ID, Name: in.Name, Kind: in.Kind, Address: in.Address}
			}
			res.Unchanged++
			continue
		}
		if found {
			// The direct route's guard: an edit must not leave a definition pointing here unrunnable.
			names, exact, reason := i.server.definitionsBrokenByTargetEdit(i.ctx, cur.ID, &in, nil)
			if reason != nil && len(i.bundleDefinitions) > 0 {
				names, exact, reason = i.server.definitionsBrokenByTargetEdit(i.ctx, cur.ID, &in,
					i.rewrittenDefinitions(cur.ID, item.ID, &in))
			}
			if reason != nil {
				res.fail(item.Name, targetEditBreaksDetail(&in, names, exact, reason))
				continue
			}
			if i.dryRun {
				i.previewTargets[cur.ID] = store.Target{ID: cur.ID, Name: in.Name, Kind: in.Kind, Address: in.Address}
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
			i.previewTargets[item.ID] = store.Target{ID: item.ID, Name: in.Name, Kind: in.Kind, Address: in.Address}
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
rewrittenDefinitions names the bundled definitions that excuse editing the live target liveID (the
bundle's bundleID) into next: the ones importDefinitions will write, judged here as it will judge
them. One it would refuse leaves the live row in place, still pointing at the edited target, so it
excuses nothing. A reference to a target the import has not mapped yet is not resolved here, and
that definition does not excuse the edit either; nor does any, when the live definitions cannot be
read.
*/
func (i *importer) rewrittenDefinitions(liveID, bundleID string, next *store.TargetInput) map[string]bool {
	out := map[string]bool{}
	live, err := i.liveDefinitionsByName()
	if err != nil {
		slog.Warn("httpapi: import could not read the definitions a target edit would affect", "error", err) //nolint:gosec // G706: structured slog fields, not string-built log injection
		return out
	}
	for idx := range i.bundleDefinitions {
		item := &i.bundleDefinitions[idx]
		targetID := item.DestinationTargetID
		switch targetID {
		case "":
		case bundleID, liveID:
			targetID = liveID
		default:
			mapped, ok := i.targetIDs[targetID]
			if !ok {
				continue
			}
			targetID = mapped
		}
		in := definitionInputFrom(item, targetID)
		if in.Validate() != nil || i.projectionRefuses(&in) {
			continue
		}
		cur, found := live[item.Name]
		if !found || in.Enabled {
			if in.DestinationKind == "target" && targetID == liveID {
				if runsAsExternalCheck(in.CheckType, in.DestinationKind) &&
					externalSpecError(in.CheckType, in.Params, next.Name, next.Kind, next.Address) != nil {
					continue
				}
			} else if i.unrunnableDefinition(&in) != nil {
				continue
			}
		}
		if found {
			if _, reason := i.server.scheduleBrokenByDefinitionEdit(i.ctx, cur.ID, &in); reason != nil {
				continue
			}
		}
		out[item.Name] = true
	}
	return out
}

// liveDefinitionsByName reads this console's definitions once per import, keyed by the name
// importDefinitions matches a bundled one on.
func (i *importer) liveDefinitionsByName() (map[string]store.Definition, error) {
	if i.liveDefinitions != nil {
		return i.liveDefinitions, nil
	}
	existing, err := i.server.listAllDefinitions(i.ctx)
	if err != nil {
		return nil, err
	}
	i.liveDefinitions = make(map[string]store.Definition, len(existing))
	for idx := range existing {
		i.liveDefinitions[existing[idx].Name] = existing[idx]
	}
	return i.liveDefinitions, nil
}

// definitionInputFrom is the row the import writes for item, its target reference already mapped to
// this console's targetID; rewrittenDefinitions has to judge exactly that row.
func definitionInputFrom(item *definitionResponse, targetID string) store.DefinitionInput {
	return store.DefinitionInput{
		Name: item.Name, SourceSelection: item.SourceSelection,
		DestinationKind: item.DestinationKind, DestinationTargetID: targetID,
		DestinationAddress: item.DestinationAddress, CheckType: item.CheckType,
		Plane: item.Plane, Params: item.Params, Enabled: item.Enabled,
	}
}

// projectionRefuses is overProjection without its log line and metric, which the definitions section
// records when it judges the same row.
func (i *importer) projectionRefuses(in *store.DefinitionInput) bool {
	if !in.Enabled {
		return false
	}
	topo, err := i.projectionTopology()
	return err == nil && projectDefinitionOn(topo, in).OverLimit
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
	topo, err := i.projectionTopology()
	if err != nil {
		i.server.metrics.ProjectionGuardFailOpen.WithLabelValues().Inc()
		slog.Warn("httpapi: import projection guard could not read the topology, allowing the write", //nolint:gosec // G706: structured slog fields, not string-built log injection
			"definition", in.Name, "error", err)
		return false, ""
	}
	proj := projectDefinitionOn(topo, in)
	if !proj.OverLimit {
		return false, ""
	}
	return true, projectionDetail(in.SourceSelection, proj)
}

// unrunnableDefinition is the server's guard, reading a target a dry run has only previewed from
// the preview, so the dry run and the real import give the same answer.
func (i *importer) unrunnableDefinition(in *store.DefinitionInput) error {
	if t, ok := i.previewTargets[in.DestinationTargetID]; ok && in.DestinationKind == "target" {
		if !runsAsExternalCheck(in.CheckType, in.DestinationKind) {
			return nil
		}
		return externalSpecError(in.CheckType, in.Params, t.Name, t.Kind, t.Address)
	}
	return i.server.unrunnableDefinition(i.ctx, in)
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

		in := definitionInputFrom(item, targetID)
		if err := in.Validate(); err != nil {
			res.fail(item.Name, importItemDetail(err))
			continue
		}
		// Before the create/edit guards: leaving a row as it is edits nothing they protect.
		if cur, found := byName[item.Name]; found && sameDefinition(&cur, &in) {
			remember(i.defIDs, item.ID, cur.ID)
			i.defShapes[cur.ID] = definitionShape{in.CheckType, in.DestinationKind}
			res.Unchanged++
			continue
		}
		// POST /api/v1/checks' projection ceiling, failing open on an unreadable topology as it does.
		if over, detail := i.overProjection(&in); over {
			res.fail(item.Name, detail)
			continue
		}
		// The routes' runnability guard: always on a create, on an update only when it leaves the row enabled.
		cur, found := byName[item.Name]
		if !found || in.Enabled {
			if err := i.unrunnableDefinition(&in); err != nil {
				res.fail(item.Name, unrunnableDefinitionDetail(err))
				continue
			}
		}

		shape := definitionShape{in.CheckType, in.DestinationKind}
		if found {
			remember(i.defIDs, item.ID, cur.ID)
			// And PUT /api/v1/checks/{id}'s: the edit may not leave one of its schedules unable to run it.
			if kind, reason := i.server.scheduleBrokenByDefinitionEdit(i.ctx, cur.ID, &in); reason != nil {
				res.fail(item.Name, definitionEditBreaksScheduleDetail(kind, reason))
				continue
			}
			if i.dryRun {
				i.defShapes[cur.ID] = shape
				res.Updated++
				continue
			}
			row, err := i.server.definitions.UpdateDefinition(i.ctx, cur.ID, in)
			if err != nil {
				res.fail(item.Name, importItemDetail(err))
				continue
			}
			i.defShapes[cur.ID] = shape
			byName[item.Name] = row
			res.Updated++
			continue
		}
		if i.dryRun {
			remember(i.defIDs, item.ID, item.ID)
			i.defShapes[item.ID] = shape
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
		i.defShapes[row.ID] = shape
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
		if _, shaped := i.defShapes[existing[idx].ID]; !shaped {
			i.defShapes[existing[idx].ID] = definitionShape{existing[idx].CheckType, existing[idx].DestinationKind}
		}
		remember(i.defNames, existing[idx].ID, existing[idx].Name)
	}
	return nil
}

// definitionShape is what decides whether a schedule of some kind can run a definition.
type definitionShape struct {
	checkType       string
	destinationKind string
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
		key := defID + "\x00" + item.Kind
		matches := byKey[key]
		if len(matches) == 1 && sameSchedule(&matches[0], &in) {
			res.Unchanged++
			continue
		}
		// The routes' guard, against the definition as this import leaves it: always on a create, on an
		// update only when it leaves the schedule enabled.
		if shape, ok := i.defShapes[defID]; ok && (len(matches) == 0 || in.Enabled) {
			if err := scheduleCannotRun(item.Kind, shape.checkType, shape.destinationKind); err != nil {
				res.fail(label, "schedule: "+err.Error())
				continue
			}
		}
		// nextFireAt is re-seeded, never imported: the bundle carries no
		// scheduler bookkeeping (exportSchedule's doc comment) and this is the
		// same seed POST /api/v1/schedules applies.
		in.NextFireAt = seedNextFireAt(&in)

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
		// Re-rendered from the bundle's kind and params with this console's metric prefix, never copied:
		// renderedExpr is derived, and the reconciler builds the rule from the builder fields alone.
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
		// The store refuses a clash of Prometheus alert names; a dry run never reaches it, so it asks the
		// store's own check against the rows it would have written.
		if i.dryRun {
			if err := store.AlertNameConflict(slices.Collect(maps.Values(byName)), byName[key].ID, item.Name); err != nil {
				res.fail(item.Name, publicValidationDetail(err))
				continue
			}
		}
		if cur, found := byName[key]; found && sameAlertRule(&cur, &in) {
			res.Unchanged++
			continue
		}
		if cur, found := byName[key]; found {
			if i.dryRun {
				byName[key] = store.AlertRule{ID: cur.ID, Name: item.Name}
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
			// A placeholder id of its own: a bundle's ids may be missing or repeated, and
			// AlertNameConflict reads a shared id as the same rule.
			byName[key] = store.AlertRule{ID: "dry-run:" + key, Name: item.Name}
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
		if cur.URL == in.URL && cur.Enabled == in.Enabled && sameStringSet(cur.Events, in.Events) {
			res.Unchanged++
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
	// The reason each window here carries, by scope and span.
	seen := make(map[string]string, len(existing))
	for idx := range existing {
		seen[maintenanceKey(existing[idx].Scope, existing[idx].StartAt, existing[idx].EndAt)] = existing[idx].Reason
	}

	for idx := range items {
		item := &items[idx]
		label := maintenanceLabel(item.Scope, item.StartAt)
		// createdBy is the authenticated caller, as on POST /api/v1/maintenance; the bundle's claim is
		// dropped.
		in := store.MaintenanceInput{
			Scope: item.Scope, StartAt: item.StartAt, EndAt: item.EndAt,
			Reason: item.Reason, CreatedBy: i.importedBy,
		}
		if err := in.Validate(); err != nil {
			res.fail(label, importItemDetail(err))
			continue
		}
		key := maintenanceKey(item.Scope, item.StartAt, item.EndAt)
		if reason, ok := seen[key]; ok {
			// Never updated: store.MaintenanceStore has no update by design.
			if reason == item.Reason {
				res.Unchanged++
				continue
			}
			res.Skipped++
			res.warn(label, maintenanceImportExistsReason)
			continue
		}
		if i.dryRun {
			seen[key] = item.Reason
			res.Created++
			continue
		}
		if _, err := i.server.maintenance.CreateMaintenanceWindow(i.ctx, in); err != nil {
			res.fail(label, importItemDetail(err))
			continue
		}
		seen[key] = item.Reason
		res.Created++
	}
	return nil
}

// maintenanceImportExistsReason is why a window whose scope and span exist here with another reason
// is left alone.
const maintenanceImportExistsReason = "skipped: a window with this scope, start and end already exists " +
	"with a different reason, and an import never rewrites a maintenance window; delete it first to replace it"

// --- unchanged rows ----------------------------------------------------------

// sameJSON compares two JSON documents by value; empty and null read as {}, as the store writes them.
func sameJSON(a, b json.RawMessage) bool {
	av, aok := decodeJSONValue(a)
	bv, bok := decodeJSONValue(b)
	return aok && bok && reflect.DeepEqual(av, bv)
}

func decodeJSONValue(raw json.RawMessage) (any, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return map[string]any{}, true
	}
	var v any
	if err := json.Unmarshal(trimmed, &v); err != nil {
		return nil, false
	}
	return v, true
}

// sameStringSet compares two lists as sets: the order of permissions or events carries no meaning.
func sameStringSet(a, b []string) bool {
	as, bs := slices.Clone(a), slices.Clone(b)
	slices.Sort(as)
	slices.Sort(bs)
	return slices.Equal(slices.Compact(as), slices.Compact(bs))
}

func sameDefinition(cur *store.Definition, in *store.DefinitionInput) bool {
	return cur.Name == in.Name && cur.SourceSelection == in.SourceSelection &&
		cur.DestinationKind == in.DestinationKind && cur.DestinationTargetID == in.DestinationTargetID &&
		cur.DestinationAddress == in.DestinationAddress && cur.CheckType == in.CheckType &&
		cur.Plane == in.Plane && cur.Enabled == in.Enabled && sameJSON(cur.Params, in.Params)
}

func sameSchedule(cur *store.Schedule, in *store.ScheduleInput) bool {
	sameRunAt := (cur.RunAt == nil) == (in.RunAt == nil) && (cur.RunAt == nil || cur.RunAt.Equal(*in.RunAt))
	return cur.Kind == in.Kind && cur.IntervalNs == in.IntervalNs && cur.Enabled == in.Enabled && sameRunAt
}

// sameAlertRule compares the builder fields; the name exactly, since a change of case is a rename.
func sameAlertRule(cur *store.AlertRule, in *store.AlertRuleInput) bool {
	return cur.Name == in.Name && cur.Kind == in.Kind && cur.Severity == in.Severity &&
		cur.ForNs == in.ForNs && cur.Enabled == in.Enabled && cur.RenderedExpr == in.RenderedExpr &&
		sameJSON(cur.Params, in.Params) && sameJSON(cur.Labels, in.Labels) && sameJSON(cur.Annotations, in.Annotations)
}

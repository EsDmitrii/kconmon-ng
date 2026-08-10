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
const webhookImportNoSecretReason = "imported without secret: a bundle never carries webhook secrets and an " +
	"endpoint cannot be created without one -- create it with POST /api/v1/webhooks (secret required), " +
	"then re-import to apply the bundle's url, events and enabled flag"

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
	writeJSON(w, bundle)
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid request",
			`body must be JSON with an optional "dryRun" boolean and a "bundle" object in the shape of GET /api/v1/export`)
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

	imp := &importer{server: s, ctx: r.Context(), dryRun: req.DryRun}
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

	res := importResponse{DryRun: i.dryRun}

	// Dependency order, and the reason for it: a definition may point at a target and a schedule at a
	// definition.
	if err := i.importTargets(bundle.Targets, &res.Targets); err != nil {
		return importResponse{}, err
	}
	if err := i.importDefinitions(bundle.CheckDefinitions, &res.CheckDefinitions); err != nil {
		return importResponse{}, err
	}
	if err := i.importSchedules(bundle.CheckSchedules, &res.CheckSchedules); err != nil {
		return importResponse{}, err
	}
	if err := i.importAlertRules(bundle.AlertRules, &res.AlertRules); err != nil {
		return importResponse{}, err
	}
	if err := i.importWebhooks(bundle.Webhooks, &res.Webhooks); err != nil {
		return importResponse{}, err
	}
	if err := i.importMaintenanceWindows(bundle.MaintenanceWindows, &res.MaintenanceWindows); err != nil {
		return importResponse{}, err
	}
	return res, nil
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
			res.fail(item.Name, publicValidationDetail(err))
			continue
		}
		if cur, found := byName[item.Name]; found {
			remember(i.targetIDs, item.ID, cur.ID)
			if i.dryRun {
				res.Updated++
				continue
			}
			row, err := i.server.targets.UpdateTarget(i.ctx, cur.ID, in)
			if err != nil {
				res.fail(item.Name, publicValidationDetail(err))
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
			res.fail(item.Name, publicValidationDetail(err))
			continue
		}
		remember(i.targetIDs, item.ID, row.ID)
		byName[row.Name] = row
		byID[row.ID] = true
		res.Created++
	}

	// Targets the bundle did not carry are still legitimate reference
	// destinations for a definition it did, so record identity mappings for
	// every id this console already has.
	for id := range byID {
		if _, mapped := i.targetIDs[id]; !mapped {
			i.targetIDs[id] = id
		}
	}
	return nil
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
			res.fail(item.Name, publicValidationDetail(err))
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
				res.fail(item.Name, publicValidationDetail(err))
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
			res.fail(item.Name, publicValidationDetail(err))
			continue
		}
		remember(i.defIDs, item.ID, row.ID)
		byName[row.Name] = row
		byID[row.ID] = true
		res.Created++
	}

	for id := range byID {
		if _, mapped := i.defIDs[id]; !mapped {
			i.defIDs[id] = id
		}
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
			res.fail(label, publicValidationDetail(err))
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
				res.fail(label, publicValidationDetail(err))
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
				res.fail(label, publicValidationDetail(err))
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
		in := store.AlertRuleInput{
			Name: item.Name, Kind: item.Kind, Params: item.Params,
			Severity: item.Severity, ForNs: item.ForNs,
			Labels: item.Labels, Annotations: item.Annotations,
			Enabled: item.Enabled, RenderedExpr: item.RenderedExpr,
		}
		if err := in.Validate(); err != nil {
			res.fail(item.Name, publicValidationDetail(err))
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
				res.fail(item.Name, publicValidationDetail(err))
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
			res.fail(item.Name, publicValidationDetail(err))
			continue
		}
		byName[key] = row
		res.Created++
	}
	return nil
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
			res.fail(item.Name, publicValidationDetail(err))
			continue
		}
		if i.dryRun {
			res.Updated++
			continue
		}
		row, err := i.server.webhooks.UpdateWebhook(i.ctx, cur.ID, in)
		if err != nil {
			res.fail(item.Name, publicValidationDetail(err))
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
		in := store.MaintenanceInput{
			Scope: item.Scope, StartAt: item.StartAt, EndAt: item.EndAt,
			Reason: item.Reason, CreatedBy: item.CreatedBy,
		}
		if err := in.Validate(); err != nil {
			res.fail(label, publicValidationDetail(err))
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
			res.fail(label, publicValidationDetail(err))
			continue
		}
		seen[key] = true
		res.Created++
	}
	return nil
}

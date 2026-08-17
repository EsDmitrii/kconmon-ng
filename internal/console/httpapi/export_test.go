package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// ---------------------------------------------------------------------------
// Doubles
// ---------------------------------------------------------------------------

// fakeAlertRuleStore is the AlertRuleService double.
type fakeAlertRuleStore struct {
	mu sync.Mutex

	rules map[string]store.AlertRule
	order []string

	listErr   error
	createErr error
	updateErr error
}

func newFakeAlertRuleStore() *fakeAlertRuleStore {
	return &fakeAlertRuleStore{rules: map[string]store.AlertRule{}}
}

func (f *fakeAlertRuleStore) CreateAlertRule(_ context.Context, in store.AlertRuleInput) (store.AlertRule, error) { //nolint:gocritic // hugeParam: test double mirrors the store signature
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return store.AlertRule{}, f.createErr
	}
	if err := in.Validate(); err != nil {
		return store.AlertRule{}, err
	}
	for _, r := range f.rules {
		if strings.EqualFold(r.Name, in.Name) {
			return store.AlertRule{}, store.ErrAlreadyExists
		}
	}
	now := time.Now().UTC()
	rule := alertRuleFromFakeInput(uuid.NewString(), &in, now, now)
	f.rules[rule.ID] = rule
	f.order = append(f.order, rule.ID)
	return rule, nil
}

func (f *fakeAlertRuleStore) UpdateAlertRule(_ context.Context, id string, in store.AlertRuleInput) (store.AlertRule, error) { //nolint:gocritic // hugeParam: test double mirrors the store signature
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateErr != nil {
		return store.AlertRule{}, f.updateErr
	}
	if err := in.Validate(); err != nil {
		return store.AlertRule{}, err
	}
	existing, ok := f.rules[id]
	if !ok {
		return store.AlertRule{}, store.ErrNotFound
	}
	for other, r := range f.rules {
		if other != id && strings.EqualFold(r.Name, in.Name) {
			return store.AlertRule{}, store.ErrAlreadyExists
		}
	}
	updated := alertRuleFromFakeInput(id, &in, existing.CreatedAt, time.Now().UTC())
	f.rules[id] = updated
	return updated, nil
}

func (f *fakeAlertRuleStore) UpdateAlertRuleSyncStatus(
	_ context.Context, id, status, message string, lastSyncedAt *time.Time,
) (store.AlertRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rule, ok := f.rules[id]
	if !ok {
		return store.AlertRule{}, store.ErrNotFound
	}
	rule.SyncStatus, rule.SyncMessage, rule.LastSyncedAt = status, message, lastSyncedAt
	f.rules[id] = rule
	return rule, nil
}

func (f *fakeAlertRuleStore) DeleteAlertRule(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.rules[id]; !ok {
		return store.ErrNotFound
	}
	delete(f.rules, id)
	return nil
}

func (f *fakeAlertRuleStore) GetAlertRule(_ context.Context, id string) (store.AlertRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rule, ok := f.rules[id]
	if !ok {
		return store.AlertRule{}, store.ErrNotFound
	}
	return rule, nil
}

func (f *fakeAlertRuleStore) ListAlertRules(_ context.Context, enabledOnly bool) ([]store.AlertRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]store.AlertRule, 0, len(f.order))
	for _, id := range f.order {
		rule, ok := f.rules[id]
		if !ok {
			continue
		}
		if enabledOnly && !rule.Enabled {
			continue
		}
		out = append(out, rule)
	}
	return out, nil
}

func alertRuleFromFakeInput(id string, in *store.AlertRuleInput, created, updated time.Time) store.AlertRule {
	orEmpty := func(raw json.RawMessage) json.RawMessage {
		if len(raw) == 0 {
			return json.RawMessage(`{}`)
		}
		return raw
	}
	return store.AlertRule{
		ID: id, Name: in.Name, Kind: in.Kind, Params: orEmpty(in.Params),
		Severity: in.Severity, ForNs: in.ForNs,
		Labels: orEmpty(in.Labels), Annotations: orEmpty(in.Annotations),
		Enabled: in.Enabled, RenderedExpr: in.RenderedExpr,
		SyncStatus: store.AlertSyncStatusUnsynced,
		CreatedAt:  created, UpdatedAt: updated,
	}
}

func (f *fakeAlertRuleStore) seed(name string, enabled bool) store.AlertRule {
	rule, err := f.CreateAlertRule(context.Background(), store.AlertRuleInput{
		Name: name, Kind: "raw", Params: json.RawMessage(`{"expr":"up == 0"}`),
		Severity: "warning", ForNs: int64(5 * time.Minute), Enabled: enabled,
		RenderedExpr: "up == 0",
	})
	if err != nil {
		panic(err)
	}
	return rule
}

// ---------------------------------------------------------------------------
// Server helpers
// ---------------------------------------------------------------------------

// linkedTargetService is fakeTargetService plus the one thing the two separate fakes do not share
// and one *store.DB does.
type linkedTargetService struct {
	*fakeTargetService
	checks *fakeChecksStore
}

func (l *linkedTargetService) CreateTarget(ctx context.Context, in store.TargetInput) (store.Target, error) { //nolint:gocritic // hugeParam: mirrors the store signature
	row, err := l.fakeTargetService.CreateTarget(ctx, in)
	if err != nil {
		return row, err
	}
	l.checks.mu.Lock()
	l.checks.targets[row.ID] = true
	l.checks.mu.Unlock()
	return row, nil
}

// exportFixture is every seam the two routes read, so a test can reach into
// the fakes after a request and assert what was (or was not) written.
type exportFixture struct {
	server      *Server
	targets     *linkedTargetService
	checks      *fakeChecksStore
	alertRules  *fakeAlertRuleStore
	webhooks    *fakeWebhookStore
	maintenance *fakeMaintenanceStore
	rbac        *fakeRoleAdmin
}

// newExportServer wires a Server at the given BUILT-IN role with every config seam behind a fake;
// the role is real (authz.NewPolicy(nil)), so these tests prove settings:write actually lands where
// roles.go put.
func newExportServer(t *testing.T, role string) exportFixture {
	t.Helper()
	checks := newFakeChecksStore()
	fx := exportFixture{
		targets:     &linkedTargetService{fakeTargetService: newFakeTargetService(), checks: checks},
		checks:      checks,
		alertRules:  newFakeAlertRuleStore(),
		webhooks:    newFakeWebhookStore(),
		maintenance: newFakeMaintenanceStore(),
		rbac:        newFakeRoleAdmin(),
	}
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u1"}}
	fx.server = newAuthzServer(t, authr, authz.NewPolicy(nil), Deps{
		Roles:       fakeRoleResolver{roles: []string{role}},
		Targets:     fx.targets,
		Definitions: fx.checks,
		Schedules:   fx.checks,
		AlertRules:  fx.alertRules,
		Webhooks:    fx.webhooks,
		Maintenance: fx.maintenance,
		RBAC:        fx.rbac,
	})
	return fx
}

func doExport(t *testing.T, fx *exportFixture) exportBundle {
	t.Helper()
	w := doRequest(t, fx.server, http.MethodGet, "/api/v1/export", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/export = %d, want 200: %s", w.Code, w.Body)
	}
	var bundle exportBundle
	if err := json.Unmarshal(w.Body.Bytes(), &bundle); err != nil {
		t.Fatalf("decode export bundle: %v (%s)", err, w.Body)
	}
	return bundle
}

func doImport(t *testing.T, fx *exportFixture, req importRequest) (int, importResponse) { //nolint:gocritic // hugeParam: test helper
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal import request: %v", err)
	}
	w := doRequest(t, fx.server, http.MethodPost, "/api/v1/import", strings.NewReader(string(raw)), mutateWithCSRF)
	var out importResponse
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode import response: %v (%s)", err, w.Body)
		}
	}
	return w.Code, out
}

// seedFullConfig fills every collection with one interlinked row set: a target, a definition
// POINTING AT IT.
func seedFullConfig(t *testing.T, fx *exportFixture) {
	t.Helper()
	ctx := context.Background()

	tgt, err := fx.targets.CreateTarget(ctx, store.TargetInput{
		Name: "edge-lb", Kind: "host", Address: "10.0.0.7",
		Labels: json.RawMessage(`{"tier":"edge"}`),
	})
	if err != nil {
		t.Fatalf("seed target: %v", err)
	}

	def, err := fx.checks.CreateDefinition(ctx, store.DefinitionInput{
		Name: "edge-tcp", SourceSelection: "one-per-zone", DestinationKind: "target",
		DestinationTargetID: tgt.ID, CheckType: "tcp", Plane: "pod", Enabled: true,
	})
	if err != nil {
		t.Fatalf("seed definition: %v", err)
	}

	if _, err := fx.checks.CreateSchedule(ctx, store.ScheduleInput{
		DefinitionID: def.ID, Kind: "interval", IntervalNs: int64(time.Minute), Enabled: true,
	}); err != nil {
		t.Fatalf("seed schedule: %v", err)
	}

	fx.alertRules.seed("EdgePairLoss", true)

	if _, err := fx.webhooks.CreateWebhook(ctx, store.WebhookInput{
		Name: "slack", URL: "https://hooks.example.test/abc",
		Events: []string{store.WebhookEventIncidentCreated}, SecretEnc: []byte("SEALED(x)"), Enabled: true,
	}); err != nil {
		t.Fatalf("seed webhook: %v", err)
	}

	start := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	fx.maintenance.seed("node-a", "switch firmware", start)
}

// ---------------------------------------------------------------------------
// Authorization
// ---------------------------------------------------------------------------

// TestExportImportAreAdminOnly pins the permission decision.
func TestExportImportAreAdminOnly(t *testing.T) {
	for _, role := range []string{"viewer", "operator", "alert-editor"} {
		fx := newExportServer(t, role)
		w := doRequest(t, fx.server, http.MethodGet, "/api/v1/export", nil, nil)
		if w.Code != http.StatusForbidden {
			t.Errorf("GET /api/v1/export as %s = %d, want 403: %s", role, w.Code, w.Body)
		}
		if !strings.Contains(w.Body.String(), string(authz.PermSettingsWrite)) {
			t.Errorf("GET /api/v1/export as %s detail = %s, want it to name %s",
				role, w.Body, authz.PermSettingsWrite)
		}
		w = doRequest(t, fx.server, http.MethodPost, "/api/v1/import",
			strings.NewReader(`{"bundle":{"version":1}}`), mutateWithCSRF)
		if w.Code != http.StatusForbidden {
			t.Errorf("POST /api/v1/import as %s = %d, want 403: %s", role, w.Code, w.Body)
		}
	}
}

func TestExportImportWithoutStoreReturns503(t *testing.T) {
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u1"}}
	s := newAuthzServer(t, authr, authz.NewPolicy(nil), Deps{Roles: fakeRoleResolver{roles: []string{"admin"}}})

	for _, c := range []struct {
		method, body string
		mutate       func(*http.Request)
	}{
		{http.MethodGet, "", nil},
		{http.MethodPost, `{"bundle":{"version":1}}`, mutateWithCSRF},
	} {
		path := "/api/v1/export"
		if c.method == http.MethodPost {
			path = "/api/v1/import"
		}
		w := doRequest(t, s, c.method, path, strings.NewReader(c.body), c.mutate)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s without a store = %d, want 503: %s", c.method, path, w.Code, w.Body)
		}
		if !strings.Contains(w.Body.String(), "console.database.mode") {
			t.Errorf("%s %s 503 detail = %s, want it to name console.database.mode", c.method, path, w.Body)
		}
	}
}

// ---------------------------------------------------------------------------
// Export
// ---------------------------------------------------------------------------

func TestExportCarriesEveryConfigCollection(t *testing.T) {
	fx := newExportServer(t, "admin")
	seedFullConfig(t, &fx)

	bundle := doExport(t, &fx)

	if bundle.Version != exportBundleVersion {
		t.Errorf("version = %d, want %d", bundle.Version, exportBundleVersion)
	}
	if bundle.ExportedAt.IsZero() {
		t.Error("exportedAt is the zero time, want the moment of the export")
	}
	for name, got := range map[string]int{
		"targets":            len(bundle.Targets),
		"checkDefinitions":   len(bundle.CheckDefinitions),
		"checkSchedules":     len(bundle.CheckSchedules),
		"alertRules":         len(bundle.AlertRules),
		"webhooks":           len(bundle.Webhooks),
		"maintenanceWindows": len(bundle.MaintenanceWindows),
	} {
		if got != 1 {
			t.Errorf("bundle.%s has %d rows, want 1", name, got)
		}
	}
}

// The sealed bytes the store holds are recognisable ("SEALED(...)"), so this asserts on the RAW
// response body.
func TestExportNeverCarriesASecret(t *testing.T) {
	fx := newExportServer(t, "admin")
	seedFullConfig(t, &fx)

	w := doRequest(t, fx.server, http.MethodGet, "/api/v1/export", nil, nil)
	body := w.Body.String()
	for _, banned := range []string{"SEALED(", "secretEnc", "secret_enc", `"secret"`} {
		if strings.Contains(body, banned) {
			t.Errorf("export body carries %q -- a bundle must never contain secret material: %s", banned, body)
		}
	}
	if len(fx.webhooks.hooks) != 1 {
		t.Fatalf("fixture has %d webhooks, want 1", len(fx.webhooks.hooks))
	}
	bundle := doExport(t, &fx)
	if !bundle.Webhooks[0].HasSecret {
		t.Error("webhook hasSecret = false, want true: the stored row has one")
	}
}

func TestExportOmitsObservationFields(t *testing.T) {
	fx := newExportServer(t, "admin")
	seedFullConfig(t, &fx)

	w := doRequest(t, fx.server, http.MethodGet, "/api/v1/export", nil, nil)
	body := w.Body.String()
	for _, banned := range []string{
		"lastFiredAt", "nextFireAt", // scheduler bookkeeping
		"syncStatus", "syncMessage", "lastSyncedAt", // reconciler outcome
		"lastStatus", "lastAttempt", "failures", // webhook delivery outcome
	} {
		if strings.Contains(body, banned) {
			t.Errorf("export body carries observation field %q: %s", banned, body)
		}
	}
}

// ---------------------------------------------------------------------------
// Import: version gate and malformed bodies
// ---------------------------------------------------------------------------

func TestImportRejectsUnknownBundleVersion(t *testing.T) {
	fx := newExportServer(t, "admin")
	w := doRequest(t, fx.server, http.MethodPost, "/api/v1/import",
		strings.NewReader(`{"bundle":{"version":2}}`), mutateWithCSRF)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("version 2 = %d, want 422: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "2") || !strings.Contains(w.Body.String(), "version") {
		t.Errorf("422 detail = %s, want it to NAME the unsupported version", w.Body)
	}
}

func TestImportRejectsMalformedAndBundlelessBodies(t *testing.T) {
	fx := newExportServer(t, "admin")

	w := doRequest(t, fx.server, http.MethodPost, "/api/v1/import", strings.NewReader(`not json`), mutateWithCSRF)
	if w.Code != http.StatusBadRequest {
		t.Errorf("unparseable body = %d, want 400: %s", w.Code, w.Body)
	}

	w = doRequest(t, fx.server, http.MethodPost, "/api/v1/import", strings.NewReader(`{"dryRun":true}`), mutateWithCSRF)
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("body with no bundle = %d, want 422: %s", w.Code, w.Body)
	}
}

// ---------------------------------------------------------------------------
// Import: the remap
// ---------------------------------------------------------------------------

// The store mints its own UUIDs (store.CreateTarget: "the id is minted here"); every
// bundle-internal reference must therefore be re-pointed at the id the store actually assigned.
func TestImportIntoFreshConsoleRemapsEveryReference(t *testing.T) {
	source := newExportServer(t, "admin")
	seedFullConfig(t, &source)
	bundle := doExport(t, &source)

	fresh := newExportServer(t, "admin")
	code, res := doImport(t, &fresh, importRequest{Bundle: &bundle})
	if code != http.StatusOK {
		t.Fatalf("import = %d, want 200", code)
	}
	for name, got := range map[string]importCollectionResult{
		"targets":            res.Targets,
		"checkDefinitions":   res.CheckDefinitions,
		"checkSchedules":     res.CheckSchedules,
		"alertRules":         res.AlertRules,
		"maintenanceWindows": res.MaintenanceWindows,
	} {
		if got.Created != 1 || got.Updated != 0 || got.Skipped != 0 || len(got.Errors) != 0 {
			t.Errorf("%s = %+v, want exactly one create and no errors", name, got)
		}
	}

	// The remap itself: the imported definition must point at the id THIS
	// store minted for the imported target, never at the bundle's.
	imported := doExport(t, &fresh)
	if len(imported.Targets) != 1 || len(imported.CheckDefinitions) != 1 || len(imported.CheckSchedules) != 1 {
		t.Fatalf("re-export shape = %d targets / %d defs / %d scheds, want 1/1/1",
			len(imported.Targets), len(imported.CheckDefinitions), len(imported.CheckSchedules))
	}
	newTargetID := imported.Targets[0].ID
	if newTargetID == bundle.Targets[0].ID {
		t.Fatal("imported target kept the bundle's uuid -- the fixture cannot prove a remap happened")
	}
	if got := imported.CheckDefinitions[0].DestinationTargetID; got != newTargetID {
		t.Errorf("definition destinationTargetId = %q, want the newly minted %q (bundle had %q)",
			got, newTargetID, bundle.CheckDefinitions[0].DestinationTargetID)
	}
	if got := imported.CheckSchedules[0].DefinitionID; got != imported.CheckDefinitions[0].ID {
		t.Errorf("schedule definitionId = %q, want the newly minted %q",
			got, imported.CheckDefinitions[0].ID)
	}
}

// TestImportIsIdempotent re-imports the same bundle into the console it came
// from: everything already exists under its natural key, so every collection
// updates in place and nothing is created twice.
func TestImportIsIdempotent(t *testing.T) {
	fx := newExportServer(t, "admin")
	seedFullConfig(t, &fx)
	bundle := doExport(t, &fx)

	before := doExport(t, &fx)
	code, res := doImport(t, &fx, importRequest{Bundle: &bundle})
	if code != http.StatusOK {
		t.Fatalf("re-import = %d, want 200", code)
	}
	for name, got := range map[string]importCollectionResult{
		"targets":          res.Targets,
		"checkDefinitions": res.CheckDefinitions,
		"checkSchedules":   res.CheckSchedules,
		"alertRules":       res.AlertRules,
		"webhooks":         res.Webhooks,
	} {
		if got.Created != 0 || got.Updated != 1 || len(got.Errors) != 0 {
			t.Errorf("%s = %+v, want exactly one update and no errors", name, got)
		}
	}
	// A maintenance window has no UPDATE in the store by design, so an identical one is SKIPPED, never
	// rewritten.
	if res.MaintenanceWindows.Skipped != 1 || res.MaintenanceWindows.Created != 0 {
		t.Errorf("maintenanceWindows = %+v, want exactly one skip", res.MaintenanceWindows)
	}

	after := doExport(t, &fx)
	for name, pair := range map[string][2]int{
		"targets":            {len(before.Targets), len(after.Targets)},
		"checkDefinitions":   {len(before.CheckDefinitions), len(after.CheckDefinitions)},
		"checkSchedules":     {len(before.CheckSchedules), len(after.CheckSchedules)},
		"alertRules":         {len(before.AlertRules), len(after.AlertRules)},
		"webhooks":           {len(before.Webhooks), len(after.Webhooks)},
		"maintenanceWindows": {len(before.MaintenanceWindows), len(after.MaintenanceWindows)},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s went from %d rows to %d across a re-import -- not idempotent", name, pair[0], pair[1])
		}
	}
	// Row identity survives too: an update keeps the id, a duplicate create
	// would not.
	if before.Targets[0].ID != after.Targets[0].ID {
		t.Error("target id changed across a re-import -- the row was recreated, not updated")
	}
}

// TestImportCrossReferencesPreExistingRowsByName is the third remap case: the bundle's target is
// NOT new here.
func TestImportCrossReferencesPreExistingRowsByName(t *testing.T) {
	source := newExportServer(t, "admin")
	seedFullConfig(t, &source)
	bundle := doExport(t, &source)

	dest := newExportServer(t, "admin")
	preExisting, err := dest.targets.CreateTarget(context.Background(), store.TargetInput{
		Name: "edge-lb", Kind: "host", Address: "10.9.9.9",
	})
	if err != nil {
		t.Fatalf("seed pre-existing target: %v", err)
	}
	if preExisting.ID == bundle.Targets[0].ID {
		t.Fatal("pre-existing target got the bundle's uuid -- the fixture cannot prove anything")
	}

	code, res := doImport(t, &dest, importRequest{Bundle: &bundle})
	if code != http.StatusOK {
		t.Fatalf("import = %d, want 200", code)
	}
	if res.Targets.Updated != 1 || res.Targets.Created != 0 {
		t.Errorf("targets = %+v, want the pre-existing row UPDATED", res.Targets)
	}

	imported := doExport(t, &dest)
	if got := imported.CheckDefinitions[0].DestinationTargetID; got != preExisting.ID {
		t.Errorf("definition destinationTargetId = %q, want the PRE-EXISTING %q", got, preExisting.ID)
	}
	if got := imported.Targets[0].Address; got != "10.0.0.7" {
		t.Errorf("pre-existing target address = %q, want the bundle's 10.0.0.7", got)
	}
}

// TestImportDanglingReferenceIsAPerItemErrorNamingBoth: a reference that is
// neither in the bundle nor in this console fails THAT ITEM and names both
// sides, while every other item in the same bundle still imports.
func TestImportDanglingReferenceIsAPerItemErrorNamingBoth(t *testing.T) {
	fx := newExportServer(t, "admin")
	missingTarget := uuid.NewString()
	missingDef := uuid.NewString()

	bundle := exportBundle{
		Version: exportBundleVersion,
		Targets: []targetResponse{{ID: uuid.NewString(), Name: "keeper", Kind: "host", Address: "10.0.0.1"}},
		CheckDefinitions: []definitionResponse{{
			ID: uuid.NewString(), Name: "orphan", SourceSelection: "all",
			DestinationKind: "target", DestinationTargetID: missingTarget,
			CheckType: "tcp", Plane: "pod",
		}},
		CheckSchedules: []exportSchedule{{
			ID: uuid.NewString(), DefinitionID: missingDef, Kind: "interval",
			IntervalNs: int64(time.Minute), Enabled: true,
		}},
	}

	code, res := doImport(t, &fx, importRequest{Bundle: &bundle})
	if code != http.StatusOK {
		t.Fatalf("import = %d, want 200 -- an item error never fails the request", code)
	}
	if res.Targets.Created != 1 {
		t.Errorf("targets = %+v, want the unrelated target created anyway", res.Targets)
	}
	if len(res.CheckDefinitions.Errors) != 1 {
		t.Fatalf("checkDefinitions errors = %+v, want exactly one", res.CheckDefinitions.Errors)
	}
	defErr := res.CheckDefinitions.Errors[0]
	if defErr.Name != "orphan" || !strings.Contains(defErr.Reason, missingTarget) {
		t.Errorf("definition error = %+v, want it to name both %q and %q", defErr, "orphan", missingTarget)
	}
	if len(res.CheckSchedules.Errors) != 1 {
		t.Fatalf("checkSchedules errors = %+v, want exactly one", res.CheckSchedules.Errors)
	}
	schedErr := res.CheckSchedules.Errors[0]
	if !strings.Contains(schedErr.Reason, missingDef) {
		t.Errorf("schedule error = %+v, want it to name the missing definition %q", schedErr, missingDef)
	}
	if schedErr.Name == "" {
		t.Error("schedule error has no name -- every error must name its item")
	}
}

// TestImportItemErrorNeverAbortsTheRest: an invalid item is reported and SKIPPED PAST.
func TestImportItemErrorNeverAbortsTheRest(t *testing.T) {
	fx := newExportServer(t, "admin")
	bundle := exportBundle{
		Version: exportBundleVersion,
		Targets: []targetResponse{
			{ID: uuid.NewString(), Name: "first", Kind: "host", Address: "10.0.0.1"},
			{ID: uuid.NewString(), Name: "bad name!", Kind: "host", Address: "10.0.0.2"},
			{ID: uuid.NewString(), Name: "third", Kind: "nonsense", Address: "10.0.0.3"},
			{ID: uuid.NewString(), Name: "fourth", Kind: "host", Address: "10.0.0.4"},
		},
	}
	code, res := doImport(t, &fx, importRequest{Bundle: &bundle})
	if code != http.StatusOK {
		t.Fatalf("import = %d, want 200", code)
	}
	if res.Targets.Created != 2 {
		t.Errorf("targets created = %d, want 2 (the two valid rows)", res.Targets.Created)
	}
	if len(res.Targets.Errors) != 2 {
		t.Fatalf("targets errors = %+v, want 2", res.Targets.Errors)
	}
	for _, e := range res.Targets.Errors {
		if e.Name == "" || e.Reason == "" {
			t.Errorf("error %+v must carry both a name and a reason", e)
		}
	}
	after := doExport(t, &fx)
	if len(after.Targets) != 2 {
		t.Errorf("store holds %d targets, want the 2 valid ones", len(after.Targets))
	}
}

// TestImportAmbiguousScheduleIsAnError: check_schedules has no name column and no unique
// constraint.
func TestImportAmbiguousScheduleIsAnError(t *testing.T) {
	fx := newExportServer(t, "admin")
	ctx := context.Background()
	def, err := fx.checks.CreateDefinition(ctx, store.DefinitionInput{
		Name: "edge-tcp", SourceSelection: "all", DestinationKind: "adhoc",
		DestinationAddress: "8.8.8.8:53", CheckType: "tcp", Plane: "pod",
	})
	if err != nil {
		t.Fatalf("seed definition: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := fx.checks.CreateSchedule(ctx, store.ScheduleInput{
			DefinitionID: def.ID, Kind: "interval", IntervalNs: int64(time.Duration(i+1) * time.Minute),
		}); err != nil {
			t.Fatalf("seed schedule %d: %v", i, err)
		}
	}

	bundle := doExport(t, &fx)
	code, res := doImport(t, &fx, importRequest{Bundle: &bundle})
	if code != http.StatusOK {
		t.Fatalf("import = %d, want 200", code)
	}
	if len(res.CheckSchedules.Errors) != 2 {
		t.Fatalf("checkSchedules errors = %+v, want 2 ambiguity errors", res.CheckSchedules.Errors)
	}
	for _, e := range res.CheckSchedules.Errors {
		if !strings.Contains(e.Reason, "edge-tcp") || !strings.Contains(e.Reason, "interval") {
			t.Errorf("ambiguity reason = %q, want it to name the definition and the kind", e.Reason)
		}
	}
}

// It is skipped, with a warning naming the remedy -- never an error the operator could fix by
// editing the bundle.
func TestImportWebhookWithoutASecretIsSkippedWithAWarning(t *testing.T) {
	fx := newExportServer(t, "admin")
	bundle := exportBundle{
		Version: exportBundleVersion,
		Webhooks: []exportWebhook{{
			ID: uuid.NewString(), Name: "slack", URL: "https://hooks.example.test/abc",
			Events: []string{store.WebhookEventIncidentCreated}, Enabled: true, HasSecret: true,
		}},
	}
	code, res := doImport(t, &fx, importRequest{Bundle: &bundle})
	if code != http.StatusOK {
		t.Fatalf("import = %d, want 200", code)
	}
	if res.Webhooks.Created != 0 || res.Webhooks.Skipped != 1 || len(res.Webhooks.Errors) != 0 {
		t.Errorf("webhooks = %+v, want one SKIP and no errors", res.Webhooks)
	}
	if len(res.Webhooks.Warnings) != 1 {
		t.Fatalf("webhooks warnings = %+v, want exactly one", res.Webhooks.Warnings)
	}
	warn := res.Webhooks.Warnings[0]
	if warn.Name != "slack" || !strings.Contains(warn.Reason, "secret") {
		t.Errorf("warning = %+v, want it to name the endpoint and the missing secret", warn)
	}
	if len(fx.webhooks.hooks) != 0 {
		t.Errorf("store holds %d webhooks, want 0 -- nothing may be created without a secret", len(fx.webhooks.hooks))
	}
}

// TestImportWebhookUpdateKeepsTheStoredSecret: the one webhook path that DOES write; the bundle
// carries no secret, so the update must read the stored ciphertext back and pass it through
// unchanged.
func TestImportWebhookUpdateKeepsTheStoredSecret(t *testing.T) {
	fx := newExportServer(t, "admin")
	ctx := context.Background()
	seeded, err := fx.webhooks.CreateWebhook(ctx, store.WebhookInput{
		Name: "slack", URL: "https://hooks.example.test/OLD",
		Events: []string{store.WebhookEventIncidentCreated}, SecretEnc: []byte("SEALED(keep-me)"), Enabled: true,
	})
	if err != nil {
		t.Fatalf("seed webhook: %v", err)
	}

	bundle := exportBundle{
		Version: exportBundleVersion,
		Webhooks: []exportWebhook{{
			ID: uuid.NewString(), Name: "slack", URL: "https://hooks.example.test/NEW",
			Events: []string{store.WebhookEventIncidentResolved}, Enabled: false, HasSecret: true,
		}},
	}
	code, res := doImport(t, &fx, importRequest{Bundle: &bundle})
	if code != http.StatusOK || res.Webhooks.Updated != 1 {
		t.Fatalf("import = %d, webhooks = %+v, want 200 and one update", code, res.Webhooks)
	}
	got, err := fx.webhooks.GetWebhook(ctx, seeded.ID)
	if err != nil {
		t.Fatalf("get webhook: %v", err)
	}
	if string(got.SecretEnc) != "SEALED(keep-me)" {
		t.Errorf("secretEnc = %q, want the stored ciphertext carried through untouched", got.SecretEnc)
	}
	if got.URL != "https://hooks.example.test/NEW" || got.Enabled {
		t.Errorf("webhook = %+v, want the bundle's url and enabled=false applied", got)
	}
}

// TestImportAlertRuleMergesCaseInsensitively pins the natural key: alert_rules is unique on
// lower(name) (migration 00007).
func TestImportAlertRuleMergesCaseInsensitively(t *testing.T) {
	fx := newExportServer(t, "admin")
	stored := fx.alertRules.seed("edgepairloss", true)

	bundle := exportBundle{
		Version: exportBundleVersion,
		AlertRules: []exportAlertRule{{
			ID: uuid.NewString(), Name: "EdgePairLoss", Kind: "raw",
			Params: json.RawMessage(`{"expr":"up == 1"}`), Severity: "critical",
			ForNs: int64(time.Minute), Enabled: false, RenderedExpr: "up == 1",
		}},
	}
	code, res := doImport(t, &fx, importRequest{Bundle: &bundle})
	if code != http.StatusOK {
		t.Fatalf("import = %d, want 200", code)
	}
	if res.AlertRules.Updated != 1 || res.AlertRules.Created != 0 {
		t.Errorf("alertRules = %+v, want one case-insensitive UPDATE", res.AlertRules)
	}
	got, err := fx.alertRules.GetAlertRule(context.Background(), stored.ID)
	if err != nil {
		t.Fatalf("get alert rule: %v", err)
	}
	if got.Severity != "critical" || got.Enabled {
		t.Errorf("rule = %+v, want the bundle's severity and enabled applied", got)
	}
}

// ---------------------------------------------------------------------------
// Dry run
// ---------------------------------------------------------------------------

// TestImportDryRunWritesNothing is the zero-write proof: the SAME bundle is run twice against the
// same fixture.
func TestImportDryRunWritesNothing(t *testing.T) {
	source := newExportServer(t, "admin")
	seedFullConfig(t, &source)
	bundle := doExport(t, &source)

	fresh := newExportServer(t, "admin")
	before := doExport(t, &fresh)

	code, dry := doImport(t, &fresh, importRequest{DryRun: true, Bundle: &bundle})
	if code != http.StatusOK {
		t.Fatalf("dry-run import = %d, want 200", code)
	}
	if !dry.DryRun {
		t.Error("response dryRun = false, want it echoed back true")
	}

	after := doExport(t, &fresh)
	// exportedAt is the one field that legitimately differs between two
	// exports of an unchanged store: it is the moment of the export, not part
	// of the configuration.
	before.ExportedAt, after.ExportedAt = time.Time{}, time.Time{}
	beforeJSON, _ := json.Marshal(before) //nolint:errchkjson // marshalling a struct this package defined
	afterJSON, _ := json.Marshal(after)   //nolint:errchkjson // marshalling a struct this package defined
	if string(beforeJSON) != string(afterJSON) {
		t.Fatalf("a dry run changed the store:\nbefore %s\nafter  %s", beforeJSON, afterJSON)
	}
	if n := len(fresh.targets.byID) + len(fresh.checks.defs) + len(fresh.checks.scheds) +
		len(fresh.alertRules.rules) + len(fresh.webhooks.hooks) + len(fresh.maintenance.windows); n != 0 {
		t.Fatalf("a dry run wrote %d rows across the fakes, want 0", n)
	}

	code, wet := doImport(t, &fresh, importRequest{Bundle: &bundle})
	if code != http.StatusOK {
		t.Fatalf("apply import = %d, want 200", code)
	}
	if wet.DryRun {
		t.Error("apply response dryRun = true, want false")
	}
	dry.DryRun = wet.DryRun
	dryJSON, _ := json.Marshal(dry) //nolint:errchkjson // marshalling a struct this package defined
	wetJSON, _ := json.Marshal(wet) //nolint:errchkjson // marshalling a struct this package defined
	if string(dryJSON) != string(wetJSON) {
		t.Errorf("dry run predicted a different outcome than the apply:\ndry %s\nwet %s", dryJSON, wetJSON)
	}
}

// TestImportDryRunResolvesBundleInternalReferences: a dry run must not report
// a bundle's own definition as a dangling reference merely because the target
// it points at has not been created yet.
func TestImportDryRunResolvesBundleInternalReferences(t *testing.T) {
	source := newExportServer(t, "admin")
	seedFullConfig(t, &source)
	bundle := doExport(t, &source)

	fresh := newExportServer(t, "admin")
	code, res := doImport(t, &fresh, importRequest{DryRun: true, Bundle: &bundle})
	if code != http.StatusOK {
		t.Fatalf("dry-run import = %d, want 200", code)
	}
	if len(res.CheckDefinitions.Errors) != 0 || res.CheckDefinitions.Created != 1 {
		t.Errorf("checkDefinitions = %+v, want one would-be create and no errors", res.CheckDefinitions)
	}
	if len(res.CheckSchedules.Errors) != 0 || res.CheckSchedules.Created != 1 {
		t.Errorf("checkSchedules = %+v, want one would-be create and no errors", res.CheckSchedules)
	}
}

// ---------------------------------------------------------------------------
// Audit
// ---------------------------------------------------------------------------

func TestImportWritesOneAuditRowWithCountsAndNoItemNames(t *testing.T) {
	audit := &fakeAuditStore{}
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u1"}}
	checks := newFakeChecksStore()
	s := newAuthzServer(t, authr, authz.NewPolicy(nil), Deps{
		Roles:       fakeRoleResolver{roles: []string{"admin"}},
		Audit:       audit,
		Targets:     newFakeTargetService(),
		Definitions: checks,
		Schedules:   checks,
		AlertRules:  newFakeAlertRuleStore(),
		Webhooks:    newFakeWebhookStore(),
		Maintenance: newFakeMaintenanceStore(),
	})

	body := `{"dryRun":false,"bundle":{"version":1,"targets":[` +
		`{"id":"` + uuid.NewString() + `","name":"secret-sounding-name","kind":"host","address":"10.1.2.3"}]}}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/import", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("import = %d, want 200: %s", w.Code, w.Body)
	}

	entries := waitForOneAuditEntry(t, audit)
	if len(entries) != 1 {
		t.Fatalf("audit rows = %d, want exactly 1", len(entries))
	}
	detail := string(entries[0].Detail)
	for _, banned := range []string{"secret-sounding-name", "10.1.2.3"} {
		if strings.Contains(detail, banned) {
			t.Errorf("audit detail carries item data %q: %s", banned, detail)
		}
	}
	var got map[string]any
	if err := json.Unmarshal(entries[0].Detail, &got); err != nil {
		t.Fatalf("audit detail is not a JSON object: %v (%s)", err, detail)
	}
	if _, ok := got["dryRun"]; !ok {
		t.Errorf("audit detail has no dryRun: %s", detail)
	}
	counts, ok := got["targets"].(map[string]any)
	if !ok {
		t.Fatalf("audit detail has no per-collection targets counts: %s", detail)
	}
	if fmt.Sprint(counts["created"]) != "1" {
		t.Errorf("audit detail targets.created = %v, want 1: %s", counts["created"], detail)
	}
}

/* ── the bundle's access-control section ─────────────────────────────────── */

// seedRBAC gives a fixture one custom role and one grant to carry.
func seedRBAC(t *testing.T, fx *exportFixture) {
	t.Helper()
	if _, err := fx.rbac.UpsertRole(context.Background(), "path-reader", []string{string(authz.PermMTRRead)}); err != nil {
		t.Fatalf("seed role: %v", err)
	}
	if _, err := fx.rbac.CreateBinding(context.Background(), "path-reader", "user", "oidc:user-sub-1"); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
}

func TestExportCarriesRolesAndBindingsForAnAdmin(t *testing.T) {
	fx := newExportServer(t, "admin")
	seedRBAC(t, &fx)

	bundle := doExport(t, &fx)
	if bundle.RBAC == nil {
		t.Fatal("bundle carries no rbac section; admin holds rbac:manage and should see it")
	}
	if len(bundle.RBAC.Roles) != 1 || bundle.RBAC.Roles[0].Name != "path-reader" {
		t.Errorf("roles = %+v, want the one custom role", bundle.RBAC.Roles)
	}
	if len(bundle.RBAC.Bindings) != 1 || bundle.RBAC.Bindings[0].SubjectID != "oidc:user-sub-1" {
		t.Errorf("bindings = %+v, want the one grant, namespaced identity intact", bundle.RBAC.Bindings)
	}
	if bundle.RBAC.Bindings[0].CreatedAt.IsZero() {
		t.Error("a grant's createdAt is half of reviewing it and must survive the export")
	}
}

// A BUILT-IN role is not a bundle's to define: Policy.Reload ignores a row named "admin", so
// exporting one would put a line in the file that means nothing where it lands.
func TestExportOmitsBuiltinRolesFromTheBundle(t *testing.T) {
	fx := newExportServer(t, "admin")
	if _, err := fx.rbac.UpsertRole(context.Background(), "admin", []string{string(authz.PermMTRRead)}); err != nil {
		t.Fatalf("seed role: %v", err)
	}

	bundle := doExport(t, &fx)
	if bundle.RBAC == nil {
		t.Fatal("bundle carries no rbac section")
	}
	for _, role := range bundle.RBAC.Roles {
		if role.Name == "admin" {
			t.Errorf("bundle carries the built-in role %q", role.Name)
		}
	}
}

// The privilege check that makes the section safe to add at all: settings:write is not rbac:manage,
// and a custom role holding only the former must not read the access map through the export route.
func TestExportOmitsRBACForACallerWithoutRBACManage(t *testing.T) {
	checks := newFakeChecksStore()
	fx := exportFixture{
		targets:     &linkedTargetService{fakeTargetService: newFakeTargetService(), checks: checks},
		checks:      checks,
		alertRules:  newFakeAlertRuleStore(),
		webhooks:    newFakeWebhookStore(),
		maintenance: newFakeMaintenanceStore(),
		rbac:        newFakeRoleAdmin(),
	}
	// A custom role with settings:write and NOTHING else — exactly the caller this guard is for.
	policy := authz.NewPolicy(map[string][]authz.Permission{"config-copier": {authz.PermSettingsWrite}})
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u1"}}
	fx.server = newAuthzServer(t, authr, policy, Deps{
		Roles:       fakeRoleResolver{roles: []string{"config-copier"}},
		Targets:     fx.targets,
		Definitions: fx.checks,
		Schedules:   fx.checks,
		AlertRules:  fx.alertRules,
		Webhooks:    fx.webhooks,
		Maintenance: fx.maintenance,
		RBAC:        fx.rbac,
	})
	seedRBAC(t, &fx)

	bundle := doExport(t, &fx)
	if bundle.RBAC != nil {
		t.Errorf("bundle carried rbac to a settings:write-only caller: %+v", bundle.RBAC)
	}
	// The rest of the bundle is still theirs to take.
	if bundle.Version != exportBundleVersion {
		t.Errorf("version = %d, want the bundle to be served all the same", bundle.Version)
	}
}

/*
A bundle may reference a row this console ALREADY has, even when its own section is EMPTY.

The identity mappings that make that work are built by reading the table — a read, needing no
permission — but they lived at the tail of importTargets/importDefinitions, so an empty section (or
one the caller may not write) skipped them and the NEXT section rejected every reference with
"neither in the bundle nor in this console". That sentence was false, and docs/console-api.yaml
promises the opposite in so many words.
*/
func TestImportResolvesReferencesToRowsAlreadyHereWithAnEmptyTargetsSection(t *testing.T) {
	/* The re-import shape: a bundle exported from THIS console, with the targets section removed —
	   which is also what a caller who may not write targets sees, since that section is skipped.
	   The definitions still name target ids this console holds, so every reference is resolvable. */
	srv := newExportServer(t, "admin")
	seedFullConfig(t, &srv)
	bundle := doExport(t, &srv)
	if len(bundle.CheckDefinitions) == 0 || bundle.CheckDefinitions[0].DestinationTargetID == "" {
		t.Fatal("fixture carries no definition with a target reference; the test cannot prove anything")
	}
	wantTargetID := bundle.CheckDefinitions[0].DestinationTargetID

	bundle.Targets = nil

	code, res := doImport(t, &srv, importRequest{Bundle: &bundle})
	if code != http.StatusOK {
		t.Fatalf("import = %d, want 200", code)
	}
	for _, e := range res.CheckDefinitions.Errors {
		t.Errorf("definition %q rejected: %s", e.Name, e.Reason)
	}
	if res.CheckDefinitions.Updated == 0 && res.CheckDefinitions.Created == 0 {
		t.Fatal("no definition was applied: a reference to a target this console already holds was " +
			"reported as existing nowhere")
	}

	after := doExport(t, &srv)
	if got := after.CheckDefinitions[0].DestinationTargetID; got != wantTargetID {
		t.Errorf("definition destinationTargetId = %q, want the unchanged %q", got, wantTargetID)
	}
}

/*
And the same for SCHEDULES, whose references are check definitions rather than targets: a bundle
whose checkDefinitions section is empty still names definitions this console holds.
*/
func TestImportResolvesScheduleReferencesWithAnEmptyDefinitionsSection(t *testing.T) {
	srv := newExportServer(t, "admin")
	seedFullConfig(t, &srv)
	bundle := doExport(t, &srv)
	if len(bundle.CheckSchedules) == 0 {
		t.Fatal("fixture carries no schedule; the test cannot prove anything")
	}

	bundle.CheckDefinitions = nil

	code, res := doImport(t, &srv, importRequest{Bundle: &bundle})
	if code != http.StatusOK {
		t.Fatalf("import = %d, want 200", code)
	}
	for _, e := range res.CheckSchedules.Errors {
		t.Errorf("schedule %q rejected: %s", e.Name, e.Reason)
	}
}

func TestImportAppliesCustomRolesAndNeverBindings(t *testing.T) {
	src := newExportServer(t, "admin")
	seedRBAC(t, &src)
	bundle := doExport(t, &src)

	dst := newExportServer(t, "admin")
	code, res := doImport(t, &dst, importRequest{Bundle: &bundle})
	if code != http.StatusOK {
		t.Fatalf("import = %d, want 200", code)
	}
	if res.RBACRoles.Created != 1 {
		t.Errorf("rbacRoles = %+v, want one created role", res.RBACRoles)
	}
	roles, err := dst.rbac.ListRoles(context.Background())
	if err != nil {
		t.Fatalf("list roles: %v", err)
	}
	if len(roles) != 1 || roles[0].Name != "path-reader" {
		t.Errorf("destination roles = %+v, want the imported one", roles)
	}

	// The binding is reported, explained, and NOT applied. Replaying a grant would hand a role to
	// whatever "oidc:user-sub-1" happens to mean on this console.
	if res.RBACBindings.Skipped != 1 || res.RBACBindings.Created != 0 {
		t.Errorf("rbacBindings = %+v, want the grant skipped rather than replayed", res.RBACBindings)
	}
	if len(res.RBACBindings.Warnings) != 1 || !strings.Contains(res.RBACBindings.Warnings[0].Reason, "not imported by design") {
		t.Errorf("warnings = %+v, want the skip explained", res.RBACBindings.Warnings)
	}
	bindings, err := dst.rbac.ListBindings(context.Background())
	if err != nil {
		t.Fatalf("list bindings: %v", err)
	}
	if len(bindings) != 0 {
		t.Errorf("destination bindings = %+v, want NONE created by an import", bindings)
	}
}

func TestImportRefusesTheRBACSectionWithoutRBACManage(t *testing.T) {
	src := newExportServer(t, "admin")
	seedRBAC(t, &src)
	bundle := doExport(t, &src)

	checks := newFakeChecksStore()
	dst := exportFixture{
		targets:     &linkedTargetService{fakeTargetService: newFakeTargetService(), checks: checks},
		checks:      checks,
		alertRules:  newFakeAlertRuleStore(),
		webhooks:    newFakeWebhookStore(),
		maintenance: newFakeMaintenanceStore(),
		rbac:        newFakeRoleAdmin(),
	}
	policy := authz.NewPolicy(map[string][]authz.Permission{"config-copier": {authz.PermSettingsWrite}})
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u1"}}
	dst.server = newAuthzServer(t, authr, policy, Deps{
		Roles:       fakeRoleResolver{roles: []string{"config-copier"}},
		Targets:     dst.targets,
		Definitions: dst.checks,
		Schedules:   dst.checks,
		AlertRules:  dst.alertRules,
		Webhooks:    dst.webhooks,
		Maintenance: dst.maintenance,
		RBAC:        dst.rbac,
	})

	code, res := doImport(t, &dst, importRequest{Bundle: &bundle})
	if code != http.StatusOK {
		t.Fatalf("import = %d, want 200 — the rest of the bundle is legitimately theirs", code)
	}
	if res.RBACRoles.Created != 0 || res.RBACRoles.Skipped != 1 {
		t.Errorf("rbacRoles = %+v, want the section skipped, not applied", res.RBACRoles)
	}
	roles, err := dst.rbac.ListRoles(context.Background())
	if err != nil {
		t.Fatalf("list roles: %v", err)
	}
	if len(roles) != 0 {
		// A bundle could otherwise mint a role carrying rbac:manage through a settings:write route.
		t.Errorf("destination roles = %+v, want NONE written by a caller without rbac:manage", roles)
	}
}

func TestImportRejectsARoleCarryingAnUnknownPermission(t *testing.T) {
	fx := newExportServer(t, "admin")
	bundle := doExport(t, &fx)
	bundle.RBAC = &exportRBAC{
		Roles:    []exportRole{{Name: "from-the-future", Permissions: []string{"quantum:entangle"}}},
		Bindings: []exportBinding{},
	}

	code, res := doImport(t, &fx, importRequest{Bundle: &bundle})
	if code != http.StatusOK {
		t.Fatalf("import = %d, want 200", code)
	}
	if len(res.RBACRoles.Errors) != 1 || !strings.Contains(res.RBACRoles.Errors[0].Reason, "unknown permission") {
		t.Errorf("errors = %+v, want the unknown permission named", res.RBACRoles.Errors)
	}
	roles, err := fx.rbac.ListRoles(context.Background())
	if err != nil {
		t.Fatalf("list roles: %v", err)
	}
	if len(roles) != 0 {
		t.Errorf("roles = %+v, want the role refused rather than stored granting a string nothing checks", roles)
	}
}

// An import may MINT A CUSTOM ROLE, including one carrying rbac:manage. Before the two counters were
// allow-listed, that request's audit row read as six all-zero collections — "this import changed
// nothing" — while the access map moved; creating the same role through POST /api/v1/rbac/roles has
// always been audited, so this was the quieter path to it.
func TestImportAuditRowCarriesTheRBACCounts(t *testing.T) {
	audit := &fakeAuditStore{}
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u1"}}
	checks := newFakeChecksStore()
	s := newAuthzServer(t, authr, authz.NewPolicy(nil), Deps{
		Roles:       fakeRoleResolver{roles: []string{"admin"}},
		Audit:       audit,
		Targets:     newFakeTargetService(),
		Definitions: checks,
		Schedules:   checks,
		AlertRules:  newFakeAlertRuleStore(),
		Webhooks:    newFakeWebhookStore(),
		Maintenance: newFakeMaintenanceStore(),
		RBAC:        newFakeRoleAdmin(),
	})

	body := `{"dryRun":false,"bundle":{"version":1,"rbac":{"roles":[{"name":"ops-plus","permissions":["rbac:manage"]}],"bindings":[]}}}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/import", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("import = %d, want 200: %s", w.Code, w.Body)
	}

	entries := waitForOneAuditEntry(t, audit)
	detail := string(entries[0].Detail)
	if !strings.Contains(detail, `"rbacRoles"`) || !strings.Contains(detail, `"created":1`) {
		t.Errorf("audit detail = %s, want the rbacRoles counts", detail)
	}
	// Counts only, the rule every other key here follows: the role's NAME and its permissions stay
	// out of the audit row.
	for _, banned := range []string{"ops-plus", "rbac:manage"} {
		if strings.Contains(detail, banned) {
			t.Errorf("audit detail = %s, must not carry %q", detail, banned)
		}
	}
}

/*
 * A bundle does not get to plant a target the direct route refuses.
 *
 * POST /api/v1/targets answers 422 for an address outside the fleet's allowedCidrs, because no agent
 * could ever probe it and every check against it would time out with no explanation. importTargets
 * went straight to the store, so the identical target inside a bundle was created silently -- and the
 * update branch could re-point an existing endpoint at one.
 */
func TestImportRefusesATargetOutsideTheProbeAllowlist(t *testing.T) {
	fx := newExportServer(t, "admin")
	// The list the Console would have learned from the controller, seeded directly: the cache is
	// what targetOutsideAllowlist reads, and the controller client is a concrete type.
	fx.server.allowlistMu.Lock()
	fx.server.allowlist = prefixList(t, "8.8.8.8/32")
	fx.server.allowlist.fetched = time.Now()
	fx.server.allowlistMu.Unlock()

	body := `{"bundle":{"version":1,"targets":[{"id":"11111111-1111-1111-1111-111111111111",` +
		`"name":"outside","kind":"host","address":"9.9.9.9"}]}}`
	w := doRequest(t, fx.server, http.MethodPost, "/api/v1/import", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("import = %d, want 200 (per-item errors, not a request failure): %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "outside the addresses this fleet") {
		t.Fatalf("import result does not refuse the unreachable target: %s", w.Body)
	}
	if strings.Contains(w.Body.String(), `"created":1`) {
		t.Errorf("the target was created anyway: %s", w.Body)
	}
}

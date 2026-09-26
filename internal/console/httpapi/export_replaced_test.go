package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

/*
A bundled definition used to excuse a target edit merely by being in the bundle. When the import then
refused that definition (here: still http, against a target the same bundle turns into a host), the
target had already been re-pointed and the live definition stayed enabled and unrunnable, the state
PUT /api/v1/targets/{id} refuses with 422. Only a definition the import will actually write excuses
the edit.
*/
func TestImportTargetEditIsNotExcusedByABundledDefinitionTheImportRefuses(t *testing.T) {
	fx := newExportServer(t, "admin")
	target, err := fx.targets.CreateTarget(context.Background(), store.TargetInput{
		Name: "portal", Kind: "url", Address: "https://svc.example.com/health",
	})
	if err != nil {
		t.Fatal(err)
	}
	seedTargetDefinition(t, fx.checks, httpDefinitionOn("portal-http", target.ID, true))

	const bundleTargetID = "11111111-1111-4111-8111-111111111111"
	bundle := &exportBundle{
		Version: exportBundleVersion,
		Targets: []targetResponse{{ID: bundleTargetID, Name: "portal", Kind: "host", Address: "svc.example.com:443"}},
		CheckDefinitions: []definitionResponse{{
			ID: "22222222-2222-4222-8222-222222222222", Name: "portal-http", SourceSelection: "all",
			DestinationKind: "target", DestinationTargetID: bundleTargetID, CheckType: "http", Plane: "pod",
			Params: json.RawMessage(`{}`), Enabled: true,
		}},
	}
	for _, dryRun := range []bool{true, false} {
		code, res := doImport(t, &fx, importRequest{DryRun: dryRun, Bundle: bundle})
		if code != http.StatusOK {
			t.Fatalf("dryRun=%v: import = %d, want 200", dryRun, code)
		}
		if res.Targets.Updated != 0 || len(res.Targets.Errors) != 1 ||
			!strings.Contains(res.Targets.Errors[0].Reason, `"portal-http"`) {
			t.Errorf("dryRun=%v: targets = %+v, want the edit refused naming portal-http", dryRun, res.Targets)
		}
	}
	if got, _ := fx.targets.GetTarget(context.Background(), target.ID); got.Kind != "url" {
		t.Errorf("the target was re-pointed although the definition that excused it was refused: %+v", got)
	}
}

// A bundled rewrite the definitions section refuses under the schedule guard excuses nothing either:
// here mtr cannot run on the definition's continuous schedule, so the live http definition stays.
func TestImportTargetEditIsNotExcusedByADefinitionItsScheduleCannotRun(t *testing.T) {
	fx := newExportServer(t, "admin")
	ctx := context.Background()
	target, err := fx.targets.CreateTarget(ctx, store.TargetInput{
		Name: "portal", Kind: "url", Address: "https://svc.example.com/health",
	})
	if err != nil {
		t.Fatal(err)
	}
	seedTargetDefinition(t, fx.checks, httpDefinitionOn("portal-http", target.ID, true))
	defs, err := fx.checks.ListDefinitions(ctx, store.DefinitionFilter{Limit: 10})
	if err != nil || len(defs.Definitions) != 1 {
		t.Fatalf("seeded definitions = %+v, %v", defs, err)
	}
	if _, err := fx.checks.CreateSchedule(ctx, store.ScheduleInput{
		DefinitionID: defs.Definitions[0].ID, Kind: "continuous", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	const bundleTargetID = "11111111-1111-4111-8111-111111111111"
	bundle := &exportBundle{
		Version: exportBundleVersion,
		Targets: []targetResponse{{ID: bundleTargetID, Name: "portal", Kind: "host", Address: "svc.example.com:443"}},
		CheckDefinitions: []definitionResponse{{
			ID: "22222222-2222-4222-8222-222222222222", Name: "portal-http", SourceSelection: "all",
			DestinationKind: "target", DestinationTargetID: bundleTargetID, CheckType: "mtr", Plane: "pod",
			Params: json.RawMessage(`{}`), Enabled: true,
		}},
	}
	for _, dryRun := range []bool{true, false} {
		code, res := doImport(t, &fx, importRequest{DryRun: dryRun, Bundle: bundle})
		if code != http.StatusOK {
			t.Fatalf("dryRun=%v: import = %d, want 200", dryRun, code)
		}
		if res.Targets.Updated != 0 || len(res.Targets.Errors) != 1 ||
			!strings.Contains(res.Targets.Errors[0].Reason, `"portal-http"`) {
			t.Errorf("dryRun=%v: targets = %+v, want the edit refused naming portal-http", dryRun, res.Targets)
		}
	}
	if got, _ := fx.targets.GetTarget(ctx, target.ID); got.Kind != "url" {
		t.Errorf("the target was re-pointed although the definition that excused it was refused: %+v", got)
	}
}

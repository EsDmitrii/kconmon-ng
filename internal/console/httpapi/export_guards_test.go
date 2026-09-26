package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// dnsDefinition is a runnable dns check toward an ad-hoc address, as a bundle carries it.
func dnsDefinition(name string) definitionResponse {
	return definitionResponse{
		ID: uuid.NewString(), Name: name, SourceSelection: "all", DestinationKind: "adhoc",
		DestinationAddress: "10.0.0.10", CheckType: "dns", Plane: "pod",
		Params: json.RawMessage(`{"query":"api.example.com"}`),
	}
}

// POST /api/v1/schedules refuses a schedule whose kind cannot run its definition's check; a bundle
// created the same schedule, and the reconciler skipped it or every fire failed.
func TestImportRefusesAScheduleItsDefinitionCannotRun(t *testing.T) {
	for _, dryRun := range []bool{true, false} {
		fx := newExportServer(t, "admin")
		def := dnsDefinition("resolve")
		bundle := exportBundle{
			Version:          exportBundleVersion,
			CheckDefinitions: []definitionResponse{def},
			CheckSchedules: []exportSchedule{
				{ID: uuid.NewString(), DefinitionID: def.ID, Kind: "interval", IntervalNs: int64(time.Minute), Enabled: true},
				{ID: uuid.NewString(), DefinitionID: def.ID, Kind: "continuous", Enabled: true},
			},
		}
		code, res := doImport(t, &fx, importRequest{Bundle: &bundle, DryRun: dryRun})
		if code != http.StatusOK {
			t.Fatalf("dryRun=%v: import = %d, want 200", dryRun, code)
		}
		if res.CheckSchedules.Created != 1 || len(res.CheckSchedules.Errors) != 1 ||
			!strings.Contains(res.CheckSchedules.Errors[0].Reason, "as a one-off or repeating run") {
			t.Errorf("dryRun=%v: checkSchedules = %+v, want the continuous one created and the interval one refused", dryRun, res.CheckSchedules)
		}
	}
}

// PUT /api/v1/checks/{id} refuses an edit that leaves one of the definition's schedules unable to run
// it; the same edit arriving in a bundle was applied.
func TestImportRefusesADefinitionEditItsScheduleCannotRun(t *testing.T) {
	for _, dryRun := range []bool{true, false} {
		fx := newExportServer(t, "admin")
		live, err := fx.checks.CreateDefinition(context.Background(), store.DefinitionInput{
			Name: "resolve", SourceSelection: "all", DestinationKind: "adhoc", DestinationAddress: "10.0.0.10:53",
			CheckType: "tcp", Plane: "pod",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fx.checks.CreateSchedule(context.Background(), store.ScheduleInput{
			DefinitionID: live.ID, Kind: "interval", IntervalNs: int64(time.Minute), Enabled: true,
		}); err != nil {
			t.Fatal(err)
		}
		bundle := exportBundle{Version: exportBundleVersion, CheckDefinitions: []definitionResponse{dnsDefinition("resolve")}}
		code, res := doImport(t, &fx, importRequest{Bundle: &bundle, DryRun: dryRun})
		if code != http.StatusOK {
			t.Fatalf("dryRun=%v: import = %d, want 200", dryRun, code)
		}
		if res.CheckDefinitions.Updated != 0 || len(res.CheckDefinitions.Errors) != 1 ||
			!strings.Contains(res.CheckDefinitions.Errors[0].Reason, "interval schedule could not run the edited check") {
			t.Errorf("dryRun=%v: checkDefinitions = %+v, want the edit refused for its interval schedule", dryRun, res.CheckDefinitions)
		}
		if got, _ := fx.checks.GetDefinition(context.Background(), live.ID); got.CheckType != "tcp" {
			t.Errorf("dryRun=%v: live definition is now %s, want it left tcp", dryRun, got.CheckType)
		}
	}
}

// The store refuses a rule whose Prometheus alert name another rule already has; a dry run never asks
// the store, so it reported such a rule, or two in one bundle, as created.
func TestImportDryRunReportsAnAlertNameClash(t *testing.T) {
	fx := newExportServer(t, "admin")
	fx.alertRules.seed("EdgePairLoss", true)
	rule := func(name string) exportAlertRule {
		return exportAlertRule{
			ID: uuid.NewString(), Name: name, Kind: "raw", Params: json.RawMessage(`{"expr":"up == 0"}`),
			Severity: "warning", ForNs: int64(5 * time.Minute), Enabled: true,
		}
	}
	bundle := exportBundle{Version: exportBundleVersion, AlertRules: []exportAlertRule{
		rule("edge-pair-loss"), rule("core.loss"), rule("Core-Loss"), rule("Other"),
	}}
	code, res := doImport(t, &fx, importRequest{Bundle: &bundle, DryRun: true})
	if code != http.StatusOK {
		t.Fatalf("import = %d, want 200", code)
	}
	if res.AlertRules.Created != 2 || len(res.AlertRules.Errors) != 2 {
		t.Fatalf("alertRules = %+v, want two created and two alert-name clashes", res.AlertRules)
	}
	for _, e := range res.AlertRules.Errors {
		if !strings.Contains(e.Reason, "Prometheus alert name") {
			t.Errorf("error %+v, want the clash named", e)
		}
	}
}

// A hand-written bundle may leave rule ids out; two such rules are still two rules to the clash check.
func TestImportDryRunReportsAnAlertNameClashBetweenIDLessRules(t *testing.T) {
	fx := newExportServer(t, "admin")
	rule := func(name string) exportAlertRule {
		return exportAlertRule{
			Name: name, Kind: "raw", Params: json.RawMessage(`{"expr":"up == 0"}`),
			Severity: "warning", ForNs: int64(5 * time.Minute), Enabled: true,
		}
	}
	bundle := exportBundle{Version: exportBundleVersion, AlertRules: []exportAlertRule{rule("core.loss"), rule("Core-Loss")}}
	code, res := doImport(t, &fx, importRequest{Bundle: &bundle, DryRun: true})
	if code != http.StatusOK {
		t.Fatalf("import = %d, want 200", code)
	}
	if res.AlertRules.Created != 1 || len(res.AlertRules.Errors) != 1 ||
		!strings.Contains(res.AlertRules.Errors[0].Reason, "Prometheus alert name") {
		t.Fatalf("alertRules = %+v, want one created and one alert-name clash", res.AlertRules)
	}
}

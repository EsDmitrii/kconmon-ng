package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// The import judges runnability as PUT /api/v1/checks/{id} and PUT /api/v1/schedules/{id} do: an
// update that leaves the row disabled is not judged, a create always is.
func TestImportJudgesADisabledUpdateAsTheRoutesDo(t *testing.T) {
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
	liveDef := defs.Definitions[0]
	if _, err := fx.checks.CreateSchedule(ctx, store.ScheduleInput{
		DefinitionID: liveDef.ID, Kind: "interval", IntervalNs: int64(time.Minute), Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	udpOn := func(id, name string) definitionResponse {
		return definitionResponse{
			ID: id, Name: name, SourceSelection: "all", DestinationKind: "target",
			DestinationTargetID: target.ID, CheckType: "udp", Plane: "pod",
			Params: json.RawMessage(`{}`), Enabled: false,
		}
	}
	runAt := time.Now().Add(time.Hour).UTC()
	bundle := &exportBundle{
		Version: exportBundleVersion,
		CheckDefinitions: []definitionResponse{
			udpOn(liveDef.ID, "portal-http"),
			udpOn("33333333-3333-4333-8333-333333333333", "portal-udp"),
		},
	}
	for _, dryRun := range []bool{true, false} {
		code, res := doImport(t, &fx, importRequest{DryRun: dryRun, Bundle: bundle})
		if code != http.StatusOK {
			t.Fatalf("dryRun=%v: import = %d, want 200", dryRun, code)
		}
		defsRes := res.CheckDefinitions
		if defsRes.Updated != 1 || defsRes.Created != 0 || len(defsRes.Errors) != 1 ||
			defsRes.Errors[0].Name != "portal-udp" || !strings.Contains(defsRes.Errors[0].Reason, "no agent could run") {
			t.Errorf("dryRun=%v: definitions = %+v, want the disabled update applied and the create refused", dryRun, defsRes)
		}
	}

	scheduleBundle := &exportBundle{
		Version: exportBundleVersion,
		CheckSchedules: []exportSchedule{
			{ID: "44444444-4444-4444-8444-444444444444", DefinitionID: liveDef.ID, Kind: "interval",
				IntervalNs: int64(time.Minute), Enabled: false},
			{ID: "55555555-5555-4555-8555-555555555555", DefinitionID: liveDef.ID, Kind: "once",
				RunAt: &runAt, Enabled: false},
		},
	}
	// The definition is back to a runnable http one, and neither schedule kind can run http.
	if _, err := fx.checks.UpdateDefinition(ctx, liveDef.ID, httpDefinitionOn("portal-http", target.ID, true)); err != nil {
		t.Fatal(err)
	}
	for _, dryRun := range []bool{true, false} {
		code, res := doImport(t, &fx, importRequest{DryRun: dryRun, Bundle: scheduleBundle})
		if code != http.StatusOK {
			t.Fatalf("dryRun=%v: import = %d, want 200", dryRun, code)
		}
		sched := res.CheckSchedules
		if sched.Updated != 1 || sched.Created != 0 || len(sched.Errors) != 1 ||
			!strings.HasSuffix(sched.Errors[0].Name, "/once") {
			t.Errorf("dryRun=%v: schedules = %+v, want the disabled update applied and the create refused", dryRun, sched)
		}
	}
}

type unlistableTargets struct {
	*linkedTargetService
}

func (unlistableTargets) ListTargets(context.Context, store.TargetFilter) (store.TargetPage, error) { //nolint:gocritic // hugeParam: mirrors the store signature
	return store.TargetPage{}, errors.New("store down")
}

// Reading what this console already holds runs even for a section the bundle does not carry, and a
// failed read is a 502 like every other one, not a reference reported missing.
func TestImportAnswers502WhenTheExistingRowsCannotBeRead(t *testing.T) {
	fx := newExportServer(t, "admin")
	target, err := fx.targets.CreateTarget(context.Background(), store.TargetInput{
		Name: "portal", Kind: "url", Address: "https://svc.example.com/health",
	})
	if err != nil {
		t.Fatal(err)
	}
	fx.server.targets = unlistableTargets{fx.targets}
	bundle := &exportBundle{
		Version: exportBundleVersion,
		CheckDefinitions: []definitionResponse{{
			ID: "22222222-2222-4222-8222-222222222222", Name: "portal-http", SourceSelection: "all",
			DestinationKind: "target", DestinationTargetID: target.ID, CheckType: "http", Plane: "pod",
			Params: json.RawMessage(`{}`), Enabled: true,
		}},
	}
	for _, dryRun := range []bool{true, false} {
		if code, res := doImport(t, &fx, importRequest{DryRun: dryRun, Bundle: bundle}); code != http.StatusBadGateway {
			t.Errorf("dryRun=%v: import = %d (%+v), want 502", dryRun, code, res.CheckDefinitions)
		}
	}
}

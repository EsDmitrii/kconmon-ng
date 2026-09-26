package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
)

/*
A dry run of the bundle this console just exported used to predict an update for every row, and one
~400-character warning per role binding: the plan said everything would change when nothing would,
and buried the one fact worth reading. Rows that match the bundle are "unchanged", and the binding
warning is said once for the section.
*/
func TestDryRunOfAnIdenticalBundlePredictsNoChange(t *testing.T) {
	fx := newExportServer(t, "admin")
	seedFullConfig(t, &fx)
	ctx := context.Background()
	for _, name := range []string{"ops-a", "ops-b"} {
		if _, err := fx.rbac.UpsertRole(ctx, name, []string{string(authz.PermRunsRead)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"u-1", "u-2", "u-3"} {
		mustBind(t, fx.rbac, "ops-a", id)
	}
	bundle := doExport(t, &fx)
	if bundle.RBAC == nil || len(bundle.RBAC.Roles) != 2 || len(bundle.RBAC.Bindings) < 3 {
		t.Fatalf("fixture rbac = %+v, want two roles and three bindings", bundle.RBAC)
	}

	for _, dryRun := range []bool{true, false} {
		code, res := doImport(t, &fx, importRequest{DryRun: dryRun, Bundle: &bundle})
		if code != http.StatusOK {
			t.Fatalf("dryRun=%v: import = %d, want 200", dryRun, code)
		}
		for name, got := range map[string]importCollectionResult{
			"targets":            res.Targets,
			"checkDefinitions":   res.CheckDefinitions,
			"checkSchedules":     res.CheckSchedules,
			"alertRules":         res.AlertRules,
			"webhooks":           res.Webhooks,
			"maintenanceWindows": res.MaintenanceWindows,
			"rbacRoles":          res.RBACRoles,
		} {
			if got.Created != 0 || got.Updated != 0 || got.Skipped != 0 || got.Unchanged == 0 || len(got.Errors) != 0 {
				t.Errorf("dryRun=%v: %s = %+v, want every row unchanged", dryRun, name, got)
			}
		}
		if res.RBACRoles.Unchanged != 2 {
			t.Errorf("dryRun=%v: rbacRoles.unchanged = %d, want 2", dryRun, res.RBACRoles.Unchanged)
		}
		b := res.RBACBindings
		if b.Skipped != len(bundle.RBAC.Bindings) {
			t.Errorf("dryRun=%v: rbacBindings.skipped = %d, want %d", dryRun, b.Skipped, len(bundle.RBAC.Bindings))
		}
		if len(b.Warnings) != 1 || !strings.Contains(b.Warnings[0].Reason, "not imported by design") {
			t.Errorf("dryRun=%v: rbacBindings.warnings = %+v, want the reason said once", dryRun, b.Warnings)
		}
	}
}

// A window matching one already here in scope and span but not in reason is skipped WITH a reason:
// the store has no update, and a bare "skipped" left the operator guessing.
func TestImportNamesWhyAnExistingMaintenanceWindowIsSkipped(t *testing.T) {
	fx := newExportServer(t, "admin")
	start := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	fx.maintenance.seed("node-a", "switch firmware", start)
	bundle := doExport(t, &fx)
	bundle.MaintenanceWindows[0].Reason = "a different reason"

	code, res := doImport(t, &fx, importRequest{DryRun: true, Bundle: &bundle})
	if code != http.StatusOK {
		t.Fatalf("import = %d, want 200", code)
	}
	got := res.MaintenanceWindows
	if got.Skipped != 1 || len(got.Warnings) != 1 || got.Warnings[0].Reason == "" {
		t.Errorf("maintenanceWindows = %+v, want one skip with its reason", got)
	}
}

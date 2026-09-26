package httpapi

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
)

// newExportServerWithPerms is newExportServer for a custom role holding exactly perms.
func newExportServerWithPerms(t *testing.T, perms ...authz.Permission) exportFixture {
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
	fx.server = newAuthzServer(t, authr, authz.NewPolicy(map[string][]authz.Permission{"custom": perms}), Deps{
		Roles:       fakeRoleResolver{roles: []string{"custom"}},
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

// exportSectionKeys are the bundle's gated sections in bundle order.
var exportSectionKeys = []string{
	"targets", "checkDefinitions", "checkSchedules", "alertRules", "webhooks", "maintenanceWindows", "rbac",
}

// rawExport is GET /api/v1/export decoded only as far as its top-level members.
func rawExport(t *testing.T, fx *exportFixture) map[string]json.RawMessage {
	t.Helper()
	w := doRequest(t, fx.server, http.MethodGet, "/api/v1/export", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/export = %d, want 200: %s", w.Code, w.Body)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	return out
}

/*
The export used to hand a settings:write caller every section, although the section routes refuse
that caller: a role holding settings:write alone got 403 from GET /api/v1/webhooks and then read every
webhook URL, which for an incoming-webhook service is the credential, out of GET /api/v1/export.
*/
func TestExportGivesEachSectionOnlyToACallerWhoCouldReadIt(t *testing.T) {
	cases := []struct {
		perm authz.Permission
		want []string
	}{
		{"", nil},
		{authz.PermTargetsRead, []string{"targets"}},
		{authz.PermChecksRead, []string{"checkDefinitions", "checkSchedules"}},
		{authz.PermAlertsRead, []string{"alertRules"}},
		{authz.PermWebhooksManage, []string{"webhooks"}},
		{authz.PermMaintenanceRead, []string{"maintenanceWindows"}},
		{authz.PermRBACManage, []string{"rbac"}},
	}
	for _, c := range cases {
		name := string(c.perm)
		if name == "" {
			name = "settings:write alone"
		}
		t.Run(name, func(t *testing.T) {
			perms := []authz.Permission{authz.PermSettingsWrite}
			if c.perm != "" {
				perms = append(perms, c.perm)
			}
			fx := newExportServerWithPerms(t, perms...)
			seedFullConfig(t, &fx)
			seedRBAC(t, &fx)

			got := rawExport(t, &fx)
			var omitted []string
			if raw, ok := got["omitted"]; ok {
				if err := json.Unmarshal(raw, &omitted); err != nil {
					t.Fatalf("omitted is not a list of names: %s", raw)
				}
			}
			for _, key := range exportSectionKeys {
				_, present := got[key]
				wantPresent := slices.Contains(c.want, key)
				if present != wantPresent {
					t.Errorf("section %s present = %v, want %v", key, present, wantPresent)
				}
				if slices.Contains(omitted, key) == wantPresent {
					t.Errorf("omitted = %v, want %s listed exactly when it is withheld", omitted, key)
				}
			}
			if !slices.Contains(c.want, "webhooks") && strings.Contains(string(got["webhooks"]), "hooks.example.test") {
				t.Errorf("a caller without webhooks:manage read a webhook URL")
			}
		})
	}
}

// The admin keeps the whole bundle, and a withheld list only appears when something was withheld.
func TestExportForAnAdminHasEverySectionAndNothingOmitted(t *testing.T) {
	fx := newExportServer(t, "admin")
	seedFullConfig(t, &fx)
	seedRBAC(t, &fx)
	got := rawExport(t, &fx)
	for _, key := range exportSectionKeys {
		if _, ok := got[key]; !ok {
			t.Errorf("admin export lacks %s", key)
		}
	}
	if raw, ok := got["omitted"]; ok {
		t.Errorf("admin export lists omitted sections: %s", raw)
	}
}

// An empty console still exports every readable section as [], so absent and empty stay different.
func TestExportKeepsAnEmptyReadableSectionAsAnEmptyList(t *testing.T) {
	fx := newExportServerWithPerms(t, authz.PermSettingsWrite, authz.PermTargetsRead)
	got := rawExport(t, &fx)
	if string(got["targets"]) != "[]" {
		t.Errorf("targets = %s, want []", got["targets"])
	}
	if _, ok := got["webhooks"]; ok {
		t.Errorf("webhooks present for a caller without webhooks:manage: %s", got["webhooks"])
	}
}

// A bundle with withheld sections re-imports: the omitted list is accepted and the absent sections
// change nothing.
func TestAnExportWithOmittedSectionsImportsBack(t *testing.T) {
	fx := newExportServerWithPerms(t, authz.PermSettingsWrite, authz.PermTargetsRead, authz.PermTargetsWrite)
	seedFullConfig(t, &fx)
	w := doRequest(t, fx.server, http.MethodGet, "/api/v1/export", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("export = %d", w.Code)
	}
	body := `{"dryRun":true,"bundle":` + w.Body.String() + `}`
	w = doRequest(t, fx.server, http.MethodPost, "/api/v1/import", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("re-import of a partial export = %d, want 200: %s", w.Code, w.Body)
	}
}

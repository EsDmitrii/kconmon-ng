package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The create route and the import apply one set of role-name rules, so a name one of them refuses
// never reaches the table through the other.
func TestRoleNameRulesAreTheSameForTheRouteAndTheImport(t *testing.T) {
	cases := map[string]string{
		"blank":    "   ",
		"control":  "ops\x07",
		"slash":    "team/ops",
		"too long": strings.Repeat("r", nameMaxLen+1),
	}
	for label, name := range cases {
		t.Run(label, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"name": name, "permissions": []string{}})
			if err != nil {
				t.Fatal(err)
			}
			w := doRequest(t, newRBACTestServer(t, newFakeRoleAdmin()), http.MethodPost, "/api/v1/rbac/roles",
				strings.NewReader(string(body)), mutateWithCSRF)
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("POST /api/v1/rbac/roles = %d %s, want 422", w.Code, w.Body)
			}
			routeDetail := problemDetail(t, w.Body.Bytes())

			fx := newExportServer(t, "admin")
			bundle := &exportBundle{Version: exportBundleVersion, RBAC: &exportRBAC{
				Roles: []exportRole{{Name: name, Permissions: []string{}}},
			}}
			code, res := doImport(t, &fx, importRequest{DryRun: true, Bundle: bundle})
			if code != http.StatusOK || len(res.RBACRoles.Errors) != 1 {
				t.Fatalf("import = %d %+v, want the role refused", code, res.RBACRoles)
			}
			if got := res.RBACRoles.Errors[0].Reason; got != routeDetail {
				t.Errorf("import reason %q differs from the route's detail %q", got, routeDetail)
			}
		})
	}
}

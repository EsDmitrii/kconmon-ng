package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
)

// Refusal details speak to the API client: what to send, not the Go function that sets it up or what
// the schema declares.
func TestRefusalDetailsDescribeTheRequestNotTheServer(t *testing.T) {
	t.Run("csrf", func(t *testing.T) {
		policy := authz.NewPolicy(map[string][]authz.Permission{"tester": {authz.PermPromQLQuery}})
		authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u1"}}
		s := newAuthzServer(t, authr, policy, Deps{Roles: fakeRoleResolver{roles: []string{"tester"}}})
		w := doRequest(t, s, http.MethodPost, "/api/v1/promql/query", strings.NewReader(`{"query":"up"}`), func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: s.cfg.Auth.Session.CookieName, Value: "sess-1"})
			r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "tok-1"})
		})
		detail := problemDetail(t, w.Body.Bytes())
		if w.Code != http.StatusForbidden || strings.Contains(detail, "maybeMintCSRFCookie") ||
			!strings.Contains(detail, csrfHeaderName) {
			t.Errorf("CSRF refusal = %d %q, want 403 naming the %s header and no Go identifier", w.Code, detail, csrfHeaderName)
		}
	})
	t.Run("binding subject kind", func(t *testing.T) {
		s := newRBACTestServer(t, newFakeRoleAdmin())
		w := doRequest(t, s, http.MethodPost, "/api/v1/rbac/bindings",
			strings.NewReader(`{"roleName":"viewer","subjectKind":"token","subjectId":"t1"}`), mutateWithCSRF)
		if detail := problemDetail(t, w.Body.Bytes()); w.Code != http.StatusUnprocessableEntity ||
			detail != `subjectKind must be "user" or "group"` {
			t.Errorf("token binding = %d %q, want 422 naming the accepted kinds", w.Code, detail)
		}
	})
	t.Run("import skips", func(t *testing.T) {
		for _, reason := range []string{sectionNoPermissionReason(authz.PermWebhooksManage), rbacImportNoPermissionReason} {
			if strings.Contains(reason, "holds only") {
				t.Errorf("skip reason %q guesses what else the caller holds", reason)
			}
		}
	})
}

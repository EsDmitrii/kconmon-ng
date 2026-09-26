package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// A body with a second JSON value after the first is refused by every mutation decoder, not applied
// as its first object with the rest dropped.
func TestMutationDecodersRefuseTrailingJSONValues(t *testing.T) {
	const webhookBody = `{"name":"hook","url":"https://example.com/hook","events":["incident.created"]}`
	const scheduleBody = `{"definitionId":"00000000-0000-4000-8000-000000000001","kind":"interval","intervalNs":60000000000}`

	decoders := []struct {
		name   string
		body   string
		decode func(w http.ResponseWriter, r *http.Request) bool
	}{
		{"target", validTargetBody, func(w http.ResponseWriter, r *http.Request) bool {
			_, ok := decodeTargetRequest(w, r)
			return ok
		}},
		{"webhook", webhookBody, func(w http.ResponseWriter, r *http.Request) bool {
			_, ok := decodeWebhookRequest(w, r)
			return ok
		}},
		{"schedule", scheduleBody, func(w http.ResponseWriter, r *http.Request) bool {
			_, ok := decodeScheduleRequest(w, r, "00000000-0000-4000-8000-000000000001")
			return ok
		}},
		{"definition", `{"name":"d","sourceSelection":"all","destinationKind":"node","checkType":"tcp","plane":"pod"}`,
			func(w http.ResponseWriter, r *http.Request) bool {
				_, ok := decodeDefinitionRequest(w, r)
				return ok
			}},
		{"alert rule", validAlertRuleBody, func(w http.ResponseWriter, r *http.Request) bool {
			_, ok := decodeAlertRuleRequest(w, r)
			return ok
		}},
	}
	for _, d := range decoders {
		w := httptest.NewRecorder()
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(d.body+d.body))
		if d.decode(w, r) {
			t.Errorf("%s: a body of two JSON objects was accepted", d.name)
			continue
		}
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "more than one JSON value") {
			t.Errorf("%s: got %d %s, want 400 naming the extra JSON value", d.name, w.Code, w.Body)
		}
	}
}

// The handlers that decode inline refuse a trailing value too, before they act on the first one.
func TestMutationHandlersRefuseTrailingJSONValues(t *testing.T) {
	admin := newFakeUserAdmin(store.User{ID: "u-admin", Username: "root"}, store.User{ID: "u-2", Username: "ops"})
	admin.roles["u-2"] = "viewer"
	users := newUsersServer(t, admin, rolesByID{"u-admin": {"admin"}, "u-2": {"viewer"}})
	tokens := newFakeTokenStore()

	cases := []struct {
		name, method, path, body string
		srv                      *Server
	}{
		{"alert rule import", http.MethodPost, "/api/v1/alert-rules/import", `{"name":"one"}`,
			newM5TestServer(t, "operator", Deps{AlertRules: newFakeAlertRuleStore(), RuleSync: &fakeRuleSyncer{}})},
		{"token", http.MethodPost, "/api/v1/tokens", `{"name":"t"}`, newM5TestServer(t, "admin", Deps{Tokens: tokens})},
		{"user create", http.MethodPost, "/api/v1/users",
			`{"username":"bob","password":"a long enough secret","role":"viewer"}`, users},
		{"user patch", http.MethodPatch, "/api/v1/users/u-2", `{"role":"operator"}`, users},
		{"user password", http.MethodPost, "/api/v1/users/u-2/password", `{"password":"a long enough secret"}`, users},
	}
	for _, c := range cases {
		w := doRequest(t, c.srv, c.method, c.path, strings.NewReader(c.body+c.body), mutateWithCSRF)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "more than one JSON value") {
			t.Errorf("%s: got %d %s, want 400 naming the extra JSON value", c.name, w.Code, w.Body)
		}
	}
	if list, _ := tokens.ListTokens(context.Background()); len(list) != 0 {
		t.Errorf("a token was minted from a two-value body: %+v", list)
	}
	if _, ok := admin.users["u-bob"]; ok {
		t.Error("a user was created from a two-value body")
	}
	if admin.roles["u-2"] != "viewer" || admin.users["u-2"].PasswordHash != "" {
		t.Errorf("u-2 changed from a two-value body: role %q, hash %q", admin.roles["u-2"], admin.users["u-2"].PasswordHash)
	}
}

func TestAuthPasswordChangeRefusesTrailingJSONValues(t *testing.T) {
	hash, err := authn.HashPassword("the current secret")
	if err != nil {
		t.Fatal(err)
	}
	admin := newFakeUserAdmin(store.User{ID: "u-1", Username: "alice", PasswordHash: hash, DisplayName: "Alice"})
	s := newLocalAuthServer(t)
	s.userAdmin = admin
	s.users = admin
	sess, _ := s.sessions.Create(context.Background(), authn.Session{Username: "alice", PasswordStamp: authn.PasswordStamp(hash)})

	body := `{"currentPassword":"the current secret","newPassword":"the next long secret"}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/auth/password", strings.NewReader(body+body),
		func(r *http.Request) {
			r.Header.Set("Content-Type", "application/json")
			r.AddCookie(&http.Cookie{Name: s.cfg.Auth.Session.CookieName, Value: sess})
		})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "more than one JSON value") {
		t.Fatalf("got %d %s, want 400 naming the extra JSON value", w.Code, w.Body)
	}
	if admin.users["u-1"].PasswordHash != hash {
		t.Error("the password changed from a two-value body")
	}
}

// A stray closer after the object is refused too: json.Decoder.More reports false for a trailing
// '}' or ']', so a More-only check let `{...}}` and `{...}]]]]` through as the first object.
func TestMutationDecodersRefuseAStrayTrailingToken(t *testing.T) {
	decode := func(w http.ResponseWriter, r *http.Request) bool {
		_, ok := decodeTargetRequest(w, r)
		return ok
	}
	for _, suffix := range []string{"}", "]", "]]]]", " }", "\n]", "x", "1", `"s"`, ","} {
		w := httptest.NewRecorder()
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(validTargetBody+suffix))
		if decode(w, r) {
			t.Errorf("suffix %q: the body was accepted", suffix)
			continue
		}
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "send exactly one object") {
			t.Errorf("suffix %q: got %d %s, want 400 asking for exactly one object", suffix, w.Code, w.Body)
		}
	}
	for _, suffix := range []string{"", " ", "\n", "\r\n\t "} {
		w := httptest.NewRecorder()
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(validTargetBody+suffix))
		if !decode(w, r) {
			t.Errorf("suffix %q: trailing whitespace was refused: %d %s", suffix, w.Code, w.Body)
		}
	}

	admin := newFakeUserAdmin(store.User{ID: "u-admin", Username: "root"}, store.User{ID: "u-2", Username: "ops"})
	admin.roles["u-2"] = "viewer"
	users := newUsersServer(t, admin, rolesByID{"u-admin": {"admin"}, "u-2": {"viewer"}})
	w := doRequest(t, users, http.MethodPatch, "/api/v1/users/u-2", strings.NewReader(`{"role":"operator"}}`), mutateWithCSRF)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "send exactly one object") {
		t.Errorf("user patch with a stray '}': got %d %s, want 400", w.Code, w.Body)
	}
	if admin.roles["u-2"] != "viewer" {
		t.Errorf("u-2 changed from a body with a stray '}': role %q", admin.roles["u-2"])
	}
}

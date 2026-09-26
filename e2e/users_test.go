//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"testing"
	"time"
)

type session struct {
	t    *testing.T
	base string
	c    *http.Client
}

func newSession(t *testing.T) *session {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &session{t: t, base: consoleBaseURL(t), c: &http.Client{Jar: jar, Timeout: 15 * time.Second}}
}

// do sends JSON; mutations carry the csrf cookie back as X-CSRF-Token, the console's double-submit rule.
func (s *session) do(method, path string, body any) (int, []byte) {
	s.t.Helper()
	r := bytes.NewReader(nil)
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			s.t.Fatalf("marshal %s %s body: %v", method, path, err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, s.base+path, r)
	if err != nil {
		s.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	u, _ := url.Parse(s.base)
	for _, c := range s.c.Jar.Cookies(u) {
		if c.Name == "csrf" {
			req.Header.Set("X-CSRF-Token", c.Value)
		}
	}
	resp, err := s.c.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.Bytes()
}

// holdsSessionCookie reports whether the jar still carries a cookie other than the csrf one.
func (s *session) holdsSessionCookie() bool {
	u, _ := url.Parse(s.base)
	for _, c := range s.c.Jar.Cookies(u) {
		if c.Name != "csrf" {
			return true
		}
	}
	return false
}

func (s *session) login(user, pass string) int {
	code, _ := s.do(http.MethodPost, "/api/v1/auth/login", map[string]string{"username": user, "password": pass})
	return code
}

func TestConsoleLocalUsers(t *testing.T) {
	adminPass := os.Getenv("KCONMON_LOCAL_ADMIN_PASSWORD")
	if adminPass == "" {
		t.Skip("KCONMON_LOCAL_ADMIN_PASSWORD not set (local-auth leg only)")
	}
	admin := newSession(t)
	if code := admin.login("admin", adminPass); code != http.StatusNoContent {
		t.Fatalf("admin login = %d", code)
	}

	code, body := admin.do(http.MethodPost, "/api/v1/users", map[string]string{
		"username": "e2e-operator", "password": "operator password 1", "role": "operator",
	})
	if code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &created); err != nil || created.ID == "" {
		t.Fatalf("create body %s: %v", body, err)
	}

	op := newSession(t)
	if code := op.login("e2e-operator", "operator password 1"); code != http.StatusNoContent {
		t.Fatalf("operator login = %d", code)
	}
	if code, body := op.do(http.MethodPost, "/api/v1/auth/password", map[string]string{
		"currentPassword": "operator password 1", "newPassword": "operator password 2",
	}); code != http.StatusNoContent {
		t.Fatalf("own password change = %d %s", code, body)
	}
	if code, _ := op.do(http.MethodGet, "/api/v1/topology", nil); code != http.StatusOK {
		t.Fatalf("the caller's reissued session does not work: %d", code)
	}
	if code := newSession(t).login("e2e-operator", "operator password 1"); code != http.StatusUnauthorized {
		t.Fatalf("old password still logs in: %d", code)
	}

	if code, body := admin.do(http.MethodPatch, "/api/v1/users/"+created.ID, map[string]bool{"disabled": true}); code != http.StatusOK {
		t.Fatalf("disable = %d %s", code, body)
	}
	if code, _ := op.do(http.MethodGet, "/api/v1/topology", nil); code != http.StatusUnauthorized {
		t.Fatalf("a disabled user's session reads the API with %d, want 401", code)
	}
	if code := newSession(t).login("e2e-operator", "operator password 2"); code != http.StatusUnauthorized {
		t.Fatalf("a disabled user's login answered %d, want 401", code)
	}

	// Re-enabling brings the account back, not the sessions it held before the disable.
	if !op.holdsSessionCookie() {
		t.Fatal("the pre-disable session cookie is gone from the jar, so a 401 below would prove nothing")
	}
	if code, body := admin.do(http.MethodPatch, "/api/v1/users/"+created.ID, map[string]bool{"disabled": false}); code != http.StatusOK {
		t.Fatalf("re-enable = %d %s", code, body)
	}
	if code, _ := op.do(http.MethodGet, "/api/v1/topology", nil); code != http.StatusUnauthorized {
		t.Fatalf("a session opened before the disable reads the API with %d after the re-enable, want 401", code)
	}
	fresh := newSession(t)
	if code := fresh.login("e2e-operator", "operator password 2"); code != http.StatusNoContent {
		t.Fatalf("login after the re-enable = %d, want 204", code)
	}
	if code, _ := fresh.do(http.MethodGet, "/api/v1/topology", nil); code != http.StatusOK {
		t.Fatalf("the session opened after the re-enable reads the API with %d, want 200", code)
	}
}

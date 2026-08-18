package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/config"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
	"github.com/EsDmitrii/kconmon-ng/internal/console/ws"
)

// fakeAuthenticator is an authn.Authenticator test double.
type fakeAuthenticator struct {
	subject authz.Subject
	err     error
	mode    string
}

func (f fakeAuthenticator) Authenticate(*http.Request) (authz.Subject, error) {
	return f.subject, f.err
}
func (f fakeAuthenticator) Mode() string {
	if f.mode == "" {
		return "fake"
	}
	return f.mode
}

// fakeRoleResolver is a RoleResolver test double.
type fakeRoleResolver struct {
	roles []string
	err   error
}

func (f fakeRoleResolver) RolesFor(context.Context, authz.Subject) ([]string, error) {
	return f.roles, f.err
}

// fakeUserStore is an authn.UserStore test double.
type fakeUserStore struct {
	users map[string]store.User
}

func (f fakeUserStore) GetUserByUsername(_ context.Context, username string) (store.User, error) {
	u, ok := f.users[username]
	if !ok {
		return store.User{}, store.ErrNotFound
	}
	return u, nil
}

// GetUserByID is unused by every auth_test.go case (they all authenticate via GetUserByUsername,
// local mode's own re-query path).
func (f fakeUserStore) GetUserByID(_ context.Context, id string) (store.User, error) {
	for _, u := range f.users {
		if u.ID == id {
			return u, nil
		}
	}
	return store.User{}, store.ErrNotFound
}

// fakeOIDCFlow is an OIDCFlow test double (I-4): a tiny fake standing in for
// *authn.OIDCAuthenticator.
type fakeOIDCFlow struct {
	authorizeURL string
	authorizeErr error
	sessionID    string
	returnTo     string
	callbackErr  error
}

func (f fakeOIDCFlow) AuthorizeURL(context.Context, string) (string, error) {
	return f.authorizeURL, f.authorizeErr
}

func (f fakeOIDCFlow) Callback(context.Context, string, string) (string, string, error) {
	return f.sessionID, f.returnTo, f.callbackErr
}

// captureLogs swaps in a buffer-backed default slog logger for the duration of a test.
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return buf
}

// syncBuffer is a mutex-guarded bytes.Buffer so concurrent log writes
// racing a test's own String() read (under -race) are safe.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// authTestConfig builds a minimal, hand-constructed config.Config for the auth tests below.
func authTestConfig(mode string) *config.Config {
	return &config.Config{
		HTTPPort: 8080, LogLevel: "info", LogFormat: "json", MetricsPrefix: "kconmon_ng",
		Auth: config.AuthConfig{
			Mode:      mode,
			Anonymous: config.AnonymousConfig{Role: "viewer"},
			Session:   config.SessionConfig{CookieName: "kconmon_session", TTL: time.Hour, Secure: false},
		},
	}
}

// newAuthzServer builds a Server with a controllable Authenticator and
// Policy and nothing else wired -- exactly what the authorize-decision
// tests below need, isolated from controller/prometheus/hub concerns.
func newAuthzServer(t *testing.T, authr authn.Authenticator, policy *authz.Policy, extra Deps) *Server { //nolint:gocritic // hugeParam: test helper, not the hot path
	t.Helper()
	cfg := authTestConfig("local")
	reg := prometheus.NewRegistry()
	m := metrics.New(cfg.MetricsPrefix, reg)
	ui := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("spa")) })
	extra.Config = cfg
	extra.Metrics = m
	extra.PromRegistry = reg
	extra.UI = ui
	extra.Authenticator = authr
	extra.Policy = policy
	return NewServer(extra)
}

// doRequest is `do` (server_test.go) generalized to any method, body, and
// request mutation (headers/cookies) the CSRF and permission tests need.
func doRequest(t *testing.T, s *Server, method, target string, body io.Reader, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), method, target, body)
	if mutate != nil {
		mutate(req)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

// withOIDCState attaches the state cookie handleOIDCStart set, which the callback now requires:
// the state must belong to the browser that STARTED the flow, not merely be one the console minted.
func withOIDCState(state string) func(*http.Request) {
	return func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: oidcStateCookieName, Value: state})
	}
}

// TestAnonymousDefaultServesEveryM1M2Route is the Phase B degraded-state guarantee.
func TestAnonymousDefaultServesEveryM1M2Route(t *testing.T) {
	s := newTestServer(t) // anonymous/viewer default, no Authenticator/Policy override
	cases := []struct {
		method, path string
		body         io.Reader
	}{
		{http.MethodGet, "/api/v1/version", nil},
		{http.MethodGet, "/api/v1/config", nil},
		{http.MethodGet, "/api/v1/topology", nil},
		{http.MethodGet, "/api/v1/matrix", nil},
		{http.MethodGet, "/api/v1/events", nil},
		{http.MethodPost, "/api/v1/promql/query", strings.NewReader(`{"query":"up"}`)},
		{http.MethodPost, "/api/v1/promql/query_range", strings.NewReader(`{"query":"up","start":"2026-01-01T00:00:00Z","end":"2026-01-01T01:00:00Z","step":60000000000}`)},
		{http.MethodGet, "/ws", nil},
		{http.MethodGet, "/api/v1/auth/me", nil},
	}
	for _, c := range cases {
		w := doRequest(t, s, c.method, c.path, c.body, nil)
		if w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
			t.Errorf("%s %s = %d, want neither 401 nor 403 under the anonymous/viewer default; body=%s",
				c.method, c.path, w.Code, w.Body.String())
		}
	}
}

// TestEveryAPIRouteHasAPermissionDecision walks the live chi router and checks two things per
// registered route; (2) is the I-1 review carry-forward -- the previous version of this test only
// asserted (1).
func TestEveryAPIRouteHasAPermissionDecision(t *testing.T) {
	// Authenticated (Kind != "") but role-less: passes authenticate.
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u1"}, mode: "local"}
	s := newAuthzServer(t, authr, authz.NewPolicy(nil), Deps{})

	err := chi.Walk(s.router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if route == "/healthz" || route == "/readyz" || route == "/metrics" {
			return nil // never authenticated by design (server.go: registered on r, outside the Group) -- deliberately outside this table
		}

		rule, ok := routeTable[method+" "+route]
		if !ok {
			t.Errorf("%s %s is registered but has no entry in routeTable", method, route)
			return nil
		}

		var body io.Reader
		if isMutatingMethod(method) {
			body = strings.NewReader(`{}`)
		}
		// This subject is Kind == authz.SubjectUser.
		w := doRequest(t, s, method, route, body, func(r *http.Request) {
			if isMutatingMethod(method) {
				r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "tok-1"})
				r.Header.Set(csrfHeaderName, "tok-1")
			}
		})
		denied := w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden

		switch {
		case rule.public && denied:
			t.Errorf("%s %s is marked public in routeTable but a role-less authenticated subject got %d -- authorize is not treating it as public; body=%s",
				method, route, w.Code, w.Body.String())
		case !rule.public && !denied:
			t.Errorf("%s %s is marked non-public in routeTable but a role-less authenticated subject got %d, want 401 or 403 -- "+
				"either it bypasses the authenticate+authorize Group entirely, or the permission check did not run", method, route, w.Code)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
}

// TestRoutePermissionTable exercises every non-public row of routeTable.
func TestRoutePermissionTable(t *testing.T) {
	for key, rule := range routeTable {
		if rule.public {
			continue
		}
		key, rule := key, rule
		parts := strings.SplitN(key, " ", 2)
		method, path := parts[0], parts[1]

		accepted := rule.accepted()
		if len(accepted) == 0 {
			t.Errorf("%s: a non-public routeTable row requires no permission at all", key)
			continue
		}

		for _, perm := range accepted {
			t.Run(key+"/granted/"+string(perm), func(t *testing.T) {
				// Roles come from a RoleResolver, not from the Authenticator's returned Subject directly.
				policy := authz.NewPolicy(map[string][]authz.Permission{"tester": {perm}})
				authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u1"}}
				s := newAuthzServer(t, authr, policy, Deps{Roles: fakeRoleResolver{roles: []string{"tester"}}})
				// Kind == authz.SubjectUser requires the CSRF double-submit pair for a mutating method now
				// (I-2).
				w := doRequest(t, s, method, path, strings.NewReader(`{"query":"up"}`), func(r *http.Request) {
					if isMutatingMethod(method) {
						r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "tok-1"})
						r.Header.Set(csrfHeaderName, "tok-1")
					}
				})
				if w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
					t.Errorf("%s %s with permission %s = %d, want to pass authorization; body=%s",
						method, path, perm, w.Code, w.Body.String())
				}
			})
		}

		t.Run(key+"/denied", func(t *testing.T) {
			authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u1"}} // no roles
			s := newAuthzServer(t, authr, nil, Deps{})
			w := doRequest(t, s, method, path, strings.NewReader(`{"query":"up"}`), nil)
			if w.Code != http.StatusForbidden {
				t.Fatalf("%s %s without permission %v = %d, want 403", method, path, accepted, w.Code)
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
				t.Errorf("Content-Type = %q, want application/problem+json", ct)
			}
			for _, perm := range accepted {
				if !strings.Contains(w.Body.String(), string(perm)) {
					t.Errorf("403 body = %s, want it to name permission %q", w.Body.String(), perm)
				}
			}
		})
	}
}

// TestAuthorizeFailsClosedWhenRouteRuleMissing pins authorize's own fail-closed behavior directly.
func TestAuthorizeFailsClosedWhenRouteRuleMissing(t *testing.T) {
	s := newAuthzServer(t, fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, Roles: []string{"admin"}}}, authz.NewPolicy(nil), Deps{})

	r := chi.NewRouter()
	r.Use(s.authenticate, s.authorize)
	r.Get("/api/v1/not-in-table", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/not-in-table", http.NoBody)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("route missing from routeTable = %d, want 403 fail-closed", w.Code)
	}
}

// TestUnauthenticatedNonAnonymousModeReturns401 covers the brief's explicit
// case: a protected route with no credentials, outside anonymous mode.
func TestUnauthenticatedNonAnonymousModeReturns401(t *testing.T) {
	authr := fakeAuthenticator{err: authn.ErrNoCredentials, mode: "local"}
	s := newAuthzServer(t, authr, nil, Deps{})

	w := doRequest(t, s, http.MethodGet, "/api/v1/topology", nil, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /api/v1/topology = %d, want 401", w.Code)
	}
	if w.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("401 response missing WWW-Authenticate header")
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
}

/*
An unreadable role store is not evidence that the subject holds auth.defaultRole -- it is no evidence
at all. This used to degrade to the default role, so a database blip GRANTED permissions rather than
withholding them. It now fails closed: 403, and never a 500 (an outage is not a bug in the request).
*/
func TestRoleResolverErrorRefusesRatherThanGrantingTheDefaultRole(t *testing.T) {
	cfg := authTestConfig("local")
	cfg.Auth.DefaultRole = "viewer"
	reg := prometheus.NewRegistry()
	m := metrics.New(cfg.MetricsPrefix, reg)
	ui := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("spa")) })
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u1"}, mode: "local"}
	s := NewServer(Deps{
		Config: cfg, Metrics: m, PromRegistry: reg, UI: ui,
		Authenticator: authr,
		Roles:         fakeRoleResolver{err: errors.New("db blip")},
	})

	w := doRequest(t, s, http.MethodGet, "/api/v1/topology", nil, nil)
	if w.Code == http.StatusInternalServerError {
		t.Fatalf("RolesFor error surfaced as 500; an unreachable role store is not a bad request")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("RolesFor error = %d, want 403: no roles resolved means no permission held; body=%s",
			w.Code, w.Body.String())
	}
}

// TestRoleResolverSuccessUsesResolvedRoles pins the other half of
// resolveRoles: when RolesFor succeeds with a non-empty result, THOSE
// roles are used, not auth.defaultRole.
func TestRoleResolverSuccessUsesResolvedRoles(t *testing.T) {
	policy := authz.NewPolicy(map[string][]authz.Permission{"resolved-role": {authz.PermTopologyRead}})
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u1"}, mode: "local"}
	s := newAuthzServer(t, authr, policy, Deps{Roles: fakeRoleResolver{roles: []string{"resolved-role"}}})

	w := doRequest(t, s, http.MethodGet, "/api/v1/topology", nil, nil)
	if w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
		t.Errorf("resolved role holding topology:read = %d, want to pass authorization; body=%s", w.Code, w.Body.String())
	}
}

// --- CSRF -------------------------------------------------------------

func TestCSRFCookieAuthedMutationRequiresMatchingToken(t *testing.T) {
	policy := authz.NewPolicy(map[string][]authz.Permission{"tester": {authz.PermPromQLQuery}})
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u1"}}
	s := newAuthzServer(t, authr, policy, Deps{Roles: fakeRoleResolver{roles: []string{"tester"}}})
	cookieName := s.cfg.Auth.Session.CookieName

	newBody := func() io.Reader { return strings.NewReader(`{"query":"up"}`) }

	t.Run("missing header", func(t *testing.T) {
		w := doRequest(t, s, http.MethodPost, "/api/v1/promql/query", newBody(), func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: cookieName, Value: "sess-1"})
			r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "tok-1"})
		})
		if w.Code != http.StatusForbidden {
			t.Errorf("cookie-authed POST with no X-CSRF-Token header = %d, want 403", w.Code)
		}
	})

	t.Run("mismatched token", func(t *testing.T) {
		w := doRequest(t, s, http.MethodPost, "/api/v1/promql/query", newBody(), func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: cookieName, Value: "sess-1"})
			r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "tok-1"})
			r.Header.Set(csrfHeaderName, "wrong-token")
		})
		if w.Code != http.StatusForbidden {
			t.Errorf("mismatched X-CSRF-Token = %d, want 403", w.Code)
		}
	})

	t.Run("matching pair", func(t *testing.T) {
		w := doRequest(t, s, http.MethodPost, "/api/v1/promql/query", newBody(), func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: cookieName, Value: "sess-1"})
			r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "tok-1"})
			r.Header.Set(csrfHeaderName, "tok-1")
		})
		if w.Code == http.StatusForbidden {
			t.Errorf("matching CSRF cookie+header pair = %d, want to pass; body=%s", w.Code, w.Body.String())
		}
	})
}

func TestCSRFBearerAuthedMutationExempt(t *testing.T) {
	policy := authz.NewPolicy(map[string][]authz.Permission{"tester": {authz.PermPromQLQuery}})
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectToken, ID: "tok-1"}}
	s := newAuthzServer(t, authr, policy, Deps{Roles: fakeRoleResolver{roles: []string{"tester"}}})

	w := doRequest(t, s, http.MethodPost, "/api/v1/promql/query", strings.NewReader(`{"query":"up"}`), func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer kcm_whatever")
	})
	if w.Code == http.StatusForbidden {
		t.Errorf("bearer-authed POST with no CSRF token = %d, want to pass; body=%s", w.Code, w.Body.String())
	}
}

// --- /healthz /readyz /metrics never authenticated ---------------------

// panicAuthenticator proves /healthz, /readyz and /metrics never even reach the authenticate
// middleware.
type panicAuthenticator struct{ mode string }

func (panicAuthenticator) Authenticate(*http.Request) (authz.Subject, error) {
	panic("authenticate must never run for /healthz, /readyz, or /metrics")
}
func (p panicAuthenticator) Mode() string { return p.mode }

func TestHealthzReadyzMetricsNeverAuthenticatedInAnyMode(t *testing.T) {
	for _, mode := range []string{"anonymous", "local", "header", "oidc"} {
		t.Run(mode, func(t *testing.T) {
			s := newAuthzServer(t, panicAuthenticator{mode: mode}, nil, Deps{})
			for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
				w := doRequest(t, s, http.MethodGet, path, nil, nil)
				if w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
					t.Errorf("%s under mode=%s = %d, want neither 401 nor 403", path, mode, w.Code)
				}
			}
		})
	}
}

// --- /ws through the full auth middleware chain (Unwrap regression) ----

// TestWSUpgradesThroughAuthMiddlewareForPermittedSubject proves the authenticate+authorize
// middleware pair does not break the /ws Hijacker path for a genuinely non-default.
func TestWSUpgradesThroughAuthMiddlewareForPermittedSubject(t *testing.T) {
	hub := ws.NewHub(cache.NewInProcessBus(), metrics.New("kconmon_ng", prometheus.NewRegistry()))
	policy := authz.NewPolicy(map[string][]authz.Permission{"tester": {authz.PermEventsRead}})
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u1"}, mode: "local"}
	s := newAuthzServer(t, authr, policy, Deps{Hub: hub, Roles: fakeRoleResolver{roles: []string{"tester"}}})

	httpSrv := httptest.NewServer(s.Handler())
	defer httpSrv.Close()

	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(httpSrv.URL, "http")+"/ws", nil)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
			_ = resp.Body.Close()
		}
		t.Fatalf("dial /ws: %v (http status %d)", err, status)
	}
	defer func() { _ = conn.Close() }()
	if resp != nil {
		_ = resp.Body.Close()
	}
}

// The property it pinned -- "the socket has ONE permission and it covers every topic" -- was never
// the goal.

func TestWSAdmitsRunsReadForRunWatching(t *testing.T) {
	hub := ws.NewHub(cache.NewInProcessBus(), metrics.New("kconmon_ng", prometheus.NewRegistry()))
	policy := authz.NewPolicy(map[string][]authz.Permission{
		"run-watcher": {authz.PermRunsRead},
	})
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u1"}, mode: "local"}
	s := newAuthzServer(t, authr, policy, Deps{Hub: hub, Roles: fakeRoleResolver{roles: []string{"run-watcher"}}})

	topic := ws.RunTopic("run-1")
	if !hub.OpenTopic(t.Context(), topic) {
		t.Fatal("OpenTopic refused the run topic")
	}

	httpSrv := httptest.NewServer(s.Handler())
	defer httpSrv.Close()

	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(httpSrv.URL, "http")+"/ws", nil)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
			_ = resp.Body.Close()
		}
		t.Fatalf("dial /ws with runs:read only: %v (http status %d) -- the upgrade must admit runs:read", err, status)
	}
	defer func() { _ = conn.Close() }()
	if resp != nil {
		_ = resp.Body.Close()
	}

	if writeErr := conn.WriteJSON(ws.ClientMessage{Action: ws.ActionSubscribe, Topic: topic}); writeErr != nil {
		t.Fatalf("subscribe %s: %v", topic, writeErr)
	}

	deadline := time.Now().Add(5 * time.Second)
	var env ws.Envelope
	for {
		hub.Broadcast(topic, ws.TypeSnapshot, json.RawMessage(`{"progress":1}`))
		if deadlineErr := conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); deadlineErr != nil {
			t.Fatalf("set read deadline: %v", deadlineErr)
		}
		if readErr := conn.ReadJSON(&env); readErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no frame on %s within 5s -- runs:read must cover its own run's topic", topic)
		}
	}
	if env.Topic != topic || env.Type == ws.TypeError {
		t.Errorf("envelope = %+v, want a frame on %s, not an error", env, topic)
	}
}

// TestWSRunsReadOnlyIsRefusedTheFleetWideTopics is the other half, and the reason the upgrade may
// be widened at all.
func TestWSRunsReadOnlyIsRefusedTheFleetWideTopics(t *testing.T) {
	hub := ws.NewHub(cache.NewInProcessBus(), metrics.New("kconmon_ng", prometheus.NewRegistry()))
	policy := authz.NewPolicy(map[string][]authz.Permission{
		"run-watcher": {authz.PermRunsRead},
	})
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u1"}, mode: "local"}
	s := newAuthzServer(t, authr, policy, Deps{Hub: hub, Roles: fakeRoleResolver{roles: []string{"run-watcher"}}})

	httpSrv := httptest.NewServer(s.Handler())
	defer httpSrv.Close()

	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(httpSrv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatalf("dial /ws with runs:read only: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if resp != nil {
		_ = resp.Body.Close()
	}

	// Each topic names the permission IT requires, not one blanket permission: the
	// socket asks the same question the REST route serving the same bytes asks.
	for _, tc := range []struct {
		topic string
		want  authz.Permission
	}{
		{ws.TopicLive, authz.PermEventsRead},
		{ws.TopicTopology, authz.PermTopologyRead},
		{ws.MatrixTopic("tcp"), authz.PermMatrixRead},
	} {
		if writeErr := conn.WriteJSON(ws.ClientMessage{Action: ws.ActionSubscribe, Topic: tc.topic}); writeErr != nil {
			t.Fatalf("subscribe %s: %v", tc.topic, writeErr)
		}
		if deadlineErr := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); deadlineErr != nil {
			t.Fatalf("set read deadline: %v", deadlineErr)
		}
		var env ws.Envelope
		if readErr := conn.ReadJSON(&env); readErr != nil {
			t.Fatalf("read the refusal for %s: %v", tc.topic, readErr)
		}
		if env.Type != ws.TypeError {
			t.Fatalf("subscribe to %s on a runs:read-only socket: envelope = %+v, want an error frame", tc.topic, env)
		}
		if !strings.Contains(string(env.Data), string(tc.want)) {
			t.Errorf("refusal for %s = %s, want it to name the missing permission %q", tc.topic, env.Data, tc.want)
		}
	}

	// Still usable afterwards: the socket survived three refusals.
	topic := ws.RunTopic("run-after-refusals")
	if !hub.OpenTopic(t.Context(), topic) {
		t.Fatal("OpenTopic refused the run topic")
	}
	if writeErr := conn.WriteJSON(ws.ClientMessage{Action: ws.ActionSubscribe, Topic: topic}); writeErr != nil {
		t.Fatalf("subscribe %s: %v", topic, writeErr)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		hub.Broadcast(topic, ws.TypeSnapshot, json.RawMessage(`{"progress":1}`))
		if deadlineErr := conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); deadlineErr != nil {
			t.Fatalf("set read deadline: %v", deadlineErr)
		}
		var env ws.Envelope
		if readErr := conn.ReadJSON(&env); readErr == nil {
			if env.Topic != topic || env.Type == ws.TypeError {
				t.Fatalf("envelope after the refusals = %+v, want a frame on %s", env, topic)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the socket stopped serving run topics after a denied subscribe")
		}
	}
}

// TestWSRefusesTheUpgradeWithoutEitherPermission pins that widening the row to anyOf did not make
// it public.
func TestWSRefusesTheUpgradeWithoutEitherPermission(t *testing.T) {
	hub := ws.NewHub(cache.NewInProcessBus(), metrics.New("kconmon_ng", prometheus.NewRegistry()))
	policy := authz.NewPolicy(map[string][]authz.Permission{
		"topology-only": {authz.PermTopologyRead},
	})
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u1"}, mode: "local"}
	s := newAuthzServer(t, authr, policy, Deps{Hub: hub, Roles: fakeRoleResolver{roles: []string{"topology-only"}}})

	w := doRequest(t, s, http.MethodGet, "/ws", nil, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("GET /ws for a role holding neither events:read nor runs:read = %d, want 403", w.Code)
	}
	for _, perm := range []authz.Permission{authz.PermEventsRead, authz.PermRunsRead} {
		if !strings.Contains(w.Body.String(), string(perm)) {
			t.Errorf("403 body = %s, want it to name %q", w.Body.String(), perm)
		}
	}
	if got := routeTable["GET /ws"].anyOf; len(got) != 2 {
		t.Errorf(`routeTable["GET /ws"].anyOf = %v, want exactly {events:read, runs:read} -- read wsTopicAuthorizer before changing it`, got)
	}
}

// TestWSEventsReadAloneCoversRunTopics is the other face of the same coupling: events:read alone;
// stated as a test so the per-connection granularity is a pinned property rather than folklore.
func TestWSEventsReadAloneCoversRunTopics(t *testing.T) {
	hub := ws.NewHub(cache.NewInProcessBus(), metrics.New("kconmon_ng", prometheus.NewRegistry()))
	policy := authz.NewPolicy(map[string][]authz.Permission{
		"events-only": {authz.PermEventsRead},
	})
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u1"}, mode: "local"}
	s := newAuthzServer(t, authr, policy, Deps{Hub: hub, Roles: fakeRoleResolver{roles: []string{"events-only"}}})

	topic := ws.RunTopic("run-1")
	if !hub.OpenTopic(t.Context(), topic) {
		t.Fatal("OpenTopic refused the run topic")
	}

	httpSrv := httptest.NewServer(s.Handler())
	defer httpSrv.Close()

	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(httpSrv.URL, "http")+"/ws", nil)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
			_ = resp.Body.Close()
		}
		t.Fatalf("dial /ws with events:read only: %v (http status %d)", err, status)
	}
	defer func() { _ = conn.Close() }()
	if resp != nil {
		_ = resp.Body.Close()
	}

	if err := conn.WriteJSON(ws.ClientMessage{Action: ws.ActionSubscribe, Topic: topic}); err != nil {
		t.Fatalf("subscribe %s: %v", topic, err)
	}

	deadline := time.Now().Add(5 * time.Second)
	var env ws.Envelope
	for {
		hub.Broadcast(topic, ws.TypeSnapshot, json.RawMessage(`{"progress":1}`))
		if err := conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
			t.Fatalf("set read deadline: %v", err)
		}
		if err := conn.ReadJSON(&env); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no frame on %s within 5s -- events:read should already cover run topics", topic)
		}
	}
	if env.Topic != topic || env.Type == ws.TypeError {
		t.Errorf("envelope = %+v, want a frame on %s, not an error", env, topic)
	}
}

// --- /api/v1/auth/* endpoints -------------------------------------------

func TestAuthMeAnonymousReturnsFixedSubject(t *testing.T) {
	s := newTestServer(t) // default anonymous/viewer

	w := doRequest(t, s, http.MethodGet, "/api/v1/auth/me", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("/api/v1/auth/me under anonymous mode = %d, want 200", w.Code)
	}
	var body struct {
		Subject struct {
			Kind  string   `json:"kind"`
			ID    string   `json:"id"`
			Roles []string `json:"roles"`
		} `json:"subject"`
		Permissions []string `json:"permissions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Subject.Kind != "anonymous" || body.Subject.ID != "anonymous" {
		t.Errorf("subject = %+v, want the fixed anonymous subject", body.Subject)
	}
	if len(body.Subject.Roles) != 1 || body.Subject.Roles[0] != "viewer" {
		t.Errorf("subject.roles = %v, want [viewer]", body.Subject.Roles)
	}
	if len(body.Permissions) == 0 {
		t.Error("permissions is empty, want viewer's permission set")
	}
}

func TestAuthMeUnauthenticatedNonAnonymousReturns401(t *testing.T) {
	authr := fakeAuthenticator{err: authn.ErrNoCredentials, mode: "local"}
	s := newAuthzServer(t, authr, nil, Deps{})

	w := doRequest(t, s, http.MethodGet, "/api/v1/auth/me", nil, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("/api/v1/auth/me with no credentials, local mode = %d, want 401", w.Code)
	}
}

func TestAuthLoginNotFoundOutsideLocalMode(t *testing.T) {
	s := newTestServer(t) // anonymous mode
	w := doRequest(t, s, http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{}`), nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("login outside local mode = %d, want 404", w.Code)
	}
}

func TestAuthLoginLocalModeSetsSessionAndCSRFCookies(t *testing.T) {
	hash, err := authn.HashPassword("s3cret!")
	if err != nil {
		t.Fatal(err)
	}
	users := fakeUserStore{users: map[string]store.User{
		"alice": {ID: "u-1", Username: "alice", PasswordHash: hash, DisplayName: "Alice"},
	}}
	sessions := authn.NewSessionStore(cache.NewInProcessKV(), time.Hour, 0)

	cfg := authTestConfig("local")
	reg := prometheus.NewRegistry()
	m := metrics.New(cfg.MetricsPrefix, reg)
	ui := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("spa")) })
	s := NewServer(Deps{
		Config: cfg, Metrics: m, PromRegistry: reg, UI: ui,
		Users: users, Sessions: sessions,
		// A login request carries no session cookie -- the real local authenticator would resolve this to
		// ErrNoCredentials.
		Authenticator: fakeAuthenticator{err: authn.ErrNoCredentials, mode: "local"},
	})

	w := doRequest(t, s, http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"alice","password":"s3cret!"}`), nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("login = %d %s, want 204", w.Code, w.Body.String())
	}

	var sessionCookie, csrfVal string
	for _, c := range w.Result().Cookies() {
		switch c.Name {
		case cfg.Auth.Session.CookieName:
			sessionCookie = c.Value
		case csrfCookieName:
			csrfVal = c.Value
		}
	}
	if sessionCookie == "" {
		t.Fatal("login did not set a session cookie")
	}
	if csrfVal == "" {
		t.Fatal("login did not set a csrf cookie")
	}

	sess, ok, err := sessions.Get(context.Background(), sessionCookie)
	if err != nil || !ok {
		t.Fatalf("session %q not found in store: ok=%v err=%v", sessionCookie, ok, err)
	}
	if sess.Username != "alice" {
		t.Errorf("session.Username = %q, want alice", sess.Username)
	}
}

func TestAuthLoginWrongPasswordReturns401(t *testing.T) {
	hash, err := authn.HashPassword("correct-horse")
	if err != nil {
		t.Fatal(err)
	}
	users := fakeUserStore{users: map[string]store.User{
		"alice": {ID: "u-1", Username: "alice", PasswordHash: hash},
	}}
	sessions := authn.NewSessionStore(cache.NewInProcessKV(), time.Hour, 0)

	cfg := authTestConfig("local")
	reg := prometheus.NewRegistry()
	m := metrics.New(cfg.MetricsPrefix, reg)
	ui := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("spa")) })
	s := NewServer(Deps{
		Config: cfg, Metrics: m, PromRegistry: reg, UI: ui, Users: users, Sessions: sessions,
		Authenticator: fakeAuthenticator{err: authn.ErrNoCredentials, mode: "local"}, // see TestAuthLoginLocalModeSetsSessionAndCSRFCookies
	})

	w := doRequest(t, s, http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"alice","password":"wrong"}`), nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d, want 401", w.Code)
	}
}

func TestAuthLoginUnknownUsernameReturns401(t *testing.T) {
	users := fakeUserStore{users: map[string]store.User{}}
	sessions := authn.NewSessionStore(cache.NewInProcessKV(), time.Hour, 0)

	cfg := authTestConfig("local")
	reg := prometheus.NewRegistry()
	m := metrics.New(cfg.MetricsPrefix, reg)
	ui := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("spa")) })
	s := NewServer(Deps{
		Config: cfg, Metrics: m, PromRegistry: reg, UI: ui, Users: users, Sessions: sessions,
		Authenticator: fakeAuthenticator{err: authn.ErrNoCredentials, mode: "local"}, // see TestAuthLoginLocalModeSetsSessionAndCSRFCookies
	})

	w := doRequest(t, s, http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"ghost","password":"whatever"}`), nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unknown username = %d, want 401 (not 404 -- that would leak account existence)", w.Code)
	}
}

func TestAuthLogoutDeletesSessionAndClearsCookies(t *testing.T) {
	sessions := authn.NewSessionStore(cache.NewInProcessKV(), time.Hour, 0)
	id, err := sessions.Create(context.Background(), authn.Session{Username: "alice"})
	if err != nil {
		t.Fatal(err)
	}

	cfg := authTestConfig("local")
	reg := prometheus.NewRegistry()
	m := metrics.New(cfg.MetricsPrefix, reg)
	ui := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("spa")) })
	s := NewServer(Deps{Config: cfg, Metrics: m, PromRegistry: reg, UI: ui, Sessions: sessions})

	// logout is a mutating request and, once a session cookie is present.
	w := doRequest(t, s, http.MethodPost, "/api/v1/auth/logout", http.NoBody, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: cfg.Auth.Session.CookieName, Value: id})
		r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "tok-1"})
		r.Header.Set(csrfHeaderName, "tok-1")
	})
	if w.Code != http.StatusNoContent {
		t.Fatalf("logout = %d %s, want 204", w.Code, w.Body.String())
	}
	if _, ok, _ := sessions.Get(context.Background(), id); ok {
		t.Error("session still resolves after logout -- Delete did not run")
	}

	var clearedSession, clearedCSRF bool
	for _, c := range w.Result().Cookies() {
		if c.Name == cfg.Auth.Session.CookieName && c.MaxAge < 0 {
			clearedSession = true
		}
		if c.Name == csrfCookieName && c.MaxAge < 0 {
			clearedCSRF = true
		}
	}
	if !clearedSession || !clearedCSRF {
		t.Errorf("logout must clear both cookies with MaxAge<0: session=%v csrf=%v", clearedSession, clearedCSRF)
	}
}

func TestAuthLogoutWithNoSessionCookieIsIdempotent(t *testing.T) {
	s := newTestServer(t) // anonymous mode, no Sessions wired
	w := doRequest(t, s, http.MethodPost, "/api/v1/auth/logout", http.NoBody, nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("logout with nothing to log out of = %d, want 204", w.Code)
	}
}

func TestOIDCStartNotFoundOutsideOIDCMode(t *testing.T) {
	s := newTestServer(t)
	w := doRequest(t, s, http.MethodGet, "/api/v1/auth/oidc/start", nil, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("oidc start outside oidc mode = %d, want 404", w.Code)
	}
}

func TestOIDCCallbackNotFoundOutsideOIDCMode(t *testing.T) {
	s := newTestServer(t)
	w := doRequest(t, s, http.MethodGet, config.OIDCCallbackPath, nil, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("oidc callback outside oidc mode = %d, want 404", w.Code)
	}
}

// --- instrument stays outermost -----------------------------------------

// TestInstrumentRecordsDeniedRequestsWithRoutePattern pins middleware
// order: instrument wraps authenticate+authorize, so a 401 is still
// counted under its route pattern, not lost as an uninstrumented rejection.
func TestInstrumentRecordsDeniedRequestsWithRoutePattern(t *testing.T) {
	authr := fakeAuthenticator{err: authn.ErrNoCredentials, mode: "local"}
	s := newAuthzServer(t, authr, nil, Deps{})

	_ = doRequest(t, s, http.MethodGet, "/api/v1/topology", nil, nil)

	w := do(t, s, "/metrics")
	body := w.Body.String()
	if !strings.Contains(body, `path="/api/v1/topology"`) || !strings.Contains(body, `status="401"`) {
		t.Errorf("metrics missing instrumentation for the denied request:\n%s", body)
	}
}

// --- NewServer defaults ---------------------------------------------------

func TestNewServerDefaultsNilAuthenticatorAndPolicyToAnonymousViewer(t *testing.T) {
	s := NewServer(Deps{
		Config:       authTestConfig("anonymous"),
		Metrics:      metrics.New("kconmon_ng", prometheus.NewRegistry()),
		PromRegistry: prometheus.NewRegistry(),
		UI:           http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("spa")) }),
		// Authenticator and Policy deliberately left unset.
	})

	w := doRequest(t, s, http.MethodGet, "/api/v1/topology", nil, nil)
	if w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
		t.Errorf("nil Authenticator+Policy = %d, want the anonymous/viewer default to pass topology:read; body=%s",
			w.Code, w.Body.String())
	}
}

// --- I-2: CSRF exemption keyed on subject kind ---------------------------

// TestCSRFHeaderModeSubjectRequiresPair pins I-2's actual fix.
func TestCSRFHeaderModeSubjectRequiresPair(t *testing.T) {
	policy := authz.NewPolicy(map[string][]authz.Permission{"tester": {authz.PermPromQLQuery}})
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "proxy-user"}, mode: "header"}
	s := newAuthzServer(t, authr, policy, Deps{Roles: fakeRoleResolver{roles: []string{"tester"}}})

	t.Run("no pair", func(t *testing.T) {
		w := doRequest(t, s, http.MethodPost, "/api/v1/promql/query", strings.NewReader(`{"query":"up"}`), nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("header-mode mutation with no csrf pair = %d, want 403", w.Code)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
			t.Errorf("Content-Type = %q, want application/problem+json", ct)
		}
	})

	t.Run("mint then echo", func(t *testing.T) {
		// GET /api/v1/auth/me is public (no permission needed) and.
		get := doRequest(t, s, http.MethodGet, "/api/v1/auth/me", nil, nil)
		var minted string
		for _, c := range get.Result().Cookies() {
			if c.Name == csrfCookieName {
				minted = c.Value
			}
		}
		if minted == "" {
			t.Fatal("a safe GET from an authenticated header-mode subject did not mint a csrf cookie")
		}

		w := doRequest(t, s, http.MethodPost, "/api/v1/promql/query", strings.NewReader(`{"query":"up"}`), func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: minted})
			r.Header.Set(csrfHeaderName, minted)
		})
		if w.Code == http.StatusForbidden {
			t.Errorf("header-mode mutation with matching minted csrf pair = %d, want to pass; body=%s", w.Code, w.Body.String())
		}
	})
}

// --- I-3: login timing oracle --------------------------------------------

// TestAuthLoginFailurePathsReturnIdenticalResponses pins I-3: unknown username, a disabled account.
func TestAuthLoginFailurePathsReturnIdenticalResponses(t *testing.T) {
	knownHash, err := authn.HashPassword("correct-horse")
	if err != nil {
		t.Fatal(err)
	}
	disabledHash, err := authn.HashPassword("does-not-matter")
	if err != nil {
		t.Fatal(err)
	}
	users := fakeUserStore{users: map[string]store.User{
		"alice":   {ID: "u-1", Username: "alice", PasswordHash: knownHash},
		"deleted": {ID: "u-2", Username: "deleted", PasswordHash: disabledHash, Disabled: true},
	}}
	sessions := authn.NewSessionStore(cache.NewInProcessKV(), time.Hour, 0)

	newServer := func(t *testing.T) *Server {
		t.Helper()
		cfg := authTestConfig("local")
		reg := prometheus.NewRegistry()
		m := metrics.New(cfg.MetricsPrefix, reg)
		ui := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("spa")) })
		return NewServer(Deps{
			Config: cfg, Metrics: m, PromRegistry: reg, UI: ui, Users: users, Sessions: sessions,
			Authenticator: fakeAuthenticator{err: authn.ErrNoCredentials, mode: "local"},
		})
	}

	type response struct {
		status int
		body   string
		ct     string
	}
	cases := []struct {
		name    string
		payload string
	}{
		{"unknown username", `{"username":"ghost","password":"whatever"}`},
		{"disabled account", `{"username":"deleted","password":"whatever"}`},
		{"wrong password", `{"username":"alice","password":"wrong"}`},
	}

	var first response
	for i, c := range cases {
		w := doRequest(t, newServer(t), http.MethodPost, "/api/v1/auth/login", strings.NewReader(c.payload), nil)
		got := response{status: w.Code, body: w.Body.String(), ct: w.Header().Get("Content-Type")}
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Errorf("%s response %+v differs from %s response %+v -- login failure paths must be indistinguishable",
				c.name, got, cases[0].name, first)
		}
	}
	if first.status != http.StatusUnauthorized {
		t.Errorf("login failure status = %d, want 401", first.status)
	}
}

// --- I-4: OIDC callback success path --------------------------------------

// TestOIDCCallbackSuccessSetsSessionAndCSRFCookiesAndRedirects is I-4's missing coverage.
func TestOIDCCallbackSuccessSetsSessionAndCSRFCookiesAndRedirects(t *testing.T) {
	cfg := authTestConfig("oidc")
	cfg.Auth.Session.CookieName = "my_nondefault_session_cookie"
	reg := prometheus.NewRegistry()
	m := metrics.New(cfg.MetricsPrefix, reg)
	ui := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("spa")) })
	flow := fakeOIDCFlow{sessionID: "sess-abc-123", returnTo: "/post-login/dashboard"}
	s := NewServer(Deps{Config: cfg, Metrics: m, PromRegistry: reg, UI: ui, OIDC: flow})

	w := doRequest(t, s, http.MethodGet, config.OIDCCallbackPath+"?state=s&code=c", nil, withOIDCState("s"))
	if w.Code != http.StatusFound {
		t.Fatalf("oidc callback success = %d, want 302; body=%s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); loc != "/post-login/dashboard" {
		t.Errorf("redirect Location = %q, want the flow's returnTo verbatim", loc)
	}

	var sessionVal, csrfVal string
	for _, c := range w.Result().Cookies() {
		switch c.Name {
		case cfg.Auth.Session.CookieName:
			sessionVal = c.Value
		case csrfCookieName:
			csrfVal = c.Value
		}
	}
	if sessionVal != "sess-abc-123" {
		t.Errorf("session cookie under %q = %q, want the flow's sessionID -- setSessionCookie must honor the configured name, not authn.OIDCSessionCookieName",
			cfg.Auth.Session.CookieName, sessionVal)
	}
	if csrfVal == "" {
		t.Error("oidc callback success did not set a csrf cookie")
	}
}

// --- M-5: csrf-mint failure is no longer swallowed ------------------------

// spyKV wraps a cache.KV, recording the key of the most recent Set.
type spyKV struct {
	cache.KV
	mu         sync.Mutex
	lastSetKey string
}

func (s *spyKV) Set(ctx context.Context, key string, val []byte, ttl time.Duration) error {
	s.mu.Lock()
	s.lastSetKey = key
	s.mu.Unlock()
	return s.KV.Set(ctx, key, val, ttl)
}

func (s *spyKV) lastKey() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSetKey
}

// withFailingCSRFRand stubs csrfRandRead to fail for the duration of the test, restoring it on
// cleanup.
func withFailingCSRFRand(t *testing.T) {
	t.Helper()
	orig := csrfRandRead
	csrfRandRead = func([]byte) (int, error) { return 0, errors.New("entropy source down") }
	t.Cleanup(func() { csrfRandRead = orig })
}

// The session created just before the failure must be deleted server-side, the session cookie
// cleared client-side.
func TestCSRFMintFailureOnLoginAbortsSessionAndReturns500(t *testing.T) {
	hash, err := authn.HashPassword("s3cret!")
	if err != nil {
		t.Fatal(err)
	}
	users := fakeUserStore{users: map[string]store.User{
		"alice": {ID: "u-1", Username: "alice", PasswordHash: hash},
	}}
	kv := &spyKV{KV: cache.NewInProcessKV()}
	sessions := authn.NewSessionStore(kv, time.Hour, 0)

	cfg := authTestConfig("local")
	reg := prometheus.NewRegistry()
	m := metrics.New(cfg.MetricsPrefix, reg)
	ui := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("spa")) })
	s := NewServer(Deps{
		Config: cfg, Metrics: m, PromRegistry: reg, UI: ui, Users: users, Sessions: sessions,
		Authenticator: fakeAuthenticator{err: authn.ErrNoCredentials, mode: "local"},
	})

	logs := captureLogs(t)
	withFailingCSRFRand(t)

	w := doRequest(t, s, http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"alice","password":"s3cret!"}`), nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("login with csrf mint failure = %d, want 500; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(logs.String(), "aborting session") {
		t.Errorf("expected an error log naming the aborted session, got:\n%s", logs.String())
	}

	if key := kv.lastKey(); key == "" {
		t.Fatal("no session was ever Set in the store -- test setup is broken")
	} else if _, ok, getErr := kv.Get(context.Background(), key); getErr != nil || ok {
		t.Errorf("session key %q still present after a csrf-mint failure (ok=%v err=%v) -- Delete did not run", key, ok, getErr)
	}

	var sessionCleared bool
	for _, c := range w.Result().Cookies() {
		if c.Name == cfg.Auth.Session.CookieName && c.MaxAge < 0 {
			sessionCleared = true
		}
	}
	if !sessionCleared {
		t.Error("login with csrf mint failure did not clear the session cookie client-side")
	}
}

// TestCSRFMintFailureOnOIDCCallbackAbortsSessionAndReturns500 is
// TestCSRFMintFailureOnLoginAbortsSessionAndReturns500's oidc-callback counterpart; the session id
// here is a REAL one, pre-created directly against the same SessionStore the server is wired with
// (standing in for what s.oidc.Callback would have just created).
func TestCSRFMintFailureOnOIDCCallbackAbortsSessionAndReturns500(t *testing.T) {
	sessions := authn.NewSessionStore(cache.NewInProcessKV(), time.Hour, 0)
	sessionID, err := sessions.Create(context.Background(), authn.Session{Username: "alice"})
	if err != nil {
		t.Fatal(err)
	}

	cfg := authTestConfig("oidc")
	reg := prometheus.NewRegistry()
	m := metrics.New(cfg.MetricsPrefix, reg)
	ui := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("spa")) })
	flow := fakeOIDCFlow{sessionID: sessionID, returnTo: "/after"}
	s := NewServer(Deps{Config: cfg, Metrics: m, PromRegistry: reg, UI: ui, OIDC: flow, Sessions: sessions})

	logs := captureLogs(t)
	withFailingCSRFRand(t)

	w := doRequest(t, s, http.MethodGet, config.OIDCCallbackPath+"?state=s&code=c", nil, withOIDCState("s"))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("oidc callback with csrf mint failure = %d, want 500; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(logs.String(), "aborting session") {
		t.Errorf("expected an error log naming the aborted session, got:\n%s", logs.String())
	}
	if _, ok, getErr := sessions.Get(context.Background(), sessionID); getErr != nil || ok {
		t.Errorf("session %q still resolves after a csrf-mint failure on oidc callback (ok=%v err=%v) -- Delete did not run", sessionID, ok, getErr)
	}

	var sessionCleared bool
	for _, c := range w.Result().Cookies() {
		if c.Name == cfg.Auth.Session.CookieName && c.MaxAge < 0 {
			sessionCleared = true
		}
	}
	if !sessionCleared {
		t.Error("oidc callback with csrf mint failure did not clear the session cookie client-side")
	}
}

// --- Task 18: oidc mode + non-default cookie name no longer needs a boot warning ---

func TestOIDCNonDefaultCookieNameNoLongerWarnsAtBoot(t *testing.T) {
	logs := captureLogs(t)

	cfg := authTestConfig("oidc")
	cfg.Auth.Session.CookieName = "not-the-oidc-default"
	reg := prometheus.NewRegistry()
	m := metrics.New(cfg.MetricsPrefix, reg)
	ui := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("spa")) })

	NewServer(Deps{Config: cfg, Metrics: m, PromRegistry: reg, UI: ui})

	if got := logs.String(); strings.Contains(got, "auth.mode=oidc") {
		t.Errorf("NewServer logged an auth.mode=oidc warning for a non-default cookie name, want none "+
			"(Task 18 fixed NewOIDC to honor it):\n%s", got)
	}
}

// --- M-10: handleOIDCStart error mapping ----------------------------------

func TestOIDCStartMapsBadReturnToTo400AndOtherFailuresTo500(t *testing.T) {
	cfg := authTestConfig("oidc")
	reg := prometheus.NewRegistry()
	m := metrics.New(cfg.MetricsPrefix, reg)
	ui := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("spa")) })

	t.Run("bad returnTo is 400, never reaches the flow", func(t *testing.T) {
		flow := fakeOIDCFlow{authorizeErr: errors.New("AuthorizeURL must not be called for an unsafe returnTo")}
		s := NewServer(Deps{Config: cfg, Metrics: m, PromRegistry: reg, UI: ui, OIDC: flow})

		w := doRequest(t, s, http.MethodGet, "/api/v1/auth/oidc/start?returnTo="+url.QueryEscape("http://evil.example/"), nil, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("oidc start with an unsafe returnTo = %d, want 400; body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("flow failure is 500 with a generic, logged detail", func(t *testing.T) {
		logs := captureLogs(t)
		flow := fakeOIDCFlow{authorizeErr: errors.New("store state: dial tcp 10.0.0.1:6379: connect: connection refused")}
		s := NewServer(Deps{Config: cfg, Metrics: m, PromRegistry: reg, UI: ui, OIDC: flow})

		w := doRequest(t, s, http.MethodGet, "/api/v1/auth/oidc/start", nil, nil)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("oidc start with a flow failure = %d, want 500; body=%s", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "10.0.0.1") {
			t.Errorf("500 body leaked the underlying error detail: %s", w.Body.String())
		}
		if !strings.Contains(logs.String(), "oidc authorize url failed") {
			t.Errorf("expected the flow failure to be logged, got:\n%s", logs.String())
		}
	})
}

// --- M-11: anonymous mode does not increment AuthRequests -----------------

func TestAnonymousModeDoesNotIncrementAuthRequests(t *testing.T) {
	cfg := authTestConfig("anonymous")
	reg := prometheus.NewRegistry()
	m := metrics.New(cfg.MetricsPrefix, reg)
	ui := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("spa")) })
	s := NewServer(Deps{Config: cfg, Metrics: m, PromRegistry: reg, UI: ui})

	for range 3 {
		doRequest(t, s, http.MethodGet, "/api/v1/version", nil, nil)
	}

	if got := testutil.ToFloat64(m.AuthRequests.WithLabelValues("anonymous", "ok")); got != 0 {
		t.Errorf("AuthRequests(anonymous, ok) = %v after 3 requests, want 0", got)
	}
}

/** newLocalAuthServer wires a local-mode server with one known user, for the login-shape cases. */
func newLocalAuthServer(t *testing.T) *Server {
	t.Helper()
	hash, err := authn.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	users := fakeUserStore{users: map[string]store.User{
		"alice": {ID: "u-1", Username: "alice", PasswordHash: hash, DisplayName: "Alice"},
	}}
	cfg := authTestConfig("local")
	reg := prometheus.NewRegistry()
	return NewServer(Deps{
		Config: cfg, Metrics: metrics.New(cfg.MetricsPrefix, reg), PromRegistry: reg,
		UI:            http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("spa")) }),
		Users:         users,
		Sessions:      authn.NewSessionStore(cache.NewInProcessKV(), time.Hour, 0),
		Authenticator: fakeAuthenticator{err: authn.ErrNoCredentials, mode: "local"},
	})
}

/* ── the login is a state change on the victim's browser ─────────────────── */

/*
 * The route is public, so authorize applies no CSRF gate to it. Without a same-origin proof,
 * evil.example could post a cross-site form (enctype=text/plain, whose body parses as this JSON) and
 * the 204's Set-Cookie would replace the victim's session with the ATTACKER's: everything the victim
 * then wrote went into the attacker's account, and everything they read came out of it.
 */
func TestLoginRefusesACrossOriginPost(t *testing.T) {
	s := newLocalAuthServer(t)

	body := `{"username":"alice","password":"correct horse battery staple"}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin login = %d, want 403: %s", rec.Code, rec.Body)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Errorf("cross-origin login set cookies: %+v", rec.Result().Cookies())
	}
}

func TestLoginRefusesAFormContentType(t *testing.T) {
	s := newLocalAuthServer(t)

	// A browser form can only ever send one of three content types, and none of them is JSON — so
	// requiring JSON is what closes the enctype=text/plain vector on its own.
	body := `{"username":"alice","password":"correct horse battery staple"}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain login = %d, want 415: %s", rec.Code, rec.Body)
	}
}

// A same-origin browser post, and a non-browser client that sends no Origin at all, both pass.
func TestLoginAcceptsSameOriginAndOriginlessClients(t *testing.T) {
	for _, name := range []string{"same-origin", "no origin header"} {
		t.Run(name, func(t *testing.T) {
			s := newLocalAuthServer(t)
			body := `{"username":"alice","password":"correct horse battery staple"}`
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/auth/login", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			if name == "same-origin" {
				req.Header.Set("Origin", "http://"+req.Host)
			}
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)

			if rec.Code == http.StatusForbidden || rec.Code == http.StatusUnsupportedMediaType {
				t.Fatalf("login = %d, want it admitted to the credential check: %s", rec.Code, rec.Body)
			}
		})
	}
}

/*
 * Anonymous mode is not a licence for cross-site writes.
 *
 * The CSRF exemption for anonymous mode was unconditional, and anonymous.role may be operator or
 * admin. A CORS *simple* request (text/plain body, no custom header) triggers no preflight, so any
 * page an operator's browser visited could POST into a console that was deliberately kept off the
 * internet. The attacker never reads the response; the write still lands.
 */
func TestAnonymousModeRefusesCrossSiteMutations(t *testing.T) {
	cfg := &config.Config{HTTPPort: 8080, LogLevel: "info", LogFormat: "json", MetricsPrefix: "kconmon_ng",
		Auth: config.AuthConfig{Mode: "anonymous", Anonymous: config.AnonymousConfig{Role: "admin"}}}
	reg := prometheus.NewRegistry()
	ui := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	s := NewServer(Deps{Config: cfg, Metrics: metrics.New(cfg.MetricsPrefix, reg), PromRegistry: reg, UI: ui,
		Authenticator: authn.NewAnonymous("admin")})

	newReq := func() *http.Request {
		r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/rbac/roles",
			strings.NewReader(`{"name":"csrf-role","permissions":[]}`))
		r.Header.Set("Content-Type", "application/json")
		return r
	}

	// A browser cross-site request: refused before it reaches a handler.
	cross := newReq()
	cross.Header.Set("Origin", "https://attacker.example")
	cross.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, cross)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "CSRF") {
		t.Errorf("cross-site POST = %d, want 403 from the CSRF gate; body=%s", w.Code, w.Body.String())
	}

	// A non-browser client sends neither header and is never stopped by the CSRF gate. (This fixture
	// wires no roleAdmin, so the write itself fails downstream -- the point is which refusal it is.)
	plain := newReq()
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, plain)
	if strings.Contains(w.Body.String(), "CSRF") {
		t.Errorf("header-less POST was refused by the CSRF gate; a script that sends no Origin must pass it: %s", w.Body.String())
	}
}

/*
 * auth.groupRoles is the DECLARATIVE grant, and it is what makes an OIDC install usable from cold.
 *
 * Before it there were two ways to hold a role and neither worked at deploy time: a row in
 * role_bindings, created through an API that already needs rbac:manage, and auth.defaultRole, which
 * is one role for every authenticated subject. A fresh database therefore had nobody able to make
 * the first binding, and the documented way out was to bring the console up in local mode, log in,
 * create the binding by hand and only then switch to oidc.
 */
func TestGroupRolesGrantFromTheClaimWithNoBindingInTheDatabase(t *testing.T) {
	cfg := authTestConfig("oidc")
	cfg.Auth.GroupRoles = map[string]string{"kconmon_admin": "admin", "kconmon_viewer": "viewer"}
	reg := prometheus.NewRegistry()
	authr := fakeAuthenticator{subject: authz.Subject{
		Kind: authz.SubjectUser, ID: "oidc:abc", Groups: []string{"unmapped", "kconmon_admin"},
	}}
	ui := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("spa")) })
	s := NewServer(Deps{
		Config: cfg, Metrics: metrics.New(cfg.MetricsPrefix, reg), PromRegistry: reg, UI: ui,
		Authenticator: authr, Policy: authz.NewPolicy(nil),
	})

	got := s.resolveRoles(context.Background(), authr.subject)
	if !slices.Contains(got.Roles, "admin") {
		t.Errorf("roles = %v, want admin from the kconmon_admin group", got.Roles)
	}
	// A group with no mapping contributes nothing -- the map is an allow-list, not a passthrough.
	if slices.Contains(got.Roles, "unmapped") {
		t.Errorf("roles = %v, want no role for a group the mapping does not name", got.Roles)
	}
}

/*
 * And it survives an unreadable role store.
 *
 * The store's half still fails CLOSED -- an unreadable database is no evidence a subject holds
 * anything -- but a grant that came from the claim and the config was never in doubt, and a database
 * outage is the moment an operator is most likely to need the console.
 */
func TestGroupRolesSurviveARoleStoreOutage(t *testing.T) {
	cfg := authTestConfig("oidc")
	cfg.Auth.GroupRoles = map[string]string{"kconmon_admin": "admin"}
	cfg.Auth.DefaultRole = "viewer"
	reg := prometheus.NewRegistry()
	authr := fakeAuthenticator{subject: authz.Subject{
		Kind: authz.SubjectUser, ID: "oidc:abc", Groups: []string{"kconmon_admin"},
	}}
	ui := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("spa")) })
	s := NewServer(Deps{
		Config: cfg, Metrics: metrics.New(cfg.MetricsPrefix, reg), PromRegistry: reg, UI: ui,
		Authenticator: authr, Policy: authz.NewPolicy(nil),
		Roles: failingRoleResolver{},
	})

	got := s.resolveRoles(context.Background(), authr.subject)
	if !slices.Contains(got.Roles, "admin") {
		t.Errorf("roles = %v, want the config-derived admin to survive the outage", got.Roles)
	}
	// The DEFAULT role must not be handed out on a store error; that is the fail-closed half.
	if slices.Contains(got.Roles, "viewer") {
		t.Errorf("roles = %v, want no defaultRole on a store error", got.Roles)
	}
}

// The union: a binding made by hand adds to what the provider's groups already grant.
func TestGroupRolesUnionWithDatabaseBindings(t *testing.T) {
	cfg := authTestConfig("oidc")
	cfg.Auth.GroupRoles = map[string]string{"kconmon_viewer": "viewer"}
	reg := prometheus.NewRegistry()
	authr := fakeAuthenticator{subject: authz.Subject{
		Kind: authz.SubjectUser, ID: "oidc:abc", Groups: []string{"kconmon_viewer"},
	}}
	ui := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("spa")) })
	s := NewServer(Deps{
		Config: cfg, Metrics: metrics.New(cfg.MetricsPrefix, reg), PromRegistry: reg, UI: ui,
		Authenticator: authr, Policy: authz.NewPolicy(nil),
		Roles: fakeRoleResolver{roles: []string{"alert-editor"}},
	})

	got := s.resolveRoles(context.Background(), authr.subject)
	for _, want := range []string{"viewer", "alert-editor"} {
		if !slices.Contains(got.Roles, want) {
			t.Errorf("roles = %v, want %q: the two sources are a union, not a replacement", got.Roles, want)
		}
	}
}

// failingRoleResolver stands in for an unreachable database.
type failingRoleResolver struct{}

func (failingRoleResolver) RolesFor(context.Context, authz.Subject) ([]string, error) { //nolint:gocritic // Subject is a value type by design
	return nil, errors.New("role store unreachable")
}

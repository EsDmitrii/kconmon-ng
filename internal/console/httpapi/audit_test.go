package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// fakeAuditStore is an Auditor test double: InsertAuditEntry records every
// call (mutex-guarded -- the drain goroutine calls it from a different
// goroutine than the test), ListAuditEntries replays them newest-first.
type fakeAuditStore struct {
	mu      sync.Mutex
	entries []store.AuditEntry
	nextID  int64
}

func (f *fakeAuditStore) InsertAuditEntry(_ context.Context, subjectKind, subjectID, action, resource, outcome, remoteAddr string, detail json.RawMessage) (store.AuditEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	e := store.AuditEntry{
		ID: f.nextID, At: time.Now(), SubjectKind: subjectKind, SubjectID: subjectID,
		Action: action, Resource: resource, Outcome: outcome, RemoteAddr: remoteAddr,
		Detail: append(json.RawMessage(nil), detail...),
	}
	f.entries = append(f.entries, e)
	return e, nil
}

func (f *fakeAuditStore) ListAuditEntries(_ context.Context, filter store.AuditFilter) (store.AuditPage, error) { //nolint:gocritic // hugeParam: test double mirrors the store signature
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.AuditEntry, len(f.entries))
	for i := range f.entries {
		out[len(f.entries)-1-i] = f.entries[i] // newest first
	}
	if limit := filter.Limit; limit > 0 && limit < len(out) {
		out = out[:limit]
	}
	return store.AuditPage{Entries: out}, nil
}

func (f *fakeAuditStore) snapshot() []store.AuditEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.AuditEntry, len(f.entries))
	copy(out, f.entries)
	return out
}

// waitForOneAuditEntry polls fakeAuditStore until it holds at least one
// entry (the drain goroutine writes asynchronously, off the request path)
// or fails the test after a generous bound.
func waitForOneAuditEntry(t *testing.T, fs *fakeAuditStore) []store.AuditEntry {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		entries := fs.snapshot()
		if len(entries) >= 1 {
			return entries
		}
		if time.Now().After(deadline) {
			t.Fatalf("audit store has %d entries after 2s, want >= 1", len(entries))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// stalledAuditStore is an Auditor whose InsertAuditEntry blocks until the
// test releases it -- used to fill the async write buffer deterministically
// for TestAuditFullBufferDropsAndCounts.
type stalledAuditStore struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newStalledAuditStore() *stalledAuditStore {
	return &stalledAuditStore{started: make(chan struct{}), release: make(chan struct{})}
}

func (f *stalledAuditStore) InsertAuditEntry(context.Context, string, string, string, string, string, string, json.RawMessage) (store.AuditEntry, error) {
	f.once.Do(func() { close(f.started) })
	<-f.release
	return store.AuditEntry{}, nil
}

func (f *stalledAuditStore) ListAuditEntries(context.Context, store.AuditFilter) (store.AuditPage, error) { //nolint:gocritic // hugeParam: test double mirrors the store signature
	return store.AuditPage{}, nil
}

// mutateWithCSRF adds the double-submit CSRF pair TestRoutePermissionTable's
// helper also adds -- a SubjectUser mutating request needs it regardless of
// the permission decision (csrfOK, middleware_auth.go).
func mutateWithCSRF(r *http.Request) {
	r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "tok-1"})
	r.Header.Set(csrfHeaderName, "tok-1")
}

// newAuditTestServer wires a Server holding perms (granted to role "tester") plus the given Auditor
// and any extra optional deps (RBAC/Tokens) the audit-detail-allowlist tests below need to exercise
// a real mutating route.
func newAuditTestServer(t *testing.T, audit Auditor, perms []authz.Permission, extra Deps) *Server { //nolint:gocritic // hugeParam: test helper
	t.Helper()
	policy := authz.NewPolicy(map[string][]authz.Permission{"tester": perms})
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u1"}}
	extra.Roles = fakeRoleResolver{roles: []string{"tester"}}
	extra.Audit = audit
	return newAuthzServer(t, authr, policy, extra)
}

func TestAuditWithoutStoreReturns503(t *testing.T) {
	s := newAuditTestServer(t, nil, []authz.Permission{authz.PermAuditRead}, Deps{})
	w := doRequest(t, s, http.MethodGet, "/api/v1/audit", nil, nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /api/v1/audit without a store = %d, want 503", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
}

func TestAuditListReturnsEntriesNewestFirst(t *testing.T) {
	fs := &fakeAuditStore{}
	s := newAuditTestServer(t, fs, []authz.Permission{authz.PermAuditRead}, Deps{})

	// Seed directly (not through the middleware) -- this test is about the
	// READ side only.
	ctx := context.Background()
	_, _ = fs.InsertAuditEntry(ctx, "user", "u1", "POST /api/v1/rbac/roles", "", "allowed", "", nil)
	_, _ = fs.InsertAuditEntry(ctx, "user", "u1", "DELETE /api/v1/tokens/{id}", "tok-1", "allowed", "", nil)

	w := doRequest(t, s, http.MethodGet, "/api/v1/audit", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var body struct {
		Entries []struct {
			Action string `json:"action"`
		} `json:"entries"`
		NextCursor string `json:"nextCursor"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Entries) != 2 || body.Entries[0].Action != "DELETE /api/v1/tokens/{id}" {
		t.Fatalf("entries = %+v, want the newer row first", body.Entries)
	}
}

func TestAuditInvalidCursorReturns400(t *testing.T) {
	fs := &fakeAuditStore{}
	s := newAuditTestServer(t, fs, []authz.Permission{authz.PermAuditRead}, Deps{})
	w := doRequest(t, s, http.MethodGet, "/api/v1/audit?cursor=not-a-cursor", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", w.Code, w.Body)
	}
}

// TestAuditMutationWritesOneRow is the first failing test.
func TestAuditMutationWritesOneRow(t *testing.T) {
	fs := &fakeAuditStore{}
	roleAdmin := newFakeRoleAdmin()
	s := newAuditTestServer(t, fs, []authz.Permission{authz.PermRBACManage}, Deps{RBAC: roleAdmin})

	w := doRequest(t, s, http.MethodPost, "/api/v1/rbac/roles",
		strings.NewReader(`{"name":"custom-1","permissions":["topology:read"]}`), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}

	entries := waitForOneAuditEntry(t, fs)
	if len(entries) != 1 {
		t.Fatalf("audit entries = %d, want exactly 1", len(entries))
	}
	e := entries[0]
	if e.Action != "POST /api/v1/rbac/roles" {
		t.Errorf("action = %q, want the route pattern", e.Action)
	}
	if e.SubjectKind != string(authz.SubjectUser) || e.SubjectID != "u1" {
		t.Errorf("subject = %s/%s, want user/u1", e.SubjectKind, e.SubjectID)
	}
	if e.Outcome != auditOutcomeAllowed {
		t.Errorf("outcome = %q, want %q", e.Outcome, auditOutcomeAllowed)
	}
}

// TestAuditDeniedGETIsAudited is the brief's second failing test: a 403 on
// a GET is audited with outcome=denied.
func TestAuditDeniedGETIsAudited(t *testing.T) {
	fs := &fakeAuditStore{}
	// Grant a permission unrelated to topology:read, so GET /api/v1/topology
	// is denied.
	s := newAuditTestServer(t, fs, []authz.Permission{authz.PermAuditRead}, Deps{})

	w := doRequest(t, s, http.MethodGet, "/api/v1/topology", nil, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403: %s", w.Code, w.Body)
	}

	entries := waitForOneAuditEntry(t, fs)
	e := entries[0]
	if e.Action != "GET /api/v1/topology" {
		t.Errorf("action = %q, want GET /api/v1/topology", e.Action)
	}
	if e.Outcome != auditOutcomeDenied {
		t.Errorf("outcome = %q, want %q", e.Outcome, auditOutcomeDenied)
	}
}

// TestAuditSuccessfulGETIsNotAudited is the brief's third failing test:
// a successful GET is not audited (volume).
func TestAuditSuccessfulGETIsNotAudited(t *testing.T) {
	fs := &fakeAuditStore{}
	s := newAuditTestServer(t, fs, []authz.Permission{authz.PermAuditRead}, Deps{})

	w := doRequest(t, s, http.MethodGet, "/api/v1/audit", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}

	// Nothing SHOULD ever be enqueued for a successful GET (isMutatingMethod
	// gates auditMutation entirely) -- a short bounded wait is enough to
	// catch a regression without an indefinite sleep.
	time.Sleep(50 * time.Millisecond)
	if got := fs.snapshot(); len(got) != 0 {
		t.Errorf("audit entries = %+v, want none for a successful GET", got)
	}
}

// TestAuditDetailAllowlistDropsSecrets is the fourth failing test: detail never contains an
// allow-list-violating key.
func TestAuditDetailAllowlistDropsSecrets(t *testing.T) {
	fs := &fakeAuditStore{}
	roleAdmin := newFakeRoleAdmin()
	s := newAuditTestServer(t, fs, []authz.Permission{authz.PermRBACManage}, Deps{RBAC: roleAdmin})

	body := `{"name":"custom-2","permissions":["topology:read"],"password":"hunter2","token":"kcm_leak","query":"up{secret=\"x\"}"}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/rbac/roles", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}

	entries := waitForOneAuditEntry(t, fs)
	detail := string(entries[0].Detail)
	for _, forbidden := range []string{"password", "hunter2", "token", "kcm_leak", "query", "secret"} {
		if strings.Contains(detail, forbidden) {
			t.Errorf("detail = %s, must not contain %q", detail, forbidden)
		}
	}
	if !strings.Contains(detail, `"name":"custom-2"`) {
		t.Errorf("detail = %s, want the allow-listed \"name\" to survive", detail)
	}
}

// TestAuditPromQLDetailIsAlwaysEmpty pins the explicit example: a PromQL query string must never
// reach detail.
func TestAuditPromQLDetailIsAlwaysEmpty(t *testing.T) {
	fs := &fakeAuditStore{}
	s := newAuditTestServer(t, fs, []authz.Permission{authz.PermPromQLQuery}, Deps{})

	// No Prometheus client wired -- handlePromQLQuery answers 503, but
	// authorize's audit hook runs regardless of the handler's own outcome
	// (outcome becomes "error" for a >=400 status, still one row).
	body := `{"query":"up{very_secret_label=\"leak-me\"}"}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/promql/query", strings.NewReader(body), mutateWithCSRF)
	if w.Code == http.StatusForbidden || w.Code == http.StatusUnauthorized {
		t.Fatalf("status %d, want to pass authorization", w.Code)
	}

	entries := waitForOneAuditEntry(t, fs)
	if string(entries[0].Detail) != "{}" {
		t.Errorf("detail = %s, want {} (PromQL routes have no allowlist entry)", entries[0].Detail)
	}
}

// TestAuditFullBufferDropsAndCounts is the fifth failing test: a full audit buffer drops and counts
// rather than blocking.
func TestAuditFullBufferDropsAndCounts(t *testing.T) {
	fs := newStalledAuditStore()
	s := newAuditTestServer(t, fs, []authz.Permission{}, Deps{})
	// auth.mode=local in authTestConfig, but logout is a public route with no permission decision at
	// all.
	post := func(r *http.Request) { mutateWithCSRF(r) }

	// First request: the drain goroutine dequeues it and blocks inside
	// InsertAuditEntry -- wait for that so the buffer's exact remaining
	// capacity is deterministic.
	w := doRequest(t, s, http.MethodPost, "/api/v1/auth/logout", strings.NewReader(`{}`), post)
	if w.Code != http.StatusNoContent {
		t.Fatalf("logout status %d, want 204", w.Code)
	}
	select {
	case <-fs.started:
	case <-time.After(2 * time.Second):
		t.Fatal("drain goroutine never entered InsertAuditEntry")
	}
	defer close(fs.release)

	before := testutil.ToFloat64(s.metrics.AuditDropped.WithLabelValues())

	start := time.Now()
	for i := 0; i < auditBufferSize+8; i++ {
		w := doRequest(t, s, http.MethodPost, "/api/v1/auth/logout", strings.NewReader(`{}`), post)
		if w.Code != http.StatusNoContent {
			t.Fatalf("logout[%d] status %d, want 204", i, w.Code)
		}
	}
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Errorf("burst of %d requests took %s, want it to stay fast (non-blocking send)", auditBufferSize+8, elapsed)
	}

	after := testutil.ToFloat64(s.metrics.AuditDropped.WithLabelValues())
	if after <= before {
		t.Errorf("AuditDropped counter did not increase (before=%v after=%v) -- buffer overflow was not counted", before, after)
	}
}

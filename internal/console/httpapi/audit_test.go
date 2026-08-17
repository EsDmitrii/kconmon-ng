package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
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
	// block, when non-nil, holds the drain goroutine inside InsertAuditEntry so a test can observe
	// the buffer under pressure rather than racing it.
	block chan struct{}
}

func (f *fakeAuditStore) InsertAuditEntry(_ context.Context, subjectKind, subjectID, action, resource, outcome, remoteAddr string, detail json.RawMessage) (store.AuditEntry, error) {
	if f.block != nil {
		<-f.block
	}
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
// a successful GET is not audited (volume). The privileged reads named in auditedReads are the
// exception — see TestAuditExportIsAudited.
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

/*
A caller must not be able to erase its own audit row, and %00 was how.

auditResource copied the path parameter verbatim into audit_log.resource; PostgreSQL cannot store a
NUL in text, so the INSERT failed and the drain logged a warning. The sharp case is a DENIED probe
of a sensitive route — middleware_auth records denials through the same function — so exactly the
rows auditSensitiveRoute promises are never dropped were the ones a caller could delete at will.

Two layers now: the path never carries a control character past the door (rejectControlPath), and if
one ever reaches here it is substituted rather than allowed to make the row unstorable.
*/
func TestAuditResourceCannotBeMadeUnstorable(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"clean", "role-a", "role-a"},
		{"nul", "role\x00a", "role\uFFFDa"},
		{"newline", "role\na", "role\uFFFDa"},
	} {
		if got := sanitizeAuditText(tc.in); got != tc.want {
			t.Errorf("%s: sanitizeAuditText(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

/*
And the request never gets that far: a control character in the PATH is a 400 at the door.

Before this, DELETE /api/v1/rbac/roles/x%00 was served as 502 "rbac unavailable" — a client's own
input reported as an outage of the RBAC backend, with an ERROR log line to match — and its audit row
vanished. The same shape applied to tokens, targets, checks, schedules, incidents and maintenance.
*/
func TestControlCharacterInThePathIs400(t *testing.T) {
	s := newRBACTestServer(t, newFakeRoleAdmin())
	/* PERCENT-ENCODED, which is how it travels: net/http decodes %00 into a literal NUL in
	   URL.Path, and that is exactly the byte the guard is there to catch. A raw control byte cannot
	   even be put in a request target — net/url refuses to parse it — so the encoded form is not a
	   convenience here, it is the only reachable shape. */
	for _, path := range []string{
		"/api/v1/rbac/roles/role%00a",
		"/api/v1/tokens/11111111-1111-1111-1111-111111111111%00",
		"/api/v1/rbac/roles/role%0Aa",
	} {
		w := doRequest(t, s, http.MethodDelete, path, nil, mutateWithCSRF)
		if w.Code != http.StatusBadRequest {
			t.Errorf("DELETE %q = %d, want 400 (got body %s)", path, w.Code, w.Body)
		}
	}
	// A clean path is untouched.
	w := doRequest(t, s, http.MethodDelete, "/api/v1/rbac/roles/does-not-exist", nil, mutateWithCSRF)
	if w.Code != http.StatusNotFound {
		t.Errorf("a clean path = %d, want 404", w.Code)
	}
}

/*
boundAuditValue sliced the DECODED string with a bound taken from the ENCODED length.

A body value made of escapes shrinks by up to 6x when decoded, so it passed the size check and then
sliced a much shorter string at an index past its end: a panic, on the audit path, reachable by an
unauthenticated request (the login route is audited).
*/
func TestBoundAuditValueSurvivesAnEscapeHeavyValue(t *testing.T) {
	// ~5 KB encoded, ~830 characters decoded: exactly the mismatch.
	encoded, err := json.Marshal(strings.Repeat("A", 830))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	escaped := []byte(`"` + strings.Repeat(`\u0041`, 830) + `"`)
	if len(escaped) <= auditValueMaxBytes {
		t.Fatalf("setup: escaped value is %d bytes, needs to exceed auditValueMaxBytes (%d)",
			len(escaped), auditValueMaxBytes)
	}

	got := boundAuditValue(json.RawMessage(escaped))
	var s string
	if err := json.Unmarshal(got, &s); err != nil {
		t.Fatalf("result is not a JSON string: %v (%s)", err, got)
	}
	if len(s) == 0 {
		t.Error("the whole value was dropped")
	}
	// And the ordinary case still truncates.
	_ = encoded
	long, _ := json.Marshal(strings.Repeat("B", auditValueMaxBytes*2))
	out := boundAuditValue(long)
	if len(out) > auditValueMaxBytes {
		t.Errorf("a genuinely long value was not truncated: %d bytes", len(out))
	}
}

/* ── who yields when the audit buffer is full ────────────────────────────── */

/*
 * A credential-less 401 is the cheapest row in the system to produce: no database, no crypto, and
 * every non-public route enqueues one. Nothing rate-limits those routes, so a flood of them kept the
 * 64-slot buffer full and an admin's DELETE of a role binding — the most sensitive operation in the
 * console — was dropped in the same instant, leaving a warning line that carries neither the subject
 * nor the resource. Those rows yield the second half of the buffer instead.
 */
func TestUnauthenticatedDenialsYieldTheAuditBufferToRealMutations(t *testing.T) {
	audit := &fakeAuditStore{block: make(chan struct{})}
	authr := fakeAuthenticator{err: authn.ErrNoCredentials, mode: "local"}
	s := newAuthzServer(t, authr, authz.NewPolicy(nil), Deps{Audit: audit})

	/* Enough credential-less denials to overrun the buffer several times over; the drain is blocked,
	   so nothing leaves it. One row is in flight inside the blocked Insert, which is why the check
	   below allows for it. */
	for range auditBufferSize * 3 {
		doRequest(t, s, http.MethodGet, "/api/v1/topology", nil, nil)
	}

	if got := len(s.auditCh); got > auditBufferSize/2 {
		t.Fatalf("audit buffer holds %d of %d after a denial flood, want at most half left standing",
			got, auditBufferSize)
	}
	close(audit.block)
}

/* ── QA round 5: the highest-value READ left no trace ─────────────────────── */

/*
 * GET /api/v1/export hands the caller every probe address, every webhook URL, every alert
 * expression, and — for a caller holding rbac:manage — the whole role map, in one file. Auditing was
 * keyed on the HTTP verb, so a stolen token could take all of it and the log an operator
 * investigates with showed nothing at all.
 */
func TestAuditExportIsAudited(t *testing.T) {
	fs := &fakeAuditStore{}
	s := newAuditTestServer(t, fs, []authz.Permission{authz.PermSettingsWrite}, Deps{})

	w := doRequest(t, s, http.MethodGet, "/api/v1/export", nil, nil)
	if w.Code != http.StatusOK && w.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /api/v1/export = %d: %s", w.Code, w.Body)
	}

	entries := waitForOneAuditEntry(t, fs)
	if entries[0].Action != "GET /api/v1/export" {
		t.Errorf("action = %q, want GET /api/v1/export", entries[0].Action)
	}
}

/* ── QA round 5: a denial flood must not evict the rows that matter ───────── */

/*
 * The first cut of the yield rule keyed on `subject.Kind == ""`, which exempted every credentialed
 * subject — and in anonymous mode, where Kind is "anonymous", exempted everyone. A denial now yields
 * on what the ROW is, and a denial on a sensitive route keeps the full buffer.
 */
func TestAuditDenialsYieldRegardlessOfSubjectKind(t *testing.T) {
	for _, kind := range []authz.SubjectKind{"", authz.SubjectAnonymous, authz.SubjectToken, authz.SubjectUser} {
		t.Run(string(kind)+"/ordinary route yields", func(t *testing.T) {
			if !auditYields(auditOutcomeDenied, "/api/v1/topology") {
				t.Error("an ordinary denial does not yield the buffer half")
			}
		})
	}
	if auditYields(auditOutcomeDenied, "/api/v1/rbac/roles") {
		t.Error("a denied RBAC attempt yields the buffer; those are the rows an investigation needs")
	}
	if auditYields(auditOutcomeAllowed, "/api/v1/topology") {
		t.Error("an ALLOWED row yields; only denials may")
	}
}

// auditYields mirrors recordAudit's drop condition for a half-full buffer.
func auditYields(outcome, pattern string) bool {
	return outcome == auditOutcomeDenied && !auditSensitiveRoute(pattern)
}

/* ── QA round 5: a caller must not be able to delete their own audit row ──── */

/*
 * PostgreSQL rejects a NUL inside jsonb (22P05), and the detail is copied out of the request body.
 * Appending the escape to an allow-listed field therefore made the row unwritable and the action
 * left no trace at all.
 */
func TestAuditDetailStripsNULEscapes(t *testing.T) {
	body := []byte(`{"name":"webhook\u0000","url":"https://example.test"}`)
	detail := auditDetailFor("POST /api/v1/webhooks", body)

	if bytes.Contains(bytes.ToLower(detail), []byte(`\u0000`)) {
		t.Fatalf("detail still carries a NUL escape, so the row cannot be inserted: %s", detail)
	}
	// The rest of the value survives: this is a sanitisation, not a drop.
	if !bytes.Contains(detail, []byte(`"webhook"`)) {
		t.Errorf("detail = %s, want the name preserved with the escape removed", detail)
	}
}

/*
 * The audit row must describe the mutation that actually happened.
 *
 * encoding/json resolves a body key to a struct field case-INSENSITIVELY and keeps the LAST match,
 * so a handler decoding {"name":"benign","Name":"real"} acts on "real". The audit path used a
 * case-sensitive map index and recorded "benign": one request, two different names, and the only
 * record of a privileged action named an object that was never created.
 */
func TestAuditDetailFollowsGoJSONFieldMatching(t *testing.T) {
	body := []byte(`{"name":"benign","permissions":[],"Name":"real","Permissions":["rbac:manage"]}`)
	detail := auditDetailFor("POST /api/v1/rbac/roles", body)

	var got map[string]any
	if err := json.Unmarshal(detail, &got); err != nil {
		t.Fatalf("detail is not JSON: %s", detail)
	}
	if got["name"] != "real" {
		t.Errorf("detail name = %v, want the value the handler decoded (%q); detail = %s", got["name"], "real", detail)
	}
	perms, _ := got["permissions"].([]any)
	if len(perms) != 1 || perms[0] != "rbac:manage" {
		t.Errorf("detail permissions = %v, want the value the handler decoded; detail = %s", got["permissions"], detail)
	}
}

/*
 * A value whose literal characters spell a NUL escape is not a NUL.
 *
 * Deleting the six-byte sequence out of the RAW JSON matched one byte late inside an escaped
 * backslash and left a dangling backslash -- invalid JSON, so the whole detail was dropped and the
 * caller got to erase the record of their own action.
 */
func TestAuditDetailSurvivesAnEscapedBackslashBeforeU0000(t *testing.T) {
	body := []byte(`{"name":"` + `\\u0000` + `","url":"https://example.test"}`)
	detail := auditDetailFor("POST /api/v1/webhooks", body)

	var got map[string]any
	if err := json.Unmarshal(detail, &got); err != nil {
		t.Fatalf("detail is not JSON, so the row is unwritable: %s", detail)
	}
	if got["name"] != `\u0000` {
		t.Errorf("detail name = %q, want the literal text preserved; detail = %s", got["name"], detail)
	}
}

/*
 * A wide body must not become a wide slice.
 *
 * The audit middleware buffers the request body BEFORE authentication on public routes, so one
 * unauthenticated 16 MiB POST /api/v1/auth/login whose body repeats "username" a million times built
 * a million-element slice -- roughly 13x the body in live heap -- and OOM-killed the console pod.
 * The map this replaced collapsed duplicates to one entry; the slice has to keep only what it needs.
 */
func TestAuditDetailIgnoresNonAllowListedMembers(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"username":"real"`)
	for i := range 5000 {
		fmt.Fprintf(&b, `,"pad%d":"x"`, i)
	}
	b.WriteString("}")

	// Past the member bound the body is not described at all, rather than described expensively.
	detail := auditDetailFor("POST /api/v1/auth/login", []byte(b.String()))
	if !bytes.Equal(detail, emptyDetail) {
		t.Errorf("a body with 5000 members produced %s, want the empty detail", detail)
	}

	// Under the bound only the allow-listed member is retained.
	fields, err := orderedJSONFields([]byte(`{"a":1,"username":"real","b":2,"c":3}`), []string{"username"})
	if err != nil {
		t.Fatalf("orderedJSONFields: %v", err)
	}
	if len(fields) != 1 || fields[0].key != "username" {
		t.Errorf("fields = %+v, want only the allow-listed member", fields)
	}
}

/*
 * A number that does not fit a float64 must not smuggle a NUL past the scrub.
 *
 * encoding/json parses every JSON number into a float64, so a value carrying 1e999 failed to decode
 * and the error path returned the value UNCHANGED -- escape and all. PostgreSQL then refused the
 * jsonb and dropped the whole row, handing the caller back exactly the choice the scrub removes.
 */
func TestAuditDetailScrubsNULsBesideAnUnrepresentableNumber(t *testing.T) {
	body := []byte(`{"name":"x","permissions":[1e999,"a` + `\u0000` + `"]}`)
	detail := auditDetailFor("POST /api/v1/rbac/roles", body)

	if bytes.Contains(bytes.ToLower(detail), []byte(`\u0000`)) {
		t.Fatalf("detail still carries a NUL escape, so the row cannot be inserted: %s", detail)
	}
}

// And a large integer keeps its digits: json.Number never round-trips through a float64.
func TestAuditDetailPreservesALargeIntegerExactly(t *testing.T) {
	body := []byte(`{"name":"x","permissions":[12345678901234567890]}`)
	detail := auditDetailFor("POST /api/v1/rbac/roles", body)
	if !bytes.Contains(detail, []byte("12345678901234567890")) {
		t.Errorf("detail = %s, want the integer recorded verbatim", detail)
	}
}

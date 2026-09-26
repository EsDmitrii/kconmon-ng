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
	"unicode/utf8"

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

// waitForAuditEntries polls for at least n entries: the drain goroutine writes them off the request
// path.
func waitForAuditEntries(t *testing.T, fs *fakeAuditStore, n int) []store.AuditEntry {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		entries := fs.snapshot()
		if len(entries) >= n {
			return entries
		}
		if time.Now().After(deadline) {
			t.Fatalf("audit store has %d entries after 2s, want %d", len(entries), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitForOneAuditEntry is waitForAuditEntries for the one row most tests expect.
func waitForOneAuditEntry(t *testing.T, fs *fakeAuditStore) []store.AuditEntry {
	t.Helper()
	return waitForAuditEntries(t, fs, 1)
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
	for i := range auditBufferSize + 8 {
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
		{"invalid utf-8", "role\xffa", "role\uFFFDa"},
		{"truncated utf-8", "role\xc3", "role\uFFFD"},
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
		"/api/v1/rbac/roles/role%FFa",
		"/api/v1/rbac/roles/role%C3",
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
			if !auditYields(kind, auditOutcomeDenied, "/api/v1/topology") {
				t.Error("an ordinary denial does not yield the buffer half")
			}
			// A failed request after authorize passed is as cheap as a denial.
			if !auditYields(kind, auditOutcomeError, "/api/v1/promql/query") {
				t.Error("an ordinary failed request does not yield the buffer half")
			}
		})
	}
	if auditYields(authz.SubjectUser, auditOutcomeDenied, "/api/v1/rbac/roles") {
		t.Error("a denied RBAC attempt yields the buffer; those are the rows an investigation needs")
	}
	if auditYields(authz.SubjectUser, auditOutcomeAllowed, "/api/v1/topology") {
		t.Error("an ALLOWED row of a signed-in caller yields; only failed rows may")
	}
}

// auditYields reports whether recordAudit drops this row once half the buffer holds allowed rows.
func auditYields(kind authz.SubjectKind, outcome, pattern string) bool {
	ch := make(chan auditJob, auditBufferSize)
	for range auditBufferSize / 2 {
		ch <- auditJob{outcome: auditOutcomeAllowed}
	}
	cheap, sensitive := auditRowTier(outcome, kind, http.MethodPost, pattern)
	job := auditJob{outcome: outcome}
	if cheap {
		job.budget, job.failedSensitive = "addr:192.0.2.1", sensitive
	}
	if !auditQueued.admit(ch, job, auditQueueLimit(cheap, sensitive, auditBufferSize)) {
		return true
	}
	auditQueued.release(ch, job.budget, job.failedSensitive)
	return false
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
 * PostgreSQL refuses a jsonb value that is not UTF-8 (22021) or carries an unpaired surrogate escape
 * (22P02). The handler decodes such a byte to U+FFFD and stores the write, so a raw copy in the
 * detail let the caller of a privileged write decide that its audit row is never written.
 */
func TestAuditDetailIsAlwaysStorableText(t *testing.T) {
	for name, body := range map[string]string{
		"invalid byte in a string":  "{\"name\":\"ops\xff\",\"permissions\":[\"rbac:manage\"]}",
		"invalid byte in an array":  "{\"name\":\"ops\",\"permissions\":[\"rbac:manage\xfe\"]}",
		"unpaired high surrogate":   `{"name":"ops\ud800","permissions":[]}`,
		"unpaired low surrogate":    `{"name":"ops\uDC00x","permissions":[]}`,
		"high surrogate at the end": `{"name":"\ud83d","permissions":[]}`,
		"invalid byte beside a NUL": "{\"name\":\"o\\u0000ps\xff\",\"permissions\":[]}",
	} {
		t.Run(name, func(t *testing.T) {
			detail := auditDetailFor("POST /api/v1/rbac/roles", []byte(body))
			if !utf8.Valid(detail) {
				t.Fatalf("detail is not UTF-8, PostgreSQL refuses the row: %q", detail)
			}
			if bytes.Contains(bytes.ToLower(detail), []byte(`\ud8`)) || bytes.Contains(bytes.ToLower(detail), []byte(`\udc`)) {
				t.Fatalf("detail keeps an unpaired surrogate escape, PostgreSQL refuses the row: %s", detail)
			}
			var got map[string]any
			if err := json.Unmarshal(detail, &got); err != nil {
				t.Fatalf("detail is not JSON: %s", detail)
			}
			if name, _ := got["name"].(string); !strings.HasPrefix(name, "o") && !strings.Contains(name, "\uFFFD") {
				t.Errorf("detail name = %q, want the value with the bad bytes replaced", name)
			}
		})
	}
	// A paired surrogate is a real character and stays one.
	detail := auditDetailFor("POST /api/v1/rbac/roles", []byte(`{"name":"ops\ud83d\ude00","permissions":[]}`))
	var got map[string]any
	if err := json.Unmarshal(detail, &got); err != nil || got["name"] != "ops\U0001F600" {
		t.Errorf("detail = %s, want the emoji kept", detail)
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

/*
 * The three tests below pin M3-5: the Investigate timeline showed a raw "user:oidc:<uuid>" because
 * the audit table stores subject kind/id only. The display name the session already carries is
 * folded into the row's detail at WRITE time (the store schema is untouched) and lifted into a
 * top-level `subjectDisplay` field at READ time.
 */

func TestAuditMutationRecordsSubjectDisplay(t *testing.T) {
	fs := &fakeAuditStore{}
	policy := authz.NewPolicy(map[string][]authz.Permission{"tester": {authz.PermRBACManage}})
	authr := fakeAuthenticator{subject: authz.Subject{
		Kind:        authz.SubjectUser,
		ID:          "oidc:2f0d3a9c-9a67-4c11-8f3e-000000000000",
		DisplayName: "d.esin@group-ib.com",
	}}
	s := newAuthzServer(t, authr, policy, Deps{
		Roles: fakeRoleResolver{roles: []string{"tester"}},
		Audit: fs,
		RBAC:  newFakeRoleAdmin(),
	})

	w := doRequest(t, s, http.MethodPost, "/api/v1/rbac/roles",
		strings.NewReader(`{"name":"custom-1","permissions":["topology:read"]}`), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}

	entries := waitForOneAuditEntry(t, fs)
	var detail map[string]json.RawMessage
	if err := json.Unmarshal(entries[0].Detail, &detail); err != nil {
		t.Fatalf("detail: %v", err)
	}
	var display string
	if err := json.Unmarshal(detail["subjectDisplay"], &display); err != nil || display != "d.esin@group-ib.com" {
		t.Errorf("detail.subjectDisplay = %s (err %v), want the session's display name", detail["subjectDisplay"], err)
	}
	// The allow-listed body keys must survive alongside it.
	if _, ok := detail["name"]; !ok {
		t.Errorf("detail lost the allow-listed name key: %s", entries[0].Detail)
	}
}

// TestWithSubjectDisplaySkipsRedundantNames: a display name that adds nothing over the id (or is
// absent) writes nothing, so header-mode rows and anonymous rows stay exactly as they were.
func TestWithSubjectDisplaySkipsRedundantNames(t *testing.T) {
	cases := []struct {
		name, id, display string
		want              string
	}{
		{"absent", "u1", "", `{}`},
		{"same as id", "d.esin", "d.esin", `{}`},
		{"adds information", "oidc:abc", "Ada", `{"subjectDisplay":"Ada"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := withSubjectDisplay(json.RawMessage(`{}`), c.id, c.display)
			if string(got) != c.want {
				t.Errorf("withSubjectDisplay = %s, want %s", got, c.want)
			}
		})
	}
}

func TestAuditListLiftsSubjectDisplayOutOfDetail(t *testing.T) {
	fs := &fakeAuditStore{}
	s := newAuditTestServer(t, fs, []authz.Permission{authz.PermAuditRead}, Deps{})

	ctx := context.Background()
	// A pre-M3-5 row with no display recorded, then a current one carrying it.
	_, _ = fs.InsertAuditEntry(ctx, "user", "u-old", "POST /api/v1/runs", "", "allowed", "", json.RawMessage(`{"type":"tcp"}`))
	_, _ = fs.InsertAuditEntry(ctx, "user", "oidc:abc", "POST /api/v1/rbac/roles", "", "allowed", "",
		json.RawMessage(`{"name":"custom-1","subjectDisplay":"d.esin@group-ib.com"}`))

	w := doRequest(t, s, http.MethodGet, "/api/v1/audit", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var body struct {
		Entries []struct {
			SubjectDisplay string                     `json:"subjectDisplay"`
			Detail         map[string]json.RawMessage `json:"detail"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(body.Entries))
	}
	newest := body.Entries[0] // newest first
	if newest.SubjectDisplay != "d.esin@group-ib.com" {
		t.Errorf("subjectDisplay = %q, want the recorded display name", newest.SubjectDisplay)
	}
	if _, ok := newest.Detail["subjectDisplay"]; ok {
		t.Errorf("detail still carries subjectDisplay after the lift: %v", newest.Detail)
	}
	if _, ok := newest.Detail["name"]; !ok {
		t.Errorf("the lift dropped an allow-listed detail key: %v", newest.Detail)
	}
	if old := body.Entries[1]; old.SubjectDisplay != "" {
		t.Errorf("pre-capture row invented a subjectDisplay: %q", old.SubjectDisplay)
	}
}

/* ── R1: a public-route flood must not evict the rows that matter ───────── */

// Failed logins are answered 4xx (outcome "error", not "denied") and need no credentials, so they
// must yield the second half of the buffer exactly like denials do.
func TestCredentialLessLoginFloodYieldsTheAuditBuffer(t *testing.T) {
	audit := &fakeAuditStore{block: make(chan struct{})}
	authr := fakeAuthenticator{err: authn.ErrNoCredentials, mode: "local"}
	s := newAuthzServer(t, authr, authz.NewPolicy(nil), Deps{Audit: audit})

	for range auditBufferSize * 3 {
		doRequest(t, s, http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"x","password":"y"}`), nil)
	}
	if got := len(s.auditCh); got > auditBufferSize/2 {
		t.Fatalf("audit buffer holds %d of %d after a login flood, want at most half", got, auditBufferSize)
	}
	close(audit.block)
}

// A row on a sensitive route waits for room instead of being dropped when the buffer is full of
// rows that do not yield.
func TestSensitiveAuditRowWaitsForRoomInAFullBuffer(t *testing.T) {
	audit := &fakeAuditStore{block: make(chan struct{})}
	s := newAuditTestServer(t, audit, nil, Deps{})
	parkAuditDrain(t, s)
	for len(s.auditCh) < cap(s.auditCh) {
		s.auditCh <- auditJob{action: "POST /api/v1/annotations", outcome: auditOutcomeAllowed, detail: emptyDetail}
	}

	req := auditRequest(t, http.MethodPost, "/api/v1/rbac/bindings", nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.recordAudit(req, authz.Subject{Kind: authz.SubjectUser, ID: "admin"}, auditOutcomeAllowed, emptyDetail)
	}()
	// Nothing can make room while the insert is blocked, so a return now means the row was dropped.
	select {
	case <-done:
		t.Fatal("recordAudit returned while the buffer was full instead of waiting for room")
	case <-time.After(50 * time.Millisecond):
	}
	close(audit.block)
	<-done

	deadline := time.Now().Add(2 * time.Second)
	for {
		for _, e := range audit.snapshot() {
			if e.Action == "POST /api/v1/rbac/bindings" {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the RBAC binding row was dropped from a full audit buffer")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Behind a trusted proxy the row names the client the rate limiter already resolved, not the proxy.
func TestAuditRecordsTheClientBehindATrustedProxy(t *testing.T) {
	fs := &fakeAuditStore{}
	s := newAuditTestServer(t, fs, nil, Deps{})
	s.trustedProxies = parseCIDRs([]string{"10.0.0.0/8"})

	doRequest(t, s, http.MethodDelete, "/api/v1/rbac/bindings/b1", nil, func(r *http.Request) {
		mutateWithCSRF(r)
		r.RemoteAddr = "10.1.2.3:4444"
		r.Header.Set("X-Forwarded-For", "203.0.113.9")
	})
	if got := waitForOneAuditEntry(t, fs)[0].RemoteAddr; got != "203.0.113.9" {
		t.Fatalf("remoteAddr = %q, want the client 203.0.113.9", got)
	}
}

// A flood of failed logins keeps the buffer half full, and the one login that SUCCEEDS carries no
// credentials yet either. Only failures yield: the successful sign-in is the row an investigation
// of that flood needs.
func TestSuccessfulLoginRowDoesNotYieldToAFailedLoginFlood(t *testing.T) {
	for _, pattern := range []string{"/api/v1/auth/login", "/api/v1/auth/password"} {
		t.Run(pattern, func(t *testing.T) {
			audit := &fakeAuditStore{block: make(chan struct{})}
			s := newAuditTestServer(t, audit, nil, Deps{})
			parkAuditDrain(t, s)
			for len(s.auditCh) < cap(s.auditCh)/2 {
				s.auditCh <- auditJob{action: "POST " + pattern, outcome: auditOutcomeError, detail: emptyDetail}
			}

			req := auditRequest(t, http.MethodPost, pattern, nil)
			before := len(s.auditCh)
			s.recordAudit(req, authz.Subject{}, auditOutcomeAllowed, emptyDetail)
			if got := len(s.auditCh); got != before+1 {
				t.Errorf("successful %s row dropped with the buffer at %d/%d", pattern, before, cap(s.auditCh))
			}

			s.recordAudit(req, authz.Subject{}, auditOutcomeError, emptyDetail)
			if got := len(s.auditCh); got != before+1 {
				t.Errorf("a failed %s row did not yield with the buffer at %d/%d", pattern, before+1, cap(s.auditCh))
			}
			close(audit.block)
		})
	}
}

// The detail kept in memory, and logged if the write fails, is the value the row stores: decoded and
// encoded again, whatever spelling the body used.
func TestAuditDetailValueIsTheValueAsStored(t *testing.T) {
	for in, want := range map[string]string{
		`{"b" : 1, "a":"\u00e9"}`: "{\"a\":\"\u00e9\",\"b\":1}",
		`"\ud800"`:                "\"\uFFFD\"",
		`"plain"`:                 `"plain"`,
	} {
		if got := string(scrubJSONNULs(json.RawMessage(in))); got != want {
			t.Errorf("scrubJSONNULs(%s) = %s, want %s", in, got, want)
		}
	}
}

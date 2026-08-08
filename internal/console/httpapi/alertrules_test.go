package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/EsDmitrii/kconmon-ng/internal/console/alerting"
	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/promql"
	"github.com/EsDmitrii/kconmon-ng/internal/console/promrules"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// validAlertRuleBody is the body every happy-path case posts: a pair-loss
// template with both required params, which the REAL renderer turns into a
// real expression -- no fake renderer anywhere in this file, because the
// render-before-store contract is precisely what these tests exist to pin.
const validAlertRuleBody = `{"name":"EdgePairLoss","kind":"pair-loss",` +
	`"params":{"protocol":"tcp","thresholdPercent":5},` +
	`"severity":"warning","forNs":300000000000,` +
	`"labels":{"team":"net"},"annotations":{"summary":"pair loss"}}`

// ---------------------------------------------------------------------------
// Doubles
// ---------------------------------------------------------------------------

// count reports how many rules the export/import double (export_test.go's
// fakeAlertRuleStore, reused here rather than duplicated) is currently
// holding -- the assertion "a rejected write stored nothing" needs.
func (f *fakeAlertRuleStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rules)
}

// fakeRuleSyncer is the RuleSyncer double: it counts kicks (the whole of the
// seam a CRUD handler uses) and answers ListForeign from a fixture.
type fakeRuleSyncer struct {
	mu         sync.Mutex
	kicks      int
	foreign    []promrules.ForeignRule
	foreignErr error
}

func (f *fakeRuleSyncer) Kick() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kicks++
}

func (f *fakeRuleSyncer) kickCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.kicks
}

func (f *fakeRuleSyncer) ListForeign(context.Context) ([]promrules.ForeignRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.foreignErr != nil {
		return nil, f.foreignErr
	}
	return f.foreign, nil
}

// newFakePrometheus wires a real *promql.Client at an httptest server, so the
// preview and firing-list handlers exercise the SAME client production uses --
// guards, envelope handling and UpstreamError included.
func newFakePrometheus(t *testing.T, h http.HandlerFunc) *promql.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return promql.New(srv.URL, promql.Guards{
		QueryTimeout: 2 * time.Second, MaxRange: time.Hour, MaxResponseBytes: 1 << 20,
	})
}

// promVector answers any instant query with n series.
func promVector(n int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		results := make([]string, 0, n)
		for i := range n {
			results = append(results,
				`{"metric":{"instance":"`+string(rune('a'+i))+`"},"value":[1,"1"]}`)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[` +
			strings.Join(results, ",") + `]}}`))
	}
}

func alertRuleRoutes(id string) []struct{ method, path, body string } {
	return []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/alert-rules", ""},
		{http.MethodPost, "/api/v1/alert-rules", validAlertRuleBody},
		{http.MethodGet, "/api/v1/alert-rules/" + id, ""},
		{http.MethodPut, "/api/v1/alert-rules/" + id, validAlertRuleBody},
		{http.MethodDelete, "/api/v1/alert-rules/" + id, ""},
		{http.MethodPost, "/api/v1/alert-rules/" + id + "/sync", ""},
		// Import is on this list and not on the 409 one below only because it
		// hits BOTH gates: it needs the store to write rows into and the
		// reconciler to read the cluster from, and the 503 wins because a
		// console with no database has nowhere to adopt anything TO.
		{http.MethodPost, "/api/v1/alert-rules/import", `{"name":"someone-elses-rules"}`},
	}
}

// ---------------------------------------------------------------------------
// Import fixtures (M7 Decision 4)
// ---------------------------------------------------------------------------

// foreignRuleFixture builds a REAL PrometheusRule object -- spec.groups goes in
// through unstructured.SetNestedSlice, the same way promrules' own tests build
// theirs, so a fixture that is not deep-copy-JSON compatible fails here rather
// than passing a shape the apiserver would never produce.
//
// The projected counters are computed from the groups rather than passed in,
// because a fixture whose Groups/Rules disagree with its own object would let a
// handler bug hide behind a wrong number.
func foreignRuleFixture(t *testing.T, name string, groups []any) promrules.ForeignRule {
	t.Helper()
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": alerting.BundleAPIVersion,
		"kind":       alerting.BundleKind,
		"metadata":   map[string]any{"name": name, "namespace": "kconmon"},
	}}
	if err := unstructured.SetNestedSlice(obj.Object, groups, "spec", "groups"); err != nil {
		t.Fatalf("SetNestedSlice: %v -- the fixture is not deep-copy-JSON compatible", err)
	}
	total := 0
	for _, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			t.Fatalf("group %v is not a map", g)
		}
		entries, ok := gm["rules"].([]any)
		if !ok {
			continue
		}
		total += len(entries)
	}
	return promrules.ForeignRule{Name: name, Groups: len(groups), Rules: total, Object: obj}
}

func group(name string, entries ...any) any {
	return map[string]any{"name": name, "rules": entries}
}

// postImport runs the route and decodes the report. It does NOT assert the
// status: every case below states its own expectation.
func postImport(t *testing.T, s *Server, name string) (*httptest.ResponseRecorder, alertRuleImportResponse) {
	t.Helper()
	w := doRequest(t, s, http.MethodPost, "/api/v1/alert-rules/import",
		strings.NewReader(`{"name":`+strconv.Quote(name)+`}`), mutateWithCSRF)
	var body alertRuleImportResponse
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode import report: %v (body=%s)", err, w.Body)
		}
	}
	return w, body
}

// skipReason returns the reason recorded for name, or "" when the report does
// not skip it.
func (r *alertRuleImportResponse) skipReason(name string) string {
	for _, s := range r.Skipped {
		if s.Name == name {
			return s.Reason
		}
	}
	return ""
}

// noteText joins every note recorded for name, so an assertion can look for a
// substring without caring how many notes one rule collected.
func (r *alertRuleImportResponse) noteText(name string) string {
	var out []string
	for _, n := range r.Notes {
		if n.Name == name {
			out = append(out, n.Note)
		}
	}
	return strings.Join(out, " | ")
}

func (r *alertRuleImportResponse) createdSet() map[string]bool {
	out := make(map[string]bool, len(r.Created))
	for _, n := range r.Created {
		out[n] = true
	}
	return out
}

// storedByName finds one row the import created, by name.
func storedByName(t *testing.T, st *fakeAlertRuleStore, name string) store.AlertRule {
	t.Helper()
	rows, err := st.ListAlertRules(context.Background(), false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for i := range rows {
		if rows[i].Name == name {
			return rows[i]
		}
	}
	t.Fatalf("no stored rule named %q; store holds %d row(s)", name, len(rows))
	return store.AlertRule{}
}

// ---------------------------------------------------------------------------
// Dependency gates: 503 (no database) vs 409 (feature off) vs honest empty
// ---------------------------------------------------------------------------

func TestAlertRulesWithoutStoreReturn503(t *testing.T) {
	s := newM5TestServer(t, "operator", Deps{})
	for _, c := range alertRuleRoutes(uuid.NewString()) {
		var mutate func(*http.Request)
		if isMutatingMethod(c.method) {
			mutate = mutateWithCSRF
		}
		w := doRequest(t, s, c.method, c.path, strings.NewReader(c.body), mutate)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s without an AlertRuleService = %d, want 503: %s", c.method, c.path, w.Code, w.Body)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
			t.Errorf("%s %s Content-Type = %q, want application/problem+json", c.method, c.path, ct)
		}
		if !strings.Contains(w.Body.String(), "console.database.mode") {
			t.Errorf("%s %s 503 detail = %s, want it to name console.database.mode", c.method, c.path, w.Body)
		}
	}
}

// The 409-not-503 distinction, which is the whole point of the separate gate:
// the database is fine and the rules are all there, the FEATURE is off.
func TestAlertingOffIs409NamingTheFeatureFlag(t *testing.T) {
	st := newFakeAlertRuleStore()
	id := st.seed("Seeded", true).ID
	s := newM5TestServer(t, "operator", Deps{AlertRules: st})

	cases := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/alert-rules/foreign"},
		{http.MethodPost, "/api/v1/alert-rules/" + id + "/sync"},
		// Import reads the cluster through the SAME seam /foreign does, so a
		// console with the sync loop off cannot see a foreign object to adopt.
		{http.MethodPost, "/api/v1/alert-rules/import"},
	}
	for _, c := range cases {
		var mutate func(*http.Request)
		if isMutatingMethod(c.method) {
			mutate = mutateWithCSRF
		}
		w := doRequest(t, s, c.method, c.path, nil, mutate)
		if w.Code != http.StatusConflict {
			t.Errorf("%s %s with alerting off = %d, want 409: %s", c.method, c.path, w.Code, w.Body)
		}
		if !strings.Contains(w.Body.String(), "console.alerting.enabled") {
			t.Errorf("%s %s 409 detail = %s, want it to name console.alerting.enabled", c.method, c.path, w.Body)
		}
		if strings.Contains(w.Body.String(), "console.database.mode") {
			t.Errorf("%s %s 409 detail names the database: %s -- the database is fine here", c.method, c.path, w.Body)
		}
	}
}

// ---------------------------------------------------------------------------
// Authorization matrix, against the REAL built-in roles
// ---------------------------------------------------------------------------

// alert-editor is NOT in this matrix: the M7 coordinator granted it
// alerts:manage (delegated alert editing is the role's entire charter —
// authz's TestM7AlertPermissionsFollowTheIncidentsPosture pins the grant),
// so it exercises the full CRUD cycle alongside operator/admin below.
func TestAlertRulesViewerReadsButCannotManage(t *testing.T) {
	for _, role := range []string{"viewer"} {
		st := newFakeAlertRuleStore()
		id := st.seed("Seeded", true).ID
		syncer := &fakeRuleSyncer{}
		s := newM5TestServer(t, role, Deps{AlertRules: st, RuleSync: syncer})

		for _, path := range []string{
			"/api/v1/alert-rules", "/api/v1/alert-rules/" + id,
			"/api/v1/alert-rules/foreign", "/api/v1/alerts",
		} {
			w := doRequest(t, s, http.MethodGet, path, nil, nil)
			if w.Code != http.StatusOK {
				t.Errorf("%s: GET %s = %d, want 200: %s", role, path, w.Code, w.Body)
			}
		}

		// Preview is a POST that persists nothing and is gated on alerts:READ:
		// asking what a draft expression matches is a read of Prometheus.
		w := doRequest(t, s, http.MethodPost, "/api/v1/alert-rules/preview",
			strings.NewReader(validAlertRuleBody), mutateWithCSRF)
		if w.Code != http.StatusOK {
			t.Errorf("%s: POST preview = %d, want 200: %s", role, w.Code, w.Body)
		}

		writes := []struct{ method, path, body string }{
			{http.MethodPost, "/api/v1/alert-rules", validAlertRuleBody},
			{http.MethodPut, "/api/v1/alert-rules/" + id, validAlertRuleBody},
			{http.MethodDelete, "/api/v1/alert-rules/" + id, ""},
			{http.MethodPost, "/api/v1/alert-rules/" + id + "/sync", ""},
		}
		for _, c := range writes {
			w := doRequest(t, s, c.method, c.path, strings.NewReader(c.body), mutateWithCSRF)
			if w.Code != http.StatusForbidden {
				t.Errorf("%s: %s %s = %d, want 403: %s", role, c.method, c.path, w.Code, w.Body)
			}
		}
		if syncer.kickCount() != 0 {
			t.Errorf("%s: a denied sync still kicked the reconciler %d time(s)", role, syncer.kickCount())
		}
	}
}

func TestAlertRulesRequireAlertsRead(t *testing.T) {
	st := newFakeAlertRuleStore()
	s := newNoTelemetryServer(t, Deps{AlertRules: st})
	for _, path := range []string{"/api/v1/alert-rules", "/api/v1/alerts"} {
		w := doRequest(t, s, http.MethodGet, path, nil, nil)
		if w.Code != http.StatusForbidden {
			t.Errorf("GET %s without alerts:read = %d, want 403: %s", path, w.Code, w.Body)
		}
	}
}

func TestAlertRulesOperatorAndAdminManageTheFullCycle(t *testing.T) {
	for _, role := range []string{"operator", "alert-editor", "admin"} {
		st := newFakeAlertRuleStore()
		syncer := &fakeRuleSyncer{}
		s := newM5TestServer(t, role, Deps{AlertRules: st, RuleSync: syncer})

		w := doRequest(t, s, http.MethodPost, "/api/v1/alert-rules",
			strings.NewReader(validAlertRuleBody), mutateWithCSRF)
		if w.Code != http.StatusCreated {
			t.Fatalf("%s: POST = %d, want 201: %s", role, w.Code, w.Body)
		}
		var created alertRuleResponse
		if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if created.ID == "" || created.Name != "EdgePairLoss" {
			t.Errorf("%s: body = %+v, want the created rule echoed back", role, created)
		}
		// The renderer ran BEFORE the store write, and its output is on the row.
		if created.RenderedExpr == "" || !strings.Contains(created.RenderedExpr, "kconmon_ng") {
			t.Errorf("%s: renderedExpr = %q, want the server-rendered PromQL", role, created.RenderedExpr)
		}
		if created.SyncStatus != store.AlertSyncStatusUnsynced {
			t.Errorf("%s: syncStatus = %q, want unsynced for a fresh rule", role, created.SyncStatus)
		}
		if created.LastSyncedAt != nil {
			t.Errorf("%s: lastSyncedAt = %v, want nil for a fresh rule", role, created.LastSyncedAt)
		}
		if want := "/api/v1/alert-rules/" + created.ID; w.Header().Get("Location") != want {
			t.Errorf("%s: Location = %q, want %q", role, w.Header().Get("Location"), want)
		}
		if syncer.kickCount() != 1 {
			t.Errorf("%s: create kicked %d time(s), want 1", role, syncer.kickCount())
		}

		w = doRequest(t, s, http.MethodGet, "/api/v1/alert-rules/"+created.ID, nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: GET one = %d: %s", role, w.Code, w.Body)
		}

		// PUT is a FULL REPLACE, not a PATCH: the incidents PATCH exception
		// stays unique to incidents.
		updated := `{"name":"EdgePairLoss","kind":"pair-loss",` +
			`"params":{"protocol":"udp","thresholdPercent":9},"severity":"critical","forNs":60000000000}`
		w = doRequest(t, s, http.MethodPut, "/api/v1/alert-rules/"+created.ID,
			strings.NewReader(updated), mutateWithCSRF)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: PUT = %d, want 200: %s", role, w.Code, w.Body)
		}
		var replaced alertRuleResponse
		if err := json.Unmarshal(w.Body.Bytes(), &replaced); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if replaced.Severity != store.AlertSeverityCritical {
			t.Errorf("%s: severity = %q, want the replaced value", role, replaced.Severity)
		}
		if replaced.RenderedExpr == created.RenderedExpr {
			t.Errorf("%s: renderedExpr did not change on a params replace: %q", role, replaced.RenderedExpr)
		}
		// The full replace dropped the labels the create carried -- that is
		// what "full replace" means, and it is asserted so a later PATCH-ish
		// drift fails here.
		if string(replaced.Labels) != "{}" {
			t.Errorf("%s: labels = %s, want {} after a full replace that omitted them", role, replaced.Labels)
		}
		if syncer.kickCount() != 2 {
			t.Errorf("%s: update kicked %d time(s) in total, want 2", role, syncer.kickCount())
		}

		w = doRequest(t, s, http.MethodDelete, "/api/v1/alert-rules/"+created.ID, nil, mutateWithCSRF)
		if w.Code != http.StatusNoContent {
			t.Fatalf("%s: DELETE = %d, want 204: %s", role, w.Code, w.Body)
		}
		if st.count() != 0 {
			t.Errorf("%s: %d rule(s) left after DELETE", role, st.count())
		}
		if syncer.kickCount() != 3 {
			t.Errorf("%s: delete kicked %d time(s) in total, want 3", role, syncer.kickCount())
		}
	}
}

// The list is UNFILTERED: it passes enabledOnly=false, so a disabled rule is
// still listed. That is the whole reason the builder can re-enable one --
// hiding disabled rules from the only route that lists them would make them
// unreachable from the UI.
func TestAlertRulesListIncludesDisabledRules(t *testing.T) {
	st := newFakeAlertRuleStore()
	st.seed("EnabledOne", true)
	st.seed("DisabledOne", false)
	s := newM5TestServer(t, "viewer", Deps{AlertRules: st})

	w := doRequest(t, s, http.MethodGet, "/api/v1/alert-rules", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET list = %d, want 200: %s", w.Code, w.Body)
	}
	var body alertRulesListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Rules) != 2 {
		t.Fatalf("rules = %+v, want both the enabled and the disabled rule", body.Rules)
	}
	seen := map[string]bool{}
	for _, r := range body.Rules {
		seen[r.Name] = r.Enabled
		// Every JSONB column is an object on the wire, never null: the frontend
		// indexes all three by key.
		for field, raw := range map[string]json.RawMessage{
			"params": r.Params, "labels": r.Labels, "annotations": r.Annotations,
		} {
			if len(raw) == 0 || string(raw) == "null" {
				t.Errorf("%s.%s = %s, want an object", r.Name, field, raw)
			}
		}
	}
	if seen["EnabledOne"] != true || seen["DisabledOne"] != false {
		t.Errorf("enabled flags = %v, want them echoed as stored", seen)
	}
}

// ---------------------------------------------------------------------------
// Validation and rendering
// ---------------------------------------------------------------------------

func TestAlertRuleWriteValidation(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		want     int
		wantText string
	}{
		{
			name: "not JSON at all is 400",
			body: `{`, want: http.StatusBadRequest, wantText: "kind",
		},
		{
			name: "unknown severity is 422 naming the field",
			body: `{"name":"A","kind":"agent-missing","severity":"pants"}`,
			want: http.StatusUnprocessableEntity, wantText: "severity",
		},
		{
			name: "unknown kind is 422 naming the field",
			body: `{"name":"A","kind":"telepathy","severity":"warning"}`,
			want: http.StatusUnprocessableEntity, wantText: "kind",
		},
		{
			name: "raw without an expression is 422 naming params.expr",
			body: `{"name":"A","kind":"raw","params":{},"severity":"warning"}`,
			want: http.StatusUnprocessableEntity, wantText: "params.expr",
		},
		{
			// The store accepts cert-expiry (it is in the column's CHECK) and
			// the renderer deliberately has no template for it. Caught HERE,
			// at write time, instead of becoming a stored row that only ever
			// fails at sync time.
			name: "a kind the store allows but the renderer cannot render is 422",
			body: `{"name":"A","kind":"cert-expiry","params":{},"severity":"warning"}`,
			want: http.StatusUnprocessableEntity, wantText: "cert-expiry",
		},
		{
			name: "a missing required param is 422 naming the param",
			body: `{"name":"A","kind":"pair-loss","params":{"protocol":"tcp"},"severity":"warning"}`,
			want: http.StatusUnprocessableEntity, wantText: "thresholdPercent",
		},
		{
			name: "an unknown param is 422 naming it -- the schema is closed",
			body: `{"name":"A","kind":"agent-missing","params":{"nope":1},"severity":"warning"}`,
			want: http.StatusUnprocessableEntity, wantText: "nope",
		},
		{
			name: "labels that are not an object is 422",
			body: `{"name":"A","kind":"agent-missing","severity":"warning","labels":[1,2]}`,
			want: http.StatusUnprocessableEntity, wantText: "labels",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeAlertRuleStore()
			s := newM5TestServer(t, "operator", Deps{AlertRules: st})
			w := doRequest(t, s, http.MethodPost, "/api/v1/alert-rules", strings.NewReader(tc.body), mutateWithCSRF)
			if w.Code != tc.want {
				t.Fatalf("POST = %d, want %d: %s", w.Code, tc.want, w.Body)
			}
			if !strings.Contains(w.Body.String(), tc.wantText) {
				t.Errorf("detail = %s, want it to name %q", w.Body, tc.wantText)
			}
			if st.count() != 0 {
				t.Errorf("a rejected write stored %d rule(s)", st.count())
			}
		})
	}
}

func TestAlertRuleDuplicateNameIs422(t *testing.T) {
	st := newFakeAlertRuleStore()
	s := newM5TestServer(t, "operator", Deps{AlertRules: st})
	for range 2 {
		w := doRequest(t, s, http.MethodPost, "/api/v1/alert-rules",
			strings.NewReader(validAlertRuleBody), mutateWithCSRF)
		if w.Code == http.StatusUnprocessableEntity {
			if !strings.Contains(w.Body.String(), "already taken") {
				t.Errorf("duplicate detail = %s, want it to say the name is taken", w.Body)
			}
			return
		}
	}
	t.Fatal("a duplicate rule name was accepted twice, want 422 on the second")
}

func TestAlertRuleUnknownIDIs404(t *testing.T) {
	st := newFakeAlertRuleStore()
	syncer := &fakeRuleSyncer{}
	s := newM5TestServer(t, "operator", Deps{AlertRules: st, RuleSync: syncer})

	for _, id := range []string{uuid.NewString(), "not-a-uuid"} {
		cases := []struct{ method, path, body string }{
			{http.MethodGet, "/api/v1/alert-rules/" + id, ""},
			{http.MethodPut, "/api/v1/alert-rules/" + id, validAlertRuleBody},
			{http.MethodDelete, "/api/v1/alert-rules/" + id, ""},
			{http.MethodPost, "/api/v1/alert-rules/" + id + "/sync", ""},
		}
		for _, c := range cases {
			var mutate func(*http.Request)
			if isMutatingMethod(c.method) {
				mutate = mutateWithCSRF
			}
			w := doRequest(t, s, c.method, c.path, strings.NewReader(c.body), mutate)
			if w.Code != http.StatusNotFound {
				t.Errorf("%s %s = %d, want 404: %s", c.method, c.path, w.Code, w.Body)
			}
		}
	}
	if syncer.kickCount() != 0 {
		t.Errorf("a sync for an unknown rule kicked the reconciler %d time(s)", syncer.kickCount())
	}
}

// ---------------------------------------------------------------------------
// Sync
// ---------------------------------------------------------------------------

func TestAlertRuleSyncKicksTheReconciler(t *testing.T) {
	st := newFakeAlertRuleStore()
	id := st.seed("Seeded", true).ID
	syncer := &fakeRuleSyncer{}
	s := newM5TestServer(t, "operator", Deps{AlertRules: st, RuleSync: syncer})

	w := doRequest(t, s, http.MethodPost, "/api/v1/alert-rules/"+id+"/sync", nil, mutateWithCSRF)
	if w.Code != http.StatusAccepted {
		t.Fatalf("POST sync = %d, want 202: %s", w.Code, w.Body)
	}
	var body syncKickResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "kicked" {
		t.Errorf("status = %q, want \"kicked\"", body.Status)
	}
	if syncer.kickCount() != 1 {
		t.Errorf("kicks = %d, want 1", syncer.kickCount())
	}
}

// ---------------------------------------------------------------------------
// Foreign rules
// ---------------------------------------------------------------------------

func TestForeignRulesServeAProjectionNeverTheRawObject(t *testing.T) {
	raw := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": alerting.BundleAPIVersion,
		"kind":       alerting.BundleKind,
		"metadata": map[string]any{
			"name": "someone-elses-rules",
			"annotations": map[string]any{
				"secret-ish": "s3cr3t-token-in-an-annotation",
			},
		},
		"spec": map[string]any{"groups": []any{map[string]any{
			"name":  "grp",
			"rules": []any{map[string]any{"alert": "X", "expr": "up{instance=\"10.1.2.3:9090\"} == 0"}},
		}}},
	}}
	syncer := &fakeRuleSyncer{foreign: []promrules.ForeignRule{{
		Name: "someone-elses-rules", Groups: 1, Rules: 1,
		ManagedBy: "some-other-chart", Object: raw,
	}}}
	st := newFakeAlertRuleStore()
	s := newM5TestServer(t, "viewer", Deps{AlertRules: st, RuleSync: syncer})

	w := doRequest(t, s, http.MethodGet, "/api/v1/alert-rules/foreign", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET foreign = %d, want 200: %s", w.Code, w.Body)
	}
	var body foreignRulesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Foreign) != 1 {
		t.Fatalf("foreign = %+v, want exactly one entry", body.Foreign)
	}
	got := body.Foreign[0]
	if got.Name != "someone-elses-rules" || got.Groups != 1 || got.Rules != 1 || got.ManagedBy != "some-other-chart" {
		t.Errorf("foreign[0] = %+v, want the four projected fields", got)
	}
	// The raw object is NOT served: it is somebody else's object, and its
	// annotations and expressions are a leak surface this API has no reason
	// to open.
	for _, leak := range []string{"s3cr3t-token-in-an-annotation", "10.1.2.3", "apiVersion", "spec"} {
		if strings.Contains(w.Body.String(), leak) {
			t.Errorf("foreign body carries %q -- the raw object must never be served: %s", leak, w.Body)
		}
	}
}

func TestForeignRulesUpstreamFailureIs502(t *testing.T) {
	syncer := &fakeRuleSyncer{foreignErr: errors.New("apiserver is having a day")}
	s := newM5TestServer(t, "viewer", Deps{AlertRules: newFakeAlertRuleStore(), RuleSync: syncer})

	w := doRequest(t, s, http.MethodGet, "/api/v1/alert-rules/foreign", nil, nil)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("GET foreign with a failing lister = %d, want 502: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "having a day") {
		t.Errorf("the apiserver error text reached the wire: %s", w.Body)
	}
}

// ---------------------------------------------------------------------------
// Import (adopt a foreign PrometheusRule -- M7 Decision 4)
// ---------------------------------------------------------------------------

// The whole-route happy path: every ALERTING entry across every group becomes
// a raw builder row, the recording entry is reported as skipped, and the
// foreign object is left byte-for-byte as it was found.
func TestAlertRulesImportAdoptsEveryAlertingRuleAcrossGroups(t *testing.T) {
	fixture := foreignRuleFixture(t, "someone-elses-rules", []any{
		group("networking",
			map[string]any{
				"alert": "EdgePairLoss",
				"expr":  `up{instance="10.1.2.3:9090"} == 0`,
				"for":   "1h30m",
				"labels": map[string]any{
					"severity": "critical",
					"team":     "net",
				},
				"annotations": map[string]any{"summary": "the edge is losing packets"},
			},
			map[string]any{"record": "job:up:sum", "expr": "sum(up)"},
		),
		group("latency",
			map[string]any{"alert": "ZoneLatencyHigh", "expr": "zone_latency > 1"},
		),
	})
	before := fixture.Object.DeepCopy()

	st := newFakeAlertRuleStore()
	syncer := &fakeRuleSyncer{foreign: []promrules.ForeignRule{fixture}}
	s := newM5TestServer(t, "operator", Deps{AlertRules: st, RuleSync: syncer})

	w, report := postImport(t, s, "someone-elses-rules")
	if w.Code != http.StatusOK {
		t.Fatalf("import = %d, want 200: %s", w.Code, w.Body)
	}

	created := report.createdSet()
	if len(report.Created) != 2 || !created["EdgePairLoss"] || !created["ZoneLatencyHigh"] {
		t.Fatalf("created = %v, want both alerting entries", report.Created)
	}
	if got := report.skipReason("job:up:sum"); !strings.Contains(got, "recording rule") {
		t.Errorf("recording entry skip reason = %q, want it to name a recording rule", got)
	}

	// The adopted row: kind raw, the expression VERBATIM in params, enabled,
	// severity lifted, `for` parsed from a COMPOSITE Prometheus duration.
	row := storedByName(t, st, "EdgePairLoss")
	if row.Kind != store.AlertRuleKindRaw {
		t.Errorf("kind = %q, want raw -- the builder has no template for somebody else's expression", row.Kind)
	}
	var params struct {
		Expr string `json:"expr"`
	}
	if err := json.Unmarshal(row.Params, &params); err != nil {
		t.Fatalf("decode params: %v", err)
	}
	if params.Expr != `up{instance="10.1.2.3:9090"} == 0` {
		t.Errorf("params.expr = %q, want the foreign expression verbatim", params.Expr)
	}
	if row.RenderedExpr != params.Expr {
		t.Errorf("renderedExpr = %q, want the raw passthrough of params.expr", row.RenderedExpr)
	}
	if !row.Enabled {
		t.Error("an adopted rule arrived disabled; import adopts what is running")
	}
	if row.Severity != store.AlertSeverityCritical {
		t.Errorf("severity = %q, want it lifted off labels.severity", row.Severity)
	}
	if want := int64(90 * time.Minute); row.ForNs != want {
		t.Errorf("forNs = %d, want %d (1h30m)", row.ForNs, want)
	}
	// severity is LIFTED, not copied: it lives in the column, and the renderer
	// stamps it back on at render time. Leaving it in labels too would make one
	// fact editable in two places.
	var labels map[string]string
	if err := json.Unmarshal(row.Labels, &labels); err != nil {
		t.Fatalf("decode labels: %v", err)
	}
	if _, present := labels["severity"]; present {
		t.Errorf("labels = %v, want severity lifted out of them", labels)
	}
	if labels["team"] != "net" {
		t.Errorf("labels = %v, want every other label verbatim", labels)
	}
	var annotations map[string]string
	if err := json.Unmarshal(row.Annotations, &annotations); err != nil {
		t.Fatalf("decode annotations: %v", err)
	}
	if annotations["summary"] != "the edge is losing packets" {
		t.Errorf("annotations = %v, want them verbatim", annotations)
	}

	// A rule with no `for` waits for nothing, which is what the object said.
	if second := storedByName(t, st, "ZoneLatencyHigh"); second.ForNs != 0 {
		t.Errorf("forNs = %d for an entry with no `for`, want 0", second.ForNs)
	}

	// The new rows should reach the cluster without waiting out the jittered
	// loop -- the same reasoning every CRUD kick has.
	if syncer.kickCount() != 1 {
		t.Errorf("import kicked %d time(s), want exactly 1", syncer.kickCount())
	}

	// ADOPTION COPIES. The foreign object is never mutated and never deleted.
	if !reflect.DeepEqual(before.Object, fixture.Object.Object) {
		t.Errorf("the foreign object changed under the import:\n before: %v\n  after: %v",
			before.Object, fixture.Object.Object)
	}
}

// Severity is a CLOSED column and a foreign object's is not, so the import
// falls back to warning -- and SAYS SO. A note is not a skip: the rule is
// imported either way, and the report exists so the operator can go fix the
// one field the console picked for them.
func TestAlertRulesImportLiftsSeverityAndNotesEveryFallback(t *testing.T) {
	fixture := foreignRuleFixture(t, "sev", []any{
		group("g",
			map[string]any{"alert": "LiftedInfo", "expr": "a", "labels": map[string]any{"severity": "info"}},
			map[string]any{"alert": "LiftedWarning", "expr": "b", "labels": map[string]any{"severity": "warning"}},
			map[string]any{"alert": "LiftedCritical", "expr": "c", "labels": map[string]any{"severity": "critical"}},
			map[string]any{"alert": "OutOfSet", "expr": "d", "labels": map[string]any{"severity": "page"}},
			map[string]any{"alert": "NoSeverityAtAll", "expr": "e"},
		),
	})
	st := newFakeAlertRuleStore()
	s := newM5TestServer(t, "operator", Deps{AlertRules: st, RuleSync: &fakeRuleSyncer{
		foreign: []promrules.ForeignRule{fixture},
	}})

	w, report := postImport(t, s, "sev")
	if w.Code != http.StatusOK {
		t.Fatalf("import = %d, want 200: %s", w.Code, w.Body)
	}
	if len(report.Created) != 5 {
		t.Fatalf("created = %v, want all five -- a severity the console had to choose is a note, not a skip", report.Created)
	}
	for name, want := range map[string]string{
		"LiftedInfo":      store.AlertSeverityInfo,
		"LiftedWarning":   store.AlertSeverityWarning,
		"LiftedCritical":  store.AlertSeverityCritical,
		"OutOfSet":        store.AlertSeverityWarning,
		"NoSeverityAtAll": store.AlertSeverityWarning,
	} {
		if got := storedByName(t, st, name).Severity; got != want {
			t.Errorf("%s severity = %q, want %q", name, got, want)
		}
	}
	// The three in-set lifts are silent; the two the console chose for are not.
	for _, silent := range []string{"LiftedInfo", "LiftedWarning", "LiftedCritical"} {
		if got := report.noteText(silent); got != "" {
			t.Errorf("%s collected a note %q for a severity that needed no choosing", silent, got)
		}
	}
	if got := report.noteText("OutOfSet"); !strings.Contains(got, "page") || !strings.Contains(got, "warning") {
		t.Errorf("OutOfSet note = %q, want it to name the rejected value and the fallback", got)
	}
	if got := report.noteText("NoSeverityAtAll"); !strings.Contains(got, "warning") {
		t.Errorf("NoSeverityAtAll note = %q, want it to say the console picked warning", got)
	}
}

// The two label names the renderer OWNS are stripped on the way in. A foreign
// object carrying kconmon_ng_rule_id is somebody's copy-paste of one of our
// rules, and it is a note rather than a fatal error: the rule is fine, the
// label is not the one this console would stamp, and keeping it would put a
// stale uuid on an alert the firing list matches rules by.
func TestAlertRulesImportStripsReservedLabelsWithANote(t *testing.T) {
	fixture := foreignRuleFixture(t, "reserved", []any{
		group("g", map[string]any{
			"alert": "CarriesOurLabel",
			"expr":  "up == 0",
			"labels": map[string]any{
				alerting.RuleIDLabel: "11111111-2222-3333-4444-555555555555",
				"severity":           "info",
				"team":               "net",
			},
		}),
	})
	st := newFakeAlertRuleStore()
	s := newM5TestServer(t, "operator", Deps{AlertRules: st, RuleSync: &fakeRuleSyncer{
		foreign: []promrules.ForeignRule{fixture},
	}})

	w, report := postImport(t, s, "reserved")
	if w.Code != http.StatusOK {
		t.Fatalf("import = %d, want 200: %s", w.Code, w.Body)
	}
	if len(report.Created) != 1 {
		t.Fatalf("created = %v, want the rule imported -- a reserved label is noted, not fatal", report.Created)
	}
	if got := report.noteText("CarriesOurLabel"); !strings.Contains(got, alerting.RuleIDLabel) {
		t.Errorf("note = %q, want it to name %s", got, alerting.RuleIDLabel)
	}
	row := storedByName(t, st, "CarriesOurLabel")
	var labels map[string]string
	if err := json.Unmarshal(row.Labels, &labels); err != nil {
		t.Fatalf("decode labels: %v", err)
	}
	if len(labels) != 1 || labels["team"] != "net" {
		t.Errorf("labels = %v, want both reserved names gone and everything else kept", labels)
	}
	if row.Severity != store.AlertSeverityInfo {
		t.Errorf("severity = %q, want the lifted info", row.Severity)
	}
}

// Adoption must never INVENT a name. A Prometheus alert name is legal in
// places this store's name column is not (colons, most obviously), and the
// answer is a per-item skip naming the problem -- not SanitizeAlertName, which
// would silently store a rule under a name the operator cannot find by
// searching for the one they wrote.
func TestAlertRulesImportSkipsNamesTheStoreRejectsRatherThanRenaming(t *testing.T) {
	fixture := foreignRuleFixture(t, "names", []any{
		group("g",
			map[string]any{"alert": "kube:node:down", "expr": "a"},
			map[string]any{"alert": "has spaces", "expr": "b"},
			map[string]any{"alert": "Fine", "expr": "c"},
		),
	})
	st := newFakeAlertRuleStore()
	s := newM5TestServer(t, "operator", Deps{AlertRules: st, RuleSync: &fakeRuleSyncer{
		foreign: []promrules.ForeignRule{fixture},
	}})

	w, report := postImport(t, s, "names")
	if w.Code != http.StatusOK {
		t.Fatalf("import = %d, want 200: %s", w.Code, w.Body)
	}
	if len(report.Created) != 1 || report.Created[0] != "Fine" {
		t.Fatalf("created = %v, want only the legal name", report.Created)
	}
	for _, rejected := range []string{"kube:node:down", "has spaces"} {
		if got := report.skipReason(rejected); !strings.Contains(got, "name") {
			t.Errorf("%q skip reason = %q, want it to say why the name was rejected", rejected, got)
		}
	}
	// Nothing was renamed into existence.
	rows, err := st.ListAlertRules(context.Background(), false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("store holds %d row(s), want exactly the one legal rule", len(rows))
	}
	for i := range rows {
		if rows[i].Name != "Fine" {
			t.Errorf("stored %q -- adoption invented a name", rows[i].Name)
		}
	}
}

// The natural key is lower(name), migration 00007's index. A collision with an
// existing row and a collision with a rule this same import just created are
// the same skip, because they are the same constraint.
func TestAlertRulesImportSkipsNamesAlreadyTaken(t *testing.T) {
	fixture := foreignRuleFixture(t, "dupes", []any{
		group("g",
			map[string]any{"alert": "edgepairloss", "expr": "a"},
			map[string]any{"alert": "Fresh", "expr": "b"},
			map[string]any{"alert": "FRESH", "expr": "c"},
		),
	})
	st := newFakeAlertRuleStore()
	st.seed("EdgePairLoss", true)
	s := newM5TestServer(t, "operator", Deps{AlertRules: st, RuleSync: &fakeRuleSyncer{
		foreign: []promrules.ForeignRule{fixture},
	}})

	w, report := postImport(t, s, "dupes")
	if w.Code != http.StatusOK {
		t.Fatalf("import = %d, want 200: %s", w.Code, w.Body)
	}
	if len(report.Created) != 1 || report.Created[0] != "Fresh" {
		t.Fatalf("created = %v, want only the one name that was free", report.Created)
	}
	for _, taken := range []string{"edgepairloss", "FRESH"} {
		if got := report.skipReason(taken); !strings.Contains(got, "already taken") {
			t.Errorf("%q skip reason = %q, want \"name already taken\"", taken, got)
		}
	}
	if st.count() != 2 {
		t.Errorf("store holds %d row(s), want the seeded one plus the one adopted rule", st.count())
	}
}

// `for` is where a silent misread does real damage: a rule that waited five
// minutes and now fires instantly is a pager at 3am. So an unparseable
// duration SKIPS the rule rather than defaulting to 0.
func TestAlertRulesImportParsesForAndSkipsAnUnreadableOne(t *testing.T) {
	fixture := foreignRuleFixture(t, "durations", []any{
		group("g",
			map[string]any{"alert": "Composite", "expr": "a", "for": "1h30m"},
			map[string]any{"alert": "Millis", "expr": "b", "for": "1500ms"},
			map[string]any{"alert": "Weeks", "expr": "c", "for": "2w"},
			map[string]any{"alert": "Absent", "expr": "d"},
			map[string]any{"alert": "Garbage", "expr": "e", "for": "5 minutes"},
			map[string]any{"alert": "Fractional", "expr": "f", "for": "1.5h"},
			map[string]any{"alert": "NotAString", "expr": "g", "for": int64(300)},
		),
	})
	st := newFakeAlertRuleStore()
	s := newM5TestServer(t, "operator", Deps{AlertRules: st, RuleSync: &fakeRuleSyncer{
		foreign: []promrules.ForeignRule{fixture},
	}})

	w, report := postImport(t, s, "durations")
	if w.Code != http.StatusOK {
		t.Fatalf("import = %d, want 200: %s", w.Code, w.Body)
	}
	for name, want := range map[string]int64{
		"Composite": int64(90 * time.Minute),
		"Millis":    int64(1500 * time.Millisecond),
		"Weeks":     int64(14 * 24 * time.Hour),
		"Absent":    0,
	} {
		if got := storedByName(t, st, name).ForNs; got != want {
			t.Errorf("%s forNs = %d, want %d", name, got, want)
		}
	}
	for _, rejected := range []string{"Garbage", "Fractional", "NotAString"} {
		if got := report.skipReason(rejected); got == "" {
			t.Errorf("%q was not skipped; an unreadable `for` must never default to 0", rejected)
		} else if !strings.Contains(got, "for") {
			t.Errorf("%q skip reason = %q, want it to name the `for` field", rejected, got)
		}
	}
}

// A report with nothing in `created` is still a 200: the REPORT is the result.
// A 4xx here would tell a caller their request was wrong when in fact the
// answer -- "every rule in that object is a recording rule" -- is the useful
// one, and a status code cannot carry it.
func TestAlertRulesImportReportsAnEmptyAdoptionAs200(t *testing.T) {
	fixture := foreignRuleFixture(t, "records-only", []any{
		group("g",
			map[string]any{"record": "job:a", "expr": "sum(a)"},
			map[string]any{"record": "job:b", "expr": "sum(b)"},
		),
	})
	st := newFakeAlertRuleStore()
	syncer := &fakeRuleSyncer{foreign: []promrules.ForeignRule{fixture}}
	s := newM5TestServer(t, "operator", Deps{AlertRules: st, RuleSync: syncer})

	w, report := postImport(t, s, "records-only")
	if w.Code != http.StatusOK {
		t.Fatalf("import = %d, want 200 -- the report IS the result: %s", w.Code, w.Body)
	}
	if len(report.Created) != 0 || len(report.Skipped) != 2 {
		t.Fatalf("report = %+v, want zero created and both recording rules skipped", report)
	}
	// The three arrays are never null on the wire: the UI renders all of them.
	for _, field := range []string{`"created":[]`, `"skipped":[`, `"notes":[]`} {
		if !strings.Contains(w.Body.String(), field) {
			t.Errorf("body = %s, want %s -- these arrays must never be null", w.Body, field)
		}
	}
	// Nothing was created, so nothing needs to reach the cluster.
	if syncer.kickCount() != 0 {
		t.Errorf("an import that created nothing kicked %d time(s), want 0", syncer.kickCount())
	}
}

// An object nobody has, and an object the console OWNS, answer the same 404 --
// and they answer it for the same reason: ListForeign is the only lookup, and
// it excludes anything carrying the managed-by label. There is no second path
// by which a caller could reach one of our own bundles through this route and
// re-adopt it into a duplicate.
func TestAlertRulesImportUnknownOrOwnObjectIs404(t *testing.T) {
	ours := foreignRuleFixture(t, "kconmon-ng-console-rules", []any{
		group("g", map[string]any{"alert": "AlreadyOurs", "expr": "up == 0"}),
	})
	unstructured.SetNestedStringMap(ours.Object.Object, //nolint:errcheck // fixture; a failure here would fail the assertions below
		map[string]string{alerting.ManagedByLabel: alerting.ManagedByValue}, "metadata", "labels")

	st := newFakeAlertRuleStore()
	// The double mirrors the real ListForeign, which FILTERS ours out: an
	// object with our label is simply not in the list this route searches.
	s := newM5TestServer(t, "operator", Deps{AlertRules: st, RuleSync: &fakeRuleSyncer{
		foreign: []promrules.ForeignRule{},
	}})

	for _, name := range []string{"no-such-object", ours.Name} {
		w, _ := postImport(t, s, name)
		if w.Code != http.StatusNotFound {
			t.Errorf("import %q = %d, want 404: %s", name, w.Code, w.Body)
		}
		if !strings.Contains(w.Body.String(), "foreign") {
			t.Errorf("import %q 404 detail = %s, want it to say no FOREIGN object has that name", name, w.Body)
		}
	}
	if st.count() != 0 {
		t.Errorf("a 404 import wrote %d row(s)", st.count())
	}
}

func TestAlertRulesImportRejectsABodyWithNoName(t *testing.T) {
	st := newFakeAlertRuleStore()
	s := newM5TestServer(t, "operator", Deps{AlertRules: st, RuleSync: &fakeRuleSyncer{}})

	cases := []struct {
		body string
		want int
	}{
		{`{`, http.StatusBadRequest},
		{`{}`, http.StatusUnprocessableEntity},
		{`{"name":"   "}`, http.StatusUnprocessableEntity},
	}
	for _, c := range cases {
		w := doRequest(t, s, http.MethodPost, "/api/v1/alert-rules/import",
			strings.NewReader(c.body), mutateWithCSRF)
		if w.Code != c.want {
			t.Errorf("import %s = %d, want %d: %s", c.body, w.Code, c.want, w.Body)
		}
	}
}

func TestAlertRulesImportUpstreamFailureIs502(t *testing.T) {
	s := newM5TestServer(t, "operator", Deps{
		AlertRules: newFakeAlertRuleStore(),
		RuleSync:   &fakeRuleSyncer{foreignErr: errors.New("apiserver is having a day")},
	})
	w, _ := postImport(t, s, "anything")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("import with a failing lister = %d, want 502: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "having a day") {
		t.Errorf("the apiserver error text reached the wire: %s", w.Body)
	}
}

// Import is a WRITE: it creates rows. alert-editor holds alerts:manage (the
// coordinator's role decision, pinned in authz), so it adopts; viewer does not
// and is refused before the handler runs -- which also means no cluster read.
func TestAlertRulesImportRequiresAlertsManage(t *testing.T) {
	fixture := foreignRuleFixture(t, "obj", []any{
		group("g", map[string]any{"alert": "Adopted", "expr": "up == 0"}),
	})
	for role, want := range map[string]int{
		"viewer":       http.StatusForbidden,
		"alert-editor": http.StatusOK,
		"operator":     http.StatusOK,
		"admin":        http.StatusOK,
	} {
		st := newFakeAlertRuleStore()
		syncer := &fakeRuleSyncer{foreign: []promrules.ForeignRule{fixture}}
		s := newM5TestServer(t, role, Deps{AlertRules: st, RuleSync: syncer})

		w, _ := postImport(t, s, "obj")
		if w.Code != want {
			t.Errorf("%s: import = %d, want %d: %s", role, w.Code, want, w.Body)
		}
		if want == http.StatusForbidden && st.count() != 0 {
			t.Errorf("%s: a denied import still wrote %d row(s)", role, st.count())
		}
	}
}

// The audit posture is the CRUD routes': the object name is on the row, and
// the names of the rules it produced are not. Same leak class -- an adopted
// rule's name is a foreign naming convention that can carry a customer, a
// cluster or a hostname, and the report the caller just received already
// listed every one of them.
func TestAlertRulesImportAuditDetailIsTheObjectNameOnly(t *testing.T) {
	fixture := foreignRuleFixture(t, "someone-elses-rules", []any{
		group("g", map[string]any{
			"alert":  "AcmeCustomerEdgeDown",
			"expr":   `up{instance="10.9.9.9:9100"} == 0`,
			"labels": map[string]any{"runbook": "https://wiki.internal.example/runbooks/net"},
		}),
	})
	audit := &fakeAuditStore{}
	s := newAuditTestServer(t, audit,
		[]authz.Permission{authz.PermAlertsRead, authz.PermAlertsManage},
		Deps{AlertRules: newFakeAlertRuleStore(), RuleSync: &fakeRuleSyncer{
			foreign: []promrules.ForeignRule{fixture},
		}})

	w, report := postImport(t, s, "someone-elses-rules")
	if w.Code != http.StatusOK {
		t.Fatalf("import = %d, want 200: %s", w.Code, w.Body)
	}
	if len(report.Created) != 1 {
		t.Fatalf("created = %v, want the one rule adopted", report.Created)
	}

	entry := waitForOneAuditEntry(t, audit)[0]
	detail := string(entry.Detail)
	if !strings.Contains(detail, `"name":"someone-elses-rules"`) {
		t.Errorf("audit detail = %s, want the allow-listed OBJECT name", detail)
	}
	for _, banned := range []string{
		"AcmeCustomerEdgeDown", "10.9.9.9", "wiki.internal.example", "created", "skipped", "notes",
	} {
		if strings.Contains(detail, banned) {
			t.Errorf("audit detail = %s, want it to carry NO %q", detail, banned)
		}
	}
	if entry.Action != "POST /api/v1/alert-rules/import" {
		t.Errorf("action = %q, want the import route pattern", entry.Action)
	}
}

// ---------------------------------------------------------------------------
// Preview
// ---------------------------------------------------------------------------

func TestAlertRulePreviewReturnsExprAndSeriesCount(t *testing.T) {
	s := newM5TestServer(t, "viewer", Deps{
		AlertRules: newFakeAlertRuleStore(), Prometheus: newFakePrometheus(t, promVector(3)),
	})
	w := doRequest(t, s, http.MethodPost, "/api/v1/alert-rules/preview",
		strings.NewReader(validAlertRuleBody), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("preview = %d, want 200: %s", w.Code, w.Body)
	}
	var body alertRulePreviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(body.Expr, "kconmon_ng") {
		t.Errorf("expr = %q, want the rendered PromQL", body.Expr)
	}
	if body.Series != 3 {
		t.Errorf("series = %d, want 3", body.Series)
	}
	if body.Error != "" {
		t.Errorf("error = %q, want empty on a successful preview", body.Error)
	}
}

// A render failure is the ONE preview outcome that is not a 200: there is no
// expression to report, so there is nothing partial to be honest about.
func TestAlertRulePreviewRenderFailureIs422(t *testing.T) {
	s := newM5TestServer(t, "viewer", Deps{
		AlertRules: newFakeAlertRuleStore(), Prometheus: newFakePrometheus(t, promVector(1)),
	})
	body := `{"name":"A","kind":"pair-loss","params":{"protocol":"tcp"},"severity":"warning"}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/alert-rules/preview", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("preview with an unrenderable rule = %d, want 422: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "thresholdPercent") {
		t.Errorf("detail = %s, want it to name the missing param", w.Body)
	}
}

// A query that Prometheus rejects, and a Prometheus that is not configured at
// all, are both PARTIAL previews: the render succeeded, so the expression is
// reported, and the query half says why it has no number.
func TestAlertRulePreviewIsHonestlyPartial(t *testing.T) {
	cases := []struct {
		name     string
		prom     *promql.Client
		wantText string
	}{
		{
			name: "prometheus rejects the query",
			prom: newFakePrometheus(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"parse error: unexpected end"}`))
			}),
			wantText: "parse error",
		},
		{
			name:     "prometheus is not configured",
			prom:     nil,
			wantText: "prometheus.url",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newM5TestServer(t, "viewer", Deps{AlertRules: newFakeAlertRuleStore(), Prometheus: tc.prom})
			w := doRequest(t, s, http.MethodPost, "/api/v1/alert-rules/preview",
				strings.NewReader(validAlertRuleBody), mutateWithCSRF)
			if w.Code != http.StatusOK {
				t.Fatalf("preview = %d, want 200 (the render succeeded): %s", w.Code, w.Body)
			}
			var body alertRulePreviewResponse
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body.Expr == "" {
				t.Errorf("expr = %q, want the rendered expression even without a series count", body.Expr)
			}
			if body.Series != 0 {
				t.Errorf("series = %d, want 0 when the query did not run", body.Series)
			}
			if !strings.Contains(body.Error, tc.wantText) {
				t.Errorf("error = %q, want it to mention %q", body.Error, tc.wantText)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/alerts -- the firing list
// ---------------------------------------------------------------------------

const promAlertsBody = `{"status":"success","data":{"alerts":[
	{"labels":{"alertname":"EdgePairLoss","severity":"critical","kconmon_ng_rule_id":"11111111-1111-1111-1111-111111111111","source_node":"n1"},
	 "annotations":{"summary":"pair loss"},"state":"firing","activeAt":"2026-08-08T10:00:00Z","value":"7e+00"},
	{"labels":{"alertname":"SomeoneElsesAlert","severity":"warning"},
	 "annotations":{},"state":"pending","activeAt":"2026-08-08T10:05:00Z","value":"1e+00"}
]}}`

func promAlertsHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/alerts" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

func TestAlertsMapsPrometheusAlertsAndFiltersManagedOnly(t *testing.T) {
	s := newM5TestServer(t, "viewer", Deps{
		AlertRules: newFakeAlertRuleStore(),
		Prometheus: newFakePrometheus(t, promAlertsHandler(promAlertsBody)),
	})

	w := doRequest(t, s, http.MethodGet, "/api/v1/alerts", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/alerts = %d, want 200: %s", w.Code, w.Body)
	}
	var body alertsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.PromConfigured {
		t.Error("promConfigured = false with a Prometheus wired")
	}
	if len(body.Alerts) != 2 {
		t.Fatalf("alerts = %+v, want both", body.Alerts)
	}
	managed := body.Alerts[0]
	if managed.Name != "EdgePairLoss" || managed.State != "firing" || managed.Severity != "critical" {
		t.Errorf("alerts[0] = %+v, want name/state/severity lifted off the labels", managed)
	}
	if managed.RuleID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("ruleId = %q, want it lifted off the %s label", managed.RuleID, alerting.RuleIDLabel)
	}
	if managed.Annotations["summary"] != "pair loss" {
		t.Errorf("annotations = %v, want the upstream annotations", managed.Annotations)
	}
	if managed.ActiveAt == nil || managed.ActiveAt.IsZero() {
		t.Errorf("activeAt = %v, want the upstream instant", managed.ActiveAt)
	}
	if managed.Value != "7e+00" {
		t.Errorf("value = %q, want the upstream sample value verbatim", managed.Value)
	}
	if body.Alerts[1].RuleID != "" {
		t.Errorf("alerts[1].ruleId = %q, want empty for an alert this console does not manage", body.Alerts[1].RuleID)
	}

	w = doRequest(t, s, http.MethodGet, "/api/v1/alerts?managedOnly=true", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("managedOnly = %d: %s", w.Code, w.Body)
	}
	body = alertsResponse{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Alerts) != 1 || body.Alerts[0].Name != "EdgePairLoss" {
		t.Errorf("managedOnly alerts = %+v, want only the console-managed one", body.Alerts)
	}

	w = doRequest(t, s, http.MethodGet, "/api/v1/alerts?managedOnly=maybe", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("managedOnly=maybe = %d, want 400 (silently reading it as false widens the answer)", w.Code)
	}
}

// Prom UNCONFIGURED is an honest empty answer, not a 503: the Overview card
// that reads this route must be able to say "nothing is firing here, and no
// Prometheus is wired" in one shape. A configured Prometheus that FAILS is a
// different fact and stays a 502.
func TestAlertsWithoutPrometheusIsHonestlyEmpty(t *testing.T) {
	s := newM5TestServer(t, "viewer", Deps{AlertRules: newFakeAlertRuleStore()})
	w := doRequest(t, s, http.MethodGet, "/api/v1/alerts", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/alerts with no Prometheus = %d, want 200: %s", w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if got := strings.TrimSpace(w.Body.String()); got != `{"alerts":[],"promConfigured":false}` {
		t.Errorf("body = %s, want an empty array and promConfigured:false (never null)", got)
	}
}

func TestAlertsUpstreamFailureIs502(t *testing.T) {
	s := newM5TestServer(t, "viewer", Deps{
		AlertRules: newFakeAlertRuleStore(),
		Prometheus: newFakePrometheus(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"status":"error","error":"tsdb is sulking"}`))
		}),
	})
	w := doRequest(t, s, http.MethodGet, "/api/v1/alerts", nil, nil)
	if w.Code != http.StatusBadGateway {
		t.Errorf("GET /api/v1/alerts with a failing Prometheus = %d, want 502: %s", w.Code, w.Body)
	}
}

// ---------------------------------------------------------------------------
// Audit
// ---------------------------------------------------------------------------

// The allow-list decision for this resource, asserted end to end: the rule
// NAME reaches the audit row and NOTHING else does -- not the rendered
// expression, not the params it was rendered from, and not the labels, whose
// VALUES are operator-typed and routinely carry addresses and team handles.
func TestAlertRuleAuditDetailIsNameOnly(t *testing.T) {
	audit := &fakeAuditStore{}
	st := newFakeAlertRuleStore()
	s := newAuditTestServer(t, audit,
		[]authz.Permission{authz.PermAlertsRead, authz.PermAlertsManage},
		Deps{AlertRules: st})

	body := `{"name":"EdgePairLoss","kind":"pair-loss",` +
		`"params":{"protocol":"tcp","thresholdPercent":5},"severity":"warning","forNs":300000000000,` +
		`"labels":{"runbook":"https://wiki.internal.example/runbooks/net"}}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/alert-rules", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST = %d, want 201: %s", w.Code, w.Body)
	}

	entry := waitForOneAuditEntry(t, audit)[0]
	detail := string(entry.Detail)
	if !strings.Contains(detail, `"name":"EdgePairLoss"`) {
		t.Errorf("audit detail = %s, want the allow-listed name", detail)
	}
	for _, banned := range []string{"thresholdPercent", "kconmon_ng", "wiki.internal.example", "runbook", "params", "labels"} {
		if strings.Contains(detail, banned) {
			t.Errorf("audit detail = %s, want it to carry NO %q", detail, banned)
		}
	}
}

// Preview has no allow-list entry at all, which is the conscious default-deny
// decision this resource needs most: its body is a DRAFT that may carry a raw
// PromQL expression, and an expression's label matchers are exactly where an
// internal address ends up.
func TestAlertRulePreviewAuditDetailIsAlwaysEmpty(t *testing.T) {
	audit := &fakeAuditStore{}
	s := newAuditTestServer(t, audit, []authz.Permission{authz.PermAlertsRead},
		Deps{AlertRules: newFakeAlertRuleStore(), Prometheus: newFakePrometheus(t, promVector(1))})

	body := `{"name":"Draft","kind":"raw","severity":"warning",` +
		`"params":{"expr":"up{instance=\"10.9.9.9:9100\"} == 0"}}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/alert-rules/preview", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("preview = %d, want 200: %s", w.Code, w.Body)
	}
	entry := waitForOneAuditEntry(t, audit)[0]
	if got := strings.TrimSpace(string(entry.Detail)); got != "{}" {
		t.Errorf("preview audit detail = %s, want {}", got)
	}
	if entry.Action != "POST /api/v1/alert-rules/preview" {
		t.Errorf("action = %q, want the preview route pattern", entry.Action)
	}
}

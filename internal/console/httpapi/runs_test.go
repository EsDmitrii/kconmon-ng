package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/checks"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// fakeRunner is a RunService test double: an in-memory map of runs, plus
// results, so runs_test.go can drive every httpapi-level scenario (happy
// paths, error mapping, RBAC, audit) without wiring a real controller, hub,
// bus, or store the way a real *checks.Runner would need.
type fakeRunner struct {
	mu       sync.Mutex
	runs     map[string]checks.Run
	results  map[string][]store.RunResult
	nextID   int
	startErr error
	started  []checks.Spec
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{runs: map[string]checks.Run{}, results: map[string][]store.RunResult{}}
}

func (f *fakeRunner) Start(_ context.Context, spec checks.Spec, initiator authz.Subject) (string, error) { //nolint:gocritic // Subject is a value type by design
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, spec)
	if f.startErr != nil {
		return "", f.startErr
	}
	f.nextID++
	id := fmt.Sprintf("run-%d", f.nextID)
	pairTotal := len(spec.Sources) * len(spec.Destinations)
	specJSON, _ := json.Marshal(spec) //nolint:errcheck // test double, spec always marshals
	f.runs[id] = checks.Run{
		ID: id, CreatedAt: time.Now().UTC(), Status: "pending",
		CheckType: spec.Type, Plane: spec.Plane, Spec: specJSON,
		InitiatorKind: string(initiator.Kind), InitiatorID: initiator.ID,
		PairTotal: int32(pairTotal), //nolint:gosec // test double, small counts only
	}
	return id, nil
}

func (f *fakeRunner) Get(_ context.Context, runID string) (checks.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	run, ok := f.runs[runID]
	if !ok {
		return checks.Run{}, fmt.Errorf("fake runner: get run: %w", store.ErrNotFound)
	}
	return run, nil
}

func (f *fakeRunner) GetResults(_ context.Context, runID string) ([]store.RunResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.results[runID], nil
}

func (f *fakeRunner) List(_ context.Context, _ checks.ListFilter) (checks.RunPage, error) { //nolint:gocritic // hugeParam: test double mirrors the real signature
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]checks.Run, 0, len(f.runs))
	for _, r := range f.runs {
		out = append(out, r)
	}
	return checks.RunPage{Runs: out}, nil
}

var _ RunService = (*fakeRunner)(nil)

// newRunsTestServer builds a Server with a fixed SubjectUser resolved to
// role (via a fakeRoleResolver, since the built-in authz.NewPolicy(nil) is
// used unmodified -- Task 10's real viewer/operator split) and runner wired.
func newRunsTestServer(t *testing.T, runner RunService, role string) *Server {
	t.Helper()
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u1"}}
	extra := Deps{Runner: runner, Roles: fakeRoleResolver{roles: []string{role}}}
	return newAuthzServer(t, authr, authz.NewPolicy(nil), extra)
}

func TestRunsCreateHappyPath(t *testing.T) {
	runner := newFakeRunner()
	s := newRunsTestServer(t, runner, "operator")

	body := `{"sources":["n1","n2"],"destinations":["n3"],"type":"tcp","plane":"pod","timeoutNs":30000000000}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/runs", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", w.Code, w.Body)
	}

	var resp runCreateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "pending" {
		t.Errorf("status = %q, want pending", resp.Status)
	}
	if resp.PairTotal != 2 {
		t.Errorf("pairTotal = %d, want 2", resp.PairTotal)
	}
	if resp.WSTopic != "run:"+resp.ID {
		t.Errorf("wsTopic = %q, want run:%s", resp.WSTopic, resp.ID)
	}
	wantLocation := "/api/v1/runs/" + resp.ID
	if got := w.Header().Get("Location"); got != wantLocation {
		t.Errorf("Location = %q, want %q", got, wantLocation)
	}

	if len(runner.started) != 1 {
		t.Fatalf("Start called %d times, want 1", len(runner.started))
	}
	spec := runner.started[0]
	if spec.Type != "tcp" || spec.Plane != "pod" || spec.Timeout != 30*time.Second {
		t.Errorf("spec decoded wrong: %+v", spec)
	}
}

func TestRunsCreateInvalidBodyReturns400(t *testing.T) {
	s := newRunsTestServer(t, newFakeRunner(), "operator")
	w := doRequest(t, s, http.MethodPost, "/api/v1/runs", strings.NewReader(`not json`), mutateWithCSRF)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body)
	}
}

// TestRunsCreateTooManyPairsReturns422 is the brief's explicit distinction:
// a well-formed spec refused on policy is 422, never 400.
func TestRunsCreateTooManyPairsReturns422(t *testing.T) {
	runner := newFakeRunner()
	runner.startErr = fmt.Errorf("checks: plan: %w: computed 401 pairs, limit 400", checks.ErrTooManyPairs)
	s := newRunsTestServer(t, runner, "operator")

	body := `{"sources":[],"destinations":[],"type":"tcp","plane":"pod","timeoutNs":30000000000}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/runs", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "400") {
		t.Errorf("body = %s, want the limit (400) named in the detail", w.Body.String())
	}
}

func TestRunsCreateUnknownTypeReturns400(t *testing.T) {
	runner := newFakeRunner()
	runner.startErr = fmt.Errorf("checks: plan: %w: %q", checks.ErrUnknownType, "bogus")
	s := newRunsTestServer(t, runner, "operator")

	body := `{"sources":["n1"],"destinations":["n2"],"type":"bogus","plane":"pod","timeoutNs":1000000000}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/runs", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body)
	}
}

func TestRunsRoutesWithNilRunnerReturn503(t *testing.T) {
	// cmd/console only ever constructs a Runner when controller.url is
	// configured, so a nil s.runner is simultaneously "no runner" and "no
	// controller" -- one gate covers both cases the brief calls out.
	s := newRunsTestServer(t, nil, "operator")

	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodPost, "/api/v1/runs"},
		{http.MethodGet, "/api/v1/runs"},
		{http.MethodGet, "/api/v1/runs/some-id"},
	} {
		w := doRequest(t, s, tc.method, tc.path, strings.NewReader(`{"type":"tcp"}`), mutateWithCSRF)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want 503: %s", tc.method, tc.path, w.Code, w.Body)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
			t.Errorf("%s %s Content-Type = %q, want application/problem+json", tc.method, tc.path, ct)
		}
	}
}

func TestRunsListHappyPath(t *testing.T) {
	runner := newFakeRunner()
	if _, err := runner.Start(context.Background(), checks.Spec{Sources: []string{"n1"}, Destinations: []string{"n2"}, Type: "tcp", Plane: "pod"}, authz.Subject{Kind: authz.SubjectUser, ID: "u1"}); err != nil {
		t.Fatalf("seed Start: %v", err)
	}
	s := newRunsTestServer(t, runner, "viewer")

	w := doRequest(t, s, http.MethodGet, "/api/v1/runs", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	var resp runsListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(resp.Runs))
	}
	if resp.Runs[0].Type != "tcp" {
		t.Errorf("runs[0].type = %q, want tcp", resp.Runs[0].Type)
	}
}

func TestRunsListInvalidCursorReturns400(t *testing.T) {
	s := newRunsTestServer(t, newFakeRunner(), "viewer")
	w := doRequest(t, s, http.MethodGet, "/api/v1/runs?cursor=not-a-real-cursor", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body)
	}
}

func TestRunsGetHappyPath(t *testing.T) {
	runner := newFakeRunner()
	id, err := runner.Start(context.Background(), checks.Spec{Sources: []string{"n1"}, Destinations: []string{"n2"}, Type: "tcp", Plane: "pod"}, authz.Subject{Kind: authz.SubjectUser, ID: "u1"})
	if err != nil {
		t.Fatalf("seed Start: %v", err)
	}
	runner.results[id] = []store.RunResult{
		{RunID: id, SourceNode: "n1", DestinationNode: "n2", Success: true, DurationNs: 1000, RecordedAt: time.Now()},
	}
	s := newRunsTestServer(t, runner, "viewer")

	w := doRequest(t, s, http.MethodGet, "/api/v1/runs/"+id, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	var resp runDetailResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != id {
		t.Errorf("id = %q, want %q", resp.ID, id)
	}
	if len(resp.Results) != 1 || resp.Results[0].SourceNode != "n1" {
		t.Errorf("results = %+v, want one row from n1", resp.Results)
	}
}

func TestRunsGetUnknownIDReturns404(t *testing.T) {
	s := newRunsTestServer(t, newFakeRunner(), "viewer")
	w := doRequest(t, s, http.MethodGet, "/api/v1/runs/does-not-exist", nil, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", w.Code, w.Body)
	}
}

// TestRunsRBACViewerReadOnlyOperatorFull exercises Task 10's role split end
// to end: viewer can GET both routes but is denied POST; operator can do
// all three.
func TestRunsRBACViewerReadOnlyOperatorFull(t *testing.T) {
	body := `{"sources":["n1"],"destinations":["n2"],"type":"tcp","plane":"pod","timeoutNs":1000000000}`

	t.Run("viewer", func(t *testing.T) {
		runner := newFakeRunner()
		s := newRunsTestServer(t, runner, "viewer")

		if w := doRequest(t, s, http.MethodGet, "/api/v1/runs", nil, nil); w.Code != http.StatusOK {
			t.Errorf("viewer GET /api/v1/runs = %d, want 200: %s", w.Code, w.Body)
		}
		if w := doRequest(t, s, http.MethodGet, "/api/v1/runs/whatever", nil, nil); w.Code != http.StatusNotFound {
			t.Errorf("viewer GET /api/v1/runs/{id} = %d, want 404 (permitted, just no such run): %s", w.Code, w.Body)
		}
		w := doRequest(t, s, http.MethodPost, "/api/v1/runs", strings.NewReader(body), mutateWithCSRF)
		if w.Code != http.StatusForbidden {
			t.Errorf("viewer POST /api/v1/runs = %d, want 403: %s", w.Code, w.Body)
		}
	})

	t.Run("operator", func(t *testing.T) {
		runner := newFakeRunner()
		s := newRunsTestServer(t, runner, "operator")

		if w := doRequest(t, s, http.MethodGet, "/api/v1/runs", nil, nil); w.Code != http.StatusOK {
			t.Errorf("operator GET /api/v1/runs = %d, want 200: %s", w.Code, w.Body)
		}
		w := doRequest(t, s, http.MethodPost, "/api/v1/runs", strings.NewReader(body), mutateWithCSRF)
		if w.Code != http.StatusAccepted {
			t.Fatalf("operator POST /api/v1/runs = %d, want 202: %s", w.Code, w.Body)
		}
		var created runCreateResponse
		if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if w := doRequest(t, s, http.MethodGet, "/api/v1/runs/"+created.ID, nil, nil); w.Code != http.StatusOK {
			t.Errorf("operator GET /api/v1/runs/{id} = %d, want 200: %s", w.Code, w.Body)
		}
	})
}

// TestRunsCreateAuditRecordsOneEntry is the brief's explicit case: the audit
// middleware records exactly one entry for the POST, keyed by the route
// PATTERN ("POST /api/v1/runs"), not the literal path.
func TestRunsCreateAuditRecordsOneEntry(t *testing.T) {
	fs := &fakeAuditStore{}
	runner := newFakeRunner()
	s := newAuditTestServer(t, fs, []authz.Permission{authz.PermRunsCreate}, Deps{Runner: runner})

	body := `{"sources":["n1"],"destinations":["n2"],"type":"tcp","plane":"pod","timeoutNs":1000000000}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/runs", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", w.Code, w.Body)
	}

	entries := waitForOneAuditEntry(t, fs)
	if len(entries) != 1 {
		t.Fatalf("audit entries = %d, want exactly 1", len(entries))
	}
	e := entries[0]
	if e.Action != "POST /api/v1/runs" {
		t.Errorf("action = %q, want %q", e.Action, "POST /api/v1/runs")
	}
	if e.Outcome != auditOutcomeAllowed {
		t.Errorf("outcome = %q, want %q", e.Outcome, auditOutcomeAllowed)
	}
	var detail map[string]json.RawMessage
	if err := json.Unmarshal(e.Detail, &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if _, ok := detail["type"]; !ok {
		t.Errorf("detail = %s, want allow-listed \"type\"", e.Detail)
	}
	if _, ok := detail["sources"]; ok {
		t.Errorf("detail = %s, must never contain \"sources\" (not allow-listed)", e.Detail)
	}
}

// TestRunsRouteTableCoversAllThreeRoutes is a narrower, targeted check
// alongside TestEveryAPIRouteHasAPermissionDecision (auth_test.go), which
// already walks the live router: this pins the exact permission each new
// route requires, so a future edit to middleware_auth.go's table cannot
// silently swap runs:read/runs:create between routes without a test noticing.
func TestRunsRouteTableCoversAllThreeRoutes(t *testing.T) {
	cases := map[string]authz.Permission{
		"POST /api/v1/runs":     authz.PermRunsCreate,
		"GET /api/v1/runs":      authz.PermRunsRead,
		"GET /api/v1/runs/{id}": authz.PermRunsRead,
	}
	for key, want := range cases {
		rule, ok := routeTable[key]
		if !ok {
			t.Errorf("%s missing from routeTable", key)
			continue
		}
		if rule.public {
			t.Errorf("%s is public, want permission %q", key, want)
			continue
		}
		if rule.permission != want {
			t.Errorf("%s permission = %q, want %q", key, rule.permission, want)
		}
	}
}

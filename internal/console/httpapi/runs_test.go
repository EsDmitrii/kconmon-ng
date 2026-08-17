package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/checks"
	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
	"github.com/EsDmitrii/kconmon-ng/internal/console/ws"
	"github.com/google/uuid"
)

// fakeRunner is a RunService test double: an in-memory map of runs.
type fakeRunner struct {
	mu        sync.Mutex
	runs      map[string]checks.Run
	results   map[string][]store.RunResult
	nextID    int
	startErr  error
	started   []checks.Spec
	cancelErr error
	cancelled []string
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

func (f *fakeRunner) GetResults(_ context.Context, runID string) ([]store.RunResult, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rows := f.results[runID]
	// The same bound the store applies, so a test that seeds a long run sees what the API would.
	truncated := len(rows) > store.RunResultsCap
	if truncated {
		rows = rows[len(rows)-store.RunResultsCap:]
	}
	return rows, truncated, nil
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

// Cancel mirrors checks.Runner.Cancel's contract closely enough for the handler's own mapping to be
// exercised.
func (f *fakeRunner) Cancel(_ context.Context, runID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancelErr != nil {
		return f.cancelErr
	}
	if _, ok := f.runs[runID]; !ok {
		return fmt.Errorf("fake runner: cancel run: %w", store.ErrNotFound)
	}
	f.cancelled = append(f.cancelled, runID)
	return nil
}

// seedRun registers a run under cancelRunID, which the cancel tests need:
// Start mints "run-N" ids, and POST /api/v1/runs/{id}/cancel rejects anything
// that is not a canonical UUID before the runner is ever consulted.
func (f *fakeRunner) seedRun(status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs[cancelRunID] = checks.Run{
		ID: cancelRunID, CreatedAt: time.Now().UTC(), Status: status, CheckType: "tcp", Plane: "pod",
	}
}

var _ RunService = (*fakeRunner)(nil)

// newRunsTestServer builds a Server with a fixed SubjectUser resolved to role.
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

// The full-mesh path, end to end, through a REAL *checks.Runner rather than fakeRunner.
func TestRunsCreateAllToAllPlansOverAgentsWithoutKubernetesNodes(t *testing.T) {
	ctrl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/topology" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// `nodes` deliberately null, exactly as the controller serialises an
		// unset node watcher (internal/controller/topology.go).
		_, _ = w.Write([]byte(`{"nodes":null,"agents":[
			{"id":"a1","nodeName":"qa-node-01","podIP":"10.0.0.1","zone":"zone-a"},
			{"id":"a2","nodeName":"qa-node-02","podIP":"10.0.0.2","zone":"zone-b"},
			{"id":"a3","nodeName":"qa-node-03","podIP":"10.0.0.3","zone":"zone-c"}],
			"timestamp":"2026-01-01T00:00:00Z"}`))
	}))
	t.Cleanup(ctrl.Close)

	m := metrics.New("kconmon_ng_test", prometheus.NewRegistry())
	bus := cache.NewInProcessBus()
	runner := checks.NewRunner(
		controllerclient.New(ctrl.URL, 5*time.Second),
		ws.NewHub(bus, m), bus, checks.NewMemoryStore(), m,
	)
	s := newRunsTestServer(t, runner, "operator")

	body := `{"sources":[],"destinations":[],"type":"tcp","plane":"pod","timeoutNs":1000000000}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/runs", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (all<->all over 3 agents): %s", w.Code, w.Body)
	}
	var resp runCreateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// 3 agents, ordered pairs, self-pairs dropped.
	if resp.PairTotal != 6 {
		t.Errorf("pairTotal = %d, want 6", resp.PairTotal)
	}
}

// durationNs reaches the spec as-is, and an absent one leaves an instant run instant.
func TestRunsCreateCarriesDurationIntoTheSpec(t *testing.T) {
	for _, tc := range []struct {
		name         string
		body         string
		wantDuration time.Duration
	}{
		{
			name:         "absent durationNs stays an instant run",
			body:         `{"sources":["n1"],"destinations":["n2"],"type":"tcp","plane":"pod","timeoutNs":1000000000}`,
			wantDuration: 0,
		},
		{
			name:         "60s interval run",
			body:         `{"sources":["n1"],"destinations":["n2"],"type":"tcp","plane":"pod","timeoutNs":1000000000,"durationNs":60000000000}`,
			wantDuration: time.Minute,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := newFakeRunner()
			s := newRunsTestServer(t, runner, "operator")
			w := doRequest(t, s, http.MethodPost, "/api/v1/runs", strings.NewReader(tc.body), mutateWithCSRF)
			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202: %s", w.Code, w.Body)
			}
			if len(runner.started) != 1 {
				t.Fatalf("Start called %d times, want 1", len(runner.started))
			}
			if got := runner.started[0].Duration; got != tc.wantDuration {
				t.Errorf("spec.Duration = %s, want %s", got, tc.wantDuration)
			}
		})
	}
}

// A duration outside the accepted window is a 422 that NAMES the window.
func TestRunsCreateDurationOutOfRangeReturns422NamingTheBound(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"under the floor", `{"sources":["n1"],"destinations":["n2"],"type":"tcp","plane":"pod","durationNs":1000000}`},
		{"over the ceiling", `{"sources":["n1"],"destinations":["n2"],"type":"tcp","plane":"pod","durationNs":90000000000000}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A REAL runner: ValidateDuration lives in Start, and fakeRunner
			// would happily accept anything.
			m := metrics.New("kconmon_ng_test", prometheus.NewRegistry())
			bus := cache.NewInProcessBus()
			runner := checks.NewRunner(nil, ws.NewHub(bus, m), bus, checks.NewMemoryStore(), m)
			s := newRunsTestServer(t, runner, "operator")

			w := doRequest(t, s, http.MethodPost, "/api/v1/runs", strings.NewReader(tc.body), mutateWithCSRF)
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422: %s", w.Code, w.Body)
			}
			body := w.Body.String()
			if !strings.Contains(body, "10s") || !strings.Contains(body, "24h") {
				t.Errorf("body = %s, want both bounds (10s, 24h0m0s) named in the detail", body)
			}
		})
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
	// cmd/console only ever constructs a Runner when controller.url is configured.
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

// TestRunsRBACViewerReadOnlyOperatorFull exercises the role split end to end: viewer can GET both
// routes but is denied POST.
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

// TestRunsCreateAuditRecordsOneEntry is the explicit case: the audit middleware records exactly one
// entry for the POST.
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

// TestRunsRouteTableCoversAllThreeRoutes is a narrower.
func TestRunsRouteTableCoversAllThreeRoutes(t *testing.T) {
	cases := map[string]authz.Permission{
		"POST /api/v1/runs":     authz.PermRunsCreate,
		"GET /api/v1/runs":      authz.PermRunsRead,
		"GET /api/v1/runs/{id}": authz.PermRunsRead,
		// Pinned here so a later edit cannot quietly relax it to runs:read and let every viewer stop an
		// operator's run.
		"POST /api/v1/runs/{id}/cancel": authz.PermRunsCreate,
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

// cancelRunID is a fixed, canonical UUID the cancel tests address. A literal
// rather than uuid.NewString() so a failure message names the same id every
// run and the "not a UUID" case below has an unambiguous counterpart.
const cancelRunID = "6f1c9a10-2a6f-4d6e-9a4c-0f0c1b2d3e4f"

// TestRunsCancelHappyPathReturns204 pins the endpoint's whole success
// contract: 204, an empty body, and the runner actually asked to cancel THAT
// run.
func TestRunsCancelHappyPathReturns204(t *testing.T) {
	runner := newFakeRunner()
	runner.seedRun("running")
	s := newRunsTestServer(t, runner, "operator")

	w := doRequest(t, s, http.MethodPost, "/api/v1/runs/"+cancelRunID+"/cancel", nil, mutateWithCSRF)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", w.Code, w.Body)
	}
	if body := w.Body.String(); body != "" {
		t.Errorf("body = %q, want empty (204 carries no content)", body)
	}
	if len(runner.cancelled) != 1 || runner.cancelled[0] != cancelRunID {
		t.Errorf("cancelled = %v, want [%s]", runner.cancelled, cancelRunID)
	}
}

// TestRunsCancelTerminalRunIsANoOp204 is the documented decision: an operator
// clicking cancel on a run that finished a moment earlier has not done
// anything wrong, so the button must not lie with a 409.
func TestRunsCancelTerminalRunIsANoOp204(t *testing.T) {
	runner := newFakeRunner()
	runner.seedRun("succeeded")
	s := newRunsTestServer(t, runner, "operator")

	w := doRequest(t, s, http.MethodPost, "/api/v1/runs/"+cancelRunID+"/cancel", nil, mutateWithCSRF)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 for an already-terminal run: %s", w.Code, w.Body)
	}
}

// TestRunsCancelUnknownRunReturns404 covers both shapes of "no such run": a well-formed UUID
// nothing was ever stored under.
func TestRunsCancelUnknownRunReturns404(t *testing.T) {
	s := newRunsTestServer(t, newFakeRunner(), "operator")

	for name, path := range map[string]string{
		"unknown uuid": "/api/v1/runs/" + cancelRunID + "/cancel",
		"not a uuid":   "/api/v1/runs/not-a-uuid/cancel",
	} {
		w := doRequest(t, s, http.MethodPost, path, nil, mutateWithCSRF)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404: %s", name, w.Code, w.Body)
		}
	}
}

// TestRunsCancelBackendFailureIs502 keeps the not-found mapping honest: a
// store error that is NOT ErrNotFound must stay a 502, or the 404 above would
// be indistinguishable from an outage.
func TestRunsCancelBackendFailureIs502(t *testing.T) {
	runner := newFakeRunner()
	runner.seedRun("running")
	runner.cancelErr = errors.New("connection refused")
	s := newRunsTestServer(t, runner, "operator")

	w := doRequest(t, s, http.MethodPost, "/api/v1/runs/"+cancelRunID+"/cancel", nil, mutateWithCSRF)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", w.Code, w.Body)
	}
}

// TestRunsCancelRequiresRunsCreate is the RBAC half of the routeTable pin: a
// viewer holds runs:read and must NOT be able to stop an operator's run.
func TestRunsCancelRequiresRunsCreate(t *testing.T) {
	runner := newFakeRunner()
	runner.seedRun("running")
	s := newRunsTestServer(t, runner, "viewer")

	w := doRequest(t, s, http.MethodPost, "/api/v1/runs/"+cancelRunID+"/cancel", nil, mutateWithCSRF)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a viewer: %s", w.Code, w.Body)
	}
	if len(runner.cancelled) != 0 {
		t.Errorf("cancelled = %v, want none: the request was denied", runner.cancelled)
	}
}

// TestRunsCancelWithoutRunnerReturns503 keeps the route on the same "nil
// dependency -> 503" convention as the other three runs routes.
func TestRunsCancelWithoutRunnerReturns503(t *testing.T) {
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u1"}}
	s := newAuthzServer(t, authr, authz.NewPolicy(nil), Deps{Roles: fakeRoleResolver{roles: []string{"operator"}}})

	w := doRequest(t, s, http.MethodPost, "/api/v1/runs/"+cancelRunID+"/cancel", nil, mutateWithCSRF)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", w.Code, w.Body)
	}
}

// TestRunsCancelIsAuditedWithEmptyDetail pins the audit decision this route makes by NOT appearing
// in auditDetailAllowlist.
func TestRunsCancelIsAuditedWithEmptyDetail(t *testing.T) {
	fs := &fakeAuditStore{}
	runner := newFakeRunner()
	runner.seedRun("running")
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u1"}}
	s := newAuthzServer(t, authr, authz.NewPolicy(nil), Deps{
		Runner: runner, Audit: fs, Roles: fakeRoleResolver{roles: []string{"operator"}},
	})

	w := doRequest(t, s, http.MethodPost, "/api/v1/runs/"+cancelRunID+"/cancel", nil, mutateWithCSRF)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", w.Code, w.Body)
	}

	e := waitForOneAuditEntry(t, fs)[0]
	if e.Action != "POST /api/v1/runs/{id}/cancel" {
		t.Errorf("action = %q, want the route pattern", e.Action)
	}
	if e.Resource != cancelRunID {
		t.Errorf("resource = %q, want the run id %q", e.Resource, cancelRunID)
	}
	if e.Outcome != auditOutcomeAllowed {
		t.Errorf("outcome = %q, want %q", e.Outcome, auditOutcomeAllowed)
	}
	if string(e.Detail) != "{}" {
		t.Errorf("detail = %s, want {} (no body, no allow-list entry)", e.Detail)
	}
}

// newRunsTargetsTestServer is newRunsTestServer with a TargetService wired,
// for the destinationKind=target path.
func newRunsTargetsTestServer(t *testing.T, runner RunService, targets TargetService) *Server {
	t.Helper()
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u1"}}
	extra := Deps{Runner: runner, Targets: targets, Roles: fakeRoleResolver{roles: []string{"operator"}}}
	return newAuthzServer(t, authr, authz.NewPolicy(nil), extra)
}

func TestRunsCreateNodeKindIsTheM3Path(t *testing.T) {
	for _, body := range []string{
		`{"sources":["n1"],"destinations":["n2"],"type":"tcp","plane":"pod","timeoutNs":1000000000}`,
		`{"sources":["n1"],"destinations":["n2"],"type":"tcp","plane":"pod","timeoutNs":1000000000,"destinationKind":"node"}`,
	} {
		runner := newFakeRunner()
		s := newRunsTestServer(t, runner, "operator")
		w := doRequest(t, s, http.MethodPost, "/api/v1/runs", strings.NewReader(body), mutateWithCSRF)
		if w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202: %s", w.Code, w.Body)
		}
		spec := runner.started[0]
		if len(spec.TypedDestinations) != 0 {
			t.Errorf("node-kind run grew TypedDestinations: %+v", spec.TypedDestinations)
		}
		if len(spec.Destinations) != 1 || spec.Destinations[0] != "n2" {
			t.Errorf("Destinations = %v, want [n2]", spec.Destinations)
		}
	}
}

func TestRunsCreateTargetKindResolvesTheTargetsRow(t *testing.T) {
	runner := newFakeRunner()
	targets := newFakeTargetService()
	created, err := targets.CreateTarget(t.Context(), store.TargetInput{
		Name: "corp-dns", Kind: "host", Address: "10.66.6.6",
	})
	if err != nil {
		t.Fatalf("seed target: %v", err)
	}
	s := newRunsTargetsTestServer(t, runner, targets)

	body := `{"sources":["n1"],"type":"tcp","plane":"pod","timeoutNs":1000000000,` +
		`"destinationKind":"target","destinationTargetId":"` + created.ID + `"}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/runs", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", w.Code, w.Body)
	}
	spec := runner.started[0]
	if len(spec.TypedDestinations) != 1 {
		t.Fatalf("TypedDestinations = %+v, want exactly one", spec.TypedDestinations)
	}
	d := spec.TypedDestinations[0]
	if d.Kind != checks.DestKindTarget || d.Name != "corp-dns" || d.Address != "10.66.6.6" {
		t.Errorf("destination = %+v, want target corp-dns 10.66.6.6", d)
	}
	if len(spec.Destinations) != 0 {
		t.Errorf("Destinations = %v, want empty for an external run", spec.Destinations)
	}
}

func TestRunsCreateAdhocKindUsesTheAddressAsName(t *testing.T) {
	runner := newFakeRunner()
	s := newRunsTestServer(t, runner, "operator")
	body := `{"sources":["n1"],"type":"icmp","plane":"pod","timeoutNs":1000000000,` +
		`"destinationKind":"adhoc","destinationAddress":"192.0.2.7"}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/runs", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", w.Code, w.Body)
	}
	d := runner.started[0].TypedDestinations[0]
	if d.Kind != checks.DestKindAdhoc || d.Name != "192.0.2.7" || d.Address != "192.0.2.7" {
		t.Errorf("destination = %+v, want adhoc with address doubling as name", d)
	}
}

// TestRunsCreateDestinationValidation walks every refusal
// resolveRunDestination produces, plus the 422/503 target cases.
func TestRunsCreateDestinationValidation(t *testing.T) {
	cases := []struct {
		name, body string
		want       int
		useTargets bool
	}{
		{"unknown kind", `{"sources":["n1"],"type":"tcp","plane":"pod","destinationKind":"pod"}`, http.StatusBadRequest, false},
		{"external fields with node kind", `{"sources":["n1"],"destinations":["n2"],"type":"tcp","plane":"pod","destinationAddress":"10.0.0.1"}`, http.StatusBadRequest, false},
		{"destinations with target kind", `{"sources":["n1"],"destinations":["n2"],"type":"tcp","plane":"pod","destinationKind":"target","destinationTargetId":"3f0e8f7e-58a4-4b7a-9a63-8f6e1c2d4b5a"}`, http.StatusBadRequest, false},
		{"adhoc without address", `{"sources":["n1"],"type":"tcp","plane":"pod","destinationKind":"adhoc"}`, http.StatusBadRequest, false},
		{"target id not a uuid", `{"sources":["n1"],"type":"tcp","plane":"pod","destinationKind":"target","destinationTargetId":"nope"}`, http.StatusBadRequest, true},
		{"unknown target row", `{"sources":["n1"],"type":"tcp","plane":"pod","destinationKind":"target","destinationTargetId":"3f0e8f7e-58a4-4b7a-9a63-8f6e1c2d4b5a"}`, http.StatusUnprocessableEntity, true},
		{"target kind without targets store", `{"sources":["n1"],"type":"tcp","plane":"pod","destinationKind":"target","destinationTargetId":"3f0e8f7e-58a4-4b7a-9a63-8f6e1c2d4b5a"}`, http.StatusServiceUnavailable, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runner := newFakeRunner()
			var s *Server
			if c.useTargets {
				s = newRunsTargetsTestServer(t, runner, newFakeTargetService())
			} else {
				s = newRunsTestServer(t, runner, "operator")
			}
			w := doRequest(t, s, http.MethodPost, "/api/v1/runs", strings.NewReader(c.body), mutateWithCSRF)
			if w.Code != c.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, c.want, w.Body)
			}
			if len(runner.started) != 0 {
				t.Errorf("refused request still started a run: %+v", runner.started)
			}
		})
	}
}

// An ad-hoc address typed into the diagnostics form is judged by the SAME rule a saved definition's
// destination_address is (store.ValidateAdhocAddress): a malformed one is refused up front and
// named, instead of being dispatched and coming back as "agent does not support external
// destinations" -- which would be a lie about a string no agent was ever asked to dial.
func TestRunsCreateAdhocAddressValidation(t *testing.T) {
	for _, c := range []struct {
		name, address string
		want          int
	}{
		{"url", "https://example.com/health", http.StatusAccepted},
		{"ipv4", "192.0.2.7", http.StatusAccepted},
		{"bracketed ipv6 with port", "[2001:db8::1]:8443", http.StatusAccepted},
		{"dns name with underscore", "qa_node.internal", http.StatusAccepted},
		{"host with port", "example.com:65535", http.StatusAccepted},
		{"prose", "not a valid address!!", http.StatusUnprocessableEntity},
		{"port zero", "example.com:0", http.StatusUnprocessableEntity},
		{"port above range", "example.com:70000", http.StatusUnprocessableEntity},
		{"doubled dot", "example..com", http.StatusUnprocessableEntity},
		{"scheme without host", "https://", http.StatusUnprocessableEntity},
	} {
		t.Run(c.name, func(t *testing.T) {
			runner := newFakeRunner()
			s := newRunsTestServer(t, runner, "operator")
			body, err := json.Marshal(map[string]any{
				"sources": []string{"n1"}, "type": "tcp", "plane": "pod", "timeoutNs": 1_000_000_000,
				"destinationKind": "adhoc", "destinationAddress": c.address,
			})
			if err != nil {
				t.Fatalf("marshal body: %v", err)
			}
			w := doRequest(t, s, http.MethodPost, "/api/v1/runs", strings.NewReader(string(body)), mutateWithCSRF)
			if w.Code != c.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, c.want, w.Body)
			}
			if c.want != http.StatusAccepted {
				if len(runner.started) != 0 {
					t.Errorf("refused address still started a run: %+v", runner.started)
				}
				if !strings.Contains(w.Body.String(), c.address) {
					t.Errorf("body %s does not name the refused address %q", w.Body, c.address)
				}
			}
		})
	}
}

func TestRunsCreateInvalidDestinationFromPlannerIs400(t *testing.T) {
	runner := newFakeRunner()
	runner.startErr = fmt.Errorf("checks: plan: %w: empty address", checks.ErrInvalidDestination)
	s := newRunsTestServer(t, runner, "operator")
	body := `{"sources":["n1"],"type":"tcp","plane":"pod","destinationKind":"adhoc","destinationAddress":"x"}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/runs", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body)
	}
}

/*
 * uuid.Parse accepts more spellings than the canonical one — uppercase, hyphenless 32-hex,
 * urn:uuid: — and the raw string used to be handed straight on. Runner.Cancel looks a run up in a
 * map keyed by the canonical form, so the lookup missed, the "not in flight here, leaving it to the
 * reaper" branch was taken, and the handler answered 204: the operator was told the run was
 * cancelled while it went on fanning out.
 */
func TestRunIDIsCanonicalisedBeforeItIsUsed(t *testing.T) {
	canonical := uuid.NewString()
	runner := newFakeRunner()
	runner.runs[canonical] = checks.Run{ID: canonical, Status: "running"}
	s := newRunsTestServer(t, runner, "operator")

	for _, spelling := range []string{
		strings.ToUpper(canonical),
		strings.ReplaceAll(canonical, "-", ""),
		"urn:uuid:" + canonical,
	} {
		runner.cancelled = nil
		w := doRequest(t, s, http.MethodPost, "/api/v1/runs/"+spelling+"/cancel", nil, mutateWithCSRF)
		if w.Code != http.StatusNoContent {
			t.Fatalf("cancel %q = %d, want 204: %s", spelling, w.Code, w.Body)
		}
		if len(runner.cancelled) != 1 || runner.cancelled[0] != canonical {
			t.Errorf("cancel %q reached the runner as %v, want the canonical %q", spelling, runner.cancelled, canonical)
		}
	}
}

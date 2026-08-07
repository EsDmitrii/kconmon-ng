package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// fakeTargetService is a TargetService double, mutex-guarded (the audit
// drain goroutine never touches it, but -race sees the httptest handler
// goroutine and the test goroutine both reading it). It reproduces the
// three sentinels the real *store.DB returns -- ErrNotFound,
// ErrAlreadyExists, ErrInUse -- which is the entire contract handleTargets*
// maps to status codes, so no database is needed to pin that mapping.
type fakeTargetService struct {
	mu sync.Mutex
	// byID is the authoritative map; byName enforces targets_name_key, the
	// unique constraint that produces ErrAlreadyExists in production.
	byID   map[string]store.Target
	byName map[string]string
	// inUse names the ids DeleteTarget refuses with ErrInUse, standing in
	// for check_definitions.destination_target_id's ON DELETE RESTRICT.
	inUse map[string]bool
	// panicOnCreate makes CreateTarget panic, so the recoverer can be
	// exercised through a REAL route (POST /api/v1/targets) rather than a
	// synthetic router assembled in the test.
	panicOnCreate bool
	// createErr, if set, is returned by CreateTarget instead of creating --
	// used for the opaque-backend-failure -> 502 case.
	createErr error
}

func newFakeTargetService() *fakeTargetService {
	return &fakeTargetService{
		byID:   map[string]store.Target{},
		byName: map[string]string{},
		inUse:  map[string]bool{},
	}
}

func (f *fakeTargetService) CreateTarget(_ context.Context, in store.TargetInput) (store.Target, error) { //nolint:gocritic // hugeParam: mirrors the store signature
	if f.panicOnCreate {
		panic("boom: fakeTargetService.CreateTarget")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return store.Target{}, f.createErr
	}
	if err := in.Validate(); err != nil {
		return store.Target{}, err
	}
	if _, taken := f.byName[in.Name]; taken {
		return store.Target{}, store.ErrAlreadyExists
	}
	now := time.Now().UTC()
	t := store.Target{
		ID: uuid.NewString(), Name: in.Name, Kind: in.Kind, Address: in.Address,
		Labels: orEmptyLabels(in.Labels), CreatedAt: now, UpdatedAt: now,
	}
	f.byID[t.ID] = t
	f.byName[t.Name] = t.ID
	return t, nil
}

func (f *fakeTargetService) UpdateTarget(_ context.Context, id string, in store.TargetInput) (store.Target, error) { //nolint:gocritic // hugeParam: mirrors the store signature
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := in.Validate(); err != nil {
		return store.Target{}, err
	}
	existing, ok := f.byID[id]
	if !ok {
		return store.Target{}, store.ErrNotFound
	}
	if other, taken := f.byName[in.Name]; taken && other != id {
		return store.Target{}, store.ErrAlreadyExists
	}
	delete(f.byName, existing.Name)
	updated := store.Target{
		ID: id, Name: in.Name, Kind: in.Kind, Address: in.Address,
		Labels: orEmptyLabels(in.Labels), CreatedAt: existing.CreatedAt, UpdatedAt: time.Now().UTC(),
	}
	f.byID[id] = updated
	f.byName[in.Name] = id
	return updated, nil
}

func (f *fakeTargetService) DeleteTarget(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inUse[id] {
		return store.ErrInUse
	}
	t, ok := f.byID[id]
	if !ok {
		return store.ErrNotFound
	}
	delete(f.byID, id)
	delete(f.byName, t.Name)
	return nil
}

func (f *fakeTargetService) GetTarget(_ context.Context, id string) (store.Target, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.byID[id]
	if !ok {
		return store.Target{}, store.ErrNotFound
	}
	return t, nil
}

func (f *fakeTargetService) ListTargets(_ context.Context, filter store.TargetFilter) (store.TargetPage, error) { //nolint:gocritic // hugeParam: mirrors the store signature
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.Target, 0, len(f.byID))
	for _, t := range f.byID {
		if filter.Kind != "" && t.Kind != filter.Kind {
			continue
		}
		out = append(out, t)
	}
	return store.TargetPage{Targets: out}, nil
}

func orEmptyLabels(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	return raw
}

// newTargetsTestServer wires a Server whose subject holds the given BUILT-IN
// role -- "operator" and "viewer" are the two the brief names, and using the
// real compiled-in role sets (authz.NewPolicy(nil)) rather than a synthetic
// "tester" role is the point: it proves targets:read/targets:write actually
// land where M4 Task 2 put them.
func newTargetsTestServer(t *testing.T, role string, targets TargetService, extra Deps) *Server { //nolint:gocritic // hugeParam: test helper
	t.Helper()
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u1"}}
	extra.Roles = fakeRoleResolver{roles: []string{role}}
	extra.Targets = targets
	return newAuthzServer(t, authr, authz.NewPolicy(nil), extra)
}

// targetRoutes is every route this task registers, with a syntactically
// valid id so the id-shaped ones reach their handler instead of the
// malformed-UUID guard.
func targetRoutes(id string) []struct{ method, path string } {
	return []struct{ method, path string }{
		{http.MethodGet, "/api/v1/targets"},
		{http.MethodPost, "/api/v1/targets"},
		{http.MethodGet, "/api/v1/targets/" + id},
		{http.MethodPut, "/api/v1/targets/" + id},
		{http.MethodDelete, "/api/v1/targets/" + id},
	}
}

const validTargetBody = `{"name":"edge-lb","kind":"host","address":"10.0.0.7"}`

func TestTargetsWithoutStoreReturns503(t *testing.T) {
	s := newTargetsTestServer(t, "operator", nil, Deps{})
	for _, c := range targetRoutes(uuid.NewString()) {
		var mutate func(*http.Request)
		if isMutatingMethod(c.method) {
			mutate = mutateWithCSRF
		}
		w := doRequest(t, s, c.method, c.path, strings.NewReader(validTargetBody), mutate)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s without a TargetService = %d, want 503: %s", c.method, c.path, w.Code, w.Body)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
			t.Errorf("%s %s Content-Type = %q, want application/problem+json", c.method, c.path, ct)
		}
		// Targets are CONFIGURATION and get no in-memory fallback, so the
		// only actionable remedy is the Helm value that turns the database
		// on -- the detail must name it.
		if !strings.Contains(w.Body.String(), "console.database.mode") {
			t.Errorf("%s %s 503 detail = %s, want it to name console.database.mode", c.method, c.path, w.Body)
		}
	}
}

func TestTargetsCreateReturns201AndLocation(t *testing.T) {
	s := newTargetsTestServer(t, "operator", newFakeTargetService(), Deps{})
	w := doRequest(t, s, http.MethodPost, "/api/v1/targets", strings.NewReader(validTargetBody), mutateWithCSRF)
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d, want 201: %s", w.Code, w.Body)
	}
	var got targetResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID == "" || got.Name != "edge-lb" || got.Kind != "host" || got.Address != "10.0.0.7" {
		t.Fatalf("body = %+v, want the created target echoed back", got)
	}
	if want := "/api/v1/targets/" + got.ID; w.Header().Get("Location") != want {
		t.Errorf("Location = %q, want %q", w.Header().Get("Location"), want)
	}
	if string(got.Labels) != "{}" {
		t.Errorf("labels = %s, want {} for an omitted labels field", got.Labels)
	}
}

func TestTargetsCreateDuplicateNameReturns422(t *testing.T) {
	s := newTargetsTestServer(t, "operator", newFakeTargetService(), Deps{})
	if w := doRequest(t, s, http.MethodPost, "/api/v1/targets", strings.NewReader(validTargetBody), mutateWithCSRF); w.Code != http.StatusCreated {
		t.Fatalf("first create status %d: %s", w.Code, w.Body)
	}
	w := doRequest(t, s, http.MethodPost, "/api/v1/targets", strings.NewReader(validTargetBody), mutateWithCSRF)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate name status %d, want 422: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "name") {
		t.Errorf("422 detail = %s, want it to name the offending field", w.Body)
	}
}

// TestTargetsCreateValidationReturns422 covers the validation half of 422:
// every case store.TargetInput.Validate rejects (which httpapi calls
// directly, so there is exactly ONE target-validation rule set in the
// codebase) must answer 422 with a detail naming the field, never 400 --
// the body is well-formed JSON shaped exactly as documented.
func TestTargetsCreateValidationReturns422(t *testing.T) {
	cases := []struct {
		name, body, wantField string
	}{
		{"bad kind", `{"name":"edge-lb","kind":"tcp","address":"10.0.0.7"}`, "kind"},
		{"bad name charset", `{"name":"edge lb!","kind":"host","address":"10.0.0.7"}`, "name"},
		{"empty name", `{"name":"","kind":"host","address":"10.0.0.7"}`, "name"},
		{"empty address", `{"name":"edge-lb","kind":"host","address":""}`, "address"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newTargetsTestServer(t, "operator", newFakeTargetService(), Deps{})
			w := doRequest(t, s, http.MethodPost, "/api/v1/targets", strings.NewReader(c.body), mutateWithCSRF)
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status %d, want 422: %s", w.Code, w.Body)
			}
			if !strings.Contains(w.Body.String(), c.wantField) {
				t.Errorf("422 detail = %s, want it to name %q", w.Body, c.wantField)
			}
			// The internal package prefix must never reach the wire.
			if strings.Contains(w.Body.String(), "store:") {
				t.Errorf("422 detail = %s, must not leak the store package prefix", w.Body)
			}
		})
	}
}

// TestTargetsCreateMalformedJSONReturns400 covers the OTHER side of the
// 400/422 split: a body that is not JSON at all -- including one whose
// "labels" value is syntactically broken, which the outer decoder rejects
// before store.TargetInput.Validate ever sees it (labels is a
// json.RawMessage inside the request struct, so the whole document has to
// parse). That is why there is no "malformed labels -> 422" case: it is
// unreachable over HTTP, and validateJSON stays a backstop for store's
// non-HTTP callers.
func TestTargetsCreateMalformedJSONReturns400(t *testing.T) {
	for _, body := range []string{
		`{"name":`,
		`not json at all`,
		`{"name":"edge-lb","kind":"host","address":"10.0.0.7","labels":{"a":}}`,
	} {
		s := newTargetsTestServer(t, "operator", newFakeTargetService(), Deps{})
		w := doRequest(t, s, http.MethodPost, "/api/v1/targets", strings.NewReader(body), mutateWithCSRF)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %q = %d, want 400: %s", body, w.Code, w.Body)
		}
	}
}

func TestTargetsGetUnknownIDReturns404(t *testing.T) {
	s := newTargetsTestServer(t, "operator", newFakeTargetService(), Deps{})
	w := doRequest(t, s, http.MethodGet, "/api/v1/targets/"+uuid.NewString(), nil, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404: %s", w.Code, w.Body)
	}
}

// TestTargetsMalformedUUIDReturns404 is M3 follow-up #5: an id that is not a
// canonical UUID must be answered 404 by httpapi itself. Before this guard
// the string reached pgx, which rejected it as a scan/encode failure and
// surfaced as a 502 -- an unknown id and an unparseable one are the same
// thing to a client, and neither is a gateway fault.
func TestTargetsMalformedUUIDReturns404(t *testing.T) {
	fake := newFakeTargetService()
	s := newTargetsTestServer(t, "operator", fake, Deps{})
	for _, id := range []string{"not-a-uuid", "12345", "{id}", "0", "..", "%20"} {
		for _, c := range []struct {
			method string
			body   string
		}{
			{http.MethodGet, ""},
			{http.MethodPut, validTargetBody},
			{http.MethodDelete, ""},
		} {
			var mutate func(*http.Request)
			if isMutatingMethod(c.method) {
				mutate = mutateWithCSRF
			}
			w := doRequest(t, s, c.method, "/api/v1/targets/"+id, strings.NewReader(c.body), mutate)
			if w.Code != http.StatusNotFound {
				t.Errorf("%s /api/v1/targets/%s = %d, want 404 (never 502): %s", c.method, id, w.Code, w.Body)
			}
		}
	}
}

func TestTargetsUpdateRoundTrips(t *testing.T) {
	s := newTargetsTestServer(t, "operator", newFakeTargetService(), Deps{})
	w := doRequest(t, s, http.MethodPost, "/api/v1/targets", strings.NewReader(validTargetBody), mutateWithCSRF)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status %d: %s", w.Code, w.Body)
	}
	var created targetResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	body := `{"name":"edge-lb-2","kind":"url","address":"https://edge/healthz","labels":{"zone":"a"}}`
	w = doRequest(t, s, http.MethodPut, "/api/v1/targets/"+created.ID, strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("update status %d, want 200: %s", w.Code, w.Body)
	}
	var updated targetResponse
	if err := json.Unmarshal(w.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode update: %v", err)
	}
	if updated.ID != created.ID || updated.Name != "edge-lb-2" || updated.Kind != "url" || updated.Address != "https://edge/healthz" {
		t.Fatalf("updated = %+v, want the replaced fields under the same id", updated)
	}

	// A follow-up GET reads the same row back -- the round trip, not just
	// the write's own echo.
	w = doRequest(t, s, http.MethodGet, "/api/v1/targets/"+created.ID, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get status %d: %s", w.Code, w.Body)
	}
	var fetched targetResponse
	if err := json.Unmarshal(w.Body.Bytes(), &fetched); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if fetched.Name != "edge-lb-2" || string(fetched.Labels) != `{"zone":"a"}` {
		t.Errorf("fetched = %+v, want the update to have persisted", fetched)
	}
}

func TestTargetsUpdateUnknownIDReturns404(t *testing.T) {
	s := newTargetsTestServer(t, "operator", newFakeTargetService(), Deps{})
	w := doRequest(t, s, http.MethodPut, "/api/v1/targets/"+uuid.NewString(), strings.NewReader(validTargetBody), mutateWithCSRF)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404: %s", w.Code, w.Body)
	}
}

func TestTargetsDeleteInUseReturns409(t *testing.T) {
	fake := newFakeTargetService()
	s := newTargetsTestServer(t, "operator", fake, Deps{})
	w := doRequest(t, s, http.MethodPost, "/api/v1/targets", strings.NewReader(validTargetBody), mutateWithCSRF)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status %d: %s", w.Code, w.Body)
	}
	var created targetResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	fake.inUse[created.ID] = true

	w = doRequest(t, s, http.MethodDelete, "/api/v1/targets/"+created.ID, nil, mutateWithCSRF)
	if w.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "definition") {
		t.Errorf("409 detail = %s, want it to name the check definitions still referencing the target", w.Body)
	}
}

func TestTargetsDeleteReturns204(t *testing.T) {
	s := newTargetsTestServer(t, "operator", newFakeTargetService(), Deps{})
	w := doRequest(t, s, http.MethodPost, "/api/v1/targets", strings.NewReader(validTargetBody), mutateWithCSRF)
	var created targetResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	w = doRequest(t, s, http.MethodDelete, "/api/v1/targets/"+created.ID, nil, mutateWithCSRF)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status %d, want 204: %s", w.Code, w.Body)
	}
	w = doRequest(t, s, http.MethodGet, "/api/v1/targets/"+created.ID, nil, nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", w.Code)
	}
}

func TestTargetsListReturnsRows(t *testing.T) {
	s := newTargetsTestServer(t, "operator", newFakeTargetService(), Deps{})
	for _, name := range []string{"a-one", "b-two"} {
		body := `{"name":"` + name + `","kind":"host","address":"10.0.0.1"}`
		if w := doRequest(t, s, http.MethodPost, "/api/v1/targets", strings.NewReader(body), mutateWithCSRF); w.Code != http.StatusCreated {
			t.Fatalf("create %s status %d: %s", name, w.Code, w.Body)
		}
	}
	w := doRequest(t, s, http.MethodGet, "/api/v1/targets", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var body targetsListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Targets) != 2 {
		t.Fatalf("targets = %+v, want 2", body.Targets)
	}
	// Empty list must marshal as [] and never null -- the frontend indexes
	// into it (same rule capabilities() follows, server.go).
	if !strings.Contains(w.Body.String(), `"targets":[`) {
		t.Errorf("body = %s, want a JSON array under \"targets\"", w.Body)
	}
}

func TestTargetsListInvalidCursorReturns400(t *testing.T) {
	s := newTargetsTestServer(t, "operator", newFakeTargetService(), Deps{})
	w := doRequest(t, s, http.MethodGet, "/api/v1/targets?cursor=not-a-cursor", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", w.Code, w.Body)
	}
}

// TestTargetsViewerIsForbiddenOnAllFive pins Decision 3 as a live-chain
// behaviour, not a permission-table opinion: viewer is what
// auth.anonymous.role defaults to, so a viewer gaining any of these would
// hand the whole anonymous internet the fleet's probe configuration.
func TestTargetsViewerIsForbiddenOnAllFive(t *testing.T) {
	s := newTargetsTestServer(t, "viewer", newFakeTargetService(), Deps{})
	for _, c := range targetRoutes(uuid.NewString()) {
		var mutate func(*http.Request)
		if isMutatingMethod(c.method) {
			mutate = mutateWithCSRF
		}
		w := doRequest(t, s, c.method, c.path, strings.NewReader(validTargetBody), mutate)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s as viewer = %d, want 403: %s", c.method, c.path, w.Code, w.Body)
		}
	}
}

// TestTargetsOperatorIsAllowedOnAllFive is the other half: operator holds
// targets:read + targets:write, so no route may answer 401/403.
func TestTargetsOperatorIsAllowedOnAllFive(t *testing.T) {
	fake := newFakeTargetService()
	s := newTargetsTestServer(t, "operator", fake, Deps{})

	w := doRequest(t, s, http.MethodPost, "/api/v1/targets", strings.NewReader(validTargetBody), mutateWithCSRF)
	if w.Code != http.StatusCreated {
		t.Fatalf("operator create = %d, want 201: %s", w.Code, w.Body)
	}
	var created targetResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	for _, c := range []struct {
		method, path string
		want         int
		body         string
	}{
		{http.MethodGet, "/api/v1/targets", http.StatusOK, ""},
		{http.MethodGet, "/api/v1/targets/" + created.ID, http.StatusOK, ""},
		{http.MethodPut, "/api/v1/targets/" + created.ID, http.StatusOK, `{"name":"edge-lb","kind":"host","address":"10.0.0.8"}`},
		{http.MethodDelete, "/api/v1/targets/" + created.ID, http.StatusNoContent, ""},
	} {
		var mutate func(*http.Request)
		if isMutatingMethod(c.method) {
			mutate = mutateWithCSRF
		}
		w := doRequest(t, s, c.method, c.path, strings.NewReader(c.body), mutate)
		if w.Code != c.want {
			t.Errorf("%s %s as operator = %d, want %d: %s", c.method, c.path, w.Code, c.want, w.Body)
		}
	}
}

// TestTargetsRouteTableCoversAllFiveRoutes is the narrow companion to
// TestEveryAPIRouteHasAPermissionDecision: it pins WHICH permission each
// route requires, so a future edit cannot silently swap read for write.
func TestTargetsRouteTableCoversAllFiveRoutes(t *testing.T) {
	cases := map[string]authz.Permission{
		"GET /api/v1/targets":         authz.PermTargetsRead,
		"POST /api/v1/targets":        authz.PermTargetsWrite,
		"GET /api/v1/targets/{id}":    authz.PermTargetsRead,
		"PUT /api/v1/targets/{id}":    authz.PermTargetsWrite,
		"DELETE /api/v1/targets/{id}": authz.PermTargetsWrite,
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

// TestTargetsAuditDetailNeverCarriesTheAddress pins the allow-list decision
// this task made consciously (DATA.md:32): name and kind only. An audit log
// is read by more people than a target list is, and the address is the one
// field that names internal infrastructure.
func TestTargetsAuditDetailNeverCarriesTheAddress(t *testing.T) {
	for _, c := range []struct{ method, pathSuffix string }{
		{http.MethodPost, ""},
		{http.MethodPut, "/{seed}"},
	} {
		fs := &fakeAuditStore{}
		fake := newFakeTargetService()
		s := newTargetsTestServer(t, "operator", fake, Deps{Audit: fs})

		path := "/api/v1/targets"
		body := `{"name":"edge-lb","kind":"host","address":"10.9.9.9-internal-secret"}`
		if c.method == http.MethodPut {
			seed, err := fake.CreateTarget(context.Background(), store.TargetInput{Name: "seed", Kind: "host", Address: "10.0.0.1"})
			if err != nil {
				t.Fatalf("seed: %v", err)
			}
			path += "/" + seed.ID
		}

		w := doRequest(t, s, c.method, path, strings.NewReader(body), mutateWithCSRF)
		if w.Code >= http.StatusBadRequest {
			t.Fatalf("%s %s status %d: %s", c.method, path, w.Code, w.Body)
		}

		entries := waitForOneAuditEntry(t, fs)
		detail := string(entries[0].Detail)
		if strings.Contains(detail, "address") || strings.Contains(detail, "10.9.9.9") {
			t.Errorf("%s detail = %s, must never carry the address", c.method, detail)
		}
		if !strings.Contains(detail, `"name":"edge-lb"`) || !strings.Contains(detail, `"kind":"host"`) {
			t.Errorf("%s detail = %s, want the allow-listed name and kind", c.method, detail)
		}
	}
}

// TestPanicInHandlerReturns500AndExactlyOneAuditRow is M3 follow-up #4: an
// unrecovered panic must become a 500 problem+json (never a dropped
// connection), and it must still be attributable -- exactly one audit row,
// outcome "error", carrying the route pattern and the acting subject. One
// row, not two: auditMutation's own recordAudit sits AFTER next.ServeHTTP,
// so a panic unwinds straight past it and only the recoverer records.
func TestPanicInHandlerReturns500AndExactlyOneAuditRow(t *testing.T) {
	captureLogs(t)
	fs := &fakeAuditStore{}
	fake := newFakeTargetService()
	fake.panicOnCreate = true
	s := newTargetsTestServer(t, "operator", fake, Deps{Audit: fs})

	w := doRequest(t, s, http.MethodPost, "/api/v1/targets", strings.NewReader(validTargetBody), mutateWithCSRF)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("panicking handler = %d, want 500: %s", w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	// The panic value itself must never reach the client.
	if strings.Contains(w.Body.String(), "boom") {
		t.Errorf("500 body = %s, must not leak the panic value", w.Body)
	}

	waitForOneAuditEntry(t, fs)
	// Give a hypothetical second row time to land before asserting there is
	// none -- the drain is asynchronous.
	time.Sleep(50 * time.Millisecond)
	entries := fs.snapshot()
	if len(entries) != 1 {
		t.Fatalf("audit entries = %d (%+v), want exactly 1", len(entries), entries)
	}
	e := entries[0]
	if e.Outcome != auditOutcomeError {
		t.Errorf("outcome = %q, want %q", e.Outcome, auditOutcomeError)
	}
	if e.Action != "POST /api/v1/targets" {
		t.Errorf("action = %q, want the route pattern", e.Action)
	}
	if e.SubjectKind != string(authz.SubjectUser) || e.SubjectID != "u1" {
		t.Errorf("subject = %s/%s, want user/u1 -- a panic row that cannot be attributed is half a row", e.SubjectKind, e.SubjectID)
	}
}

// TestPanicWithoutAuditStoreStillReturns500 proves the recoverer does not
// depend on a wired Auditor: database.mode=disabled must still answer 500
// problem+json instead of dropping the connection.
func TestPanicWithoutAuditStoreStillReturns500(t *testing.T) {
	captureLogs(t)
	fake := newFakeTargetService()
	fake.panicOnCreate = true
	s := newTargetsTestServer(t, "operator", fake, Deps{})

	w := doRequest(t, s, http.MethodPost, "/api/v1/targets", strings.NewReader(validTargetBody), mutateWithCSRF)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("panicking handler with no Auditor = %d, want 500: %s", w.Code, w.Body)
	}
}

// TestPanicIsCountedByInstrument proves the recoverer sits INSIDE
// s.instrument (brief: "outermost middleware after s.instrument"), which is
// the only arrangement where a panic-born 500 is still counted with its
// route pattern rather than escaping the metric entirely.
func TestPanicIsCountedByInstrument(t *testing.T) {
	captureLogs(t)
	fake := newFakeTargetService()
	fake.panicOnCreate = true
	s := newTargetsTestServer(t, "operator", fake, Deps{})

	doRequest(t, s, http.MethodPost, "/api/v1/targets", strings.NewReader(validTargetBody), mutateWithCSRF)

	got := testutil.ToFloat64(s.metrics.HTTPRequests.WithLabelValues(http.MethodPost, "/api/v1/targets", "500"))
	if got != 1 {
		t.Errorf("http_requests_total{path=/api/v1/targets,status=500} = %v, want 1", got)
	}
}

// TestAuditFlushLogsDroppedCount is the last piece of follow-up #4: on
// shutdown the drain reports how many rows this process dropped, so a
// silently lossy audit trail leaves a trace in the pod's own logs rather
// than only in a metric nobody scraped before the pod went away.
func TestAuditFlushLogsDroppedCount(t *testing.T) {
	logs := captureLogs(t)
	fs := newStalledAuditStore()
	s := newAuditTestServer(t, fs, []authz.Permission{}, Deps{})

	w := doRequest(t, s, http.MethodPost, "/api/v1/auth/logout", strings.NewReader(`{}`), mutateWithCSRF)
	if w.Code != http.StatusNoContent {
		t.Fatalf("logout status %d, want 204", w.Code)
	}
	select {
	case <-fs.started:
	case <-time.After(2 * time.Second):
		t.Fatal("drain goroutine never entered InsertAuditEntry")
	}
	defer close(fs.release)

	for i := 0; i < auditBufferSize+4; i++ {
		doRequest(t, s, http.MethodPost, "/api/v1/auth/logout", strings.NewReader(`{}`), mutateWithCSRF)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	s.flushAudit(ctx)

	out := logs.String()
	if !strings.Contains(out, "dropped") {
		t.Fatalf("shutdown flush logged %q, want a dropped-count field", out)
	}
	if strings.Contains(out, "dropped=0") {
		t.Errorf("shutdown flush logged dropped=0 after overflowing the buffer: %s", out)
	}
}

// TestAuditFlushIsNoOpWithoutAuditor pins the database.mode=disabled path:
// no Auditor means no drain goroutine and nothing to flush, and flushAudit
// must return immediately rather than block on a nil channel.
func TestAuditFlushIsNoOpWithoutAuditor(t *testing.T) {
	s := newTargetsTestServer(t, "operator", newFakeTargetService(), Deps{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.flushAudit(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("flushAudit blocked with no Auditor wired")
	}
}

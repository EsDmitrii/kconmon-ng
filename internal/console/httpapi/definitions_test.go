package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// fakeChecksStore is one double for BOTH DefinitionService and ScheduleService; keeping them in one
// struct is what lets it reproduce the two cross-table behaviours the handlers depend on and that
// no separate pair of fakes could show.
type fakeChecksStore struct {
	mu sync.Mutex

	defs     map[string]store.Definition
	defNames map[string]string
	scheds   map[string]store.Schedule
	// targets names the ids a definition may point at, standing in for the
	// check_definitions.destination_target_id FK.
	targets map[string]bool

	// deletedDefinitions records every DeleteDefinition call in order, so a
	// test can assert the handler reached the store at all.
	deletedDefinitions []string

	// createDefErr, if set, is returned by CreateDefinition instead of
	// creating -- the opaque-backend-failure -> 502 case.
	createDefErr error
}

func newFakeChecksStore() *fakeChecksStore {
	return &fakeChecksStore{
		defs:     map[string]store.Definition{},
		defNames: map[string]string{},
		scheds:   map[string]store.Schedule{},
		targets:  map[string]bool{},
	}
}

func (f *fakeChecksStore) CreateDefinition(_ context.Context, in store.DefinitionInput) (store.Definition, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createDefErr != nil {
		return store.Definition{}, f.createDefErr
	}
	if err := in.Validate(); err != nil {
		return store.Definition{}, err
	}
	if _, taken := f.defNames[in.Name]; taken {
		return store.Definition{}, store.ErrAlreadyExists
	}
	if in.DestinationTargetID != "" && !f.targets[in.DestinationTargetID] {
		return store.Definition{}, store.ErrNotFound
	}
	now := time.Now().UTC()
	d := definitionFromInput(uuid.NewString(), &in, now, now)
	f.defs[d.ID] = d
	f.defNames[d.Name] = d.ID
	return d, nil
}

func (f *fakeChecksStore) UpdateDefinition(_ context.Context, id string, in store.DefinitionInput) (store.Definition, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := in.Validate(); err != nil {
		return store.Definition{}, err
	}
	existing, ok := f.defs[id]
	if !ok {
		return store.Definition{}, store.ErrNotFound
	}
	// The real store folds an unresolved destination FK into ErrNotFound too, exactly as the create
	// path above does; a fake that skipped it made the handler look like it distinguished them.
	if in.DestinationTargetID != "" && !f.targets[in.DestinationTargetID] {
		return store.Definition{}, store.ErrNotFound
	}
	if other, taken := f.defNames[in.Name]; taken && other != id {
		return store.Definition{}, store.ErrAlreadyExists
	}
	delete(f.defNames, existing.Name)
	updated := definitionFromInput(id, &in, existing.CreatedAt, time.Now().UTC())
	f.defs[id] = updated
	f.defNames[in.Name] = id
	return updated, nil
}

func (f *fakeChecksStore) DeleteDefinition(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletedDefinitions = append(f.deletedDefinitions, id)
	d, ok := f.defs[id]
	if !ok {
		return store.ErrNotFound
	}
	delete(f.defs, id)
	delete(f.defNames, d.Name)
	// ON DELETE CASCADE, modelled.
	for sid, s := range f.scheds {
		if s.DefinitionID == id {
			delete(f.scheds, sid)
		}
	}
	return nil
}

func (f *fakeChecksStore) GetDefinition(_ context.Context, id string) (store.Definition, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.defs[id]
	if !ok {
		return store.Definition{}, store.ErrNotFound
	}
	return d, nil
}

func (f *fakeChecksStore) ListDefinitions(_ context.Context, filter store.DefinitionFilter) (store.DefinitionPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.Definition, 0, len(f.defs))
	for _, d := range f.defs {
		if filter.TargetID != "" && d.DestinationTargetID != filter.TargetID {
			continue
		}
		if filter.Enabled != nil && d.Enabled != *filter.Enabled {
			continue
		}
		out = append(out, d)
	}
	return store.DefinitionPage{Definitions: out}, nil
}

func definitionFromInput(id string, in *store.DefinitionInput, created, updated time.Time) store.Definition {
	params := in.Params
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	return store.Definition{
		ID: id, Name: in.Name, SourceSelection: in.SourceSelection,
		DestinationKind: in.DestinationKind, DestinationTargetID: in.DestinationTargetID,
		DestinationAddress: in.DestinationAddress, CheckType: in.CheckType, Plane: in.Plane,
		Params: params, Enabled: in.Enabled, CreatedAt: created, UpdatedAt: updated,
	}
}

// fakeTopology is a TopologySource pinned to a FIXED snapshot -- the whole
// point of TopologySource being an interface. err, when set, stands in for a
// controller that did not answer.
type fakeTopology struct {
	topo *controllerclient.Topology
	err  error
}

func (f fakeTopology) Topology(context.Context) (*controllerclient.Topology, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.topo, nil
}

// topologyWith builds a snapshot of perZone agents in each of the named
// zones, so a test states its topology as "3 agents in each of 2 zones"
// rather than as a literal slice.
func topologyWith(perZone int, zones ...string) *controllerclient.Topology {
	topo := &controllerclient.Topology{Timestamp: time.Now().UTC()}
	for _, zone := range zones {
		for i := 0; i < perZone; i++ {
			name := fmt.Sprintf("%s-node-%d", zone, i)
			topo.Nodes = append(topo.Nodes, controllerclient.Node{Name: name, Zone: zone, Ready: true})
			topo.Agents = append(topo.Agents, controllerclient.Agent{
				ID: name + "-agent", NodeName: name, PodIP: "10.0.0.1", Zone: zone,
			})
		}
	}
	return topo
}

// newChecksTestServer wires a Server whose subject holds the given BUILT-IN role, using the real
// compiled-in role sets.
func newChecksTestServer(t *testing.T, role string, extra Deps) *Server {
	t.Helper()
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u1"}}
	extra.Roles = fakeRoleResolver{roles: []string{role}}
	return newAuthzServer(t, authr, authz.NewPolicy(nil), extra)
}

// newOperatorChecksServer is newChecksTestServer for the common case.
func newOperatorChecksServer(t *testing.T, st *fakeChecksStore, topo TopologySource) *Server {
	t.Helper()
	return newChecksTestServer(t, "operator", Deps{Definitions: st, Schedules: st, Topology: topo})
}

func definitionRoutes(id string) []struct{ method, path string } {
	return []struct{ method, path string }{
		{http.MethodGet, "/api/v1/checks"},
		{http.MethodPost, "/api/v1/checks"},
		{http.MethodGet, "/api/v1/checks/" + id},
		{http.MethodPut, "/api/v1/checks/" + id},
		{http.MethodDelete, "/api/v1/checks/" + id},
	}
}

const validDefinitionBody = `{"name":"edge-tcp","sourceSelection":"one-per-zone",` +
	`"destinationKind":"adhoc","destinationAddress":"8.8.8.8:53","checkType":"tcp","plane":"pod"}`

// definitionBody renders a create/update body with the two fields the
// projection guard turns on: the selection mode and whether it arrives
// enabled.
func definitionBody(name, selection string, enabled bool) string {
	return fmt.Sprintf(`{"name":%q,"sourceSelection":%q,"destinationKind":"adhoc",`+
		`"destinationAddress":"8.8.8.8:53","checkType":"tcp","plane":"pod","enabled":%t}`,
		name, selection, enabled)
}

func TestDefinitionsWithoutStoreReturns503(t *testing.T) {
	s := newChecksTestServer(t, "operator", Deps{})
	for _, c := range definitionRoutes(uuid.NewString()) {
		var mutate func(*http.Request)
		if isMutatingMethod(c.method) {
			mutate = mutateWithCSRF
		}
		w := doRequest(t, s, c.method, c.path, strings.NewReader(validDefinitionBody), mutate)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s without a DefinitionService = %d, want 503: %s", c.method, c.path, w.Code, w.Body)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
			t.Errorf("%s %s Content-Type = %q, want application/problem+json", c.method, c.path, ct)
		}
		if !strings.Contains(w.Body.String(), "console.database.mode") {
			t.Errorf("%s %s 503 detail = %s, want it to name console.database.mode", c.method, c.path, w.Body)
		}
	}
}

func TestDefinitionsCreateReturns201AndLocation(t *testing.T) {
	s := newOperatorChecksServer(t, newFakeChecksStore(), nil)
	w := doRequest(t, s, http.MethodPost, "/api/v1/checks", strings.NewReader(validDefinitionBody), mutateWithCSRF)
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d, want 201: %s", w.Code, w.Body)
	}
	var got definitionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID == "" || got.Name != "edge-tcp" || got.CheckType != "tcp" || got.SourceSelection != "one-per-zone" {
		t.Fatalf("body = %+v, want the created definition echoed back", got)
	}
	if string(got.Params) != `{}` {
		t.Errorf("params = %s, want {} -- never JSON null, the frontend reads it as an object", got.Params)
	}
	if want := "/api/v1/checks/" + got.ID; w.Header().Get("Location") != want {
		t.Errorf("Location = %q, want %q", w.Header().Get("Location"), want)
	}
}

func TestDefinitionsCreateDuplicateNameReturns422(t *testing.T) {
	s := newOperatorChecksServer(t, newFakeChecksStore(), nil)
	for i := 0; i < 2; i++ {
		w := doRequest(t, s, http.MethodPost, "/api/v1/checks", strings.NewReader(validDefinitionBody), mutateWithCSRF)
		if i == 0 && w.Code != http.StatusCreated {
			t.Fatalf("first create = %d, want 201: %s", w.Code, w.Body)
		}
		if i == 1 && w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("duplicate name = %d, want 422: %s", w.Code, w.Body)
		}
	}
}

// A destinationTargetId naming no target is a rejected FIELD VALUE, not a
// missing definition -- store returns ErrNotFound for it, and answering 404
// would claim the endpoint just POSTed to does not exist.
func TestDefinitionsCreateUnknownTargetReturns422(t *testing.T) {
	s := newOperatorChecksServer(t, newFakeChecksStore(), nil)
	missing := uuid.NewString()
	body := fmt.Sprintf(`{"name":"edge-tcp","sourceSelection":"all","destinationKind":"target",`+
		`"destinationTargetId":%q,"checkType":"tcp","plane":"pod"}`, missing)
	w := doRequest(t, s, http.MethodPost, "/api/v1/checks", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown destination target = %d, want 422: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), missing) {
		t.Errorf("422 detail = %s, want it to name the target id", w.Body)
	}
}

func TestDefinitionsInvalidBodyReturns400And422(t *testing.T) {
	s := newOperatorChecksServer(t, newFakeChecksStore(), nil)

	w := doRequest(t, s, http.MethodPost, "/api/v1/checks", strings.NewReader(`not json`), mutateWithCSRF)
	if w.Code != http.StatusBadRequest {
		t.Errorf("unparseable body = %d, want 400: %s", w.Code, w.Body)
	}

	w = doRequest(t, s, http.MethodPost, "/api/v1/checks",
		strings.NewReader(`{"name":"x","sourceSelection":"everywhere","destinationKind":"node","checkType":"tcp","plane":"pod"}`),
		mutateWithCSRF)
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad sourceSelection = %d, want 422: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "store:") {
		t.Errorf("422 detail = %s, want store's package prefix trimmed off the wire", w.Body)
	}
}

// A malformed id must be 404, never 502: without the uuid.Parse pre-check it
// would reach pgx and fail while ENCODING the parameter (M3 follow-up #5).
func TestDefinitionsMalformedAndUnknownIDReturn404(t *testing.T) {
	s := newOperatorChecksServer(t, newFakeChecksStore(), nil)
	for _, id := range []string{"not-a-uuid", uuid.NewString()} {
		for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
			var mutate func(*http.Request)
			if isMutatingMethod(method) {
				mutate = mutateWithCSRF
			}
			w := doRequest(t, s, method, "/api/v1/checks/"+id, strings.NewReader(validDefinitionBody), mutate)
			if w.Code != http.StatusNotFound {
				t.Errorf("%s /api/v1/checks/%s = %d, want 404: %s", method, id, w.Code, w.Body)
			}
		}
	}
}

func TestDefinitionsUpdateRoundTrips(t *testing.T) {
	st := newFakeChecksStore()
	s := newOperatorChecksServer(t, st, nil)

	w := doRequest(t, s, http.MethodPost, "/api/v1/checks", strings.NewReader(validDefinitionBody), mutateWithCSRF)
	var created definitionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	w = doRequest(t, s, http.MethodPut, "/api/v1/checks/"+created.ID,
		strings.NewReader(definitionBody("edge-icmp", "all", false)), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("update = %d, want 200: %s", w.Code, w.Body)
	}
	var updated definitionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode update: %v", err)
	}
	if updated.ID != created.ID || updated.Name != "edge-icmp" || updated.SourceSelection != "all" {
		t.Fatalf("update body = %+v, want the stored row, not an echo", updated)
	}
	if !updated.UpdatedAt.After(created.CreatedAt) && !updated.UpdatedAt.Equal(created.CreatedAt) {
		t.Errorf("updatedAt = %v, want the server's own view", updated.UpdatedAt)
	}
}

// The cascade is the database's (ON DELETE CASCADE, migration 00004), so what
// the handler owes is exactly one DeleteDefinition call -- asserted here --
// and no second call to clean schedules up by hand.
func TestDefinitionsDeleteCascadesSchedules(t *testing.T) {
	st := newFakeChecksStore()
	s := newOperatorChecksServer(t, st, nil)

	w := doRequest(t, s, http.MethodPost, "/api/v1/checks", strings.NewReader(validDefinitionBody), mutateWithCSRF)
	var def definitionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &def); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	w = doRequest(t, s, http.MethodPost, "/api/v1/schedules",
		strings.NewReader(fmt.Sprintf(`{"definitionId":%q,"kind":"interval","intervalNs":%d,"enabled":true}`,
			def.ID, int64(time.Minute))), mutateWithCSRF)
	if w.Code != http.StatusCreated {
		t.Fatalf("create schedule = %d, want 201: %s", w.Code, w.Body)
	}

	w = doRequest(t, s, http.MethodDelete, "/api/v1/checks/"+def.ID, nil, mutateWithCSRF)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204: %s", w.Code, w.Body)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.deletedDefinitions) != 1 || st.deletedDefinitions[0] != def.ID {
		t.Fatalf("DeleteDefinition calls = %v, want exactly one for %s", st.deletedDefinitions, def.ID)
	}
	if len(st.scheds) != 0 {
		t.Errorf("schedules after cascade = %d, want 0", len(st.scheds))
	}
}

func TestDefinitionsStoreFailureReturns502(t *testing.T) {
	st := newFakeChecksStore()
	st.createDefErr = errors.New("connection refused")
	s := newOperatorChecksServer(t, st, nil)
	w := doRequest(t, s, http.MethodPost, "/api/v1/checks", strings.NewReader(validDefinitionBody), mutateWithCSRF)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("opaque store failure = %d, want 502: %s", w.Code, w.Body)
	}
}

func TestDefinitionsViewerIsForbiddenOperatorIsNot(t *testing.T) {
	routes := append(definitionRoutes(uuid.NewString()),
		struct{ method, path string }{http.MethodPost, "/api/v1/checks/projection"})

	viewer := newChecksTestServer(t, "viewer", Deps{Definitions: newFakeChecksStore(), Schedules: newFakeChecksStore()})
	for _, c := range routes {
		var mutate func(*http.Request)
		if isMutatingMethod(c.method) {
			mutate = mutateWithCSRF
		}
		w := doRequest(t, viewer, c.method, c.path, strings.NewReader(validDefinitionBody), mutate)
		if w.Code != http.StatusForbidden {
			t.Errorf("viewer %s %s = %d, want 403 -- checks:* stops at operator (Decision 3): %s",
				c.method, c.path, w.Code, w.Body)
		}
	}

	operator := newOperatorChecksServer(t, newFakeChecksStore(), fakeTopology{topo: topologyWith(1, "zone-a")})
	for _, c := range routes {
		var mutate func(*http.Request)
		if isMutatingMethod(c.method) {
			mutate = mutateWithCSRF
		}
		w := doRequest(t, operator, c.method, c.path, strings.NewReader(validDefinitionBody), mutate)
		if w.Code == http.StatusForbidden || w.Code == http.StatusUnauthorized {
			t.Errorf("operator %s %s = %d, want to pass authorization: %s", c.method, c.path, w.Code, w.Body)
		}
	}
}

// The projection math, per selection mode, against a FIXED topology: 3 agents in each of 2 zones.
func TestProjectionMathPerSelectionMode(t *testing.T) {
	topo := topologyWith(3, "zone-a", "zone-b")
	s := newOperatorChecksServer(t, newFakeChecksStore(), fakeTopology{topo: topo})

	cases := []struct {
		selection  string
		wantAgents int
	}{
		{"all", 6},
		{"per-zone", 6},
		{"one-per-zone", 2},
	}
	for _, c := range cases {
		t.Run(c.selection, func(t *testing.T) {
			w := doRequest(t, s, http.MethodPost, "/api/v1/checks/projection",
				strings.NewReader(definitionBody("edge-tcp", c.selection, true)), mutateWithCSRF)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d, want 200: %s", w.Code, w.Body)
			}
			var got projectionResponse
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			want := projectionResponse{
				Agents: c.wantAgents, Protocols: 1, Series: c.wantAgents,
				Limit: maxProjectedSeries, OverLimit: false,
			}
			if got != want {
				t.Fatalf("projection = %+v, want %+v", got, want)
			}
		})
	}
}

// Nothing is persisted by a projection: the endpoint is a calculator, and a
// UI that calls it on every keystroke must not leave a trail of definitions.
func TestProjectionPersistsNothing(t *testing.T) {
	st := newFakeChecksStore()
	s := newOperatorChecksServer(t, st, fakeTopology{topo: topologyWith(1, "zone-a")})
	w := doRequest(t, s, http.MethodPost, "/api/v1/checks/projection",
		strings.NewReader(validDefinitionBody), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", w.Code, w.Body)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.defs) != 0 {
		t.Fatalf("definitions after a projection = %d, want 0", len(st.defs))
	}
}

func TestProjectionReportsOverLimit(t *testing.T) {
	s := newOperatorChecksServer(t, newFakeChecksStore(),
		fakeTopology{topo: topologyWith(maxProjectedSeries+1, "zone-a")})
	w := doRequest(t, s, http.MethodPost, "/api/v1/checks/projection",
		strings.NewReader(definitionBody("edge-tcp", "all", false)), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 -- the projection endpoint REPORTS, it never refuses: %s", w.Code, w.Body)
	}
	var got projectionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.OverLimit || got.Series != maxProjectedSeries+1 {
		t.Fatalf("projection = %+v, want series %d and overLimit true", got, maxProjectedSeries+1)
	}
}

func TestProjectionWithoutTopologyReturns503(t *testing.T) {
	s := newOperatorChecksServer(t, newFakeChecksStore(), nil)
	w := doRequest(t, s, http.MethodPost, "/api/v1/checks/projection",
		strings.NewReader(validDefinitionBody), mutateWithCSRF)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("projection with no topology = %d, want 503: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "console.controller.url") {
		t.Errorf("503 detail = %s, want it to name console.controller.url", w.Body)
	}
}

func TestProjectionControllerErrorReturns502(t *testing.T) {
	s := newOperatorChecksServer(t, newFakeChecksStore(),
		fakeTopology{err: errors.New("no leader answered")})
	w := doRequest(t, s, http.MethodPost, "/api/v1/checks/projection",
		strings.NewReader(validDefinitionBody), mutateWithCSRF)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("projection with a broken controller = %d, want 502: %s", w.Code, w.Body)
	}
}

// The gated action is ENABLING, not saving: the same definition is refused
// with 422 when it arrives enabled and accepted with 201 when it does not,
// against the identical over-limit topology.
func TestDefinitionsEnablingOverTheLimitReturns422(t *testing.T) {
	topo := fakeTopology{topo: topologyWith(maxProjectedSeries+1, "zone-a")}
	s := newOperatorChecksServer(t, newFakeChecksStore(), topo)

	w := doRequest(t, s, http.MethodPost, "/api/v1/checks",
		strings.NewReader(definitionBody("edge-tcp", "all", true)), mutateWithCSRF)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("enabling over the limit = %d, want 422: %s", w.Code, w.Body)
	}
	body := w.Body.String()
	// The projected NUMBER has to be in the detail: the bare word "too many"
	// tells an operator nothing about which knob to turn.
	for _, want := range []string{
		strconv.Itoa(maxProjectedSeries + 1),
		strconv.Itoa(maxProjectedSeries),
		"agents",
		"one-per-zone",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("422 detail = %s, want it to contain %q", body, want)
		}
	}
}

func TestDefinitionsSavingOverTheLimitWhileDisabledReturns201(t *testing.T) {
	st := newFakeChecksStore()
	s := newOperatorChecksServer(t, st, fakeTopology{topo: topologyWith(maxProjectedSeries+1, "zone-a")})
	w := doRequest(t, s, http.MethodPost, "/api/v1/checks",
		strings.NewReader(definitionBody("edge-tcp", "all", false)), mutateWithCSRF)
	if w.Code != http.StatusCreated {
		t.Fatalf("saving over the limit while disabled = %d, want 201 -- drafting must not be blocked: %s", w.Code, w.Body)
	}
}

func TestDefinitionsUpdateEnablingOverTheLimitReturns422(t *testing.T) {
	st := newFakeChecksStore()
	s := newOperatorChecksServer(t, st, fakeTopology{topo: topologyWith(maxProjectedSeries+1, "zone-a")})

	w := doRequest(t, s, http.MethodPost, "/api/v1/checks",
		strings.NewReader(definitionBody("edge-tcp", "all", false)), mutateWithCSRF)
	var created definitionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	w = doRequest(t, s, http.MethodPut, "/api/v1/checks/"+created.ID,
		strings.NewReader(definitionBody("edge-tcp", "all", true)), mutateWithCSRF)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("update that enables over the limit = %d, want 422: %s", w.Code, w.Body)
	}

	// ...and the same update stays accepted while it keeps the definition
	// disabled, and while it shrinks the selection under the limit.
	w = doRequest(t, s, http.MethodPut, "/api/v1/checks/"+created.ID,
		strings.NewReader(definitionBody("edge-tcp", "one-per-zone", true)), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("update to one-per-zone = %d, want 200: %s", w.Code, w.Body)
	}
}

// A controller outage must not become a configuration-write outage.
func TestProjectionGuardFailsOpenWhenTopologyIsUnavailable(t *testing.T) {
	for name, topo := range map[string]TopologySource{
		"no topology source": nil,
		"controller error":   fakeTopology{err: errors.New("no leader answered")},
	} {
		t.Run(name, func(t *testing.T) {
			s := newOperatorChecksServer(t, newFakeChecksStore(), topo)
			w := doRequest(t, s, http.MethodPost, "/api/v1/checks",
				strings.NewReader(definitionBody("edge-tcp", "all", true)), mutateWithCSRF)
			if w.Code != http.StatusCreated {
				t.Fatalf("create with an unreadable topology = %d, want 201 (fail open): %s", w.Code, w.Body)
			}
		})
	}
}

// NewServer falls back to the controller when no explicit Topology is wired, and -- the trap
// Deps.Events documents.
func TestTopologyFallsBackToControllerWithoutTypedNil(t *testing.T) {
	s := newChecksTestServer(t, "operator", Deps{Definitions: newFakeChecksStore()})
	if s.topology != nil {
		t.Fatalf("topology = %#v with no Controller and no Topology, want a genuine nil interface", s.topology)
	}

	ctrl := controllerclient.New("http://controller.invalid", time.Second)
	s = newChecksTestServer(t, "operator", Deps{Definitions: newFakeChecksStore(), Controller: ctrl})
	if s.topology == nil {
		t.Fatal("topology = nil with a Controller wired, want the fallback to have kicked in")
	}
}

func TestProjectedAgentsCountsEmptyZoneAsItsOwnBucket(t *testing.T) {
	topo := &controllerclient.Topology{Agents: []controllerclient.Agent{
		{ID: "a1", Zone: "zone-a"},
		{ID: "a2", Zone: ""},
		{ID: "a3", Zone: "zone-a"},
	}}
	if got := projectedAgents("one-per-zone", topo); got != 2 {
		t.Errorf("one-per-zone over {zone-a, \"\"} = %d, want 2 -- a zoneless agent still exports series", got)
	}
	if got := projectedAgents("all", nil); got != 0 {
		t.Errorf("all over a nil topology = %d, want 0", got)
	}
}

// TestChecksAuditDetailNeverCarriesDestinationOrParams pins the audit allow-list decision the same
// way targets_test pins its address exclusion; allow-listed keys (name, checkType) must appear.
func TestChecksAuditDetailNeverCarriesDestinationOrParams(t *testing.T) {
	fs := &fakeAuditStore{}
	st := newFakeChecksStore()
	s := newChecksTestServer(t, "operator", Deps{Definitions: st, Schedules: st, Audit: fs})

	body := `{"name":"edge-tcp","sourceSelection":"one-per-zone","destinationKind":"adhoc",` +
		`"destinationAddress":"10.66.6.6-internal-secret","checkType":"tcp","plane":"pod",` +
		`"params":{"secretKnob":"leakme"}}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/checks", strings.NewReader(body), mutateWithCSRF)
	if w.Code >= http.StatusBadRequest {
		t.Fatalf("POST /api/v1/checks status %d: %s", w.Code, w.Body)
	}

	entries := waitForOneAuditEntry(t, fs)
	detail := string(entries[0].Detail)
	for _, banned := range []string{"destinationAddress", "10.66.6.6", "secretKnob", "leakme"} {
		if strings.Contains(detail, banned) {
			t.Errorf("audit detail = %s, must never carry %q", detail, banned)
		}
	}
	if !strings.Contains(detail, `"name":"edge-tcp"`) || !strings.Contains(detail, `"checkType":"tcp"`) {
		t.Errorf("audit detail = %s, want the allow-listed name and checkType", detail)
	}
}

// TestAuditDetailAllowlistIsPinned is the generic guard the per-route tests back up: the WHOLE
// allow-list.
func TestAuditDetailAllowlistIsPinned(t *testing.T) {
	want := map[string][]string{
		"POST /api/v1/auth/login":    {"username"},
		"POST /api/v1/rbac/roles":    {"name", "permissions"},
		"POST /api/v1/rbac/bindings": {"roleName", "subjectKind", "subjectId"},
		"POST /api/v1/tokens":        {"name", "expiresAt"},
		// destinationKind joined the runs entry.
		"POST /api/v1/runs":        {"type", "plane", "destinationKind"},
		"POST /api/v1/targets":     {"name", "kind"},
		"PUT /api/v1/targets/{id}": {"name", "kind"},
		// checks: name + the three safe non-body fields (enum, bool) — NEVER
		// destinationAddress (internal infrastructure) or params (an open
		// object that can carry anything).
		"POST /api/v1/checks":     {"name", "checkType", "sourceSelection", "enabled"},
		"PUT /api/v1/checks/{id}": {"name", "checkType", "sourceSelection", "enabled"},
		// schedules: identifying UUID + enum + bool; intervalNs/runAt stay
		// out (reconstructible from the row, noise in the audit trail).
		"POST /api/v1/schedules":     {"definitionId", "kind", "enabled"},
		"PUT /api/v1/schedules/{id}": {"definitionId", "kind", "enabled"},
		// annotations: the SCOPE alone; "text" is free-form operator prose and must never reach an audit
		// row.
		"POST /api/v1/annotations": {"scope"},
		// incidents: what was opened, about what, and where it stands; "notes" (free-form prose) and
		// "pinned" (an open array whose refs carry more free-form prose) must never reach an audit row.
		"POST /api/v1/incidents":       {"title", "scope", "status"},
		"PATCH /api/v1/incidents/{id}": {"status"},
		// maintenance: the SCOPE alone. "reason" is free text, on the exact
		// annotations "text" line.
		"POST /api/v1/maintenance": {"scope"},
		// webhooks: name + the event filter; NEVER "secret" (the HMAC signing key) and NEVER "url"
		// (external infrastructure whose path routinely embeds a token).
		"POST /api/v1/webhooks":     {"name", "events"},
		"PUT /api/v1/webhooks/{id}": {"name", "events"},
		// alert rules: the NAME alone.
		"POST /api/v1/alert-rules":     {"name"},
		"PUT /api/v1/alert-rules/{id}": {"name"},
		// import (adopt-foreign): the FOREIGN OBJECT's name, which is the whole body.
		"POST /api/v1/alert-rules/import": {"name"},
		// import: the dryRun FLAG alone.
		"POST /api/v1/import": {"dryRun"},
	}
	assertAllowlistPinned(t, "auditDetailAllowlist", auditDetailAllowlist, want)
}

// TestAuditResultAllowlistIsPinned is TestAuditDetailAllowlistIsPinned for the second; same
// default-deny posture, same conscious-widening guard. A key here names either a COUNT (import) or
// the identity of the RBAC row the request acted on — never anything a body carried unexamined.
func TestAuditResultAllowlistIsPinned(t *testing.T) {
	want := map[string][]string{
		"POST /api/v1/rbac/bindings":        {"bindingId"},
		"DELETE /api/v1/rbac/bindings/{id}": {"bindingId", "roleName", "subjectKind", "subjectId"},
		"POST /api/v1/import": {
			"dryRun",
			"targets", "checkDefinitions", "checkSchedules",
			"alertRules", "webhooks", "maintenanceWindows",
			"rbacRoles", "rbacBindings",
		},
	}
	assertAllowlistPinned(t, "auditResultAllowlist", auditResultAllowlist, want)
}

// assertAllowlistPinned compares one allow-list against its hand-pinned copy,
// route by route and key by key, order included.
func assertAllowlistPinned(t *testing.T, name string, got, want map[string][]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s has %d routes, pinned copy has %d — update BOTH, consciously", name, len(got), len(want))
	}
	for route, wantKeys := range want {
		gotKeys, ok := got[route]
		if !ok {
			t.Errorf("%s is missing pinned route %q", name, route)
			continue
		}
		if len(gotKeys) != len(wantKeys) {
			t.Errorf("%s[%s] = %v, pinned %v", name, route, gotKeys, wantKeys)
			continue
		}
		for i := range wantKeys {
			if gotKeys[i] != wantKeys[i] {
				t.Errorf("%s[%s][%d] = %q, pinned %q", name, route, i, gotKeys[i], wantKeys[i])
			}
		}
	}
}

// TestChecksListFiltersAndBadInputs is the list-surface coverage the targets
// precedent set: rows come back, filters narrow, and malformed cursor or
// targetId are the CLIENT's 400 — never a 502 that reads as an outage.
func TestChecksListFiltersAndBadInputs(t *testing.T) {
	st := newFakeChecksStore()
	s := newOperatorChecksServer(t, st, nil)

	seed, err := st.CreateDefinition(context.Background(), store.DefinitionInput{
		Name: "edge-tcp", SourceSelection: "all", DestinationKind: "adhoc",
		DestinationAddress: "8.8.8.8:53", CheckType: "tcp", Plane: "pod",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	w := doRequest(t, s, http.MethodGet, "/api/v1/checks", nil, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), seed.ID) {
		t.Fatalf("list = %d %s, want 200 carrying the seeded id", w.Code, w.Body)
	}

	w = doRequest(t, s, http.MethodGet, "/api/v1/checks?cursor=garbage", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("garbage cursor = %d, want 400", w.Code)
	}
	w = doRequest(t, s, http.MethodGet, "/api/v1/checks?targetId=not-a-uuid", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("garbage targetId = %d, want 400 (M4 Task 4 fix pass: a typo is never a 502)", w.Code)
	}
}

/*
 * A body naming a target that does not exist is a 422 about the BODY, not a 404 about the check.
 *
 * The store folds "no such definition row" and "the destination target FK does not resolve" into one
 * ErrNotFound, so the update path answered 404 "no check definition with that id" for a definition
 * sitting untouched in the table -- telling the operator their check had vanished when the real
 * problem was one field they had just typed.
 */
func TestChecksUpdateWithAnUnknownTargetIs422NotAMissingDefinition(t *testing.T) {
	st := newFakeChecksStore()
	known := uuid.NewString()
	st.targets[known] = true
	s := newOperatorChecksServer(t, st, nil)

	created := doRequest(t, s, http.MethodPost, "/api/v1/checks",
		strings.NewReader(fmt.Sprintf(`{"name":"edge-tcp","sourceSelection":"all","destinationKind":"target",`+
			`"destinationTargetId":%q,"checkType":"tcp","plane":"pod"}`, known)), mutateWithCSRF)
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", created.Code, created.Body)
	}
	var def struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &def); err != nil {
		t.Fatalf("decode created definition: %v", err)
	}

	missing := uuid.NewString()
	w := doRequest(t, s, http.MethodPut, "/api/v1/checks/"+def.ID,
		strings.NewReader(fmt.Sprintf(`{"name":"edge-tcp","sourceSelection":"all","destinationKind":"target",`+
			`"destinationTargetId":%q,"checkType":"tcp","plane":"pod"}`, missing)), mutateWithCSRF)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("update with an unknown destination target = %d, want 422: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "names no target") {
		t.Errorf("body does not name the real problem: %s", w.Body)
	}
}

/*
 * A check no agent could ever run is refused at the door.
 *
 * The agent parses every assignment entry and drops the ones it cannot -- checkType=http against a
 * host destination, checkType=dns with no params.query. That drop was agent-local: a WARN in one
 * pod's log, repeated on every push, forever, while the Console listed the definition as enabled and
 * the operator waited for results that could not arrive from any node.
 */
func TestChecksCreateRefusesADefinitionNoAgentCanRun(t *testing.T) {
	s := newOperatorChecksServer(t, newFakeChecksStore(), nil)

	for _, c := range []struct{ name, body string }{
		{"http against a host destination", `{"name":"portal","sourceSelection":"all","destinationKind":"adhoc",` +
			`"destinationAddress":"api.example.com:443","checkType":"http","plane":"pod"}`},
		{"dns with no query", `{"name":"resolve","sourceSelection":"all","destinationKind":"adhoc",` +
			`"destinationAddress":"10.0.0.10","checkType":"dns","plane":"pod"}`},
	} {
		w := doRequest(t, s, http.MethodPost, "/api/v1/checks", strings.NewReader(c.body), mutateWithCSRF)
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s = %d, want 422: %s", c.name, w.Code, w.Body)
		}
	}

	// And the shapes that DO run are untouched.
	for _, c := range []struct{ name, body string }{
		{"http against a URL", `{"name":"portal","sourceSelection":"all","destinationKind":"adhoc",` +
			`"destinationAddress":"https://api.example.com/healthz","checkType":"http","plane":"pod"}`},
		{"dns with a query", `{"name":"resolve","sourceSelection":"all","destinationKind":"adhoc",` +
			`"destinationAddress":"10.0.0.10","checkType":"dns","plane":"pod","params":{"query":"api.example.com"}}`},
		{"tcp against a host", `{"name":"edge","sourceSelection":"all","destinationKind":"adhoc",` +
			`"destinationAddress":"api.example.com:443","checkType":"tcp","plane":"pod"}`},
	} {
		w := doRequest(t, s, http.MethodPost, "/api/v1/checks", strings.NewReader(c.body), mutateWithCSRF)
		if w.Code != http.StatusCreated {
			t.Errorf("%s = %d, want 201: %s", c.name, w.Code, w.Body)
		}
	}
}

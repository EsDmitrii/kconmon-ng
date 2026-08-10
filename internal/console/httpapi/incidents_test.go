package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// fakeIncidentStore is one double for IncidentService (read seam + write seam), mutex-guarded like
// fakeAnnotationStore.
type fakeIncidentStore struct {
	mu sync.Mutex

	incidents map[string]store.Incident
	order     []string // creation order

	lastFilter store.IncidentFilter

	createErr error
	listErr   error
	getErr    error
	statusErr error
	notesErr  error
	pinnedErr error
	deleteErr error
}

func newFakeIncidentStore() *fakeIncidentStore {
	return &fakeIncidentStore{incidents: map[string]store.Incident{}}
}

func (f *fakeIncidentStore) CreateIncident(_ context.Context, in store.IncidentInput) (store.Incident, error) { //nolint:gocritic // hugeParam: test double mirrors the store signature
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return store.Incident{}, f.createErr
	}
	if err := in.Validate(); err != nil {
		return store.Incident{}, err
	}
	status := in.Status
	if status == "" {
		status = store.IncidentStatusOpen
	}
	pinned := in.Pinned
	if len(pinned) == 0 {
		pinned = json.RawMessage(`[]`)
	}
	inc := store.Incident{
		ID: uuid.NewString(), Title: in.Title, Scope: in.Scope, FromAt: in.FromAt, ToAt: in.ToAt,
		Status: status, Notes: in.Notes, Pinned: pinned, CreatedBy: in.CreatedBy,
		CreatedAt: time.Now().UTC(), ResolvedAt: in.ResolvedAt,
	}
	f.incidents[inc.ID] = inc
	f.order = append(f.order, inc.ID)
	return inc, nil
}

func (f *fakeIncidentStore) GetIncident(_ context.Context, id string) (store.Incident, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return store.Incident{}, f.getErr
	}
	inc, ok := f.incidents[id]
	if !ok {
		return store.Incident{}, store.ErrNotFound
	}
	return inc, nil
}

func (f *fakeIncidentStore) ListIncidents(_ context.Context, filter store.IncidentFilter) (store.IncidentPage, error) { //nolint:gocritic // hugeParam: test double mirrors the store signature
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastFilter = filter
	if f.listErr != nil {
		return store.IncidentPage{}, f.listErr
	}
	out := make([]store.Incident, 0, len(f.order))
	for _, id := range f.order {
		inc, ok := f.incidents[id]
		if !ok {
			continue
		}
		if filter.Status != "" && inc.Status != filter.Status {
			continue
		}
		if filter.Scope != nil && inc.Scope != *filter.Scope {
			continue
		}
		out = append(out, inc)
	}
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return store.IncidentPage{Incidents: out}, nil
}

func (f *fakeIncidentStore) UpdateIncidentStatus(_ context.Context, id, status string, resolvedAt *time.Time) (store.Incident, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statusErr != nil {
		return store.Incident{}, f.statusErr
	}
	inc, ok := f.incidents[id]
	if !ok {
		return store.Incident{}, store.ErrNotFound
	}
	inc.Status, inc.ResolvedAt = status, resolvedAt
	f.incidents[id] = inc
	return inc, nil
}

func (f *fakeIncidentStore) UpdateIncidentNotes(_ context.Context, id, notes string) (store.Incident, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.notesErr != nil {
		return store.Incident{}, f.notesErr
	}
	inc, ok := f.incidents[id]
	if !ok {
		return store.Incident{}, store.ErrNotFound
	}
	inc.Notes = notes
	f.incidents[id] = inc
	return inc, nil
}

func (f *fakeIncidentStore) UpdateIncidentPinned(_ context.Context, id string, pinned json.RawMessage) (store.Incident, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pinnedErr != nil {
		return store.Incident{}, f.pinnedErr
	}
	inc, ok := f.incidents[id]
	if !ok {
		return store.Incident{}, store.ErrNotFound
	}
	if len(pinned) == 0 {
		pinned = json.RawMessage(`[]`)
	}
	inc.Pinned = pinned
	f.incidents[id] = inc
	return inc, nil
}

func (f *fakeIncidentStore) DeleteIncident(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if _, ok := f.incidents[id]; !ok {
		return store.ErrNotFound
	}
	delete(f.incidents, id)
	return nil
}

func (f *fakeIncidentStore) seed(title, scope string, from time.Time) string {
	inc, err := f.CreateIncident(context.Background(), store.IncidentInput{
		Title: title, Scope: scope, FromAt: from, CreatedBy: "user:seed",
	})
	if err != nil {
		panic(err)
	}
	return inc.ID
}

// get reads one row back out for an assertion, without going through HTTP.
func (f *fakeIncidentStore) get(t *testing.T, id string) store.Incident {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	inc, ok := f.incidents[id]
	if !ok {
		t.Fatalf("incident %q is not in the fake store", id)
	}
	return inc
}

const validIncidentBody = `{"title":"pair loss spike","scope":"node-a","fromAt":"2026-08-07T10:00:00Z"}`

func incidentRoutes(id string) []struct{ method, path, body string } {
	return []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/incidents", ""},
		{http.MethodPost, "/api/v1/incidents", validIncidentBody},
		{http.MethodGet, "/api/v1/incidents/" + id, ""},
		{http.MethodPatch, "/api/v1/incidents/" + id, `{"status":"resolved"}`},
		{http.MethodDelete, "/api/v1/incidents/" + id, ""},
	}
}

func TestIncidentsWithoutStoreReturn503(t *testing.T) {
	s := newM5TestServer(t, "operator", Deps{})
	for _, c := range incidentRoutes(uuid.NewString()) {
		var mutate func(*http.Request)
		if isMutatingMethod(c.method) {
			mutate = mutateWithCSRF
		}
		w := doRequest(t, s, c.method, c.path, strings.NewReader(c.body), mutate)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s without an IncidentService = %d, want 503: %s", c.method, c.path, w.Code, w.Body)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
			t.Errorf("%s %s Content-Type = %q, want application/problem+json", c.method, c.path, ct)
		}
		if !strings.Contains(w.Body.String(), "console.database.mode") {
			t.Errorf("%s %s 503 detail = %s, want it to name console.database.mode", c.method, c.path, w.Body)
		}
	}
}

func TestIncidentsViewerReadsButCannotWrite(t *testing.T) {
	for _, role := range []string{"viewer", "alert-editor"} {
		st := newFakeIncidentStore()
		id := st.seed("seeded", "", time.Now().UTC())
		s := newM5TestServer(t, role, Deps{Incidents: st})

		for _, path := range []string{"/api/v1/incidents", "/api/v1/incidents/" + id} {
			w := doRequest(t, s, http.MethodGet, path, nil, nil)
			if w.Code != http.StatusOK {
				t.Errorf("%s: GET %s = %d, want 200: %s", role, path, w.Code, w.Body)
			}
		}

		writes := []struct{ method, path, body string }{
			{http.MethodPost, "/api/v1/incidents", validIncidentBody},
			{http.MethodPatch, "/api/v1/incidents/" + id, `{"status":"resolved"}`},
			{http.MethodDelete, "/api/v1/incidents/" + id, ""},
		}
		for _, c := range writes {
			w := doRequest(t, s, c.method, c.path, strings.NewReader(c.body), mutateWithCSRF)
			if w.Code != http.StatusForbidden {
				t.Errorf("%s: %s %s = %d, want 403: %s", role, c.method, c.path, w.Code, w.Body)
			}
		}
	}
}

func TestIncidentsRequireIncidentsRead(t *testing.T) {
	st := newFakeIncidentStore()
	s := newNoTelemetryServer(t, Deps{Incidents: st})
	w := doRequest(t, s, http.MethodGet, "/api/v1/incidents", nil, nil)
	if w.Code != http.StatusForbidden {
		t.Errorf("GET /api/v1/incidents without incidents:read = %d, want 403: %s", w.Code, w.Body)
	}
}

func TestIncidentsOperatorAndAdminWriteTheFullCycle(t *testing.T) {
	for _, role := range []string{"operator", "admin"} {
		st := newFakeIncidentStore()
		s := newM5TestServer(t, role, Deps{Incidents: st})

		w := doRequest(t, s, http.MethodPost, "/api/v1/incidents", strings.NewReader(validIncidentBody), mutateWithCSRF)
		if w.Code != http.StatusCreated {
			t.Fatalf("%s: POST = %d, want 201: %s", role, w.Code, w.Body)
		}
		var got incidentResponse
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.ID == "" || got.Title != "pair loss spike" || got.Scope != "node-a" {
			t.Errorf("%s: body = %+v, want the created incident echoed back", role, got)
		}
		if got.Status != store.IncidentStatusOpen {
			t.Errorf("%s: status = %q, want a new incident to be open", role, got.Status)
		}
		if got.ResolvedAt != nil {
			t.Errorf("%s: resolvedAt = %v, want nil for a new incident", role, got.ResolvedAt)
		}
		if string(got.Pinned) != "[]" {
			t.Errorf("%s: pinned = %s, want an empty array, never null", role, got.Pinned)
		}
		if want := "/api/v1/incidents/" + got.ID; w.Header().Get("Location") != want {
			t.Errorf("%s: Location = %q, want %q", role, w.Header().Get("Location"), want)
		}

		w = doRequest(t, s, http.MethodGet, "/api/v1/incidents/"+got.ID, nil, nil)
		if w.Code != http.StatusOK {
			t.Errorf("%s: GET one = %d, want 200: %s", role, w.Code, w.Body)
		}
		w = doRequest(t, s, http.MethodDelete, "/api/v1/incidents/"+got.ID, nil, mutateWithCSRF)
		if w.Code != http.StatusNoContent {
			t.Errorf("%s: DELETE = %d, want 204: %s", role, w.Code, w.Body)
		}
	}
}

// created_by is the SERVER's view of who opened the incident, never a body
// field -- annotationAuthor's rule, applied to the same kind of attribution.
func TestIncidentsCreateRecordsTheSubjectAndIgnoresClientState(t *testing.T) {
	st := newFakeIncidentStore()
	s := newM5TestServer(t, "operator", Deps{Incidents: st})
	body := `{"title":"t","fromAt":"2026-08-07T10:00:00Z","createdBy":"somebody-else",` +
		`"status":"resolved","resolvedAt":"2026-08-07T11:00:00Z"}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/incidents", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d, want 201: %s", w.Code, w.Body)
	}
	var got incidentResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.CreatedBy != "user:u1" {
		t.Errorf("createdBy = %q, want the authenticated subject, never the body's value", got.CreatedBy)
	}
	if got.Status != store.IncidentStatusOpen || got.ResolvedAt != nil {
		t.Errorf("status/resolvedAt = %q/%v, want an incident to always be CREATED open", got.Status, got.ResolvedAt)
	}
}

// ?scope= carries the annotations three-state semantics -- absent = every
// scope, present-but-empty = the GLOBAL ones, anything else exact.
func TestIncidentsListScopePointerSemantics(t *testing.T) {
	cases := []struct {
		path string
		want *string
	}{
		{"/api/v1/incidents", nil},
		{"/api/v1/incidents?scope=", ptrTo("")},
		{"/api/v1/incidents?scope=node-a", ptrTo("node-a")},
	}
	for _, c := range cases {
		st := newFakeIncidentStore()
		s := newM5TestServer(t, "viewer", Deps{Incidents: st})
		w := doRequest(t, s, http.MethodGet, c.path, nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200: %s", c.path, w.Code, w.Body)
		}
		st.mu.Lock()
		got := st.lastFilter.Scope
		st.mu.Unlock()
		switch {
		case c.want == nil && got != nil:
			t.Errorf("GET %s reached the store with scope %q, want nil (every scope)", c.path, *got)
		case c.want != nil && got == nil:
			t.Errorf("GET %s reached the store with a nil scope, want %q", c.path, *c.want)
		case c.want != nil && got != nil && *got != *c.want:
			t.Errorf("GET %s reached the store with scope %q, want %q", c.path, *got, *c.want)
		}
	}
}

func TestIncidentsListFiltersAndBadInputs(t *testing.T) {
	st := newFakeIncidentStore()
	st.seed("open one", "node-a", time.Now().UTC())
	resolved := st.seed("resolved one", "node-b", time.Now().UTC())
	if _, err := st.UpdateIncidentStatus(context.Background(), resolved,
		store.IncidentStatusResolved, ptrTo(time.Now().UTC())); err != nil {
		t.Fatalf("seed resolve: %v", err)
	}
	s := newM5TestServer(t, "viewer", Deps{Incidents: st})

	w := doRequest(t, s, http.MethodGet, "/api/v1/incidents?status=open", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", w.Code, w.Body)
	}
	var page incidentsListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Incidents) != 1 || page.Incidents[0].Title != "open one" {
		t.Fatalf("incidents = %+v, want exactly the open row", page.Incidents)
	}

	from, to := "2026-08-07T00:00:00Z", "2026-08-08T00:00:00Z"
	w = doRequest(t, s, http.MethodGet, "/api/v1/incidents?from="+from+"&to="+to, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("window status %d, want 200: %s", w.Code, w.Body)
	}
	st.mu.Lock()
	gotFrom, gotTo := st.lastFilter.From, st.lastFilter.To
	st.mu.Unlock()
	if gotFrom.Format(time.RFC3339) != from || gotTo.Format(time.RFC3339) != to {
		t.Errorf("window reached the store as [%s, %s), want [%s, %s)", gotFrom, gotTo, from, to)
	}

	for _, path := range []string{
		"/api/v1/incidents?from=yesterday",
		"/api/v1/incidents?to=tomorrow",
		"/api/v1/incidents?cursor=not-a-cursor",
		"/api/v1/incidents?status=on-fire",
	} {
		w = doRequest(t, s, http.MethodGet, path, nil, nil)
		if w.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400: %s", path, w.Code, w.Body)
		}
	}
}

func TestIncidentsCreateValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"unparseable body", `not json`, http.StatusBadRequest},
		{"missing title", `{"fromAt":"2026-08-07T10:00:00Z"}`, http.StatusUnprocessableEntity},
		{"missing fromAt", `{"title":"t"}`, http.StatusUnprocessableEntity},
		{"bad fromAt", `{"title":"t","fromAt":"yesterday"}`, http.StatusBadRequest},
		{
			"title over 255 bytes",
			fmt.Sprintf(`{"title":%q,"fromAt":"2026-08-07T10:00:00Z"}`, strings.Repeat("x", 256)),
			http.StatusUnprocessableEntity,
		},
		{
			"toAt before fromAt",
			`{"title":"t","fromAt":"2026-08-07T10:00:00Z","toAt":"2026-08-07T09:00:00Z"}`,
			http.StatusUnprocessableEntity,
		},
		{
			"notes over 16384 bytes",
			fmt.Sprintf(`{"title":"t","fromAt":"2026-08-07T10:00:00Z","notes":%q}`, strings.Repeat("n", 16385)),
			http.StatusUnprocessableEntity,
		},
		{
			"pinned with an unknown kind",
			`{"title":"t","fromAt":"2026-08-07T10:00:00Z","pinned":[{"kind":"wat","id":"1"}]}`,
			http.StatusUnprocessableEntity,
		},
		{
			"pinned is an object, not an array",
			`{"title":"t","fromAt":"2026-08-07T10:00:00Z","pinned":{"kind":"event","id":"1"}}`,
			http.StatusUnprocessableEntity,
		},
	}
	for _, c := range cases {
		st := newFakeIncidentStore()
		s := newM5TestServer(t, "operator", Deps{Incidents: st})
		w := doRequest(t, s, http.MethodPost, "/api/v1/incidents", strings.NewReader(c.body), mutateWithCSRF)
		if w.Code != c.want {
			t.Errorf("%s: POST = %d, want %d: %s", c.name, w.Code, c.want, w.Body)
		}
		if w.Code >= http.StatusBadRequest && !strings.Contains(w.Body.String(), "incident") {
			t.Errorf("%s: detail = %s, want it to name the resource", c.name, w.Body)
		}
	}
}

// The PATCH exception in test form: a subset body updates exactly the fields
// it names and leaves the others alone.
func TestIncidentsPatchAppliesOnlyTheNamedFields(t *testing.T) {
	st := newFakeIncidentStore()
	id := st.seed("t", "node-a", time.Now().UTC())
	if _, err := st.UpdateIncidentNotes(context.Background(), id, "original notes"); err != nil {
		t.Fatalf("seed notes: %v", err)
	}
	s := newM5TestServer(t, "operator", Deps{Incidents: st})

	w := doRequest(t, s, http.MethodPatch, "/api/v1/incidents/"+id,
		strings.NewReader(`{"pinned":[{"kind":"event","id":"42","note":"first drop"}]}`), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH pinned = %d, want 200: %s", w.Code, w.Body)
	}
	var got incidentResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Notes != "original notes" {
		t.Errorf("notes = %q, want the untouched original -- PATCH is a subset, not a replace", got.Notes)
	}
	if !strings.Contains(string(got.Pinned), `"id":"42"`) {
		t.Errorf("pinned = %s, want the patched ref", got.Pinned)
	}
	if got.Status != store.IncidentStatusOpen {
		t.Errorf("status = %q, want it untouched", got.Status)
	}

	w = doRequest(t, s, http.MethodPatch, "/api/v1/incidents/"+id,
		strings.NewReader(`{"notes":"rewritten"}`), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH notes = %d, want 200: %s", w.Code, w.Body)
	}
	stored := st.get(t, id)
	if stored.Notes != "rewritten" {
		t.Errorf("stored notes = %q, want the patched value", stored.Notes)
	}
	if !strings.Contains(string(stored.Pinned), `"id":"42"`) {
		t.Errorf("stored pinned = %s, want the earlier patch preserved", stored.Pinned)
	}
}

// The status ladder: resolving STAMPS resolvedAt, reopening CLEARS it.
func TestIncidentsPatchResolveStampsAndReopenClears(t *testing.T) {
	st := newFakeIncidentStore()
	id := st.seed("t", "", time.Now().UTC())
	s := newM5TestServer(t, "operator", Deps{Incidents: st})

	before := time.Now().UTC().Add(-time.Second)
	w := doRequest(t, s, http.MethodPatch, "/api/v1/incidents/"+id,
		strings.NewReader(`{"status":"resolved"}`), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("resolve = %d, want 200: %s", w.Code, w.Body)
	}
	var got incidentResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Status != store.IncidentStatusResolved {
		t.Fatalf("status = %q, want resolved", got.Status)
	}
	if got.ResolvedAt == nil {
		t.Fatalf("resolvedAt = nil, want the server's own resolution time")
	}
	if got.ResolvedAt.Before(before) || got.ResolvedAt.After(time.Now().UTC().Add(time.Second)) {
		t.Errorf("resolvedAt = %v, want a stamp taken now", got.ResolvedAt)
	}

	w = doRequest(t, s, http.MethodPatch, "/api/v1/incidents/"+id,
		strings.NewReader(`{"status":"open"}`), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("reopen = %d, want 200: %s", w.Code, w.Body)
	}
	// A FRESH value, never the one decoded above: encoding/json leaves a field
	// the payload omits at whatever the destination already held, so reusing
	// `got` would carry the resolved stamp forward and pass regardless.
	var reopened incidentResponse
	if err := json.Unmarshal(w.Body.Bytes(), &reopened); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if reopened.Status != store.IncidentStatusOpen {
		t.Errorf("status = %q, want open", reopened.Status)
	}
	if reopened.ResolvedAt != nil {
		t.Errorf("resolvedAt = %v, want it CLEARED on reopen -- a reopened incident is not resolved", reopened.ResolvedAt)
	}
	if stored := st.get(t, id); stored.ResolvedAt != nil {
		t.Errorf("stored resolvedAt = %v, want nil", stored.ResolvedAt)
	}
}

// Re-resolving an already-resolved incident must not move its resolution time:
// the stamp records WHEN it was resolved, and a second PATCH did not re-resolve
// anything.
func TestIncidentsPatchStatusIsIdempotent(t *testing.T) {
	st := newFakeIncidentStore()
	id := st.seed("t", "", time.Now().UTC())
	s := newM5TestServer(t, "operator", Deps{Incidents: st})

	w := doRequest(t, s, http.MethodPatch, "/api/v1/incidents/"+id,
		strings.NewReader(`{"status":"resolved"}`), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("resolve = %d: %s", w.Code, w.Body)
	}
	first := st.get(t, id).ResolvedAt
	if first == nil {
		t.Fatal("resolvedAt = nil after resolving")
	}

	time.Sleep(2 * time.Millisecond)
	w = doRequest(t, s, http.MethodPatch, "/api/v1/incidents/"+id,
		strings.NewReader(`{"status":"resolved"}`), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("re-resolve = %d: %s", w.Code, w.Body)
	}
	if second := st.get(t, id).ResolvedAt; !second.Equal(*first) {
		t.Errorf("resolvedAt moved from %v to %v -- re-resolving must not re-stamp", first, second)
	}
}

func TestIncidentsPatchValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"unparseable body", `not json`, http.StatusBadRequest},
		{"invalid status", `{"status":"on-fire"}`, http.StatusUnprocessableEntity},
		{"empty status", `{"status":""}`, http.StatusUnprocessableEntity},
		{"no fields at all", `{}`, http.StatusUnprocessableEntity},
		{"unknown fields only", `{"title":"renamed"}`, http.StatusUnprocessableEntity},
		{
			"notes over 16384 bytes",
			fmt.Sprintf(`{"notes":%q}`, strings.Repeat("n", 16385)),
			http.StatusUnprocessableEntity,
		},
		{"pinned with an unknown kind", `{"pinned":[{"kind":"wat","id":"1"}]}`, http.StatusUnprocessableEntity},
		{"pinned is an object", `{"pinned":{"kind":"event","id":"1"}}`, http.StatusUnprocessableEntity},
	}
	for _, c := range cases {
		st := newFakeIncidentStore()
		id := st.seed("t", "", time.Now().UTC())
		s := newM5TestServer(t, "operator", Deps{Incidents: st})
		w := doRequest(t, s, http.MethodPatch, "/api/v1/incidents/"+id, strings.NewReader(c.body), mutateWithCSRF)
		if w.Code != c.want {
			t.Errorf("%s: PATCH = %d, want %d: %s", c.name, w.Code, c.want, w.Body)
		}
	}
}

// A rejected PATCH must not have half-applied: validation runs on the WHOLE
// body before the first store call.
func TestIncidentsPatchValidatesBeforeApplyingAnything(t *testing.T) {
	st := newFakeIncidentStore()
	id := st.seed("t", "", time.Now().UTC())
	s := newM5TestServer(t, "operator", Deps{Incidents: st})

	w := doRequest(t, s, http.MethodPatch, "/api/v1/incidents/"+id,
		strings.NewReader(`{"notes":"applied?","status":"on-fire"}`), mutateWithCSRF)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PATCH = %d, want 422: %s", w.Code, w.Body)
	}
	if stored := st.get(t, id); stored.Notes != "" {
		t.Errorf("notes = %q, want the rejected body to have changed NOTHING", stored.Notes)
	}
}

func TestIncidentsUnknownAndMalformedIDsAreBoth404(t *testing.T) {
	st := newFakeIncidentStore()
	s := newM5TestServer(t, "operator", Deps{Incidents: st})
	for _, id := range []string{uuid.NewString(), "not-a-uuid"} {
		for _, c := range []struct{ method, body string }{
			{http.MethodGet, ""},
			{http.MethodPatch, `{"status":"resolved"}`},
			{http.MethodDelete, ""},
		} {
			var mutate func(*http.Request)
			if isMutatingMethod(c.method) {
				mutate = mutateWithCSRF
			}
			w := doRequest(t, s, c.method, "/api/v1/incidents/"+id, strings.NewReader(c.body), mutate)
			if w.Code != http.StatusNotFound {
				t.Errorf("%s /api/v1/incidents/%s = %d, want 404: %s", c.method, id, w.Code, w.Body)
			}
		}
	}
}

func TestIncidentsStoreFailureReturns502AndNeverEchoesTheDriverError(t *testing.T) {
	st := newFakeIncidentStore()
	st.createErr = errors.New("pool exhausted")
	s := newM5TestServer(t, "operator", Deps{Incidents: st})
	w := doRequest(t, s, http.MethodPost, "/api/v1/incidents", strings.NewReader(validIncidentBody), mutateWithCSRF)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "pool exhausted") {
		t.Errorf("body = %s, must never echo the driver error", w.Body)
	}
}

// The audit row for a created incident carries title/scope/status and NOTHING
// else: notes are free-form operator prose, and pinned is an open list of
// references -- neither belongs in a log read by more people than the incident.
func TestIncidentsCreateAuditDetailIsTitleScopeStatusOnly(t *testing.T) {
	fs := &fakeAuditStore{}
	st := newFakeIncidentStore()
	s := newM5TestServer(t, "operator", Deps{Incidents: st, Audit: fs})

	body := `{"title":"pair loss","scope":"node-a","fromAt":"2026-08-07T10:00:00Z",` +
		`"notes":"root password is hunter2","pinned":[{"kind":"event","id":"7","note":"leakme"}]}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/incidents", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST status %d: %s", w.Code, w.Body)
	}

	entries := waitForOneAuditEntry(t, fs)
	detail := string(entries[0].Detail)
	for _, banned := range []string{"notes", "hunter2", "pinned", "leakme", "fromAt"} {
		if strings.Contains(detail, banned) {
			t.Errorf("audit detail = %s, must never carry %q", detail, banned)
		}
	}
	if !strings.Contains(detail, `"title":"pair loss"`) || !strings.Contains(detail, `"scope":"node-a"`) {
		t.Errorf("audit detail = %s, want the allow-listed title and scope", detail)
	}
	if entries[0].Action != "POST /api/v1/incidents" {
		t.Errorf("audit action = %q, want the route pattern", entries[0].Action)
	}
}

// The PATCH audit row carries the STATUS transition and nothing else -- that is
// the one bit of an incident's evolution an auditor is read to answer for.
func TestIncidentsPatchAuditDetailIsStatusOnly(t *testing.T) {
	fs := &fakeAuditStore{}
	st := newFakeIncidentStore()
	id := st.seed("t", "", time.Now().UTC())
	s := newM5TestServer(t, "operator", Deps{Incidents: st, Audit: fs})

	body := `{"status":"resolved","notes":"the fix was to rotate hunter2","pinned":[{"kind":"run","id":"9"}]}`
	w := doRequest(t, s, http.MethodPatch, "/api/v1/incidents/"+id, strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH status %d: %s", w.Code, w.Body)
	}

	entries := waitForOneAuditEntry(t, fs)
	detail := string(entries[0].Detail)
	for _, banned := range []string{"notes", "hunter2", "pinned"} {
		if strings.Contains(detail, banned) {
			t.Errorf("audit detail = %s, must never carry %q", detail, banned)
		}
	}
	if !strings.Contains(detail, `"status":"resolved"`) {
		t.Errorf("audit detail = %s, want the allow-listed status", detail)
	}
	if entries[0].Resource != id {
		t.Errorf("audit resource = %q, want the incident id %q", entries[0].Resource, id)
	}
	if entries[0].Action != "PATCH /api/v1/incidents/{id}" {
		t.Errorf("audit action = %q, want the route pattern", entries[0].Action)
	}
}

// fakeIncidentNotifier is the IncidentNotifier double: it records the
// (event, incident) pairs the handlers hand over, which is the whole of the
// seam the dispatcher implements.
type fakeIncidentNotifier struct {
	mu     sync.Mutex
	events []notifiedIncident
}

type notifiedIncident struct {
	event  string
	id     string
	status string
}

func (f *fakeIncidentNotifier) Notify(_ context.Context, event string, inc store.Incident) { //nolint:gocritic // hugeParam: test double mirrors the seam
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, notifiedIncident{event: event, id: inc.ID, status: inc.Status})
}

func (f *fakeIncidentNotifier) notified() []notifiedIncident {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]notifiedIncident(nil), f.events...)
}

// The three lifecycle events, in the order an incident actually goes through them.
func TestIncidentLifecycleNotifiesCreatedResolvedAndReopened(t *testing.T) {
	st := newFakeIncidentStore()
	notifier := &fakeIncidentNotifier{}
	s := newM5TestServer(t, "operator", Deps{Incidents: st, IncidentNotifier: notifier})

	body := `{"title":"loss spike","scope":"node-a","fromAt":"2026-08-08T12:00:00Z"}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/incidents", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST = %d: %s", w.Code, w.Body)
	}
	var created incidentResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	for _, status := range []string{store.IncidentStatusResolved, store.IncidentStatusOpen} {
		w = doRequest(t, s, http.MethodPatch, "/api/v1/incidents/"+created.ID,
			strings.NewReader(`{"status":"`+status+`"}`), mutateWithCSRF)
		if w.Code != http.StatusOK {
			t.Fatalf("PATCH %s = %d: %s", status, w.Code, w.Body)
		}
	}

	got := notifier.notified()
	want := []string{
		store.WebhookEventIncidentCreated,
		store.WebhookEventIncidentResolved,
		store.WebhookEventIncidentReopened,
	}
	if len(got) != len(want) {
		t.Fatalf("notifier saw %+v, want exactly %v", got, want)
	}
	for i, event := range want {
		if got[i].event != event {
			t.Errorf("notification %d = %q, want %q", i, got[i].event, event)
		}
		if got[i].id != created.ID {
			t.Errorf("notification %d carried id %q, want %q", i, got[i].id, created.ID)
		}
	}
	// The incident handed over is the row as it now stands, not as it stood
	// before the patch that caused the notification.
	if got[1].status != store.IncidentStatusResolved || got[2].status != store.IncidentStatusOpen {
		t.Errorf("notified statuses = %q/%q, want resolved/open", got[1].status, got[2].status)
	}
}

// A PATCH that does not CHANGE the status announces nothing: re-sending
// "resolved" did not re-resolve anything, and a webhook that fired again would
// tell a receiver an incident was resolved twice.
func TestIncidentNotifierIsSilentWithoutARealTransition(t *testing.T) {
	st := newFakeIncidentStore()
	id := st.seed("t", "", time.Now().UTC())
	notifier := &fakeIncidentNotifier{}
	s := newM5TestServer(t, "operator", Deps{Incidents: st, IncidentNotifier: notifier})

	for _, body := range []string{
		`{"notes":"just typing"}`,
		`{"pinned":[{"kind":"run","id":"9"}]}`,
		`{"status":"open"}`, // already open
	} {
		w := doRequest(t, s, http.MethodPatch, "/api/v1/incidents/"+id, strings.NewReader(body), mutateWithCSRF)
		if w.Code != http.StatusOK {
			t.Fatalf("PATCH %s = %d: %s", body, w.Code, w.Body)
		}
	}
	if got := notifier.notified(); len(got) != 0 {
		t.Errorf("notifier saw %+v, want nothing -- none of those changed the status", got)
	}
}

// Nothing is announced that was not recorded: a failed store write must not
// produce a webhook claiming an incident exists.
func TestIncidentNotifierNeverFiresOnAFailedWrite(t *testing.T) {
	st := newFakeIncidentStore()
	st.createErr = errors.New("connection refused")
	notifier := &fakeIncidentNotifier{}
	s := newM5TestServer(t, "operator", Deps{Incidents: st, IncidentNotifier: notifier})

	body := `{"title":"loss spike","fromAt":"2026-08-08T12:00:00Z"}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/incidents", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("POST = %d, want 502: %s", w.Code, w.Body)
	}
	if got := notifier.notified(); len(got) != 0 {
		t.Errorf("notifier saw %+v after a failed create, want nothing", got)
	}
}

// The whole seam is optional. Without a notifier -- no encryption key, or no
// database -- every incident route behaves exactly as it did before it
// existed, and nothing nil-panics.
func TestIncidentRoutesWorkWithoutANotifier(t *testing.T) {
	st := newFakeIncidentStore()
	id := st.seed("t", "", time.Now().UTC())
	s := newM5TestServer(t, "operator", Deps{Incidents: st})

	body := `{"title":"loss spike","fromAt":"2026-08-08T12:00:00Z"}`
	if w := doRequest(t, s, http.MethodPost, "/api/v1/incidents", strings.NewReader(body), mutateWithCSRF); w.Code != http.StatusCreated {
		t.Errorf("POST without a notifier = %d, want 201: %s", w.Code, w.Body)
	}
	if w := doRequest(t, s, http.MethodPatch, "/api/v1/incidents/"+id,
		strings.NewReader(`{"status":"resolved"}`), mutateWithCSRF); w.Code != http.StatusOK {
		t.Errorf("PATCH without a notifier = %d, want 200: %s", w.Code, w.Body)
	}
}

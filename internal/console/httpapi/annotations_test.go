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

// fakeAnnotationStore is one double for AnnotationService (read seam + write
// seam), mutex-guarded like fakeChecksStore. Validation goes through the real
// store.AnnotationInput.Validate so the 422 tests exercise the SAME rules the
// database layer enforces, not a second copy of them.
type fakeAnnotationStore struct {
	mu sync.Mutex

	anns  map[string]store.Annotation
	order []string // creation order

	lastFilter store.AnnotationFilter

	createErr error
	listErr   error
	deleteErr error
}

func newFakeAnnotationStore() *fakeAnnotationStore {
	return &fakeAnnotationStore{anns: map[string]store.Annotation{}}
}

func (f *fakeAnnotationStore) CreateAnnotation(_ context.Context, in store.AnnotationInput) (store.Annotation, error) { //nolint:gocritic // hugeParam: test double mirrors the store signature
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return store.Annotation{}, f.createErr
	}
	if err := in.Validate(); err != nil {
		return store.Annotation{}, err
	}
	a := store.Annotation{
		ID: uuid.NewString(), StartAt: in.StartAt, EndAt: in.EndAt, Scope: in.Scope,
		Text: in.Text, CreatedBy: in.CreatedBy, CreatedAt: time.Now().UTC(),
	}
	f.anns[a.ID] = a
	f.order = append(f.order, a.ID)
	return a, nil
}

func (f *fakeAnnotationStore) DeleteAnnotation(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if _, ok := f.anns[id]; !ok {
		return store.ErrNotFound
	}
	delete(f.anns, id)
	return nil
}

func (f *fakeAnnotationStore) GetAnnotation(_ context.Context, id string) (store.Annotation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.anns[id]
	if !ok {
		return store.Annotation{}, store.ErrNotFound
	}
	return a, nil
}

func (f *fakeAnnotationStore) ListAnnotations(_ context.Context, filter store.AnnotationFilter) (store.AnnotationPage, error) { //nolint:gocritic // hugeParam: test double mirrors the store signature
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastFilter = filter
	if f.listErr != nil {
		return store.AnnotationPage{}, f.listErr
	}
	out := make([]store.Annotation, 0, len(f.order))
	for _, id := range f.order {
		a, ok := f.anns[id]
		if !ok {
			continue
		}
		if filter.Scope != nil && a.Scope != *filter.Scope {
			continue
		}
		out = append(out, a)
	}
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return store.AnnotationPage{Annotations: out}, nil
}

func (f *fakeAnnotationStore) seed(scope, text string, at time.Time) string {
	a, err := f.CreateAnnotation(context.Background(), store.AnnotationInput{
		StartAt: at, Scope: scope, Text: text, CreatedBy: "user:seed",
	})
	if err != nil {
		panic(err)
	}
	return a.ID
}

func annotationRoutes(id string) []struct {
	method, path string
	body         string
} {
	return []struct {
		method, path string
		body         string
	}{
		{http.MethodGet, "/api/v1/annotations", ""},
		{http.MethodPost, "/api/v1/annotations", validAnnotationBody},
		{http.MethodDelete, "/api/v1/annotations/" + id, ""},
	}
}

const validAnnotationBody = `{"startAt":"2026-08-07T10:00:00Z","scope":"node-a","text":"rolled the switch"}`

func TestAnnotationsWithoutStoreReturn503(t *testing.T) {
	s := newM5TestServer(t, "operator", Deps{})
	for _, c := range annotationRoutes(uuid.NewString()) {
		var mutate func(*http.Request)
		if isMutatingMethod(c.method) {
			mutate = mutateWithCSRF
		}
		w := doRequest(t, s, c.method, c.path, strings.NewReader(c.body), mutate)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s without an AnnotationService = %d, want 503: %s", c.method, c.path, w.Code, w.Body)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
			t.Errorf("%s %s Content-Type = %q, want application/problem+json", c.method, c.path, ct)
		}
		if !strings.Contains(w.Body.String(), "console.database.mode") {
			t.Errorf("%s %s 503 detail = %s, want it to name console.database.mode", c.method, c.path, w.Body)
		}
	}
}

// M5 Decision 11 in test form: viewer READS annotations (chart markers are
// telemetry) and CANNOT write them (a note pinned to the fleet's history is
// an operator statement).
func TestAnnotationsViewerReadsButCannotWrite(t *testing.T) {
	for _, role := range []string{"viewer", "alert-editor"} {
		st := newFakeAnnotationStore()
		id := st.seed("", "seeded", time.Now().UTC())
		s := newM5TestServer(t, role, Deps{Annotations: st})

		w := doRequest(t, s, http.MethodGet, "/api/v1/annotations", nil, nil)
		if w.Code != http.StatusOK {
			t.Errorf("%s: GET /api/v1/annotations = %d, want 200: %s", role, w.Code, w.Body)
		}
		w = doRequest(t, s, http.MethodPost, "/api/v1/annotations", strings.NewReader(validAnnotationBody), mutateWithCSRF)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s: POST /api/v1/annotations = %d, want 403: %s", role, w.Code, w.Body)
		}
		w = doRequest(t, s, http.MethodDelete, "/api/v1/annotations/"+id, nil, mutateWithCSRF)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s: DELETE /api/v1/annotations/{id} = %d, want 403: %s", role, w.Code, w.Body)
		}
	}
}

func TestAnnotationsRequireAnnotationsRead(t *testing.T) {
	st := newFakeAnnotationStore()
	s := newNoTelemetryServer(t, Deps{Annotations: st})
	w := doRequest(t, s, http.MethodGet, "/api/v1/annotations", nil, nil)
	if w.Code != http.StatusForbidden {
		t.Errorf("GET /api/v1/annotations without annotations:read = %d, want 403: %s", w.Code, w.Body)
	}
}

func TestAnnotationsOperatorCreatesAndDeletes(t *testing.T) {
	for _, role := range []string{"operator", "admin"} {
		st := newFakeAnnotationStore()
		s := newM5TestServer(t, role, Deps{Annotations: st})

		w := doRequest(t, s, http.MethodPost, "/api/v1/annotations", strings.NewReader(validAnnotationBody), mutateWithCSRF)
		if w.Code != http.StatusCreated {
			t.Fatalf("%s: POST = %d, want 201: %s", role, w.Code, w.Body)
		}
		var got annotationResponse
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.ID == "" || got.Scope != "node-a" || got.Text != "rolled the switch" {
			t.Errorf("%s: body = %+v, want the created annotation echoed back", role, got)
		}
		if got.EndAt != nil {
			t.Errorf("%s: endAt = %v, want nil for an instant mark", role, got.EndAt)
		}
		if want := "/api/v1/annotations/" + got.ID; w.Header().Get("Location") != want {
			t.Errorf("%s: Location = %q, want %q", role, w.Header().Get("Location"), want)
		}

		w = doRequest(t, s, http.MethodDelete, "/api/v1/annotations/"+got.ID, nil, mutateWithCSRF)
		if w.Code != http.StatusNoContent {
			t.Errorf("%s: DELETE = %d, want 204: %s", role, w.Code, w.Body)
		}
	}
}

// created_by is the SERVER's view of who wrote the note, never a body field:
// a client cannot forge attribution.
func TestAnnotationsCreateRecordsTheSubject(t *testing.T) {
	st := newFakeAnnotationStore()
	s := newM5TestServer(t, "operator", Deps{Annotations: st})
	body := `{"startAt":"2026-08-07T10:00:00Z","text":"note","createdBy":"somebody-else"}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/annotations", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d, want 201: %s", w.Code, w.Body)
	}
	var got annotationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.CreatedBy != "user:u1" {
		t.Errorf("createdBy = %q, want the authenticated subject (user:u1), never the body's value", got.CreatedBy)
	}
}

// scope is a POINTER filter because "" is a REAL value here -- the global
// scope. Absent means every scope; present-but-empty means the global ones.
func TestAnnotationsListScopePointerSemantics(t *testing.T) {
	cases := []struct {
		path string
		want *string
	}{
		{"/api/v1/annotations", nil},
		{"/api/v1/annotations?scope=", ptrTo("")},
		{"/api/v1/annotations?scope=node-a", ptrTo("node-a")},
	}
	for _, c := range cases {
		st := newFakeAnnotationStore()
		s := newM5TestServer(t, "viewer", Deps{Annotations: st})
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

func ptrTo[T any](v T) *T { return &v }

func TestAnnotationsListFiltersAndReturnsRows(t *testing.T) {
	st := newFakeAnnotationStore()
	st.seed("", "global note", time.Now().UTC())
	st.seed("node-a", "node note", time.Now().UTC())
	s := newM5TestServer(t, "viewer", Deps{Annotations: st})

	w := doRequest(t, s, http.MethodGet, "/api/v1/annotations?scope=node-a", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", w.Code, w.Body)
	}
	var got annotationsListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Annotations) != 1 || got.Annotations[0].Text != "node note" {
		t.Fatalf("annotations = %+v, want exactly the node-a row", got.Annotations)
	}
}

func TestAnnotationsListTimeWindowAndBadInputs(t *testing.T) {
	st := newFakeAnnotationStore()
	s := newM5TestServer(t, "viewer", Deps{Annotations: st})

	from := "2026-08-07T00:00:00Z"
	to := "2026-08-08T00:00:00Z"
	w := doRequest(t, s, http.MethodGet, "/api/v1/annotations?from="+from+"&to="+to, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", w.Code, w.Body)
	}
	st.mu.Lock()
	gotFrom, gotTo := st.lastFilter.From, st.lastFilter.To
	st.mu.Unlock()
	if gotFrom.Format(time.RFC3339) != from || gotTo.Format(time.RFC3339) != to {
		t.Errorf("window reached the store as [%s, %s), want [%s, %s)", gotFrom, gotTo, from, to)
	}

	for _, path := range []string{
		"/api/v1/annotations?from=yesterday",
		"/api/v1/annotations?to=tomorrow",
		"/api/v1/annotations?cursor=not-a-cursor",
	} {
		w = doRequest(t, s, http.MethodGet, path, nil, nil)
		if w.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400: %s", path, w.Code, w.Body)
		}
	}
}

func TestAnnotationsCreateValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"unparseable body", `not json`, http.StatusBadRequest},
		{"missing text", `{"startAt":"2026-08-07T10:00:00Z"}`, http.StatusUnprocessableEntity},
		{"missing startAt", `{"text":"note"}`, http.StatusUnprocessableEntity},
		{"bad startAt", `{"startAt":"yesterday","text":"note"}`, http.StatusBadRequest},
		{
			"text over 1024 bytes",
			fmt.Sprintf(`{"startAt":"2026-08-07T10:00:00Z","text":%q}`, strings.Repeat("x", 1025)),
			http.StatusUnprocessableEntity,
		},
		{
			"endAt before startAt",
			`{"startAt":"2026-08-07T10:00:00Z","endAt":"2026-08-07T09:00:00Z","text":"note"}`,
			http.StatusUnprocessableEntity,
		},
		{
			"scope over 255 bytes",
			fmt.Sprintf(`{"startAt":"2026-08-07T10:00:00Z","scope":%q,"text":"note"}`, strings.Repeat("s", 256)),
			http.StatusUnprocessableEntity,
		},
	}
	for _, c := range cases {
		st := newFakeAnnotationStore()
		s := newM5TestServer(t, "operator", Deps{Annotations: st})
		w := doRequest(t, s, http.MethodPost, "/api/v1/annotations", strings.NewReader(c.body), mutateWithCSRF)
		if w.Code != c.want {
			t.Errorf("%s: POST = %d, want %d: %s", c.name, w.Code, c.want, w.Body)
		}
		if w.Code >= http.StatusBadRequest && !strings.Contains(w.Body.String(), "annotation") {
			t.Errorf("%s: detail = %s, want it to name the resource", c.name, w.Body)
		}
	}
}

// A span IS accepted -- endAt at or after startAt (M5 Decision 10).
func TestAnnotationsCreateAcceptsASpan(t *testing.T) {
	st := newFakeAnnotationStore()
	s := newM5TestServer(t, "operator", Deps{Annotations: st})
	body := `{"startAt":"2026-08-07T10:00:00Z","endAt":"2026-08-07T11:00:00Z","text":"maintenance"}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/annotations", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d, want 201: %s", w.Code, w.Body)
	}
	var got annotationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.EndAt == nil || !got.EndAt.Equal(time.Date(2026, 8, 7, 11, 0, 0, 0, time.UTC)) {
		t.Errorf("endAt = %v, want the span's end", got.EndAt)
	}
	if got.Scope != "" {
		t.Errorf("scope = %q, want the global scope for an omitted scope", got.Scope)
	}
}

func TestAnnotationsCreateStoreFailureReturns502(t *testing.T) {
	st := newFakeAnnotationStore()
	st.createErr = errors.New("pool exhausted")
	s := newM5TestServer(t, "operator", Deps{Annotations: st})
	w := doRequest(t, s, http.MethodPost, "/api/v1/annotations", strings.NewReader(validAnnotationBody), mutateWithCSRF)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "pool exhausted") {
		t.Errorf("body = %s, must never echo the driver error", w.Body)
	}
}

func TestAnnotationsDeleteUnknownAndMalformedAreBoth404(t *testing.T) {
	st := newFakeAnnotationStore()
	s := newM5TestServer(t, "operator", Deps{Annotations: st})
	for _, id := range []string{uuid.NewString(), "not-a-uuid"} {
		w := doRequest(t, s, http.MethodDelete, "/api/v1/annotations/"+id, nil, mutateWithCSRF)
		if w.Code != http.StatusNotFound {
			t.Errorf("DELETE /api/v1/annotations/%s = %d, want 404: %s", id, w.Code, w.Body)
		}
	}
}

// The audit row for a created annotation carries the SCOPE and nothing else.
// text is free-form operator prose -- an audit log is read by more people and
// retained longer than the note is, so it must never land there.
func TestAnnotationsCreateAuditDetailIsScopeOnlyAndNeverText(t *testing.T) {
	fs := &fakeAuditStore{}
	st := newFakeAnnotationStore()
	s := newM5TestServer(t, "operator", Deps{Annotations: st, Audit: fs})

	body := `{"startAt":"2026-08-07T10:00:00Z","scope":"node-a",` +
		`"text":"root password is hunter2, rotating at 3am"}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/annotations", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /api/v1/annotations status %d: %s", w.Code, w.Body)
	}

	entries := waitForOneAuditEntry(t, fs)
	detail := string(entries[0].Detail)
	for _, banned := range []string{"text", "hunter2", "rotating", "password"} {
		if strings.Contains(detail, banned) {
			t.Errorf("audit detail = %s, must never carry %q", detail, banned)
		}
	}
	if !strings.Contains(detail, `"scope":"node-a"`) {
		t.Errorf("audit detail = %s, want the allow-listed scope", detail)
	}
	if entries[0].Action != "POST /api/v1/annotations" {
		t.Errorf("audit action = %q, want the route pattern", entries[0].Action)
	}
}

// DELETE carries no body, so its detail is the default-deny {} -- the id it
// names is already in the row's resource column.
func TestAnnotationsDeleteIsAuditedWithEmptyDetail(t *testing.T) {
	fs := &fakeAuditStore{}
	st := newFakeAnnotationStore()
	id := st.seed("node-a", "note", time.Now().UTC())
	s := newM5TestServer(t, "operator", Deps{Annotations: st, Audit: fs})

	w := doRequest(t, s, http.MethodDelete, "/api/v1/annotations/"+id, nil, mutateWithCSRF)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE status %d: %s", w.Code, w.Body)
	}
	entries := waitForOneAuditEntry(t, fs)
	if got := string(entries[0].Detail); got != "{}" {
		t.Errorf("audit detail = %s, want {}", got)
	}
	if entries[0].Resource != id {
		t.Errorf("audit resource = %q, want the annotation id %q", entries[0].Resource, id)
	}
	if entries[0].Action != "DELETE /api/v1/annotations/{id}" {
		t.Errorf("audit action = %q, want the route pattern", entries[0].Action)
	}
}

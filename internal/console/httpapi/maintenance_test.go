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

// fakeMaintenanceStore is one double for MaintenanceService, mutex-guarded like
// fakeAnnotationStore.
type fakeMaintenanceStore struct {
	mu sync.Mutex

	windows map[string]store.MaintenanceWindow
	order   []string

	lastFilter store.MaintenanceFilter

	createErr error
	listErr   error
	deleteErr error
}

func newFakeMaintenanceStore() *fakeMaintenanceStore {
	return &fakeMaintenanceStore{windows: map[string]store.MaintenanceWindow{}}
}

func (f *fakeMaintenanceStore) CreateMaintenanceWindow(_ context.Context, in store.MaintenanceInput) (store.MaintenanceWindow, error) { //nolint:gocritic // hugeParam: test double mirrors the store signature
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return store.MaintenanceWindow{}, f.createErr
	}
	if err := in.Validate(); err != nil {
		return store.MaintenanceWindow{}, err
	}
	win := store.MaintenanceWindow{
		ID: uuid.NewString(), Scope: in.Scope, StartAt: in.StartAt, EndAt: in.EndAt,
		Reason: in.Reason, CreatedBy: in.CreatedBy, CreatedAt: time.Now().UTC(),
	}
	f.windows[win.ID] = win
	f.order = append(f.order, win.ID)
	return win, nil
}

func (f *fakeMaintenanceStore) ListMaintenanceWindows(_ context.Context, filter store.MaintenanceFilter) (store.MaintenancePage, error) { //nolint:gocritic // hugeParam: test double mirrors the store signature
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastFilter = filter
	if f.listErr != nil {
		return store.MaintenancePage{}, f.listErr
	}
	out := make([]store.MaintenanceWindow, 0, len(f.order))
	for _, id := range f.order {
		win, ok := f.windows[id]
		if !ok {
			continue
		}
		if filter.Scope != nil && win.Scope != *filter.Scope {
			continue
		}
		out = append(out, win)
	}
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return store.MaintenancePage{Windows: out}, nil
}

func (f *fakeMaintenanceStore) DeleteMaintenanceWindow(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if _, ok := f.windows[id]; !ok {
		return store.ErrNotFound
	}
	delete(f.windows, id)
	return nil
}

func (f *fakeMaintenanceStore) seed(scope, reason string, start time.Time) string {
	win, err := f.CreateMaintenanceWindow(context.Background(), store.MaintenanceInput{
		Scope: scope, StartAt: start, EndAt: start.Add(time.Hour), Reason: reason, CreatedBy: "user:seed",
	})
	if err != nil {
		panic(err)
	}
	return win.ID
}

const validMaintenanceBody = `{"scope":"node-a","startAt":"2026-08-07T10:00:00Z",` +
	`"endAt":"2026-08-07T12:00:00Z","reason":"switch firmware"}`

func maintenanceRoutes(id string) []struct{ method, path, body string } {
	return []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/maintenance", ""},
		{http.MethodPost, "/api/v1/maintenance", validMaintenanceBody},
		{http.MethodDelete, "/api/v1/maintenance/" + id, ""},
	}
}

func TestMaintenanceWithoutStoreReturn503(t *testing.T) {
	s := newM5TestServer(t, "operator", Deps{})
	for _, c := range maintenanceRoutes(uuid.NewString()) {
		var mutate func(*http.Request)
		if isMutatingMethod(c.method) {
			mutate = mutateWithCSRF
		}
		w := doRequest(t, s, c.method, c.path, strings.NewReader(c.body), mutate)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s without a MaintenanceService = %d, want 503: %s", c.method, c.path, w.Code, w.Body)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
			t.Errorf("%s %s Content-Type = %q, want application/problem+json", c.method, c.path, ct)
		}
		if !strings.Contains(w.Body.String(), "console.database.mode") {
			t.Errorf("%s %s 503 detail = %s, want it to name console.database.mode", c.method, c.path, w.Body)
		}
	}
}

func TestMaintenanceViewerReadsButCannotWrite(t *testing.T) {
	for _, role := range []string{"viewer", "alert-editor"} {
		st := newFakeMaintenanceStore()
		id := st.seed("", "seeded", time.Now().UTC())
		s := newM5TestServer(t, role, Deps{Maintenance: st})

		w := doRequest(t, s, http.MethodGet, "/api/v1/maintenance", nil, nil)
		if w.Code != http.StatusOK {
			t.Errorf("%s: GET /api/v1/maintenance = %d, want 200: %s", role, w.Code, w.Body)
		}
		w = doRequest(t, s, http.MethodPost, "/api/v1/maintenance", strings.NewReader(validMaintenanceBody), mutateWithCSRF)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s: POST /api/v1/maintenance = %d, want 403: %s", role, w.Code, w.Body)
		}
		w = doRequest(t, s, http.MethodDelete, "/api/v1/maintenance/"+id, nil, mutateWithCSRF)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s: DELETE /api/v1/maintenance/{id} = %d, want 403: %s", role, w.Code, w.Body)
		}
	}
}

func TestMaintenanceRequiresMaintenanceRead(t *testing.T) {
	st := newFakeMaintenanceStore()
	s := newNoTelemetryServer(t, Deps{Maintenance: st})
	w := doRequest(t, s, http.MethodGet, "/api/v1/maintenance", nil, nil)
	if w.Code != http.StatusForbidden {
		t.Errorf("GET /api/v1/maintenance without maintenance:read = %d, want 403: %s", w.Code, w.Body)
	}
}

func TestMaintenanceOperatorCreatesAndDeletes(t *testing.T) {
	for _, role := range []string{"operator", "admin"} {
		st := newFakeMaintenanceStore()
		s := newM5TestServer(t, role, Deps{Maintenance: st})

		w := doRequest(t, s, http.MethodPost, "/api/v1/maintenance", strings.NewReader(validMaintenanceBody), mutateWithCSRF)
		if w.Code != http.StatusCreated {
			t.Fatalf("%s: POST = %d, want 201: %s", role, w.Code, w.Body)
		}
		var got maintenanceResponse
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.ID == "" || got.Scope != "node-a" || got.Reason != "switch firmware" {
			t.Errorf("%s: body = %+v, want the created window echoed back", role, got)
		}
		if got.CreatedBy != "user:u1" {
			t.Errorf("%s: createdBy = %q, want the authenticated subject", role, got.CreatedBy)
		}
		if want := "/api/v1/maintenance/" + got.ID; w.Header().Get("Location") != want {
			t.Errorf("%s: Location = %q, want %q", role, w.Header().Get("Location"), want)
		}

		w = doRequest(t, s, http.MethodDelete, "/api/v1/maintenance/"+got.ID, nil, mutateWithCSRF)
		if w.Code != http.StatusNoContent {
			t.Errorf("%s: DELETE = %d, want 204: %s", role, w.Code, w.Body)
		}
	}
}

func TestMaintenanceListScopePointerSemanticsAndWindow(t *testing.T) {
	cases := []struct {
		path string
		want *string
	}{
		{"/api/v1/maintenance", nil},
		{"/api/v1/maintenance?scope=", ptrTo("")},
		{"/api/v1/maintenance?scope=node-a", ptrTo("node-a")},
	}
	for _, c := range cases {
		st := newFakeMaintenanceStore()
		s := newM5TestServer(t, "viewer", Deps{Maintenance: st})
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

	st := newFakeMaintenanceStore()
	st.seed("node-a", "switch firmware", time.Now().UTC())
	s := newM5TestServer(t, "viewer", Deps{Maintenance: st})
	from, to := "2026-08-07T00:00:00Z", "2026-08-08T00:00:00Z"
	w := doRequest(t, s, http.MethodGet, "/api/v1/maintenance?from="+from+"&to="+to, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", w.Code, w.Body)
	}
	var page maintenanceListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Windows) != 1 {
		t.Fatalf("windows = %+v, want the seeded row", page.Windows)
	}
	st.mu.Lock()
	gotFrom, gotTo := st.lastFilter.From, st.lastFilter.To
	st.mu.Unlock()
	if gotFrom.Format(time.RFC3339) != from || gotTo.Format(time.RFC3339) != to {
		t.Errorf("window reached the store as [%s, %s), want [%s, %s)", gotFrom, gotTo, from, to)
	}
}

func TestMaintenanceListBadInputs(t *testing.T) {
	st := newFakeMaintenanceStore()
	s := newM5TestServer(t, "viewer", Deps{Maintenance: st})
	for _, path := range []string{
		"/api/v1/maintenance?from=yesterday",
		"/api/v1/maintenance?to=tomorrow",
		"/api/v1/maintenance?cursor=not-a-cursor",
	} {
		w := doRequest(t, s, http.MethodGet, path, nil, nil)
		if w.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400: %s", path, w.Code, w.Body)
		}
	}
}

func TestMaintenanceCreateValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"unparseable body", `not json`, http.StatusBadRequest},
		{"missing reason", `{"startAt":"2026-08-07T10:00:00Z","endAt":"2026-08-07T12:00:00Z"}`, http.StatusUnprocessableEntity},
		{"missing startAt", `{"endAt":"2026-08-07T12:00:00Z","reason":"r"}`, http.StatusUnprocessableEntity},
		{"missing endAt", `{"startAt":"2026-08-07T10:00:00Z","reason":"r"}`, http.StatusUnprocessableEntity},
		{"bad startAt", `{"startAt":"yesterday","endAt":"2026-08-07T12:00:00Z","reason":"r"}`, http.StatusBadRequest},
		{
			"endAt equal to startAt",
			`{"startAt":"2026-08-07T10:00:00Z","endAt":"2026-08-07T10:00:00Z","reason":"r"}`,
			http.StatusUnprocessableEntity,
		},
		{
			"endAt before startAt",
			`{"startAt":"2026-08-07T12:00:00Z","endAt":"2026-08-07T10:00:00Z","reason":"r"}`,
			http.StatusUnprocessableEntity,
		},
		{
			"reason over 512 bytes",
			fmt.Sprintf(`{"startAt":"2026-08-07T10:00:00Z","endAt":"2026-08-07T12:00:00Z","reason":%q}`,
				strings.Repeat("r", 513)),
			http.StatusUnprocessableEntity,
		},
	}
	for _, c := range cases {
		st := newFakeMaintenanceStore()
		s := newM5TestServer(t, "operator", Deps{Maintenance: st})
		w := doRequest(t, s, http.MethodPost, "/api/v1/maintenance", strings.NewReader(c.body), mutateWithCSRF)
		if w.Code != c.want {
			t.Errorf("%s: POST = %d, want %d: %s", c.name, w.Code, c.want, w.Body)
		}
		if w.Code >= http.StatusBadRequest && !strings.Contains(w.Body.String(), "maintenance") {
			t.Errorf("%s: detail = %s, want it to name the resource", c.name, w.Body)
		}
	}
}

func TestMaintenanceDeleteUnknownAndMalformedAreBoth404(t *testing.T) {
	st := newFakeMaintenanceStore()
	s := newM5TestServer(t, "operator", Deps{Maintenance: st})
	for _, id := range []string{uuid.NewString(), "not-a-uuid"} {
		w := doRequest(t, s, http.MethodDelete, "/api/v1/maintenance/"+id, nil, mutateWithCSRF)
		if w.Code != http.StatusNotFound {
			t.Errorf("DELETE /api/v1/maintenance/%s = %d, want 404: %s", id, w.Code, w.Body)
		}
	}
}

func TestMaintenanceStoreFailureReturns502(t *testing.T) {
	st := newFakeMaintenanceStore()
	st.createErr = errors.New("pool exhausted")
	s := newM5TestServer(t, "operator", Deps{Maintenance: st})
	w := doRequest(t, s, http.MethodPost, "/api/v1/maintenance", strings.NewReader(validMaintenanceBody), mutateWithCSRF)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "pool exhausted") {
		t.Errorf("body = %s, must never echo the driver error", w.Body)
	}
}

// The audit row for a declared window carries the SCOPE and nothing else --
// annotations' "text" rule, applied to "reason", which is where a change
// ticket id or an internal hostname ends up.
func TestMaintenanceCreateAuditDetailIsScopeOnlyAndNeverReason(t *testing.T) {
	fs := &fakeAuditStore{}
	st := newFakeMaintenanceStore()
	s := newM5TestServer(t, "operator", Deps{Maintenance: st, Audit: fs})

	body := `{"scope":"node-a","startAt":"2026-08-07T10:00:00Z","endAt":"2026-08-07T12:00:00Z",` +
		`"reason":"CHG-9 on mgmt-gw-1.internal, creds in hunter2"}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/maintenance", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST status %d: %s", w.Code, w.Body)
	}

	entries := waitForOneAuditEntry(t, fs)
	detail := string(entries[0].Detail)
	for _, banned := range []string{"reason", "CHG-9", "mgmt-gw-1", "hunter2", "startAt"} {
		if strings.Contains(detail, banned) {
			t.Errorf("audit detail = %s, must never carry %q", detail, banned)
		}
	}
	if !strings.Contains(detail, `"scope":"node-a"`) {
		t.Errorf("audit detail = %s, want the allow-listed scope", detail)
	}
	if entries[0].Action != "POST /api/v1/maintenance" {
		t.Errorf("audit action = %q, want the route pattern", entries[0].Action)
	}
}

func TestMaintenanceDeleteIsAuditedWithEmptyDetail(t *testing.T) {
	fs := &fakeAuditStore{}
	st := newFakeMaintenanceStore()
	id := st.seed("node-a", "switch firmware", time.Now().UTC())
	s := newM5TestServer(t, "operator", Deps{Maintenance: st, Audit: fs})

	w := doRequest(t, s, http.MethodDelete, "/api/v1/maintenance/"+id, nil, mutateWithCSRF)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE status %d: %s", w.Code, w.Body)
	}
	entries := waitForOneAuditEntry(t, fs)
	if got := string(entries[0].Detail); got != "{}" {
		t.Errorf("audit detail = %s, want {}", got)
	}
	if entries[0].Resource != id {
		t.Errorf("audit resource = %q, want the window id %q", entries[0].Resource, id)
	}
}

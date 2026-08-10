package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// fakeK8sEventStore is the K8sEventService double: read-only, matching the
// narrowed seam the handler is typed to.
type fakeK8sEventStore struct {
	mu sync.Mutex

	events []store.K8sEvent
	nextID int64

	lastFilter store.K8sEventFilter
	nextCursor string
	listErr    error
}

func newFakeK8sEventStore() *fakeK8sEventStore { return &fakeK8sEventStore{} }

func (f *fakeK8sEventStore) ListK8sEvents(_ context.Context, filter store.K8sEventFilter) (store.K8sEventPage, error) { //nolint:gocritic // hugeParam: test double mirrors the store signature
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastFilter = filter
	if f.listErr != nil {
		return store.K8sEventPage{}, f.listErr
	}
	out := make([]store.K8sEvent, 0, len(f.events))
	for i := range f.events {
		e := f.events[i]
		if filter.Name != "" && e.Name != filter.Name {
			continue
		}
		if filter.Kind != "" && e.Kind != filter.Kind {
			continue
		}
		if filter.Type != "" && e.Type != filter.Type {
			continue
		}
		out = append(out, e)
	}
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return store.K8sEventPage{Events: out, NextCursor: f.nextCursor}, nil
}

func (f *fakeK8sEventStore) seed(kind, name, eventType, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	ns := "kconmon"
	if kind == "Node" {
		ns = "" // Node events are cluster-scoped
	}
	f.events = append(f.events, store.K8sEvent{
		ID: f.nextID, UID: "uid-" + strconv.FormatInt(f.nextID, 10), ResourceVersion: "1",
		EventTime: time.Now().UTC(), Kind: kind, Name: name, Namespace: ns,
		Reason: reason, Type: eventType, Message: reason + " happened", Count: 1,
	})
}

func (f *fakeK8sEventStore) filter(t *testing.T) store.K8sEventFilter {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastFilter
}

// The 503 must name BOTH knobs: the one that makes the endpoint exist
// (database.mode) and the one that makes it non-empty (kubernetesContext),
// or an operator with a database and no reader will fix the wrong thing.
func TestK8sEventsWithoutStoreReturn503NamingBothKnobs(t *testing.T) {
	s := newM5TestServer(t, "viewer", Deps{})
	w := doRequest(t, s, http.MethodGet, "/api/v1/k8s-events", nil, nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /api/v1/k8s-events without a store = %d, want 503: %s", w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	for _, knob := range []string{"console.database.mode", "console.kubernetesContext.enabled"} {
		if !strings.Contains(w.Body.String(), knob) {
			t.Errorf("503 detail = %s, want it to name %s", w.Body, knob)
		}
	}
}

// A console WITH a database and WITHOUT the reader answers an empty page, not
// a 503: nothing was captured is a different fact from the endpoint being
// unavailable, and conflating them sends the operator hunting an API bug.
func TestK8sEventsWithNoCapturedRowsIsAnEmptyPageNot503(t *testing.T) {
	st := newFakeK8sEventStore()
	s := newM5TestServer(t, "viewer", Deps{K8sEvents: st})
	w := doRequest(t, s, http.MethodGet, "/api/v1/k8s-events", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", w.Code, w.Body)
	}
	var page k8sEventsListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if page.Events == nil {
		t.Errorf("events = null, want an empty ARRAY -- the frontend iterates it")
	}
	if len(page.Events) != 0 {
		t.Errorf("events = %+v, want none", page.Events)
	}
}

func TestK8sEventsRideEventsRead(t *testing.T) {
	for _, role := range []string{"viewer", "alert-editor", "operator", "admin"} {
		st := newFakeK8sEventStore()
		st.seed("Node", "node-a", "Warning", "NodeNotReady")
		s := newM5TestServer(t, role, Deps{K8sEvents: st})
		w := doRequest(t, s, http.MethodGet, "/api/v1/k8s-events", nil, nil)
		if w.Code != http.StatusOK {
			t.Errorf("%s: GET /api/v1/k8s-events = %d, want 200: %s", role, w.Code, w.Body)
		}
	}

	st := newFakeK8sEventStore()
	s := newNoTelemetryServer(t, Deps{K8sEvents: st})
	w := doRequest(t, s, http.MethodGet, "/api/v1/k8s-events", nil, nil)
	if w.Code != http.StatusForbidden {
		t.Errorf("GET /api/v1/k8s-events without events:read = %d, want 403: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "events:read") {
		t.Errorf("403 detail = %s, want it to name events:read", w.Body)
	}
}

func TestK8sEventsFiltersReachTheStore(t *testing.T) {
	st := newFakeK8sEventStore()
	st.seed("Node", "node-a", "Warning", "NodeNotReady")
	st.seed("Pod", "kconmon-agent-x", "Normal", "Started")
	s := newM5TestServer(t, "viewer", Deps{K8sEvents: st})

	w := doRequest(t, s, http.MethodGet, "/api/v1/k8s-events?kind=Node&type=Warning&name=node-a", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", w.Code, w.Body)
	}
	var page k8sEventsListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("events = %+v, want exactly the node-a warning", page.Events)
	}
	got := page.Events[0]
	if got.Kind != "Node" || got.Name != "node-a" || got.Reason != "NodeNotReady" {
		t.Errorf("event = %+v, want the seeded node warning", got)
	}
	if got.ID != "1" {
		t.Errorf("id = %q, want the bigint rendered as a STRING -- pinned refs spell every id that way", got.ID)
	}
	if got.Namespace != "" {
		t.Errorf("namespace = %q, want \"\" for a cluster-scoped Node event", got.Namespace)
	}

	f := st.filter(t)
	if f.Kind != "Node" || f.Type != "Warning" || f.Name != "node-a" {
		t.Errorf("filter reached the store as %+v, want kind/type/name carried through", f)
	}
	if f.Limit != pageDefaultLimit {
		t.Errorf("limit = %d, want the default %d", f.Limit, pageDefaultLimit)
	}

	from, to := "2026-08-07T00:00:00Z", "2026-08-08T00:00:00Z"
	w = doRequest(t, s, http.MethodGet, "/api/v1/k8s-events?from="+from+"&to="+to+"&limit=9000", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("window status %d, want 200: %s", w.Code, w.Body)
	}
	f = st.filter(t)
	if f.From.Format(time.RFC3339) != from || f.To.Format(time.RFC3339) != to {
		t.Errorf("window reached the store as [%s, %s), want [%s, %s)", f.From, f.To, from, to)
	}
	if f.Limit != pageMaxLimit {
		t.Errorf("limit = %d, want it CLAMPED to %d, never rejected", f.Limit, pageMaxLimit)
	}
}

// A closed vocabulary rejected at the edge: kind and type outside the sets the
// writer can ever produce are typos, and an empty page would hide them.
func TestK8sEventsBadInputsAre400(t *testing.T) {
	st := newFakeK8sEventStore()
	s := newM5TestServer(t, "viewer", Deps{K8sEvents: st})
	for _, path := range []string{
		"/api/v1/k8s-events?kind=Deployment",
		"/api/v1/k8s-events?type=Critical",
		"/api/v1/k8s-events?from=yesterday",
		"/api/v1/k8s-events?to=tomorrow",
		"/api/v1/k8s-events?cursor=not-a-cursor",
	} {
		w := doRequest(t, s, http.MethodGet, path, nil, nil)
		if w.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400: %s", path, w.Code, w.Body)
		}
	}
}

// The cursor is the (event_time, id) BIGINT codec, not the UUID one: a cursor
// minted by store.EncodeCursor round-trips, and nextCursor comes straight back
// out of the page.
func TestK8sEventsCursorRoundTrips(t *testing.T) {
	st := newFakeK8sEventStore()
	st.seed("Pod", "kconmon-agent-x", "Normal", "Started")
	cursor := store.EncodeCursor(time.Now().UTC(), 7)
	st.nextCursor = store.EncodeCursor(time.Now().UTC(), 42)
	s := newM5TestServer(t, "viewer", Deps{K8sEvents: st})

	w := doRequest(t, s, http.MethodGet, "/api/v1/k8s-events?cursor="+cursor, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", w.Code, w.Body)
	}
	if got := st.filter(t).Cursor; got != cursor {
		t.Errorf("cursor reached the store as %q, want %q", got, cursor)
	}
	var page k8sEventsListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if page.NextCursor != st.nextCursor {
		t.Errorf("nextCursor = %q, want the store's %q", page.NextCursor, st.nextCursor)
	}
}

func TestK8sEventsStoreFailureReturns502(t *testing.T) {
	st := newFakeK8sEventStore()
	st.listErr = errors.New("pool exhausted")
	s := newM5TestServer(t, "viewer", Deps{K8sEvents: st})
	w := doRequest(t, s, http.MethodGet, "/api/v1/k8s-events", nil, nil)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "pool exhausted") {
		t.Errorf("body = %s, must never echo the driver error", w.Body)
	}
}

package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// A query parameter that is not valid UTF-8 is the caller's malformed input, the same as a NUL:
// PostgreSQL refuses it in a text column (SQLSTATE 22021) and the handler used to answer 502
// "<subsystem> unavailable" plus an ERROR log line. It is a 400 before any store call now.
func TestQueryParamsRefuseInvalidUTF8(t *testing.T) {
	srv := newM5TestServer(t, "admin", Deps{
		Incidents:   newFakeIncidentStore(),
		Maintenance: newFakeMaintenanceStore(),
		Annotations: newFakeAnnotationStore(),
		MTR:         newFakeMTRStore(),
		K8sEvents:   newFakeK8sEventStore(),
		Events:      emptyEventLister{},
		Targets:     newFakeTargetService(),
		Runner:      newFakeRunner(),
	})
	pairCursor := func(source, destination string) string {
		return url.QueryEscape(store.EncodePairCursor(source, destination))
	}
	targets := []string{
		"/api/v1/incidents?scope=%FF",
		"/api/v1/incidents?scope=a%C3",
		"/api/v1/maintenance?scope=%FF",
		"/api/v1/annotations?scope=%FF",
		"/api/v1/mtr/snapshots?source=%FF&destination=b",
		"/api/v1/mtr/snapshots?source=a&destination=%FF",
		"/api/v1/k8s-events?name=%FF",
		"/api/v1/events?scope=%FF",
		"/api/v1/events?scopeNode=%FF",
		"/api/v1/targets?kind=%FF",
		"/api/v1/runs?type=%FF",
		"/api/v1/runs?status=%FF",
		// The destinations cursor carries the pair it resumes after, and the store puts both
		// fields into the keyset predicate.
		"/api/v1/mtr/destinations?cursor=" + pairCursor("a\xff", "b"),
		"/api/v1/mtr/destinations?cursor=" + pairCursor("a", "b\x00"),
	}
	for _, target := range targets {
		w := doRequest(t, srv, http.MethodGet, target, nil, nil)
		if w.Code != http.StatusBadRequest {
			t.Errorf("GET %s: got %d %s, want 400", target, w.Code, w.Body)
			continue
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
			t.Errorf("GET %s: Content-Type %q, want application/problem+json", target, ct)
		}
	}

	// Valid multi-byte UTF-8 is an ordinary name and passes.
	for _, target := range []string{
		"/api/v1/incidents?scope=%C3%A9",
		"/api/v1/events?scope=n%C3%B3de",
		"/api/v1/mtr/destinations?cursor=" + pairCursor("nóde", "b"),
	} {
		if w := doRequest(t, srv, http.MethodGet, target, nil, nil); w.Code != http.StatusOK {
			t.Errorf("GET %s: got %d %s, want 200", target, w.Code, w.Body)
		}
	}
}

type emptyEventLister struct{}

func (emptyEventLister) ListEvents(context.Context, store.EventFilter) (store.EventPage, error) { //nolint:gocritic // hugeParam: mirrors the interface
	return store.EventPage{}, nil
}

func TestRejectControlCharsNamesTheRule(t *testing.T) {
	for _, v := range []string{"a\xffb", "a\x00b", "\xc3"} {
		w := httptest.NewRecorder()
		if !rejectControlChars(w, "scope", v) {
			t.Errorf("%q passed", v)
			continue
		}
		if !strings.Contains(w.Body.String(), "valid UTF-8") {
			t.Errorf("%q: detail %s does not say what is allowed", v, w.Body)
		}
	}
	for _, v := range []string{"", "node-1", "nóde", "a->b"} {
		if rejectControlChars(httptest.NewRecorder(), "scope", v) {
			t.Errorf("%q was refused", v)
		}
	}
}

// limit, enabled and enrich were the parameters a NUL or invalid UTF-8 slipped through with a 200,
// because an unparseable value there reads as unset. Garbage bytes are malformed input, not an
// unparseable number, so they answer 400 like every other parameter.
func TestLenientQueryParamsStillRefuseInvalidText(t *testing.T) {
	srv := newM5TestServer(t, "admin", Deps{
		Incidents:   newFakeIncidentStore(),
		Maintenance: newFakeMaintenanceStore(),
		Annotations: newFakeAnnotationStore(),
		MTR:         newFakeMTRStore(),
		K8sEvents:   newFakeK8sEventStore(),
		Events:      emptyEventLister{},
		Targets:     newFakeTargetService(),
		Runner:      newFakeRunner(),
	})
	for _, target := range []string{
		"/api/v1/events?limit=%00",
		"/api/v1/events?limit=%FF",
		"/api/v1/events?limit=%C3%28",
		"/api/v1/events?limit=%ED%A0%80",
		"/api/v1/events?limit=%01",
		"/api/v1/targets?limit=%FF",
		"/api/v1/runs?limit=%00",
		"/api/v1/incidents?limit=%00",
		"/api/v1/checks?enabled=%00",
		"/api/v1/schedules?enabled=%FF",
		"/api/v1/mtr/snapshots/00000000-0000-4000-8000-000000000001?enrich=%00",
	} {
		w := doRequest(t, srv, http.MethodGet, target, nil, nil)
		if w.Code != http.StatusBadRequest {
			t.Errorf("GET %s: got %d %s, want 400", target, w.Code, w.Body)
			continue
		}
		if !strings.Contains(w.Body.String(), "valid UTF-8") {
			t.Errorf("GET %s: detail %s does not say what is allowed", target, w.Body)
		}
	}
	// A value that is merely not a number keeps the documented "treated as unset".
	for _, target := range []string{"/api/v1/events?limit=abc", "/api/v1/targets?limit=-"} {
		if w := doRequest(t, srv, http.MethodGet, target, nil, nil); w.Code != http.StatusOK {
			t.Errorf("GET %s: got %d %s, want 200", target, w.Code, w.Body)
		}
	}
}

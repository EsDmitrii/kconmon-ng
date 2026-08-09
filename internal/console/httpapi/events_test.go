package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/config"
	"github.com/EsDmitrii/kconmon-ng/internal/console/httpapi"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
	"github.com/prometheus/client_golang/prometheus"
)

// fakeEventLister is an EventLister double: the same fake-not-driver style
// fakeRealtime (server_test.go) and push.Nudger already use. It records the
// filter handleEvents built and returns a canned page or error.
type fakeEventLister struct {
	gotFilter store.EventFilter
	page      store.EventPage
	err       error
}

func (f *fakeEventLister) ListEvents(_ context.Context, filter store.EventFilter) (store.EventPage, error) {
	f.gotFilter = filter
	return f.page, f.err
}

// newEventsServer wires a server with only what GET /api/v1/events needs.
// lister is passed as the httpapi.EventLister interface param itself (not a
// concrete *fakeEventLister variable that might be nil) so that passing nil
// here is a genuine nil interface, never a typed-nil trap.
func newEventsServer(t *testing.T, lister httpapi.EventLister) *httpapi.Server {
	t.Helper()
	cfg, err := config.Load("/nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	m := metrics.New(cfg.MetricsPrefix, reg)
	ui := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("spa")) })
	return httpapi.NewServer(httpapi.Deps{Config: cfg, Metrics: m, PromRegistry: reg, UI: ui, Events: lister})
}

func TestEventsWithoutListerReturns503(t *testing.T) {
	srv := newEventsServer(t, nil)
	rec := do(t, srv, http.MethodGet, "/api/v1/events", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	if !strings.Contains(rec.Body.String(), "console.database.mode") {
		t.Errorf("detail should name console.database.mode: %s", rec.Body)
	}
}

func TestEventsEmptyPageReturnsEmptyArrayNotNull(t *testing.T) {
	lister := &fakeEventLister{page: store.EventPage{}}
	srv := newEventsServer(t, lister)
	rec := do(t, srv, http.MethodGet, "/api/v1/events", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"events":[]`) {
		t.Errorf("body = %s, want a literal empty array for events", body)
	}
	if !strings.Contains(body, `"nextCursor":""`) {
		t.Errorf("body = %s, want an empty nextCursor", body)
	}
}

func TestEventsFilterPlumbing(t *testing.T) {
	lister := &fakeEventLister{}
	srv := newEventsServer(t, lister)

	cur := store.EncodeCursor(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC), 42)
	target := "/api/v1/events?type=topology_changed&type=check_observed&scope=node-a" +
		"&from=2026-01-01T00:00:00Z&to=2026-01-02T00:00:00Z&limit=25&cursor=" + cur

	rec := do(t, srv, http.MethodGet, target, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}

	got := lister.gotFilter
	wantFrom := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	wantTo := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	if !reflect.DeepEqual(got.Types, []string{"topology_changed", "check_observed"}) {
		t.Errorf("Types = %v, want [topology_changed check_observed]", got.Types)
	}
	if got.Scope != "node-a" {
		t.Errorf("Scope = %q, want node-a", got.Scope)
	}
	if !got.From.Equal(wantFrom) {
		t.Errorf("From = %v, want %v", got.From, wantFrom)
	}
	if !got.To.Equal(wantTo) {
		t.Errorf("To = %v, want %v", got.To, wantTo)
	}
	if got.Cursor != cur {
		t.Errorf("Cursor = %q, want %q", got.Cursor, cur)
	}
	if got.Limit != 25 {
		t.Errorf("Limit = %d, want 25", got.Limit)
	}
}

// TestEventsScopeNodePlumbing is the node-card filter: ?scopeNode= reaches the
// lister as EventFilter.ScopeNode and leaves the exact-match Scope alone, so
// the store applies the pair-aware clause and nothing else.
func TestEventsScopeNodePlumbing(t *testing.T) {
	lister := &fakeEventLister{}
	srv := newEventsServer(t, lister)

	rec := do(t, srv, http.MethodGet, "/api/v1/events?scopeNode=node-a", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if lister.gotFilter.ScopeNode != "node-a" {
		t.Errorf("ScopeNode = %q, want node-a", lister.gotFilter.ScopeNode)
	}
	if lister.gotFilter.Scope != "" {
		t.Errorf("Scope = %q, want empty -- scopeNode must not also set the exact filter", lister.gotFilter.Scope)
	}
}

// TestEventsScopeAndScopeNodeAreMutuallyExclusive: the two filters answer
// different questions ("this exact scope" vs "this name on either side of a
// pair"), and silently AND-ing them would return a subset an operator never
// asked for. 422 rather than 400: both params are individually well-formed --
// it is the combination the server refuses (the same posture every other
// semantic refusal in this API takes).
func TestEventsScopeAndScopeNodeAreMutuallyExclusive(t *testing.T) {
	lister := &fakeEventLister{}
	srv := newEventsServer(t, lister)

	rec := do(t, srv, http.MethodGet, "/api/v1/events?scope=node-a&scopeNode=node-a", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	if !strings.Contains(rec.Body.String(), "scopeNode") {
		t.Errorf("detail should name scopeNode: %s", rec.Body)
	}
	if lister.gotFilter.Scope != "" || lister.gotFilter.ScopeNode != "" {
		t.Errorf("the lister was called despite the 422: %+v", lister.gotFilter)
	}
}

func TestEventsValidation400s(t *testing.T) {
	lister := &fakeEventLister{}
	srv := newEventsServer(t, lister)

	cases := []struct {
		name   string
		target string
	}{
		{"unknown type", "/api/v1/events?type=bogus"},
		{"from after to", "/api/v1/events?from=2026-01-02T00:00:00Z&to=2026-01-01T00:00:00Z"},
		{"from equal to", "/api/v1/events?from=2026-01-01T00:00:00Z&to=2026-01-01T00:00:00Z"},
		{"unparseable from", "/api/v1/events?from=not-a-time"},
		{"unparseable to", "/api/v1/events?to=not-a-time"},
		{"malformed cursor", "/api/v1/events?cursor=abc123"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, srv, http.MethodGet, tc.target, "")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
				t.Errorf("Content-Type = %q, want application/problem+json", ct)
			}
		})
	}
}

func TestEventsLimitClamped(t *testing.T) {
	lister := &fakeEventLister{}
	srv := newEventsServer(t, lister)

	if rec := do(t, srv, http.MethodGet, "/api/v1/events?limit=0", ""); rec.Code != http.StatusOK {
		t.Fatalf("limit=0: status %d: %s", rec.Code, rec.Body)
	}
	if lister.gotFilter.Limit != 100 {
		t.Errorf("limit=0 clamped to %d, want 100", lister.gotFilter.Limit)
	}

	if rec := do(t, srv, http.MethodGet, "/api/v1/events?limit=9999", ""); rec.Code != http.StatusOK {
		t.Fatalf("limit=9999: status %d: %s", rec.Code, rec.Body)
	}
	if lister.gotFilter.Limit != 500 {
		t.Errorf("limit=9999 clamped to %d, want 500", lister.gotFilter.Limit)
	}
}

func TestEventsListerErrorReturns502WithoutLeakingDetail(t *testing.T) {
	lister := &fakeEventLister{err: errors.New("pq: connection reset by peer, dsn=postgres://secret")}
	srv := newEventsServer(t, lister)
	rec := do(t, srv, http.MethodGet, "/api/v1/events", "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502: %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	if strings.Contains(rec.Body.String(), "secret") || strings.Contains(rec.Body.String(), "pq:") {
		t.Errorf("driver error text leaked into response: %s", rec.Body)
	}
}

func TestEventsRouteRecordedWithBoundedCardinality(t *testing.T) {
	lister := &fakeEventLister{}
	srv := newEventsServer(t, lister)
	_ = do(t, srv, http.MethodGet, "/api/v1/events", "")

	rec := do(t, srv, http.MethodGet, "/metrics", "")
	body := rec.Body.String()
	if !strings.Contains(body, "kconmon_ng_console_http_requests_total{") || !strings.Contains(body, `path="/api/v1/events"`) {
		t.Errorf("metrics missing bounded-cardinality path=/api/v1/events label:\n%s", body)
	}
}

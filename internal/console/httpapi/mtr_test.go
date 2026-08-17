package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/enrich"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// fakeMTRStore is one double for MTRService; it records the LAST filter ListPathSnapshots was
// called.
type fakeMTRStore struct {
	mu sync.Mutex

	dests []store.MTRDestination
	snaps map[string]store.PathSnapshot
	order []string // snapshot ids, oldest first, so listing order is stable
	enr   map[string]store.Enrichment

	// traces are the individual check_results rows behind the routes, keyed by nothing: the store
	// filters by pair and window, and the handler filters by path.
	traces []store.RunResult

	lastFilter store.SnapshotFilter
	lastTraces store.TraceFilter
	lastIPs    []string

	destsErr  error
	listErr   error
	getErr    error
	enrErr    error
	tracesErr error
}

func newFakeMTRStore() *fakeMTRStore {
	return &fakeMTRStore{
		snaps: map[string]store.PathSnapshot{},
		enr:   map[string]store.Enrichment{},
	}
}

func (f *fakeMTRStore) ListPathTraces(_ context.Context, filter store.TraceFilter) (store.TracePage, error) { //nolint:gocritic // hugeParam: mirrors the real signature
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastTraces = filter
	if f.tracesErr != nil {
		return store.TracePage{}, f.tracesErr
	}
	out := make([]store.RunResult, 0, len(f.traces))
	for i := range f.traces {
		t := f.traces[i]
		// The window is the real query's, applied here so a test can prove the handler passed the
		// snapshot's own bounds rather than the whole table.
		if t.RecordedAt.Before(filter.From) || t.RecordedAt.After(filter.To) {
			continue
		}
		out = append(out, t)
	}
	return store.TracePage{Traces: out}, nil
}

func (f *fakeMTRStore) ListMTRDestinations(_ context.Context, limit int, cursor string) (store.MTRDestinationPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.destsErr != nil {
		return store.MTRDestinationPage{}, f.destsErr
	}
	/* The fake PAGES, because the real store does: a double that hands back everything regardless of
	   limit and cursor cannot show whether the handler walks the listing or silently truncates it. */
	sorted := make([]store.MTRDestination, len(f.dests))
	copy(sorted, f.dests)
	slices.SortFunc(sorted, func(a, b store.MTRDestination) int {
		if c := strings.Compare(a.SourceNode, b.SourceNode); c != 0 {
			return c
		}
		return strings.Compare(a.Destination, b.Destination)
	})
	start := 0
	if cursor != "" {
		curSrc, curDst, _, err := store.DecodePairCursor(cursor)
		if err != nil {
			return store.MTRDestinationPage{}, err
		}
		for start < len(sorted) &&
			(sorted[start].SourceNode < curSrc ||
				(sorted[start].SourceNode == curSrc && sorted[start].Destination <= curDst)) {
			start++
		}
	}
	end := min(start+limit, len(sorted))
	page := store.MTRDestinationPage{Destinations: sorted[start:end]}
	if end < len(sorted) {
		last := sorted[end-1]
		page.NextCursor = store.EncodePairCursor(last.SourceNode, last.Destination)
	}
	return page, nil
}

func (f *fakeMTRStore) ListPathSnapshots(_ context.Context, filter store.SnapshotFilter) (store.SnapshotPage, error) { //nolint:gocritic // hugeParam: test double mirrors the store signature
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastFilter = filter
	if f.listErr != nil {
		return store.SnapshotPage{}, f.listErr
	}
	out := make([]store.PathSnapshot, 0, len(f.order))
	for _, id := range f.order {
		s := f.snaps[id]
		if filter.SourceNode != "" && s.SourceNode != filter.SourceNode {
			continue
		}
		if filter.Destination != "" && s.Destination != filter.Destination {
			continue
		}
		out = append(out, s)
	}
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	var next string
	if filter.Limit > 0 && len(out) == filter.Limit && len(out) > 0 {
		last := out[len(out)-1]
		next = store.EncodeUUIDCursor(last.LastSeen, last.ID)
	}
	return store.SnapshotPage{Snapshots: out, NextCursor: next}, nil
}

func (f *fakeMTRStore) GetPathSnapshot(_ context.Context, id string) (store.PathSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return store.PathSnapshot{}, f.getErr
	}
	s, ok := f.snaps[id]
	if !ok {
		return store.PathSnapshot{}, store.ErrNotFound
	}
	return s, nil
}

func (f *fakeMTRStore) GetEnrichment(_ context.Context, ips []string) (map[string]store.Enrichment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastIPs = append([]string(nil), ips...)
	if f.enrErr != nil {
		return nil, f.enrErr
	}
	out := map[string]store.Enrichment{}
	for _, ip := range ips {
		if e, ok := f.enr[ip]; ok {
			out[ip] = e
		}
	}
	return out, nil
}

// addSnapshot seeds one row and returns its id.
func (f *fakeMTRStore) addSnapshot(source, dest string, hops []store.PathHop, seen time.Time) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := uuid.NewString()
	raw, err := json.Marshal(hops)
	if err != nil { // impossible for []PathHop; the fake refuses to hide it anyway
		panic(err)
	}
	f.snaps[id] = store.PathSnapshot{
		ID: id, SourceNode: source, Destination: dest,
		PathHash: store.HashPath(hops), HopCount: int32(len(hops)), //nolint:gosec // test fixture
		Hops: raw, FirstSeen: seen, LastSeen: seen, TraceCount: 1,
	}
	f.order = append(f.order, id)
	return id
}

// newM5TestServer wires a Server whose subject holds the given BUILT-IN role, using the real
// compiled-in role sets.
func newM5TestServer(t *testing.T, role string, extra Deps) *Server { //nolint:gocritic // hugeParam: test helper
	t.Helper()
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u1"}}
	extra.Roles = fakeRoleResolver{roles: []string{role}}
	return newAuthzServer(t, authr, authz.NewPolicy(nil), extra)
}

// newNoTelemetryServer holds a CUSTOM role with a permission that is not
// mtr:read or annotations:*, so a 403 here proves the route demands its own
// permission rather than merely "some permission".
func newNoTelemetryServer(t *testing.T, extra Deps) *Server { //nolint:gocritic // hugeParam: test helper
	t.Helper()
	policy := authz.NewPolicy(map[string][]authz.Permission{"nothing": {authz.PermTopologyRead}})
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u1"}}
	extra.Roles = fakeRoleResolver{roles: []string{"nothing"}}
	return newAuthzServer(t, authr, policy, extra)
}

func mtrRoutes(id string) []struct{ method, path string } {
	return []struct{ method, path string }{
		{http.MethodGet, "/api/v1/mtr/destinations"},
		{http.MethodGet, "/api/v1/mtr/snapshots?source=a&destination=b"},
		{http.MethodGet, "/api/v1/mtr/snapshots/" + id},
	}
}

func sampleHops() []store.PathHop {
	return []store.PathHop{
		{Number: 1, IP: "10.0.0.1", RTTNs: 1_000_000, LossRatio: 0},
		{Number: 2, IP: "10.0.0.2", RTTNs: 2_000_000, LossRatio: 0.1},
	}
}

func TestMTRRoutesWithoutStoreReturn503(t *testing.T) {
	s := newM5TestServer(t, "viewer", Deps{})
	for _, c := range mtrRoutes(uuid.NewString()) {
		w := doRequest(t, s, c.method, c.path, nil, nil)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s without an MTRService = %d, want 503: %s", c.method, c.path, w.Code, w.Body)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
			t.Errorf("%s %s Content-Type = %q, want application/problem+json", c.method, c.path, ct)
		}
		if !strings.Contains(w.Body.String(), "console.database.mode") {
			t.Errorf("%s %s 503 detail = %s, want it to name console.database.mode", c.method, c.path, w.Body)
		}
	}
}

// A regression that moved mtr:read up to operator would fail here.
func TestMTRRoutesAreReadableByViewer(t *testing.T) {
	st := newFakeMTRStore()
	id := st.addSnapshot("node-a", "node-b", sampleHops(), time.Now().UTC())
	for _, role := range []string{"viewer", "alert-editor", "operator", "admin"} {
		s := newM5TestServer(t, role, Deps{MTR: st})
		for _, c := range mtrRoutes(id) {
			w := doRequest(t, s, c.method, c.path, nil, nil)
			if w.Code != http.StatusOK {
				t.Errorf("%s: %s %s = %d, want 200: %s", role, c.method, c.path, w.Code, w.Body)
			}
		}
	}
}

func TestMTRRoutesRequireMTRRead(t *testing.T) {
	st := newFakeMTRStore()
	id := st.addSnapshot("node-a", "node-b", sampleHops(), time.Now().UTC())
	s := newNoTelemetryServer(t, Deps{MTR: st})
	for _, c := range mtrRoutes(id) {
		w := doRequest(t, s, c.method, c.path, nil, nil)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s without mtr:read = %d, want 403: %s", c.method, c.path, w.Code, w.Body)
		}
	}
}

func TestMTRDestinationsReturnsRows(t *testing.T) {
	now := time.Now().UTC()
	st := newFakeMTRStore()
	st.dests = []store.MTRDestination{{
		SourceNode: "node-a", Destination: "node-b",
		SnapshotCount: 2, TraceCount: 40, FirstSeen: now.Add(-time.Hour), LastSeen: now,
	}}
	s := newM5TestServer(t, "viewer", Deps{MTR: st})

	w := doRequest(t, s, http.MethodGet, "/api/v1/mtr/destinations", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", w.Code, w.Body)
	}
	var got mtrDestinationsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Destinations) != 1 {
		t.Fatalf("destinations = %+v, want exactly one", got.Destinations)
	}
	d := got.Destinations[0]
	if d.SourceNode != "node-a" || d.Destination != "node-b" || d.SnapshotCount != 2 || d.TraceCount != 40 {
		t.Errorf("destination = %+v, want the seeded pair echoed back", d)
	}
}

// An empty table is an empty ARRAY, never JSON null: the frontend iterates it.
func TestMTRDestinationsEmptyIsAnArray(t *testing.T) {
	s := newM5TestServer(t, "viewer", Deps{MTR: newFakeMTRStore()})
	w := doRequest(t, s, http.MethodGet, "/api/v1/mtr/destinations", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", w.Code, w.Body)
	}
	if body := strings.TrimSpace(w.Body.String()); body != `{"destinations":[]}` {
		t.Errorf("body = %s, want {\"destinations\":[]}", body)
	}
}

func TestMTRDestinationsStoreFailureReturns502(t *testing.T) {
	st := newFakeMTRStore()
	st.destsErr = errors.New("pool exhausted")
	s := newM5TestServer(t, "viewer", Deps{MTR: st})
	w := doRequest(t, s, http.MethodGet, "/api/v1/mtr/destinations", nil, nil)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "pool exhausted") {
		t.Errorf("body = %s, must never echo the driver error", w.Body)
	}
}

// Both filters are REQUIRED: an unfiltered snapshot listing has no UI and no bound; the 422 detail
// must name both parameters, or the caller cannot tell which one it forgot.
func TestMTRSnapshotsRequireSourceAndDestination(t *testing.T) {
	st := newFakeMTRStore()
	s := newM5TestServer(t, "viewer", Deps{MTR: st})
	for _, path := range []string{
		"/api/v1/mtr/snapshots",
		"/api/v1/mtr/snapshots?source=node-a",
		"/api/v1/mtr/snapshots?destination=node-b",
		"/api/v1/mtr/snapshots?source=&destination=node-b",
	} {
		w := doRequest(t, s, http.MethodGet, path, nil, nil)
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("GET %s = %d, want 422: %s", path, w.Code, w.Body)
			continue
		}
		body := w.Body.String()
		if !strings.Contains(body, "source") || !strings.Contains(body, "destination") {
			t.Errorf("GET %s 422 detail = %s, want it to name source and destination", path, body)
		}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.lastFilter != (store.SnapshotFilter{}) {
		t.Errorf("store was called with %+v, want no call at all for a rejected request", st.lastFilter)
	}
}

func TestMTRSnapshotsListReturnsRowsAndHops(t *testing.T) {
	st := newFakeMTRStore()
	st.addSnapshot("node-a", "node-b", sampleHops(), time.Now().UTC())
	// Two near-misses: same source, other destination -- and same
	// destination, other source. Both must be filtered out, which is what
	// proves BOTH halves of the required filter reach the store.
	st.addSnapshot("node-a", "other", sampleHops(), time.Now().UTC())
	st.addSnapshot("node-z", "node-b", sampleHops(), time.Now().UTC())
	s := newM5TestServer(t, "viewer", Deps{MTR: st})

	w := doRequest(t, s, http.MethodGet, "/api/v1/mtr/snapshots?source=node-a&destination=node-b", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", w.Code, w.Body)
	}
	var got mtrSnapshotsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Snapshots) != 1 {
		t.Fatalf("snapshots = %+v, want exactly the node-a -> node-b row", got.Snapshots)
	}
	snap := got.Snapshots[0]
	if snap.HopCount != 2 || snap.PathHash == "" {
		t.Errorf("snapshot = %+v, want hopCount 2 and a path hash", snap)
	}
	var hops []store.PathHop
	if err := json.Unmarshal(snap.Hops, &hops); err != nil {
		t.Fatalf("decode hops: %v", err)
	}
	if len(hops) != 2 || hops[1].IP != "10.0.0.2" || hops[1].RTTNs != 2_000_000 {
		t.Errorf("hops = %+v, want the stored payload passed through verbatim", hops)
	}
}

func TestMTRSnapshotsBadCursorReturns400(t *testing.T) {
	st := newFakeMTRStore()
	s := newM5TestServer(t, "viewer", Deps{MTR: st})
	w := doRequest(t, s, http.MethodGet,
		"/api/v1/mtr/snapshots?source=a&destination=b&cursor=not-a-cursor", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", w.Code, w.Body)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.lastFilter.Cursor != "" {
		t.Errorf("store saw cursor %q, want the handler to reject it before the store", st.lastFilter.Cursor)
	}
}

func TestMTRSnapshotsLimitClamps(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"", 100},
		{"garbage", 100},
		{"0", 100},
		{"-5", 1},
		{"9000", 500},
		{"7", 7},
	}
	for _, c := range cases {
		st := newFakeMTRStore()
		s := newM5TestServer(t, "viewer", Deps{MTR: st})
		path := "/api/v1/mtr/snapshots?source=a&destination=b"
		if c.raw != "" {
			path += "&limit=" + c.raw
		}
		w := doRequest(t, s, http.MethodGet, path, nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("limit=%q status %d, want 200: %s", c.raw, w.Code, w.Body)
		}
		st.mu.Lock()
		got := st.lastFilter.Limit
		st.mu.Unlock()
		if got != c.want {
			t.Errorf("limit=%q reached the store as %d, want %d", c.raw, got, c.want)
		}
	}
}

func TestMTRSnapshotGetUnknownAndMalformedAreBoth404(t *testing.T) {
	st := newFakeMTRStore()
	s := newM5TestServer(t, "viewer", Deps{MTR: st})
	for _, id := range []string{uuid.NewString(), "not-a-uuid"} {
		w := doRequest(t, s, http.MethodGet, "/api/v1/mtr/snapshots/"+id, nil, nil)
		if w.Code != http.StatusNotFound {
			t.Errorf("GET /api/v1/mtr/snapshots/%s = %d, want 404: %s", id, w.Code, w.Body)
		}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.lastIPs) != 0 {
		t.Errorf("enrichment was consulted for a missing snapshot: %v", st.lastIPs)
	}
}

func TestMTRSnapshotGetStoreFailureReturns502(t *testing.T) {
	st := newFakeMTRStore()
	st.getErr = errors.New("pool exhausted")
	s := newM5TestServer(t, "viewer", Deps{MTR: st})
	w := doRequest(t, s, http.MethodGet, "/api/v1/mtr/snapshots/"+uuid.NewString(), nil, nil)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "pool exhausted") {
		t.Errorf("body = %s, must never echo the driver error", w.Body)
	}
}

// Without ?enrich=true the field is ABSENT, and the cache is never touched.
func TestMTRSnapshotGetWithoutEnrichOmitsTheField(t *testing.T) {
	st := newFakeMTRStore()
	id := st.addSnapshot("node-a", "node-b", sampleHops(), time.Now().UTC())
	s := newM5TestServer(t, "viewer", Deps{MTR: st})

	w := doRequest(t, s, http.MethodGet, "/api/v1/mtr/snapshots/"+id, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "enrichment") {
		t.Errorf("body = %s, want no enrichment field at all", w.Body)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.lastIPs) != 0 {
		t.Errorf("enrichment cache consulted without ?enrich=true: %v", st.lastIPs)
	}
}

// ?enrich=true with NO Deps.Enricher is cache-only: the map holds exactly the hop addresses already
// in mtr_hop_enrichment.
func TestMTRSnapshotGetEnrichIsCacheOnly(t *testing.T) {
	st := newFakeMTRStore()
	id := st.addSnapshot("node-a", "node-b", sampleHops(), time.Now().UTC())
	st.enr["10.0.0.2"] = store.Enrichment{
		IP: "10.0.0.2", RDNS: "edge.example.", ASN: 64512, Provider: "example",
		Geo: json.RawMessage(`{"country":"DE"}`), ResolvedAt: time.Now().UTC(),
	}
	s := newM5TestServer(t, "viewer", Deps{MTR: st})

	w := doRequest(t, s, http.MethodGet, "/api/v1/mtr/snapshots/"+id+"?enrich=true", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", w.Code, w.Body)
	}
	var got mtrSnapshotDetailResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Enrichment) != 1 {
		t.Fatalf("enrichment = %+v, want exactly the one cached hop", got.Enrichment)
	}
	row, ok := got.Enrichment["10.0.0.2"]
	if !ok || row.RDNS != "edge.example." || row.ASN != 64512 {
		t.Errorf("enrichment[10.0.0.2] = %+v, want the cached row", row)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if fmt.Sprint(st.lastIPs) != "[10.0.0.1 10.0.0.2]" {
		t.Errorf("cache was asked about %v, want both hop IPs in trace order", st.lastIPs)
	}
}

// A cache miss for every hop still answers with the (empty) map, so a client
// can tell "asked and nothing known" from "did not ask".
func TestMTRSnapshotGetEnrichAllMissesIsAnEmptyMap(t *testing.T) {
	st := newFakeMTRStore()
	id := st.addSnapshot("node-a", "node-b", sampleHops(), time.Now().UTC())
	s := newM5TestServer(t, "viewer", Deps{MTR: st})

	w := doRequest(t, s, http.MethodGet, "/api/v1/mtr/snapshots/"+id+"?enrich=true", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"enrichment":{}`) {
		t.Errorf("body = %s, want an empty enrichment object", w.Body)
	}
}

// An enrichment cache failure must NEVER fail the trace itself: the snapshot still answers 200.
func TestMTRSnapshotGetEnrichFailureStillServesTheTrace(t *testing.T) {
	st := newFakeMTRStore()
	id := st.addSnapshot("node-a", "node-b", sampleHops(), time.Now().UTC())
	st.enrErr = errors.New("pool exhausted")
	s := newM5TestServer(t, "viewer", Deps{MTR: st})

	w := doRequest(t, s, http.MethodGet, "/api/v1/mtr/snapshots/"+id+"?enrich=true", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"enrichment":{}`) {
		t.Errorf("body = %s, want the trace served with an empty enrichment map", w.Body)
	}
	if strings.Contains(w.Body.String(), "pool exhausted") {
		t.Errorf("body = %s, must never echo the driver error", w.Body)
	}
}

// --- M5 Task 5: the resolver seam ------------------------------------------

// If it stops compiling, the seam has drifted and the wiring in cmd/console is the thing that would
// otherwise break at runtime.
var _ EnrichmentReader = (*enrich.Resolver)(nil)

// fakeEnricher stands in for *enrich.Resolver: same read-only method, and it
// records what it was asked so a test can prove the handler went through the
// resolver rather than the store's own cache read.
type fakeEnricher struct {
	mu      sync.Mutex
	rows    map[string]store.Enrichment
	lastIPs []string
	err     error
}

func (f *fakeEnricher) GetEnrichment(_ context.Context, ips []string) (map[string]store.Enrichment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastIPs = append([]string(nil), ips...)
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]store.Enrichment{}
	for _, ip := range ips {
		if e, ok := f.rows[ip]; ok {
			out[ip] = e
		}
	}
	return out, nil
}

// TestMTRSnapshotGetEnrichPrefersTheResolver: with Deps.Enricher wired.
func TestMTRSnapshotGetEnrichPrefersTheResolver(t *testing.T) {
	st := newFakeMTRStore()
	id := st.addSnapshot("node-a", "node-b", sampleHops(), time.Now().UTC())
	st.enr["10.0.0.2"] = store.Enrichment{IP: "10.0.0.2", RDNS: "stale-cache-only.example.", ResolvedAt: time.Now().UTC()}

	res := &fakeEnricher{rows: map[string]store.Enrichment{
		"10.0.0.1": {IP: "10.0.0.1", RDNS: "resolved-one.example.", ResolvedAt: time.Now().UTC()},
		"10.0.0.2": {IP: "10.0.0.2", RDNS: "resolved-two.example.", ASN: 64496, Provider: "Example Transit Network",
			Geo: json.RawMessage(`{"country":"GB","city":"London"}`), ResolvedAt: time.Now().UTC()},
	}}
	s := newM5TestServer(t, "viewer", Deps{MTR: st, Enricher: res})

	w := doRequest(t, s, http.MethodGet, "/api/v1/mtr/snapshots/"+id+"?enrich=true", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", w.Code, w.Body)
	}
	var got mtrSnapshotDetailResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Enrichment) != 2 {
		t.Fatalf("enrichment = %+v, want both hops resolved", got.Enrichment)
	}
	if got.Enrichment["10.0.0.2"].RDNS != "resolved-two.example." {
		t.Errorf("enrichment[10.0.0.2].rdns = %q, want the RESOLVER's answer, not the cache-only one",
			got.Enrichment["10.0.0.2"].RDNS)
	}
	if got.Enrichment["10.0.0.2"].Provider != "Example Transit Network" {
		t.Errorf("enrichment[10.0.0.2].provider = %q, want the resolved provider", got.Enrichment["10.0.0.2"].Provider)
	}

	res.mu.Lock()
	defer res.mu.Unlock()
	if fmt.Sprint(res.lastIPs) != "[10.0.0.1 10.0.0.2]" {
		t.Errorf("resolver was asked about %v, want both hop IPs in trace order", res.lastIPs)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.lastIPs) != 0 {
		t.Errorf("the store's cache read was ALSO called (%v); the resolver owns that read", st.lastIPs)
	}
}

// TestMTRSnapshotGetEnrichResolverFailureStillServesTheTrace: the handler's degradation contract is
// unchanged by the swap.
func TestMTRSnapshotGetEnrichResolverFailureStillServesTheTrace(t *testing.T) {
	st := newFakeMTRStore()
	id := st.addSnapshot("node-a", "node-b", sampleHops(), time.Now().UTC())
	res := &fakeEnricher{err: errors.New("resolver exploded: 10.0.0.2 dial udp 10.96.0.10:53")}
	s := newM5TestServer(t, "viewer", Deps{MTR: st, Enricher: res})

	w := doRequest(t, s, http.MethodGet, "/api/v1/mtr/snapshots/"+id+"?enrich=true", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"enrichment":{}`) {
		t.Errorf("body = %s, want the trace served with an empty enrichment map", w.Body)
	}
	if strings.Contains(w.Body.String(), "10.96.0.10") {
		t.Errorf("body = %s, must never echo the resolver's internals", w.Body)
	}
}

// TestMTRSnapshotGetWithoutEnrichNeverResolves: resolving costs DNS queries
// and mmdb lookups, so it must stay gated on the caller actually asking.
func TestMTRSnapshotGetWithoutEnrichNeverResolves(t *testing.T) {
	st := newFakeMTRStore()
	id := st.addSnapshot("node-a", "node-b", sampleHops(), time.Now().UTC())
	res := &fakeEnricher{}
	s := newM5TestServer(t, "viewer", Deps{MTR: st, Enricher: res})

	w := doRequest(t, s, http.MethodGet, "/api/v1/mtr/snapshots/"+id, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", w.Code, w.Body)
	}
	res.mu.Lock()
	defer res.mu.Unlock()
	if len(res.lastIPs) != 0 {
		t.Errorf("resolver consulted without ?enrich=true: %v", res.lastIPs)
	}
}

// Enrichment is decoration, and its absence must never change a status code.
func TestMTRSnapshotGetEnrichFallsBackToTheCacheWhenNoResolverIsWired(t *testing.T) {
	st := newFakeMTRStore()
	id := st.addSnapshot("node-a", "node-b", sampleHops(), time.Now().UTC())
	st.enr["10.0.0.1"] = store.Enrichment{IP: "10.0.0.1", RDNS: "cache.example.", ResolvedAt: time.Now().UTC()}
	s := newM5TestServer(t, "viewer", Deps{MTR: st})
	if s.enricher != nil {
		t.Fatal("Deps.Enricher was not set; Server.enricher must stay a genuine nil interface")
	}

	w := doRequest(t, s, http.MethodGet, "/api/v1/mtr/snapshots/"+id+"?enrich=true", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", w.Code, w.Body)
	}
	var got mtrSnapshotDetailResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Enrichment["10.0.0.1"].RDNS != "cache.example." {
		t.Errorf("enrichment[10.0.0.1] = %+v, want the cached row served through the fallback", got.Enrichment["10.0.0.1"])
	}
}

/* ── the traces behind a route ───────────────────────────────────────────── */

/*
 * The owner, at a route row reading "147 traces": «а как их посмотреть???». The path history folds
 * every trace that walked one path into a single row, and the hop table it shows is the LAST reading
 * folded into it. The traces themselves were in check_results the whole time, each with its own
 * clock and its own RTTs, and nothing in the console led to them.
 */

// mtrTrace builds a stored check_results row carrying an mtr payload with the given hops.
func mtrTrace(id int64, at time.Time, hops []store.PathHop) store.RunResult {
	type hop struct {
		Number    int     `json:"number"`
		IP        string  `json:"ip"`
		Hostname  string  `json:"hostname,omitempty"`
		RTT       int64   `json:"rtt"`
		LossRatio float64 `json:"lossRatio"`
	}
	payload := struct {
		Type    string `json:"type"`
		Details struct {
			Hops []hop `json:"hops"`
		} `json:"details"`
	}{Type: "mtr"}
	for i := range hops {
		payload.Details.Hops = append(payload.Details.Hops, hop{
			Number: hops[i].Number, IP: hops[i].IP, Hostname: hops[i].Hostname,
			RTT: hops[i].RTTNs, LossRatio: hops[i].LossRatio,
		})
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return store.RunResult{
		ID: id, RunID: uuid.NewString(), SourceNode: "node-a", DestinationNode: "node-b",
		Success: true, DurationNs: 2_000_000, Result: raw, RecordedAt: at,
	}
}

func TestMTRSnapshotTracesReturnsTheTracesThatWalkedThatRoute(t *testing.T) {
	st := newFakeMTRStore()
	hops := []store.PathHop{{Number: 1, IP: "10.0.0.1", RTTNs: 1_000_000}}
	base := time.Date(2026, 8, 11, 14, 20, 0, 0, time.UTC)
	id := st.addSnapshot("node-a", "node-b", hops, base)
	snap := st.snaps[id]
	snap.FirstSeen = base.Add(-time.Minute)
	snap.LastSeen = base.Add(time.Minute)
	snap.TraceCount = 3
	st.snaps[id] = snap
	st.traces = []store.RunResult{
		mtrTrace(3, base.Add(30*time.Second), hops),
		mtrTrace(2, base, hops),
		mtrTrace(1, base.Add(-30*time.Second), hops),
	}

	s := newM5TestServer(t, "viewer", Deps{MTR: st})
	w := doRequest(t, s, http.MethodGet, "/api/v1/mtr/snapshots/"+id+"/traces", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", w.Code, w.Body)
	}
	var body mtrTracesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body)
	}
	if len(body.Traces) != 3 {
		t.Fatalf("traces = %d, want 3: %s", len(body.Traces), w.Body)
	}
	// Each trace carries its OWN clock — that is the whole point of listing them.
	if !body.Traces[0].RecordedAt.Equal(base.Add(30 * time.Second)) {
		t.Errorf("first trace recordedAt = %v, want the newest", body.Traces[0].RecordedAt)
	}
	if len(body.Traces[0].Hops) == 0 || !strings.Contains(string(body.Traces[0].Hops), "10.0.0.1") {
		t.Errorf("trace hops = %s, want the trace's own hop list", body.Traces[0].Hops)
	}
	/* The window handed to the store is the SNAPSHOT's, not the whole table's — with a small grace
	   on the leading edge for routes stored before the projection started stamping them with the
	   result row's own clock, whose first trace is otherwise a few hundred microseconds outside
	   their own window. */
	if !st.lastTraces.From.Equal(snap.FirstSeen.Add(-traceWindowGrace)) || !st.lastTraces.To.Equal(snap.LastSeen) {
		t.Errorf("filter window = %v..%v, want the snapshot's own %v..%v (less the grace)",
			st.lastTraces.From, st.lastTraces.To, snap.FirstSeen, snap.LastSeen)
	}
}

// Two routes can interleave in time — a flapping hop alternates between them — so the window alone
// is not the answer. A trace listed under a route it did not walk would be a confident lie.
func TestMTRSnapshotTracesExcludesTracesOfAnotherRouteInTheSameWindow(t *testing.T) {
	st := newFakeMTRStore()
	mine := []store.PathHop{{Number: 1, IP: "10.0.0.1", RTTNs: 1_000_000}}
	theirs := []store.PathHop{{Number: 1, IP: "10.0.0.9", RTTNs: 1_000_000}}
	base := time.Date(2026, 8, 11, 14, 20, 0, 0, time.UTC)
	id := st.addSnapshot("node-a", "node-b", mine, base)
	snap := st.snaps[id]
	snap.FirstSeen, snap.LastSeen = base.Add(-time.Minute), base.Add(time.Minute)
	st.snaps[id] = snap
	st.traces = []store.RunResult{
		mtrTrace(2, base, theirs),
		mtrTrace(1, base.Add(-10*time.Second), mine),
	}

	s := newM5TestServer(t, "viewer", Deps{MTR: st})
	w := doRequest(t, s, http.MethodGet, "/api/v1/mtr/snapshots/"+id+"/traces", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", w.Code, w.Body)
	}
	var body mtrTracesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Traces) != 1 || !strings.Contains(string(body.Traces[0].Hops), "10.0.0.1") {
		t.Fatalf("traces = %s, want only the one that walked THIS route", w.Body)
	}
	// And the reader is told the window held more, so "1 of 2" cannot read as loss.
	if body.Scanned != 2 {
		t.Errorf("scanned = %d, want 2 — the count before the path filter", body.Scanned)
	}
}

func TestMTRSnapshotTracesUnknownSnapshotIs404(t *testing.T) {
	s := newM5TestServer(t, "viewer", Deps{MTR: newFakeMTRStore()})
	for _, id := range []string{uuid.NewString(), "not-a-uuid"} {
		w := doRequest(t, s, http.MethodGet, "/api/v1/mtr/snapshots/"+id+"/traces", nil, nil)
		if w.Code != http.StatusNotFound {
			t.Errorf("GET traces for %q = %d, want 404: %s", id, w.Code, w.Body)
		}
	}
}

func TestMTRSnapshotTracesStoreFailureReturns502(t *testing.T) {
	st := newFakeMTRStore()
	id := st.addSnapshot("node-a", "node-b", []store.PathHop{{Number: 1, IP: "10.0.0.1"}}, time.Now())
	st.tracesErr = errors.New("pq: connection reset, dsn=postgres://secret")

	s := newM5TestServer(t, "viewer", Deps{MTR: st})
	w := doRequest(t, s, http.MethodGet, "/api/v1/mtr/snapshots/"+id+"/traces", nil, nil)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "secret") {
		t.Errorf("driver error leaked: %s", w.Body)
	}
}

// A route can outlive the traces behind it: they live in check_results and age out with the RUN
// sweep, not the path-history one. An empty list for a non-zero traceCount is the honest answer.
func TestMTRSnapshotTracesEmptyIsAnArray(t *testing.T) {
	st := newFakeMTRStore()
	id := st.addSnapshot("node-a", "node-b", []store.PathHop{{Number: 1, IP: "10.0.0.1"}}, time.Now())

	s := newM5TestServer(t, "viewer", Deps{MTR: st})
	w := doRequest(t, s, http.MethodGet, "/api/v1/mtr/snapshots/"+id+"/traces", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"traces":[]`) {
		t.Errorf("body = %s, want an empty ARRAY", w.Body)
	}
}

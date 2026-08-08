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

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/enrich"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// fakeMTRStore is one double for MTRService: the three path-snapshot reads
// plus the enrichment cache read, mutex-guarded like fakeChecksStore for the
// same -race reason (the audit drain and the handler run on different
// goroutines).
//
// It records the LAST filter ListPathSnapshots was called with, which is how
// the limit-clamp and required-filter tests assert what the handler actually
// asked the store for rather than merely what came back.
type fakeMTRStore struct {
	mu sync.Mutex

	dests []store.MTRDestination
	snaps map[string]store.PathSnapshot
	order []string // snapshot ids, oldest first, so listing order is stable
	enr   map[string]store.Enrichment

	lastFilter store.SnapshotFilter
	lastIPs    []string

	destsErr error
	listErr  error
	getErr   error
	enrErr   error
}

func newFakeMTRStore() *fakeMTRStore {
	return &fakeMTRStore{
		snaps: map[string]store.PathSnapshot{},
		enr:   map[string]store.Enrichment{},
	}
}

func (f *fakeMTRStore) ListMTRDestinations(context.Context) ([]store.MTRDestination, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.destsErr != nil {
		return nil, f.destsErr
	}
	out := make([]store.MTRDestination, len(f.dests))
	copy(out, f.dests)
	return out, nil
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

// newM5TestServer wires a Server whose subject holds the given BUILT-IN role,
// using the real compiled-in role sets -- newChecksTestServer's pattern, so
// these tests prove mtr:read/annotations:read/annotations:write land where M5
// Task 3 actually put them (Decision 11: viewer READS both), not where a
// synthetic test role would.
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

// TestMTRRoutesAreReadableByViewer is M5 Decision 11 in test form: path
// history is TELEMETRY, so the role auth.mode=anonymous defaults to reads it.
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

// Both filters are REQUIRED: an unfiltered snapshot listing has no UI and no
// bound (plan Task 4). The 422 detail must name both parameters, or the
// caller cannot tell which one it forgot.
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

// ?enrich=true with NO Deps.Enricher is cache-only: the map holds exactly the
// hop addresses already in mtr_hop_enrichment, keyed by IP, and a miss is
// simply an absent key -- never an error. This is the shape a console with
// mtr.enrichment.enabled=false serves, which is the default.
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

// An enrichment cache failure must NEVER fail the trace itself (M5 Decision
// 5's degradation rule): the snapshot still answers 200, with the field
// present and empty.
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

// This assertion is the whole point of Task 5's seam decision: an
// *enrich.Resolver IS an EnrichmentReader, so swapping the cache-only read for
// a resolving one is ONE Deps field and mtr.go's handler is untouched. If it
// stops compiling, the seam has drifted and the wiring in cmd/console is the
// thing that would otherwise break at runtime, in production. It lives in the
// TEST file on purpose: httpapi must not import enrich, or the dependency
// would point the wrong way -- the HTTP layer defines the seam, the resolver
// satisfies it.
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

// TestMTRSnapshotGetEnrichPrefersTheResolver: with Deps.Enricher wired, the
// hop addresses go to the RESOLVER and the store's cache read is not called
// directly at all -- the resolver owns the whole resolve-then-write-back
// cycle, and a handler that also read the cache would be racing it.
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

// TestMTRSnapshotGetEnrichResolverFailureStillServesTheTrace: the handler's
// degradation contract is unchanged by the swap. A resolver that errors is
// exactly as harmless as a cache that errors -- 200, empty map, no internals
// in the body.
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

// TestMTRSnapshotGetEnrichFallsBackToTheCacheWhenNoResolverIsWired pins the
// nil-Enricher case: with mtr.enrichment.enabled false (or no database), the
// route keeps Task 4's cache-only behaviour rather than answering 503 or
// dropping the field. Enrichment is decoration, and its absence must never
// change a status code.
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

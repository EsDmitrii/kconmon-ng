package checks_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/checks"
	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
	"github.com/EsDmitrii/kconmon-ng/internal/console/ws"
)

// projectedAt is the fixed observation time the projector tests hand in, so
// SeenAt is an assertion rather than a moving target.
var projectedAt = time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

// testRunID is a real UUID: PathSnapshotInput.Validate parses RunID, so a
// placeholder like "run-1" would make every round-trip-through-Validate
// assertion fail for a reason that has nothing to do with the projection.
const testRunID = "6f1b2c3d-4e5a-4b7c-8d9e-0a1b2c3d4e5f"

func mtrSpec() *checks.Spec {
	return &checks.Spec{Type: "mtr", Plane: "pod", Timeout: 5 * time.Second}
}

func mtrPair(src, dst string) *checks.Pair {
	return &checks.Pair{Source: src, Destination: checks.NodeDestination(dst)}
}

// mtrResultJSON builds the bytes the runner actually stores; written as a literal rather than
// marshalled from model types so the test pins the WIRE shape the projector has to parse.
func mtrResultJSON(hops string) json.RawMessage {
	return json.RawMessage(`{"type":"mtr","success":true,"source":"n1","destination":"n2",` +
		`"duration":5000000,"timestamp":"2026-08-07T12:00:00Z",` +
		`"details":{"target":"10.0.0.2","hops":[` + hops + `]}}`)
}

func hop(number int, ip string, rttNs int64, loss float64) string {
	b, err := json.Marshal(map[string]any{"number": number, "ip": ip, "rtt": rttNs, "lossRatio": loss})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// TestProjectMTRSnapshotBuildsInputFromStoredResult is the happy path.
func TestProjectMTRSnapshotBuildsInputFromStoredResult(t *testing.T) {
	raw := json.RawMessage(`{"type":"mtr","success":true,"details":{"target":"10.0.0.2","hops":[` +
		`{"number":1,"ip":"10.0.0.254","hostname":"gw.local","rtt":500000,"lossRatio":0},` +
		`{"number":2,"ip":"10.0.0.2","rtt":2000000,"lossRatio":0.25}]}}`)

	in, ok := checks.ProjectMTRSnapshot(mtrSpec(), mtrPair("node-a", "node-b"), raw, projectedAt, testRunID)
	if !ok {
		t.Fatal("ProjectMTRSnapshot returned false for a well-formed mtr result")
	}
	if in.SourceNode != "node-a" {
		t.Errorf("SourceNode = %q, want node-a", in.SourceNode)
	}
	if in.Destination != "node-b" {
		t.Errorf("Destination = %q, want node-b", in.Destination)
	}
	if !in.SeenAt.Equal(projectedAt) {
		t.Errorf("SeenAt = %v, want %v", in.SeenAt, projectedAt)
	}
	if in.RunID != testRunID {
		t.Errorf("RunID = %q, want %q", in.RunID, testRunID)
	}

	want := []store.PathHop{
		{Number: 1, IP: "10.0.0.254", Hostname: "gw.local", RTTNs: 500000, LossRatio: 0},
		{Number: 2, IP: "10.0.0.2", RTTNs: 2000000, LossRatio: 0.25},
	}
	if len(in.Hops) != len(want) {
		t.Fatalf("Hops = %+v, want %+v", in.Hops, want)
	}
	for i := range want {
		if in.Hops[i] != want[i] {
			t.Errorf("Hops[%d] = %+v, want %+v", i, in.Hops[i], want[i])
		}
	}
	if in.PathHash != store.HashPath(want) {
		t.Errorf("PathHash = %q, want %q (the hash of the projected hops)", in.PathHash, store.HashPath(want))
	}
	// Validate is the store's own gate AND the cross-check that the hash the
	// projector supplied really describes these hops.
	if err := in.Validate(); err != nil {
		t.Errorf("the projected input does not pass store.PathSnapshotInput.Validate: %v", err)
	}
}

// TestProjectMTRSnapshotDropsUnreachableHopsPreservingNumbering pins the one normalization decision
// the projector makes; a hop that never answered carries no address ("*" from
// internal/checker/mtr.go, or "" from any other producer) and cannot be part of a route's identity.
func TestProjectMTRSnapshotDropsUnreachableHopsPreservingNumbering(t *testing.T) {
	raw := mtrResultJSON(
		hop(1, "10.0.0.254", 500000, 0) + "," +
			hop(2, "10.0.1.1", 900000, 0) + "," +
			hop(3, "*", 0, 1) + "," +
			hop(4, "", 0, 1) + "," +
			hop(5, "10.0.0.2", 2000000, 0))

	in, ok := checks.ProjectMTRSnapshot(mtrSpec(), mtrPair("node-a", "node-b"), raw, projectedAt, testRunID)
	if !ok {
		t.Fatal("ProjectMTRSnapshot returned false for a trace with reachable hops")
	}
	gotNumbers := make([]int, len(in.Hops))
	gotIPs := make([]string, len(in.Hops))
	for i := range in.Hops {
		gotNumbers[i] = in.Hops[i].Number
		gotIPs[i] = in.Hops[i].IP
	}
	wantNumbers := []int{1, 2, 5}
	wantIPs := []string{"10.0.0.254", "10.0.1.1", "10.0.0.2"}
	for i := range wantNumbers {
		if i >= len(gotNumbers) || gotNumbers[i] != wantNumbers[i] || gotIPs[i] != wantIPs[i] {
			t.Fatalf("hops = %v %v, want numbers %v ips %v (silent hops dropped, survivors NOT renumbered)",
				gotNumbers, gotIPs, wantNumbers, wantIPs)
		}
	}
	if len(gotNumbers) != len(wantNumbers) {
		t.Fatalf("hops = %v, want exactly %v", gotNumbers, wantNumbers)
	}
}

// TestProjectMTRSnapshotHashIsStableAcrossJitterAndSilentHops is the property the whole dedupe
// rests on.
func TestProjectMTRSnapshotHashIsStableAcrossJitterAndSilentHops(t *testing.T) {
	first := mtrResultJSON(
		hop(1, "10.0.0.254", 500000, 0) + "," +
			hop(2, "10.0.1.1", 900000, 0) + "," +
			hop(3, "10.0.0.2", 2000000, 0))
	// Same routers, wildly different RTTs, a hostname where there was none,
	// and one extra silent hop wedged in between.
	second := mtrResultJSON(
		`{"number":1,"ip":"10.0.0.254","hostname":"gw.local","rtt":41000000,"lossRatio":0.5},` +
			hop(2, "10.0.1.1", 77000000, 0) + "," +
			hop(3, "*", 0, 1) + "," +
			hop(4, "10.0.0.2", 88000000, 0))

	a, ok := checks.ProjectMTRSnapshot(mtrSpec(), mtrPair("node-a", "node-b"), first, projectedAt, testRunID)
	if !ok {
		t.Fatal("first trace did not project")
	}
	b, ok := checks.ProjectMTRSnapshot(mtrSpec(), mtrPair("node-a", "node-b"), second, projectedAt, testRunID)
	if !ok {
		t.Fatal("second trace did not project")
	}
	if a.PathHash != b.PathHash {
		t.Errorf("PathHash differs across two traces of the same route: %q vs %q "+
			"(rtt jitter, rDNS and a silent hop must not change a route's identity)", a.PathHash, b.PathHash)
	}

	// ... and a genuinely different route hashes differently. Same set of
	// addresses, visited in a different order: a different route.
	reordered := mtrResultJSON(
		hop(1, "10.0.1.1", 900000, 0) + "," +
			hop(2, "10.0.0.254", 500000, 0) + "," +
			hop(3, "10.0.0.2", 2000000, 0))
	c, ok := checks.ProjectMTRSnapshot(mtrSpec(), mtrPair("node-a", "node-b"), reordered, projectedAt, testRunID)
	if !ok {
		t.Fatal("reordered trace did not project")
	}
	if c.PathHash == a.PathHash {
		t.Error("a route visiting the same routers in a different order hashed the same -- HashPath must be order-sensitive")
	}
}

// TestProjectMTRSnapshotRejects enumerates everything that must NOT become a
// path-history row. Every one of these is an ordinary, expected outcome of the
// runner's ingest path, not an error: false simply means "nothing to project".
func TestProjectMTRSnapshotRejects(t *testing.T) {
	tests := []struct {
		name string
		spec *checks.Spec
		raw  json.RawMessage
		why  string
	}{
		{
			name: "non-mtr spec",
			spec: &checks.Spec{Type: "icmp"},
			raw:  mtrResultJSON(hop(1, "10.0.0.2", 100, 0)),
			why:  "only an mtr run produces a path",
		},
		{
			name: "empty result json",
			spec: mtrSpec(),
			raw:  nil,
			why:  "a pair whose dispatch failed carries no result at all",
		},
		{
			name: "the runner's empty-object placeholder",
			spec: mtrSpec(),
			raw:  json.RawMessage(`{}`),
			why:  "runOne normalizes an empty resultJSON to {} for the NOT NULL column",
		},
		{
			name: "unparseable json",
			spec: mtrSpec(),
			raw:  json.RawMessage(`{"type":"mtr","details":`),
			why:  "a truncated payload is not a trace",
		},
		{
			name: "details is not an mtr details block",
			spec: mtrSpec(),
			raw:  json.RawMessage(`{"type":"mtr","details":[{"name":"api-prod"}]}`),
			why:  "an external result's Details is a slice; it decodes to nothing here",
		},
		{
			name: "result type contradicts the spec",
			spec: mtrSpec(),
			raw:  json.RawMessage(`{"type":"icmp","details":{"hops":[{"number":1,"ip":"10.0.0.2"}]}}`),
			why:  "a payload that names a non-mtr type is not an mtr trace whatever the spec said",
		},
		{
			name: "no hops at all",
			spec: mtrSpec(),
			raw:  mtrResultJSON(""),
			why:  "a hopless trace has no path to identify",
		},
		{
			name: "every hop silent",
			spec: mtrSpec(),
			raw:  mtrResultJSON(hop(1, "*", 0, 1) + "," + hop(2, "*", 0, 1)),
			why:  "normalization drops them all, leaving nothing to hash",
		},
		{
			name: "hop addresses are whitespace only",
			spec: mtrSpec(),
			raw:  mtrResultJSON(hop(1, "   ", 0, 1)),
			why:  "a blank address is no address",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := checks.ProjectMTRSnapshot(tc.spec, mtrPair("node-a", "node-b"), tc.raw, projectedAt, testRunID); ok {
				t.Errorf("ProjectMTRSnapshot returned true; want false: %s", tc.why)
			}
		})
	}
}

// TestProjectMTRSnapshotNilSpecOrPairIsFalse: the projector is called from a
// hot path and must not panic its caller's goroutine (which, in the runner, is
// one pair's dispatch) on a programming error.
func TestProjectMTRSnapshotNilSpecOrPairIsFalse(t *testing.T) {
	raw := mtrResultJSON(hop(1, "10.0.0.2", 100, 0))
	if _, ok := checks.ProjectMTRSnapshot(nil, mtrPair("a", "b"), raw, projectedAt, testRunID); ok {
		t.Error("nil spec projected true")
	}
	if _, ok := checks.ProjectMTRSnapshot(mtrSpec(), nil, raw, projectedAt, testRunID); ok {
		t.Error("nil pair projected true")
	}
}

func TestProjectMTRSnapshotUsesDestinationLabelNeverAddress(t *testing.T) {
	raw := mtrResultJSON(hop(1, "10.0.0.2", 100, 0))

	named := &checks.Pair{Source: "node-a", Destination: checks.Destination{
		Kind: checks.DestKindTarget, Name: "api-prod", Address: "203.0.113.7",
	}}
	in, ok := checks.ProjectMTRSnapshot(mtrSpec(), named, raw, projectedAt, testRunID)
	if !ok {
		t.Fatal("a target destination did not project")
	}
	if in.Destination != "api-prod" {
		t.Errorf("Destination = %q, want the target NAME api-prod, never the address", in.Destination)
	}

	adhoc := &checks.Pair{Source: "node-a", Destination: checks.Destination{
		Kind: checks.DestKindAdhoc, Address: "203.0.113.7",
	}}
	in, ok = checks.ProjectMTRSnapshot(mtrSpec(), adhoc, raw, projectedAt, testRunID)
	if !ok {
		t.Fatal("an adhoc destination did not project")
	}
	if in.Destination != "203.0.113.7" {
		t.Errorf("Destination = %q, want Label's documented address fallback for an unnamed adhoc destination", in.Destination)
	}
}

// ---------------------------------------------------------------------------
// The runner hook
// ---------------------------------------------------------------------------

// mtrFakeCtrl answers every Diagnose with a fixed body (or a fixed error); it stands in for the
// HTTP fake the rest of runner_test.go uses because these tests need control over the RESULT
// PAYLOAD.
type mtrFakeCtrl struct {
	body json.RawMessage
	err  error
}

func (f *mtrFakeCtrl) Topology(context.Context) (*controllerclient.Topology, error) {
	return &controllerclient.Topology{}, nil
}

func (f *mtrFakeCtrl) Diagnose(context.Context, controllerclient.DiagnoseRequest, time.Duration) (json.RawMessage, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.body, nil
}

// recordingSnapshotStore is a checks.Store that records every UpsertPathSnapshot call and can be
// made to fail them; it EMBEDS a *checks.MemoryStore rather than reimplementing the seam.
type recordingSnapshotStore struct {
	*checks.MemoryStore

	mu    sync.Mutex
	calls []store.PathSnapshotInput
	// ctxLive records, per call, whether the context handed in was still live
	// -- the terminal-op discipline this write shares with UpsertRunResult.
	ctxLive []bool
	err     error
}

func newRecordingSnapshotStore() *recordingSnapshotStore {
	return &recordingSnapshotStore{MemoryStore: checks.NewMemoryStore()}
}

func (s *recordingSnapshotStore) UpsertPathSnapshot(ctx context.Context, in store.PathSnapshotInput) (store.PathSnapshot, bool, error) { //nolint:gocritic // hugeParam: matches store.PathSnapshotStore's own signature
	s.mu.Lock()
	s.calls = append(s.calls, in)
	s.ctxLive = append(s.ctxLive, ctx.Err() == nil)
	failWith := s.err
	s.mu.Unlock()
	if failWith != nil {
		return store.PathSnapshot{}, false, failWith
	}
	return s.MemoryStore.UpsertPathSnapshot(ctx, in)
}

func (s *recordingSnapshotStore) recorded() []store.PathSnapshotInput {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]store.PathSnapshotInput, len(s.calls))
	copy(out, s.calls)
	return out
}

func (s *recordingSnapshotStore) ctxWasLive() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]bool, len(s.ctxLive))
	copy(out, s.ctxLive)
	return out
}

// mtrRunner wires a Runner over a fake controller that always answers with
// body, plus the recording snapshot store and a metrics registry the test can
// read back.
func mtrRunner(t *testing.T, body json.RawMessage, dispatchErr error) (*checks.Runner, *recordingSnapshotStore, *metrics.Metrics) {
	t.Helper()
	m := metrics.New("kconmon_ng_test", prometheus.NewRegistry())
	bus := cache.NewInProcessBus()
	hub := ws.NewHub(bus, m)
	st := newRecordingSnapshotStore()
	return checks.NewRunner(&mtrFakeCtrl{body: body, err: dispatchErr}, hub, bus, st, m), st, m
}

func runMTRPair(t *testing.T, runner *checks.Runner, st *recordingSnapshotStore, checkType string) string {
	t.Helper()
	spec := checks.Spec{
		Sources: []string{"node-a"}, Destinations: []string{"node-b"},
		Type: checkType, Plane: "pod", Timeout: 2 * time.Second,
	}
	id, err := runner.Start(context.Background(), spec, testInitiator())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForTerminal(t, st, id)
	return id
}

// TestRunnerProjectsMTRResultIntoPathHistory is the hook's happy path.
func TestRunnerProjectsMTRResultIntoPathHistory(t *testing.T) {
	body := mtrResultJSON(hop(1, "10.0.0.254", 500000, 0) + "," + hop(2, "10.0.0.2", 2000000, 0))
	runner, st, m := mtrRunner(t, body, nil)

	id := runMTRPair(t, runner, st, "mtr")

	calls := st.recorded()
	if len(calls) != 1 {
		t.Fatalf("UpsertPathSnapshot called %d times, want 1", len(calls))
	}
	got := calls[0]
	if got.SourceNode != "node-a" || got.Destination != "node-b" {
		t.Errorf("snapshot pair = %q -> %q, want node-a -> node-b", got.SourceNode, got.Destination)
	}
	if got.RunID != id {
		t.Errorf("snapshot RunID = %q, want the run's own id %q", got.RunID, id)
	}
	if len(got.Hops) != 2 {
		t.Errorf("snapshot has %d hops, want 2", len(got.Hops))
	}
	if got.SeenAt.IsZero() {
		t.Error("snapshot SeenAt is zero -- the store would reject it")
	}
	if live := st.ctxWasLive(); len(live) != 1 || !live[0] {
		t.Error("UpsertPathSnapshot's ctx was already Done -- it must run on a context derived from context.WithoutCancel, like the result write it follows")
	}
	if v := testutil.ToFloat64(m.MTRSnapshots.WithLabelValues("new-path")); v != 1 {
		t.Errorf("MTRSnapshots(new-path) = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.MTRSnapshots.WithLabelValues("repeat")); v != 0 {
		t.Errorf("MTRSnapshots(repeat) = %v, want 0", v)
	}
}

// TestRunnerCountsARepeatedRouteAsRepeat: the same route traced twice is one row and one new-path
// increment; this is the property that makes new-path an alerting primitive.
func TestRunnerCountsARepeatedRouteAsRepeat(t *testing.T) {
	body := mtrResultJSON(hop(1, "10.0.0.254", 500000, 0) + "," + hop(2, "10.0.0.2", 2000000, 0))
	runner, st, m := mtrRunner(t, body, nil)

	runMTRPair(t, runner, st, "mtr")
	runMTRPair(t, runner, st, "mtr")

	if n := len(st.recorded()); n != 2 {
		t.Fatalf("UpsertPathSnapshot called %d times, want 2", n)
	}
	if v := testutil.ToFloat64(m.MTRSnapshots.WithLabelValues("new-path")); v != 1 {
		t.Errorf("MTRSnapshots(new-path) = %v, want 1 (only the first trace is a new path)", v)
	}
	if v := testutil.ToFloat64(m.MTRSnapshots.WithLabelValues("repeat")); v != 1 {
		t.Errorf("MTRSnapshots(repeat) = %v, want 1", v)
	}
}

// TestRunnerSnapshotFailureDoesNotFailThePair is the projection's cardinal rule.
func TestRunnerSnapshotFailureDoesNotFailThePair(t *testing.T) {
	body := mtrResultJSON(hop(1, "10.0.0.254", 500000, 0) + "," + hop(2, "10.0.0.2", 2000000, 0))
	runner, st, m := mtrRunner(t, body, nil)
	st.err = errors.New("induced path snapshot failure")

	id := runMTRPair(t, runner, st, "mtr")

	run, err := runner.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if run.Status != "succeeded" {
		t.Errorf("run.Status = %q, want succeeded -- a projection failure must never fail the pair", run.Status)
	}
	if run.PairOK != 1 || run.PairFailed != 0 {
		t.Errorf("pair counts = ok:%d failed:%d, want ok:1 failed:0", run.PairOK, run.PairFailed)
	}
	results, err := runner.GetResults(context.Background(), id)
	if err != nil {
		t.Fatalf("GetResults: %v", err)
	}
	if len(results) != 1 || !results[0].Success {
		t.Errorf("results = %+v, want one successful result row (the authority is unaffected)", results)
	}
	if v := testutil.ToFloat64(m.MTRSnapshots.WithLabelValues("error")); v != 1 {
		t.Errorf("MTRSnapshots(error) = %v, want 1", v)
	}
}

// TestRunnerDoesNotProjectNonMTRRuns: the hook is on every pair's ingest path,
// so the cheap gate matters -- a tcp run must not touch path history at all.
func TestRunnerDoesNotProjectNonMTRRuns(t *testing.T) {
	runner, st, m := mtrRunner(t, json.RawMessage(`{"type":"tcp","success":true}`), nil)

	runMTRPair(t, runner, st, "tcp")

	if n := len(st.recorded()); n != 0 {
		t.Errorf("UpsertPathSnapshot called %d times for a tcp run, want 0", n)
	}
	for _, result := range []string{"new-path", "repeat", "error"} {
		if v := testutil.ToFloat64(m.MTRSnapshots.WithLabelValues(result)); v != 0 {
			t.Errorf("MTRSnapshots(%s) = %v, want 0", result, v)
		}
	}
}

// TestRunnerDoesNotProjectAFailedMTRDispatch; counting it on the error label would make a
// controller outage look like a path-history outage.
func TestRunnerDoesNotProjectAFailedMTRDispatch(t *testing.T) {
	runner, st, m := mtrRunner(t, nil, errors.New("induced dispatch failure"))

	runMTRPair(t, runner, st, "mtr")

	if n := len(st.recorded()); n != 0 {
		t.Errorf("UpsertPathSnapshot called %d times for a failed dispatch, want 0", n)
	}
	if v := testutil.ToFloat64(m.MTRSnapshots.WithLabelValues("error")); v != 0 {
		t.Errorf("MTRSnapshots(error) = %v, want 0 -- a pair with no result is nothing to project, not a projection failure", v)
	}
}

// ---------------------------------------------------------------------------
// MemoryStore parity
// ---------------------------------------------------------------------------

func memSnapshotInput(hops []store.PathHop, seenAt time.Time) store.PathSnapshotInput {
	return store.PathSnapshotInput{
		SourceNode: "node-a", Destination: "node-b",
		Hops: hops, SeenAt: seenAt, RunID: testRunID,
	}
}

// TestMemoryStoreUpsertPathSnapshotDedupesLikeTheDatabase pins the parity the database-disabled
// mode depends on.
func TestMemoryStoreUpsertPathSnapshotDedupesLikeTheDatabase(t *testing.T) {
	m := checks.NewMemoryStore()
	ctx := context.Background()
	first := []store.PathHop{{Number: 1, IP: "10.0.0.254", RTTNs: 500000}, {Number: 2, IP: "10.0.0.2", RTTNs: 2000000}}
	// Same route, different RTTs: the payload must stay the first trace's.
	second := []store.PathHop{{Number: 1, IP: "10.0.0.254", RTTNs: 9000000}, {Number: 2, IP: "10.0.0.2", RTTNs: 9000000}}
	t0 := projectedAt
	t1 := projectedAt.Add(time.Hour)

	snapA, isNew, err := m.UpsertPathSnapshot(ctx, memSnapshotInput(first, t0))
	if err != nil {
		t.Fatalf("first UpsertPathSnapshot: %v", err)
	}
	if !isNew {
		t.Error("first trace on a route reported isNew=false, want true")
	}
	if snapA.TraceCount != 1 || !snapA.FirstSeen.Equal(t0) || !snapA.LastSeen.Equal(t0) {
		t.Errorf("first snapshot = count %d first %v last %v, want 1/%v/%v",
			snapA.TraceCount, snapA.FirstSeen, snapA.LastSeen, t0, t0)
	}
	if snapA.HopCount != 2 {
		t.Errorf("HopCount = %d, want 2", snapA.HopCount)
	}

	snapB, isNew, err := m.UpsertPathSnapshot(ctx, memSnapshotInput(second, t1))
	if err != nil {
		t.Fatalf("second UpsertPathSnapshot: %v", err)
	}
	if isNew {
		t.Error("a repeat of the same route reported isNew=true, want false")
	}
	if snapB.ID != snapA.ID {
		t.Errorf("repeat produced a new row id %q, want the existing %q", snapB.ID, snapA.ID)
	}
	if snapB.TraceCount != 2 {
		t.Errorf("TraceCount = %d, want 2", snapB.TraceCount)
	}
	if !snapB.FirstSeen.Equal(t0) {
		t.Errorf("FirstSeen = %v, want the untouched %v", snapB.FirstSeen, t0)
	}
	if !snapB.LastSeen.Equal(t1) {
		t.Errorf("LastSeen = %v, want the bumped %v", snapB.LastSeen, t1)
	}
	var storedHops []store.PathHop
	if err := json.Unmarshal(snapB.Hops, &storedHops); err != nil {
		t.Fatalf("decode stored hops: %v", err)
	}
	if len(storedHops) != 2 || storedHops[0].RTTNs != 500000 {
		t.Errorf("stored hops = %+v, want the FIRST trace's payload", storedHops)
	}
}

// TestMemoryStoreUpsertPathSnapshotNewRouteIsANewRow: a changed hop list is a
// different path_hash and therefore a different row -- the "route changed"
// observable, in memory too.
func TestMemoryStoreUpsertPathSnapshotNewRouteIsANewRow(t *testing.T) {
	m := checks.NewMemoryStore()
	ctx := context.Background()

	if _, isNew, err := m.UpsertPathSnapshot(ctx, memSnapshotInput(
		[]store.PathHop{{Number: 1, IP: "10.0.0.254"}, {Number: 2, IP: "10.0.0.2"}}, projectedAt)); err != nil || !isNew {
		t.Fatalf("first: isNew=%v err=%v, want true/nil", isNew, err)
	}
	_, isNew, err := m.UpsertPathSnapshot(ctx, memSnapshotInput(
		[]store.PathHop{{Number: 1, IP: "10.0.9.9"}, {Number: 2, IP: "10.0.0.2"}}, projectedAt))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !isNew {
		t.Error("a route through a different first hop reported isNew=false, want true")
	}
}

func TestMemoryStoreUpsertPathSnapshotValidatesLikeTheDatabase(t *testing.T) {
	m := checks.NewMemoryStore()
	ctx := context.Background()

	if _, _, err := m.UpsertPathSnapshot(ctx, memSnapshotInput(nil, projectedAt)); err == nil {
		t.Error("an empty hop list was accepted, want an error")
	}
	if _, _, err := m.UpsertPathSnapshot(ctx, memSnapshotInput(
		[]store.PathHop{{Number: 1, IP: "10.0.0.2"}}, time.Time{})); err == nil {
		t.Error("a zero SeenAt was accepted, want an error")
	}
	bad := memSnapshotInput([]store.PathHop{{Number: 1, IP: "10.0.0.2"}}, projectedAt)
	bad.PathHash = "not-this-hop-list's-hash"
	if _, _, err := m.UpsertPathSnapshot(ctx, bad); err == nil {
		t.Error("a path hash that does not describe the hops was accepted, want an error")
	}
}

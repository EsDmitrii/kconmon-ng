//go:build integration

package store_test

// TestPathSnapshot* / TestMTRDestinations* / TestEnrichment* require a real PostgreSQL.
// Run: docker run --rm -d -p 5432:5432 -e POSTGRES_PASSWORD=test -e POSTGRES_DB=kconmon postgres:17-alpine
// Then: KCONMON_TEST_DATABASE_DSN='postgres://postgres:test@127.0.0.1:5432/kconmon?sslmode=disable' \
//       go test -tags=integration ./internal/console/store/... -v

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// newMTRDB opens a *store.DB with migrations applied, dropping and re-creating
// the schema first -- same convention as newTargetsDB; this file shares one
// database with every other file in package store_test, so each test must
// leave it clean.
func newMTRDB(t *testing.T) (*store.DB, string) {
	t.Helper()
	dsn := testDSN(t)
	dropSchema(t, dsn)
	t.Cleanup(func() { dropSchema(t, dsn) })

	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()

	db, err := store.Open(ctx, dsn, 5, connectTimeout, true)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(db.Close)
	return db, dsn
}

// pathAB and pathAC are two distinct routes to the same destination: same
// first hop, different second one, which is what a real route change looks
// like.
func pathAB() []store.PathHop {
	return []store.PathHop{
		{Number: 1, IP: "10.0.0.1", Hostname: "gw-a", RTTNs: 1_000_000},
		{Number: 2, IP: "10.0.0.2", Hostname: "gw-b", RTTNs: 2_000_000, LossRatio: 0.25},
	}
}

func pathAC() []store.PathHop {
	return []store.PathHop{
		{Number: 1, IP: "10.0.0.1", Hostname: "gw-a", RTTNs: 1_000_000},
		{Number: 2, IP: "10.0.0.3", Hostname: "gw-c", RTTNs: 3_000_000},
	}
}

func snapshotInput(source, destination string, hops []store.PathHop, seenAt time.Time) store.PathSnapshotInput {
	return store.PathSnapshotInput{
		SourceNode:  source,
		Destination: destination,
		Hops:        hops,
		SeenAt:      seenAt,
	}
}

// TestUpsertPathSnapshotDedupesTheSamePath is Decision 2's observable, checked
// in both directions against a real server. The same route twice must produce
// ONE row whose trace_count reached 2, with the second call reporting isNew ==
// false; a different route to the same destination must produce a second row
// reporting isNew == true. Everything downstream -- the "when did the route
// change?" alert, the changes timeline, the whole reason this table is
// content-hashed -- rests on those two booleans being right.
func TestUpsertPathSnapshotDedupesTheSamePath(t *testing.T) {
	db, _ := newMTRDB(t)
	ctx := context.Background()

	first := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	second := first.Add(30 * time.Minute)

	created, isNew, err := db.UpsertPathSnapshot(ctx, snapshotInput("node-a", "edge-gw", pathAB(), first))
	if err != nil {
		t.Fatalf("first UpsertPathSnapshot: %v", err)
	}
	if !isNew {
		t.Error("first UpsertPathSnapshot reported isNew = false, want true: nothing was in the table")
	}
	if created.ID == "" {
		t.Fatal("UpsertPathSnapshot: ID is empty, want a minted UUID")
	}
	if created.TraceCount != 1 {
		t.Errorf("first UpsertPathSnapshot: TraceCount = %d, want 1", created.TraceCount)
	}
	if created.HopCount != 2 {
		t.Errorf("first UpsertPathSnapshot: HopCount = %d, want 2", created.HopCount)
	}
	if created.PathHash != store.HashPath(pathAB()) {
		t.Errorf("first UpsertPathSnapshot: PathHash = %q, want the hops' hash", created.PathHash)
	}
	if !created.FirstSeen.Equal(first) || !created.LastSeen.Equal(first) {
		t.Errorf("first UpsertPathSnapshot: first/last seen = %v/%v, want both %v",
			created.FirstSeen, created.LastSeen, first)
	}

	repeat, isNew, err := db.UpsertPathSnapshot(ctx, snapshotInput("node-a", "edge-gw", pathAB(), second))
	if err != nil {
		t.Fatalf("repeat UpsertPathSnapshot: %v", err)
	}
	if isNew {
		t.Error("repeat UpsertPathSnapshot reported isNew = true, want false: the path was already known")
	}
	if repeat.ID != created.ID {
		t.Errorf("repeat UpsertPathSnapshot minted a new row %s, want the existing %s", repeat.ID, created.ID)
	}
	if repeat.TraceCount != 2 {
		t.Errorf("repeat UpsertPathSnapshot: TraceCount = %d, want 2", repeat.TraceCount)
	}
	if !repeat.FirstSeen.Equal(first) {
		t.Errorf("repeat UpsertPathSnapshot moved FirstSeen to %v, want it pinned at %v", repeat.FirstSeen, first)
	}
	if !repeat.LastSeen.Equal(second) {
		t.Errorf("repeat UpsertPathSnapshot: LastSeen = %v, want %v", repeat.LastSeen, second)
	}

	changed, isNew, err := db.UpsertPathSnapshot(ctx,
		snapshotInput("node-a", "edge-gw", pathAC(), second.Add(time.Minute)))
	if err != nil {
		t.Fatalf("changed-path UpsertPathSnapshot: %v", err)
	}
	if !isNew {
		t.Error("a different route reported isNew = false, want true")
	}
	if changed.ID == created.ID {
		t.Error("a different route landed on the existing row, want a second one")
	}

	page, err := db.ListPathSnapshots(ctx, store.SnapshotFilter{SourceNode: "node-a", Destination: "edge-gw"})
	if err != nil {
		t.Fatalf("ListPathSnapshots: %v", err)
	}
	if len(page.Snapshots) != 2 {
		t.Fatalf("the pair has %d snapshot rows, want exactly 2 (three traces, two routes)", len(page.Snapshots))
	}
}

// TestUpsertPathSnapshotKeepsTheFirstTracesHops pins migration 00005's
// "hops is the FIRST trace at this path" claim: a repeat over the same route
// must not overwrite the stored measurements, or the row would describe
// whichever trace happened last rather than a stable sample of the route.
func TestUpsertPathSnapshotKeepsTheFirstTracesHops(t *testing.T) {
	db, _ := newMTRDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if _, _, err := db.UpsertPathSnapshot(ctx, snapshotInput("node-a", "edge-gw", pathAB(), now)); err != nil {
		t.Fatalf("first UpsertPathSnapshot: %v", err)
	}

	// Same IPs (so the same path hash), wildly different measurements.
	later := pathAB()
	later[0].RTTNs = 999_000_000
	later[1].Hostname = "renamed"
	later[1].LossRatio = 1

	repeat, isNew, err := db.UpsertPathSnapshot(ctx,
		snapshotInput("node-a", "edge-gw", later, now.Add(time.Minute)))
	if err != nil {
		t.Fatalf("repeat UpsertPathSnapshot: %v", err)
	}
	if isNew {
		t.Fatal("changing only RTTs and hostnames produced a new path, want the same one (Decision 2)")
	}

	hops, err := store.DecodeHops(repeat.Hops)
	if err != nil {
		t.Fatalf("DecodeHops: %v", err)
	}
	if len(hops) != 2 {
		t.Fatalf("stored hops = %d, want 2", len(hops))
	}
	if hops[0].RTTNs != 1_000_000 || hops[1].Hostname != "gw-b" || hops[1].LossRatio != 0.25 {
		t.Errorf("the repeat overwrote the stored payload: got %+v, want the first trace's", hops)
	}
}

// TestUpsertPathSnapshotNeverWalksLastSeenBackwards is the GREATEST in the
// ON CONFLICT clause, checked. Results arrive from several agents through
// several replicas; a late-delivered older trace must bump trace_count but
// must not make the pair look staler than it is.
func TestUpsertPathSnapshotNeverWalksLastSeenBackwards(t *testing.T) {
	db, _ := newMTRDB(t)
	ctx := context.Background()

	recent := time.Now().UTC().Truncate(time.Microsecond)
	stale := recent.Add(-time.Hour)

	if _, _, err := db.UpsertPathSnapshot(ctx, snapshotInput("node-a", "edge-gw", pathAB(), recent)); err != nil {
		t.Fatalf("first UpsertPathSnapshot: %v", err)
	}
	late, _, err := db.UpsertPathSnapshot(ctx, snapshotInput("node-a", "edge-gw", pathAB(), stale))
	if err != nil {
		t.Fatalf("late UpsertPathSnapshot: %v", err)
	}
	if !late.LastSeen.Equal(recent) {
		t.Errorf("a late older trace moved LastSeen to %v, want it left at %v", late.LastSeen, recent)
	}
	if late.TraceCount != 2 {
		t.Errorf("the late trace was not counted: TraceCount = %d, want 2", late.TraceCount)
	}
}

// TestUpsertPathSnapshotSeparatesPairs asserts the dedupe key really is
// (source, destination, hash) and not the hash alone: two nodes that route
// over the same hops are two separate histories.
func TestUpsertPathSnapshotSeparatesPairs(t *testing.T) {
	db, _ := newMTRDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	a, _, err := db.UpsertPathSnapshot(ctx, snapshotInput("node-a", "edge-gw", pathAB(), now))
	if err != nil {
		t.Fatalf("node-a: %v", err)
	}
	b, isNew, err := db.UpsertPathSnapshot(ctx, snapshotInput("node-b", "edge-gw", pathAB(), now))
	if err != nil {
		t.Fatalf("node-b: %v", err)
	}
	if !isNew || a.ID == b.ID {
		t.Errorf("node-b's identical path landed on node-a's row (isNew=%v): the pair is part of the key", isNew)
	}

	c, isNew, err := db.UpsertPathSnapshot(ctx, snapshotInput("node-a", "other-gw", pathAB(), now))
	if err != nil {
		t.Fatalf("other destination: %v", err)
	}
	if !isNew || c.ID == a.ID {
		t.Errorf("a second destination's identical path landed on the first row (isNew=%v)", isNew)
	}
}

// TestUpsertPathSnapshotRunIDSurvivesTheRunsPrune is migration 00005's
// ON DELETE SET NULL, checked end to end: the retention sweep deletes old
// check_runs, and a snapshot outliving the run that produced it is the point
// of path history. The alternative (CASCADE) would silently delete route
// history along with the run rows, which is the exact data this milestone
// exists to keep.
func TestUpsertPathSnapshotRunIDSurvivesTheRunsPrune(t *testing.T) {
	db, dsn := newMTRDB(t)
	ctx := context.Background()

	runID := uuid.NewString()
	if _, err := db.CreateRun(ctx, runID, "mtr", "pod", json.RawMessage(`{}`), "user", "admin", 1); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	in := snapshotInput("node-a", "edge-gw", pathAB(), time.Now().UTC())
	in.RunID = runID
	snap, _, err := db.UpsertPathSnapshot(ctx, in)
	if err != nil {
		t.Fatalf("UpsertPathSnapshot: %v", err)
	}
	if snap.RunID != runID {
		t.Fatalf("UpsertPathSnapshot: RunID = %q, want %q", snap.RunID, runID)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if _, err = pool.Exec(ctx, `DELETE FROM check_runs WHERE id = $1`, runID); err != nil {
		t.Fatalf("delete the run: %v", err)
	}

	after, err := db.GetPathSnapshot(ctx, snap.ID)
	if err != nil {
		t.Fatalf("GetPathSnapshot after the run was deleted: %v -- ON DELETE SET NULL should have kept the row", err)
	}
	if after.RunID != "" {
		t.Errorf("GetPathSnapshot: RunID = %q, want \"\" (the FK was set null)", after.RunID)
	}
	if after.PathHash != snap.PathHash {
		t.Errorf("the snapshot changed when its run was deleted: %q -> %q", snap.PathHash, after.PathHash)
	}
}

// TestUpsertPathSnapshotRejectsAnUnknownRunID asserts a run_id naming no run
// fails loudly rather than writing a dangling reference.
func TestUpsertPathSnapshotRejectsAnUnknownRunID(t *testing.T) {
	db, _ := newMTRDB(t)
	ctx := context.Background()

	in := snapshotInput("node-a", "edge-gw", pathAB(), time.Now().UTC())
	in.RunID = uuid.NewString()
	if _, _, err := db.UpsertPathSnapshot(ctx, in); err == nil {
		t.Fatal("UpsertPathSnapshot with a run id naming no run succeeded, want a foreign-key error")
	}
}

// TestPathSnapshotInvalidInputNeverReachesTheDatabase asserts validation runs
// before the INSERT: a rejected input leaves no row and produces no raw
// constraint violation.
func TestPathSnapshotInvalidInputNeverReachesTheDatabase(t *testing.T) {
	db, _ := newMTRDB(t)
	ctx := context.Background()

	bad := snapshotInput("node-a", "edge-gw", nil, time.Now().UTC())
	if _, _, err := db.UpsertPathSnapshot(ctx, bad); err == nil {
		t.Fatal("UpsertPathSnapshot with no hops succeeded, want a validation error")
	}
	page, err := db.ListPathSnapshots(ctx, store.SnapshotFilter{})
	if err != nil {
		t.Fatalf("ListPathSnapshots: %v", err)
	}
	if len(page.Snapshots) != 0 {
		t.Errorf("a rejected upsert left %d rows behind", len(page.Snapshots))
	}
}

// TestGetPathSnapshotUnknownIDIsNotFound is the seam's miss contract: a
// well-formed id naming nothing is ErrNotFound, which is what the edge turns
// into a 404.
func TestGetPathSnapshotUnknownIDIsNotFound(t *testing.T) {
	db, _ := newMTRDB(t)

	_, err := db.GetPathSnapshot(context.Background(), uuid.NewString())
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetPathSnapshot(unknown) = %v, want ErrNotFound", err)
	}
}

// TestListMTRDestinationsAggregatesPerPair covers the Explorer's left pane:
// distinct routes counted per pair, traces summed across them, and the pairs
// ordered most-recently-traced first.
func TestListMTRDestinationsAggregatesPerPair(t *testing.T) {
	db, _ := newMTRDB(t)
	ctx := context.Background()

	old := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	recent := time.Now().UTC().Truncate(time.Microsecond)

	// node-a/edge-gw: two routes, three traces, most recently traced.
	for _, in := range []store.PathSnapshotInput{
		snapshotInput("node-a", "edge-gw", pathAB(), old),
		snapshotInput("node-a", "edge-gw", pathAB(), recent),
		snapshotInput("node-a", "edge-gw", pathAC(), recent.Add(-time.Minute)),
	} {
		if _, _, err := db.UpsertPathSnapshot(ctx, in); err != nil {
			t.Fatalf("seed %s: %v", in.Destination, err)
		}
	}
	// node-b/edge-gw: one route, one trace, older.
	if _, _, err := db.UpsertPathSnapshot(ctx, snapshotInput("node-b", "edge-gw", pathAB(), old)); err != nil {
		t.Fatalf("seed node-b: %v", err)
	}

	dests, err := db.ListMTRDestinations(ctx)
	if err != nil {
		t.Fatalf("ListMTRDestinations: %v", err)
	}
	if len(dests) != 2 {
		t.Fatalf("ListMTRDestinations returned %d pairs, want 2: %+v", len(dests), dests)
	}
	if dests[0].SourceNode != "node-a" {
		t.Errorf("ListMTRDestinations[0] = %s, want node-a (most recently traced first)", dests[0].SourceNode)
	}
	if dests[0].SnapshotCount != 2 {
		t.Errorf("node-a SnapshotCount = %d, want 2 distinct routes", dests[0].SnapshotCount)
	}
	if dests[0].TraceCount != 3 {
		t.Errorf("node-a TraceCount = %d, want 3 traces", dests[0].TraceCount)
	}
	if !dests[0].FirstSeen.Equal(old) {
		t.Errorf("node-a FirstSeen = %v, want the oldest trace's %v", dests[0].FirstSeen, old)
	}
	if !dests[0].LastSeen.Equal(recent) {
		t.Errorf("node-a LastSeen = %v, want the newest trace's %v", dests[0].LastSeen, recent)
	}
	if dests[1].SourceNode != "node-b" || dests[1].SnapshotCount != 1 || dests[1].TraceCount != 1 {
		t.Errorf("ListMTRDestinations[1] = %+v, want node-b with one route and one trace", dests[1])
	}
}

// TestListMTRDestinationsEmptyIsAnEmptySlice pins the "no rows is not itself a
// failure" contract every listing in this package gives.
func TestListMTRDestinationsEmptyIsAnEmptySlice(t *testing.T) {
	db, _ := newMTRDB(t)

	dests, err := db.ListMTRDestinations(context.Background())
	if err != nil {
		t.Fatalf("ListMTRDestinations on an empty table: %v", err)
	}
	if dests == nil {
		t.Error("ListMTRDestinations returned a nil slice, want an empty non-nil one")
	}
	if len(dests) != 0 {
		t.Errorf("ListMTRDestinations = %+v, want empty", dests)
	}
}

// hopsAtDepth builds a path whose second hop varies with n, so every n yields
// a distinct path hash and therefore its own snapshot row.
func hopsAtDepth(n int) []store.PathHop {
	return []store.PathHop{
		{Number: 1, IP: "10.0.0.1"},
		{Number: 2, IP: "10.1." + strconv.Itoa(n/256) + "." + strconv.Itoa(n%256)},
	}
}

// TestListPathSnapshotsPagesNewestFirst covers the keyset: a full page hands
// back a cursor, the next page continues strictly after it with no repeats and
// no gaps, and the last page's cursor is empty.
func TestListPathSnapshotsPagesNewestFirst(t *testing.T) {
	db, _ := newMTRDB(t)
	ctx := context.Background()

	const total = 7
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	for i := 0; i < total; i++ {
		in := snapshotInput("node-a", "edge-gw", hopsAtDepth(i), base.Add(time.Duration(i)*time.Minute))
		if _, _, err := db.UpsertPathSnapshot(ctx, in); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	// A snapshot for another pair, which the filter must exclude entirely.
	if _, _, err := db.UpsertPathSnapshot(ctx,
		snapshotInput("node-b", "edge-gw", pathAB(), base.Add(time.Hour))); err != nil {
		t.Fatalf("seed the other pair: %v", err)
	}

	seen := make(map[string]bool, total)
	var last time.Time
	cursor := ""
	pages := 0
	for {
		page, err := db.ListPathSnapshots(ctx, store.SnapshotFilter{
			SourceNode:  "node-a",
			Destination: "edge-gw",
			Cursor:      cursor,
			Limit:       3,
		})
		if err != nil {
			t.Fatalf("ListPathSnapshots(page %d): %v", pages, err)
		}
		pages++
		for _, s := range page.Snapshots {
			if s.SourceNode != "node-a" {
				t.Fatalf("the filter leaked %s into the page", s.SourceNode)
			}
			if seen[s.ID] {
				t.Fatalf("snapshot %s appeared on two pages", s.ID)
			}
			seen[s.ID] = true
			if !last.IsZero() && s.LastSeen.After(last) {
				t.Fatalf("page order broke: %v came after %v", s.LastSeen, last)
			}
			last = s.LastSeen
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if pages > total {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != total {
		t.Errorf("paged through %d snapshots, want %d", len(seen), total)
	}
}

// snapshotIdxSeedRows is how many snapshot rows the index test seeds. Large
// enough that a seq scan is genuinely the more expensive plan for a
// single-pair page, so the index wins on its own merits and no planner knob is
// touched anywhere in this test -- the same reasoning dueIdxSeedRows carries.
const snapshotIdxSeedRows = 20000

// listPathSnapshotsSQL returns the exact SQL text sqlc generated for
// ListPathSnapshots, read out of the generated file at test time.
//
// This is the whole point of the test: EXPLAINing a hand-copied duplicate of
// the query proves nothing, because queries/mtr.sql could then lose its
// "ORDER BY last_seen DESC, id DESC" or grow a clause the index cannot be
// matched against, and every assertion below would keep passing against the
// stale copy. The generated constant is unexported, so it is extracted with
// go/parser rather than referenced -- same technique as listDueSchedulesSQL.
func listPathSnapshotsSQL(t *testing.T) string {
	t.Helper()
	return generatedSQL(t, "gen/mtr.sql.go", "listPathSnapshots")
}

// generatedSQL extracts one unexported string constant from a generated file.
func generatedSQL(t *testing.T, file, ident string) string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if name.Name != ident || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("%s.%s is not a string literal", file, ident)
				}
				sql, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s.%s: %v", file, ident, err)
				}
				return sql
			}
		}
	}
	t.Fatalf("no const %q in %s -- did sqlc's naming change?", ident, file)
	return ""
}

// TestListPathSnapshotsUsesPairSeenIndex asserts the REAL shipped pair browse
// -- store.DB.ListPathSnapshots, not a copy of its SQL -- is answered by
// mtr_snapshots_pair_seen_idx, which is the whole reason that index leads with
// (source_node, destination) and trails with (last_seen DESC, id DESC).
//
// Two independent halves, because neither alone is the full claim (the pattern
// TestListDueSchedulesUsesPartialIndex established):
//
//   - The counter half calls db.ListPathSnapshots and watches
//     pg_stat_user_indexes.idx_scan for the index move. Nothing about the query
//     text is assumed; if the shipped query stopped matching the index the
//     counter would stay put.
//   - The plan half EXPLAINs the SQL extracted from the generated code and
//     asserts the plan names the index AND contains no Sort node. Both trailing
//     index columns are DESC, so ORDER BY last_seen DESC, id DESC is supposed
//     to come out of the scan for free -- an index scan followed by a sort
//     would satisfy the "uses the index" half while quietly breaking the
//     promise that a long-lived pair's history stays cheap to page.
func TestListPathSnapshotsUsesPairSeenIndex(t *testing.T) {
	db, dsn := newMTRDB(t)
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	// One pinned connection for every statement below: stats reads and the
	// snapshot clearing that precedes them have to happen on the same backend.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	// Seeded in a single statement rather than through UpsertPathSnapshot:
	// 20k round trips would dominate the suite's runtime, and the rows only
	// need to be plausible, not created through the public API (which the
	// lifecycle tests above already cover). 200 pairs share the table so the
	// leading index columns have real selectivity to exploit, which is
	// precisely what a seq scan cannot.
	if _, err = conn.Exec(ctx, `
INSERT INTO mtr_path_snapshots (
    id, source_node, destination, path_hash, hop_count, hops, first_seen, last_seen, trace_count
)
SELECT gen_random_uuid(),
       'node-' || (g % 200)::text,
       'edge-gw',
       md5(g::text) || md5((g + 1)::text),
       2,
       '[{"number":1,"ip":"10.0.0.1","rttNs":1000000,"lossRatio":0}]'::jsonb,
       now() - make_interval(secs => $1::int - g),
       now() - make_interval(secs => $1::int - g),
       1
FROM generate_series(1, $1::int) AS g`, snapshotIdxSeedRows); err != nil {
		t.Fatalf("seed %d snapshots: %v", snapshotIdxSeedRows, err)
	}
	if _, err = conn.Exec(ctx, "ANALYZE mtr_path_snapshots"); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}

	// --- half one: the shipped call moves the index's scan counter ---------

	before := idxScans(t, ctx, conn, "mtr_snapshots_pair_seen_idx")

	page, err := db.ListPathSnapshots(ctx, store.SnapshotFilter{
		SourceNode:  "node-7",
		Destination: "edge-gw",
		Limit:       50,
	})
	if err != nil {
		t.Fatalf("ListPathSnapshots: %v", err)
	}
	if len(page.Snapshots) != 50 {
		t.Fatalf("ListPathSnapshots returned %d rows, want 50", len(page.Snapshots))
	}
	for i := 1; i < len(page.Snapshots); i++ {
		if page.Snapshots[i-1].LastSeen.Before(page.Snapshots[i].LastSeen) {
			t.Fatalf("row %d is newer than row %d: %v < %v", i, i-1,
				page.Snapshots[i-1].LastSeen, page.Snapshots[i].LastSeen)
		}
	}

	// The counter does not move the instant the query returns: a backend
	// flushes its pending stats at the end of a command, but no more often
	// than once a second, and this backend already flushed while seeding. The
	// loop nudges it with an unrelated statement (a mtr_path_snapshots_pkey
	// lookup that misses, touching no index this test measures) on each pass;
	// the nudge landing more than a second after the last flush carries the
	// pending counts out with it.
	deadline := time.Now().Add(30 * time.Second)
	var after int64
	for {
		after = idxScans(t, ctx, conn, "mtr_snapshots_pair_seen_idx")
		if after > before {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("mtr_snapshots_pair_seen_idx idx_scan stayed at %d after ListPathSnapshots "+
				"(was %d before): the shipped query is not being answered by the index", after, before)
		}
		time.Sleep(300 * time.Millisecond)
		if _, err = db.GetPathSnapshot(ctx, "0f1d1a2f-6f8e-4a3a-9a0e-7f3f9d0f1c22"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("stats-flush nudge: GetPathSnapshot err = %v, want ErrNotFound", err)
		}
	}

	// --- half two: the plan names the index and carries no sort ------------

	sql := listPathSnapshotsSQL(t)
	if !strings.Contains(sql, "mtr_path_snapshots") {
		t.Fatalf("extracted SQL does not look like the pair browse:\n%s", sql)
	}

	rows, err := conn.Query(ctx, "EXPLAIN\n"+sql, "node-7", "edge-gw", nil, nil, int32(50))
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()

	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("EXPLAIN scan: %v", err)
		}
		plan.WriteString(line)
		plan.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("EXPLAIN rows: %v", err)
	}

	if !strings.Contains(plan.String(), "mtr_snapshots_pair_seen_idx") {
		t.Errorf("the pair browse does not use mtr_snapshots_pair_seen_idx; plan was:\n%s", plan.String())
	}
	if strings.Contains(plan.String(), "Sort") {
		t.Errorf("the pair browse sorts instead of reading ORDER BY last_seen DESC, id DESC out of "+
			"mtr_snapshots_pair_seen_idx; plan was:\n%s", plan.String())
	}
}

// TestEnrichmentRoundTrip covers the TTL cache both ways: a batch write comes
// back keyed by IP, a re-resolve replaces the row rather than inserting a
// second one, and an IP the cache never saw is simply absent.
func TestEnrichmentRoundTrip(t *testing.T) {
	db, _ := newMTRDB(t)
	ctx := context.Background()
	resolved := time.Now().UTC().Truncate(time.Microsecond)

	rows := []store.Enrichment{
		{IP: "10.0.0.1", RDNS: "gw-a.example.test", ASN: 64512, Provider: "Example Transit",
			Geo: json.RawMessage(`{"country":"NL"}`), ResolvedAt: resolved},
		{IP: "2001:db8::1", ResolvedAt: resolved},
	}
	if err := db.PutEnrichment(ctx, rows); err != nil {
		t.Fatalf("PutEnrichment: %v", err)
	}

	got, err := db.GetEnrichment(ctx, []string{"10.0.0.1", "2001:db8::1", "192.0.2.9"})
	if err != nil {
		t.Fatalf("GetEnrichment: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("GetEnrichment returned %d rows, want 2 (the third IP was never cached)", len(got))
	}
	if _, ok := got["192.0.2.9"]; ok {
		t.Error("GetEnrichment invented a row for an uncached IP")
	}

	first := got["10.0.0.1"]
	if first.RDNS != "gw-a.example.test" || first.ASN != 64512 || first.Provider != "Example Transit" {
		t.Errorf("GetEnrichment: %+v, want the written values back", first)
	}
	if !first.ResolvedAt.Equal(resolved) {
		t.Errorf("GetEnrichment: ResolvedAt = %v, want %v", first.ResolvedAt, resolved)
	}
	var geo map[string]string
	if err = json.Unmarshal(first.Geo, &geo); err != nil {
		t.Fatalf("GetEnrichment: geo is not a JSON object: %v", err)
	}
	if geo["country"] != "NL" {
		t.Errorf("GetEnrichment: geo = %v, want country=NL", geo)
	}
	// The bare row's defaults, spelled out: an unresolved-but-cached IP must
	// come back as empty strings and a zero ASN, not as a missing key.
	bare := got["2001:db8::1"]
	if bare.RDNS != "" || bare.ASN != 0 || bare.Provider != "" || string(bare.Geo) != "{}" {
		t.Errorf("GetEnrichment: bare row = %+v, want the column defaults", bare)
	}

	// A re-resolve past the TTL replaces the row.
	later := resolved.Add(time.Hour)
	if err = db.PutEnrichment(ctx, []store.Enrichment{
		{IP: "10.0.0.1", RDNS: "renamed.example.test", ASN: 65000, ResolvedAt: later},
	}); err != nil {
		t.Fatalf("re-resolve PutEnrichment: %v", err)
	}
	got, err = db.GetEnrichment(ctx, []string{"10.0.0.1"})
	if err != nil {
		t.Fatalf("GetEnrichment after the re-resolve: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("the re-resolve produced %d rows for one IP, want 1", len(got))
	}
	if got["10.0.0.1"].RDNS != "renamed.example.test" || got["10.0.0.1"].ASN != 65000 {
		t.Errorf("the re-resolve did not replace the row: %+v", got["10.0.0.1"])
	}
	if !got["10.0.0.1"].ResolvedAt.Equal(later) {
		t.Errorf("ResolvedAt = %v, want it moved to %v -- the TTL depends on it", got["10.0.0.1"].ResolvedAt, later)
	}
}

// TestEnrichmentBatchIsNotMisZipped is the reason PutEnrichment sends one
// array of objects rather than six parallel arrays: with several rows in
// flight, each row's fields must stay with that row. A silent transposition
// here would attribute every hop's provider to its neighbour.
func TestEnrichmentBatchIsNotMisZipped(t *testing.T) {
	db, _ := newMTRDB(t)
	ctx := context.Background()
	resolved := time.Now().UTC().Truncate(time.Microsecond)

	const n = 5
	want := make(map[string]store.Enrichment, n)
	rows := make([]store.Enrichment, n)
	for i := 0; i < n; i++ {
		e := store.Enrichment{
			IP:         "10.0.0." + strconv.Itoa(i+1),
			RDNS:       "host-" + strconv.Itoa(i) + ".example.test",
			ASN:        int64(64512 + i),
			Provider:   "provider-" + strconv.Itoa(i),
			Geo:        json.RawMessage(`{"country":"C` + strconv.Itoa(i) + `"}`),
			ResolvedAt: resolved.Add(time.Duration(i) * time.Second),
		}
		rows[i] = e
		want[e.IP] = e
	}
	if err := db.PutEnrichment(ctx, rows); err != nil {
		t.Fatalf("PutEnrichment: %v", err)
	}

	ips := make([]string, 0, n)
	for ip := range want {
		ips = append(ips, ip)
	}
	got, err := db.GetEnrichment(ctx, ips)
	if err != nil {
		t.Fatalf("GetEnrichment: %v", err)
	}
	for ip, w := range want {
		g, ok := got[ip]
		if !ok {
			t.Errorf("%s is missing from the cache", ip)
			continue
		}
		if g.RDNS != w.RDNS || g.ASN != w.ASN || g.Provider != w.Provider {
			t.Errorf("%s came back as %+v, want %+v -- the batch was mis-zipped", ip, g, w)
		}
		if !g.ResolvedAt.Equal(w.ResolvedAt) {
			t.Errorf("%s ResolvedAt = %v, want %v", ip, g.ResolvedAt, w.ResolvedAt)
		}
	}
}

// TestDeletePathSnapshotsBeforeUsesLastSeen pins which column retention reads:
// a route first observed long ago but still being taken must survive, and one
// that stopped being taken must not. Getting this backwards would delete
// exactly the long-lived routes path history is for.
func TestDeletePathSnapshotsBeforeUsesLastSeen(t *testing.T) {
	db, _ := newMTRDB(t)
	ctx := context.Background()

	ancient := time.Now().UTC().Add(-200 * 24 * time.Hour)
	recent := time.Now().UTC()
	cutoff := time.Now().UTC().Add(-90 * 24 * time.Hour)

	// Old first_seen, recent last_seen: still current, must survive.
	if _, _, err := db.UpsertPathSnapshot(ctx, snapshotInput("node-a", "edge-gw", pathAB(), ancient)); err != nil {
		t.Fatalf("seed the long-lived path: %v", err)
	}
	if _, _, err := db.UpsertPathSnapshot(ctx, snapshotInput("node-a", "edge-gw", pathAB(), recent)); err != nil {
		t.Fatalf("bump the long-lived path: %v", err)
	}
	// Never seen again: must go.
	if _, _, err := db.UpsertPathSnapshot(ctx, snapshotInput("node-a", "edge-gw", pathAC(), ancient)); err != nil {
		t.Fatalf("seed the abandoned path: %v", err)
	}

	n, err := db.DeletePathSnapshotsBefore(ctx, cutoff, 100)
	if err != nil {
		t.Fatalf("DeletePathSnapshotsBefore: %v", err)
	}
	if n != 1 {
		t.Fatalf("DeletePathSnapshotsBefore deleted %d rows, want 1", n)
	}

	page, err := db.ListPathSnapshots(ctx, store.SnapshotFilter{})
	if err != nil {
		t.Fatalf("ListPathSnapshots: %v", err)
	}
	if len(page.Snapshots) != 1 {
		t.Fatalf("%d rows left, want 1", len(page.Snapshots))
	}
	if page.Snapshots[0].PathHash != store.HashPath(pathAB()) {
		t.Error("retention deleted the still-current route and kept the abandoned one")
	}
}

// TestDeleteEnrichmentBeforeUsesResolvedAt is the cache's sweep, same shape.
func TestDeleteEnrichmentBeforeUsesResolvedAt(t *testing.T) {
	db, _ := newMTRDB(t)
	ctx := context.Background()

	stale := time.Now().UTC().Add(-200 * 24 * time.Hour)
	fresh := time.Now().UTC()
	cutoff := time.Now().UTC().Add(-90 * 24 * time.Hour)

	if err := db.PutEnrichment(ctx, []store.Enrichment{
		{IP: "10.0.0.1", ResolvedAt: stale},
		{IP: "10.0.0.2", ResolvedAt: fresh},
	}); err != nil {
		t.Fatalf("PutEnrichment: %v", err)
	}

	n, err := db.DeleteEnrichmentBefore(ctx, cutoff, 100)
	if err != nil {
		t.Fatalf("DeleteEnrichmentBefore: %v", err)
	}
	if n != 1 {
		t.Fatalf("DeleteEnrichmentBefore deleted %d rows, want 1", n)
	}
	got, err := db.GetEnrichment(ctx, []string{"10.0.0.1", "10.0.0.2"})
	if err != nil {
		t.Fatalf("GetEnrichment: %v", err)
	}
	if _, ok := got["10.0.0.1"]; ok {
		t.Error("the stale cache row survived the sweep")
	}
	if _, ok := got["10.0.0.2"]; !ok {
		t.Error("the fresh cache row was swept")
	}
}

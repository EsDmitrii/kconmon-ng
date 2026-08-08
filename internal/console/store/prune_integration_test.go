//go:build integration

package store_test

// TestPruneOnce* / TestRun* require a real PostgreSQL.
// Run: docker run --rm -d -p 5432:5432 -e POSTGRES_PASSWORD=test -e POSTGRES_DB=kconmon postgres:17-alpine
// Then: KCONMON_TEST_DATABASE_DSN='postgres://postgres:test@127.0.0.1:5432/kconmon?sslmode=disable' \
//       go test -tags=integration ./internal/console/store/... -v

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

const retention90d = 90 * 24 * time.Hour

// newPrunerDB opens a *store.DB with migrations applied, dropping and
// re-creating the schema first -- same convention as newEventStoreDB
// (events_integration_test.go); this file shares one database with every
// other file in package store_test, so each test must leave it clean.
func newPrunerDB(t *testing.T) (*store.DB, string) {
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

// seedTopologyEventsAtAges inserts one row per element of agesDays: row i has
// event_time = now - agesDays[i] days and event_seq = seqBase+i. It bulk-inserts
// via UNNEST in a single round trip rather than one store.EventStore.InsertEvent
// call per row, since the batch-crossing tests below seed thousands of rows and
// a round trip each would make the suite slow.
func seedTopologyEventsAtAges(t *testing.T, dsn string, seqBase int64, agesDays []int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("seedTopologyEventsAtAges: connect: %v", err)
	}
	defer pool.Close()

	n := len(agesDays)
	seqs := make([]int64, n)
	times := make([]time.Time, n)
	now := time.Now()
	for i, age := range agesDays {
		seqs[i] = seqBase + int64(i)
		times[i] = now.Add(-time.Duration(age) * 24 * time.Hour)
	}

	_, err = pool.Exec(ctx, `
		INSERT INTO topology_events (event_seq, event_time, type, severity, scope, summary, details)
		SELECT seq, ts, 'check_observed', 'info', 'seed', 'seed', '{}'::jsonb
		FROM UNNEST($1::bigint[], $2::timestamptz[]) AS u(seq, ts)
	`, seqs, times)
	if err != nil {
		t.Fatalf("seedTopologyEventsAtAges: insert: %v", err)
	}
}

// countTopologyEvents returns the current row count of topology_events.
func countTopologyEvents(t *testing.T, dsn string) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("countTopologyEvents: connect: %v", err)
	}
	defer pool.Close()

	var n int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM topology_events`).Scan(&n); err != nil {
		t.Fatalf("countTopologyEvents: query: %v", err)
	}
	return n
}

// agesSpanning200Days returns 10 ages just inside a 90d retention window and
// 10 just outside it, with plenty of clearance on both sides of the cutoff so
// DeleteTopologyEventsBefore's strict "event_time < cutoff" never has to
// resolve a boundary tie.
func agesSpanning200Days() []int {
	ages := make([]int, 0, 20)
	for d := 1; d <= 10; d++ {
		ages = append(ages, d) // well inside 90d: kept
	}
	for d := 191; d <= 200; d++ {
		ages = append(ages, d) // well past 90d: deleted
	}
	return ages
}

// TestPruneOnceDeletesRowsPastRetention seeds 20 rows spanning roughly 200
// days and asserts a 90d-retention PruneOnce deletes exactly the 10 older
// ones, and that a second sweep against the now-clean table deletes 0.
func TestPruneOnceDeletesRowsPastRetention(t *testing.T) {
	db, dsn := newPrunerDB(t)
	p := store.NewPruner(db, retention90d, newTestMetrics())
	seedTopologyEventsAtAges(t, dsn, 1, agesSpanning200Days())

	ctx := context.Background()

	deleted, err := p.PruneOnce(ctx)
	if err != nil {
		t.Fatalf("PruneOnce: %v", err)
	}
	if got := deleted["topology_events"]; got != 10 {
		t.Fatalf("PruneOnce: deleted[topology_events] = %d, want 10", got)
	}
	if got := countTopologyEvents(t, dsn); got != 10 {
		t.Fatalf("rows remaining after PruneOnce = %d, want 10", got)
	}

	second, err := p.PruneOnce(ctx)
	if err != nil {
		t.Fatalf("second PruneOnce: %v", err)
	}
	if got := second["topology_events"]; got != 0 {
		t.Fatalf("second PruneOnce: deleted[topology_events] = %d, want 0", got)
	}
}

// TestPruneOnceSweepsEveryTable is the whole sweep list in one call: each of
// the six tables gets one row past retention and one inside it, and PruneOnce
// must delete exactly the expired one from each and report it under that
// table's own key. Seeding through the public store API rather than raw SQL is
// deliberate -- it is what makes the test also prove the sweeps are pointed at
// the columns the writers actually populate.
func TestPruneOnceSweepsEveryTable(t *testing.T) {
	db, dsn := newPrunerDB(t)
	p := store.NewPruner(db, retention90d, newTestMetrics())
	ctx := context.Background()

	expired := time.Now().UTC().Add(-200 * 24 * time.Hour)
	current := time.Now().UTC()

	// topology_events (by event_time) and audit_log are covered by the other
	// tests in this file; seed topology_events here too so the returned map
	// carries a non-zero entry for it as well.
	seedTopologyEventsAtAges(t, dsn, 1, []int{200, 1})

	// check_runs (by created_at). created_at has a column DEFAULT of now(), so
	// the expired run is aged with one direct UPDATE -- the store API has no
	// way to backdate a run, and inventing one for a test would be worse.
	oldRun := uuid.NewString()
	if _, err := db.CreateRun(ctx, oldRun, "mtr", "pod", json.RawMessage(`{}`), "user", "admin", 1); err != nil {
		t.Fatalf("CreateRun(old): %v", err)
	}
	if _, err := db.CreateRun(ctx, uuid.NewString(), "mtr", "pod", json.RawMessage(`{}`), "user", "admin", 1); err != nil {
		t.Fatalf("CreateRun(new): %v", err)
	}
	pool, poolErr := pgxpool.New(ctx, dsn)
	if poolErr != nil {
		t.Fatalf("connect: %v", poolErr)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `UPDATE check_runs SET created_at = $1 WHERE id = $2`, expired, oldRun); err != nil {
		t.Fatalf("backdate the old run: %v", err)
	}

	// mtr_path_snapshots (by last_seen).
	for _, tc := range []struct {
		hops []store.PathHop
		at   time.Time
	}{{pathAB(), expired}, {pathAC(), current}} {
		if _, _, err := db.UpsertPathSnapshot(ctx, snapshotInput("node-a", "edge-gw", tc.hops, tc.at)); err != nil {
			t.Fatalf("UpsertPathSnapshot: %v", err)
		}
	}

	// mtr_hop_enrichment (by resolved_at).
	if err := db.PutEnrichment(ctx, []store.Enrichment{
		{IP: "10.0.0.1", ResolvedAt: expired},
		{IP: "10.0.0.2", ResolvedAt: current},
	}); err != nil {
		t.Fatalf("PutEnrichment: %v", err)
	}

	// annotations (by start_at).
	for _, at := range []time.Time{expired, current} {
		if _, err := db.CreateAnnotation(ctx, store.AnnotationInput{
			StartAt: at, Text: "mark", CreatedBy: "user:admin",
		}); err != nil {
			t.Fatalf("CreateAnnotation: %v", err)
		}
	}

	deleted, err := p.PruneOnce(ctx)
	if err != nil {
		t.Fatalf("PruneOnce: %v", err)
	}

	for table, want := range map[string]int64{
		"topology_events":    1,
		"check_runs":         1,
		"mtr_path_snapshots": 1,
		"mtr_hop_enrichment": 1,
		"annotations":        1,
	} {
		got, ok := deleted[table]
		if !ok {
			t.Errorf("PruneOnce reported no entry for %s: the sweep is missing from the list", table)
			continue
		}
		if got != want {
			t.Errorf("PruneOnce: deleted[%s] = %d, want %d", table, got, want)
		}
	}
	// audit_log has no expired rows here, but its sweep must still have run
	// and reported zero rather than being absent.
	if _, ok := deleted["audit_log"]; !ok {
		t.Error("PruneOnce reported no entry for audit_log")
	}
	if len(deleted) != 6 {
		t.Errorf("PruneOnce reported %d tables, want 6: %v", len(deleted), deleted)
	}

	// The survivors, read back through the store rather than counted in SQL.
	snaps, err := db.ListPathSnapshots(ctx, store.SnapshotFilter{})
	if err != nil {
		t.Fatalf("ListPathSnapshots: %v", err)
	}
	if len(snaps.Snapshots) != 1 || !snaps.Snapshots[0].LastSeen.Equal(current.Truncate(time.Microsecond)) {
		t.Errorf("after the sweep %d snapshots remain, want just the current one", len(snaps.Snapshots))
	}
	cache, err := db.GetEnrichment(ctx, []string{"10.0.0.1", "10.0.0.2"})
	if err != nil {
		t.Fatalf("GetEnrichment: %v", err)
	}
	if _, gone := cache["10.0.0.1"]; gone {
		t.Error("the expired enrichment row survived the sweep")
	}
	if _, kept := cache["10.0.0.2"]; !kept {
		t.Error("the fresh enrichment row was swept")
	}
	anns, err := db.ListAnnotations(ctx, store.AnnotationFilter{})
	if err != nil {
		t.Fatalf("ListAnnotations: %v", err)
	}
	if len(anns.Annotations) != 1 {
		t.Errorf("after the sweep %d annotations remain, want 1", len(anns.Annotations))
	}
}

// TestPruneOnceCrossesBatchBoundary seeds 12000 rows, all past retention --
// more than pruneBatchSize (5000) -- and asserts one PruneOnce call still
// deletes every one of them, proving the internal batch loop keeps issuing
// DELETEs until a batch comes back short rather than stopping after the
// first 5000.
func TestPruneOnceCrossesBatchBoundary(t *testing.T) {
	db, dsn := newPrunerDB(t)
	p := store.NewPruner(db, retention90d, newTestMetrics())

	const n = 12000
	ages := make([]int, n)
	for i := range ages {
		ages[i] = 200 // comfortably past the 90d cutoff
	}
	seedTopologyEventsAtAges(t, dsn, 1, ages)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	deleted, err := p.PruneOnce(ctx)
	if err != nil {
		t.Fatalf("PruneOnce: %v", err)
	}
	if got := deleted["topology_events"]; got != n {
		t.Fatalf("PruneOnce: deleted[topology_events] = %d, want %d", got, n)
	}
	if got := countTopologyEvents(t, dsn); got != 0 {
		t.Fatalf("rows remaining = %d, want 0", got)
	}
}

// TestPruneOnceConcurrentReplicasDoNotDoubleWork is the multi-replica
// guarantee: two *store.DB backed by two independent pgxpool.Pools (modeling
// two console replicas against the same PostgreSQL) call PruneOnce at
// roughly the same time. Exactly one must do the deleting; the other must
// find pruneLockKey already held and return promptly with nothing deleted.
func TestPruneOnceConcurrentReplicasDoNotDoubleWork(t *testing.T) {
	dsn := testDSN(t)
	dropSchema(t, dsn)
	t.Cleanup(func() { dropSchema(t, dsn) })

	openCtx, openCancel := context.WithTimeout(context.Background(), connectTimeout)
	db1, err := store.Open(openCtx, dsn, 5, connectTimeout, true)
	openCancel()
	if err != nil {
		t.Fatalf("Open db1: %v", err)
	}
	t.Cleanup(db1.Close)

	// migrate=false: db1's Open call above already applied every migration,
	// and running goose's own advisory-locked migration path a second time
	// here would just be redundant.
	openCtx2, openCancel2 := context.WithTimeout(context.Background(), connectTimeout)
	db2, err := store.Open(openCtx2, dsn, 5, connectTimeout, false)
	openCancel2()
	if err != nil {
		t.Fatalf("Open db2: %v", err)
	}
	t.Cleanup(db2.Close)

	const n = 12000
	ages := make([]int, n)
	for i := range ages {
		ages[i] = 200
	}
	seedTopologyEventsAtAges(t, dsn, 1, ages)

	p1 := store.NewPruner(db1, retention90d, newTestMetrics())
	p2 := store.NewPruner(db2, retention90d, newTestMetrics())

	type outcome struct {
		deleted map[string]int64
		err     error
	}
	runCtx, runCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer runCancel()

	resultCh := make(chan outcome, 1)
	go func() {
		d, err := p1.PruneOnce(runCtx)
		resultCh <- outcome{d, err}
	}()

	// Give p1 a head start: 12000 rows over a 5000-row batch size means at
	// least one pruneBatchPause between batches, which alone comfortably
	// outlasts this delay, so p2's own pg_try_advisory_lock call below lands
	// while p1 still holds pruneLockKey.
	time.Sleep(30 * time.Millisecond)

	r2, err2 := p2.PruneOnce(runCtx)
	if err2 != nil {
		t.Fatalf("p2.PruneOnce: %v", err2)
	}

	var r1 outcome
	select {
	case r1 = <-resultCh:
	case <-time.After(60 * time.Second):
		t.Fatal("p1.PruneOnce did not complete in time")
	}
	if r1.err != nil {
		t.Fatalf("p1.PruneOnce: %v", r1.err)
	}

	if got := r2["topology_events"]; got != 0 {
		t.Errorf("p2 (the late starter) deleted %d rows, want 0: pruneLockKey should already have been held by p1", got)
	}

	total := r1.deleted["topology_events"] + r2["topology_events"]
	if total != n {
		t.Fatalf("total rows deleted across both replicas = %d, want %d", total, n)
	}
	if got := countTopologyEvents(t, dsn); got != 0 {
		t.Fatalf("rows remaining = %d, want 0", got)
	}
}

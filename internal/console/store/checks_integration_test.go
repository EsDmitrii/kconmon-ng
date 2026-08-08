//go:build integration

package store_test

// TestRunStore* / TestRunReader* / TestPruneOnce*CheckRuns require a real
// PostgreSQL.
// Run: docker run --rm -d -p 5432:5432 -e POSTGRES_PASSWORD=test -e POSTGRES_DB=kconmon postgres:17-alpine
// Then: KCONMON_TEST_DATABASE_DSN='postgres://postgres:test@127.0.0.1:5432/kconmon?sslmode=disable' \
//       go test -tags=integration ./internal/console/store/... -v

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// newChecksDB opens a *store.DB with migrations applied, dropping and
// re-creating the schema first -- same convention as newEventStoreDB /
// newPrunerDB; this file shares one database with every other file in
// package store_test, so each test must leave it clean.
func newChecksDB(t *testing.T) (*store.DB, string) {
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

// mustCreateRun creates a run with a fresh random UUID and a minimal, valid
// spec, and returns it.
func mustCreateRun(t *testing.T, ctx context.Context, db *store.DB, pairTotal int32) store.Run {
	t.Helper()
	id := uuid.NewString()
	run, err := db.CreateRun(ctx, id, "ping", "pod", json.RawMessage(`{"source":"a"}`), "user", "u-1", pairTotal)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return run
}

// TestCreateRunThenMarkStartedThenUpsertResultsThenFinish is the run
// lifecycle's happy path: create (pending) -> mark started (running) ->
// upsert 3 results -> finish (succeeded, with counters), asserting the
// status and counters persist through GetRun after each step.
func TestCreateRunThenMarkStartedThenUpsertResultsThenFinish(t *testing.T) {
	db, _ := newChecksDB(t)
	ctx := context.Background()

	run := mustCreateRun(t, ctx, db, 3)
	if run.Status != "pending" {
		t.Fatalf("CreateRun: status = %q, want pending", run.Status)
	}
	if run.StartedAt != nil || run.FinishedAt != nil {
		t.Fatalf("CreateRun: StartedAt/FinishedAt = %v/%v, want both nil", run.StartedAt, run.FinishedAt)
	}
	if run.PairTotal != 3 || run.PairOK != 0 || run.PairFailed != 0 {
		t.Fatalf("CreateRun: counters = (%d,%d,%d), want (3,0,0)", run.PairTotal, run.PairOK, run.PairFailed)
	}

	if err := db.MarkRunStarted(ctx, run.ID); err != nil {
		t.Fatalf("MarkRunStarted: %v", err)
	}
	started, err := db.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun after MarkRunStarted: %v", err)
	}
	if started.Status != "running" {
		t.Errorf("after MarkRunStarted: status = %q, want running", started.Status)
	}
	if started.StartedAt == nil {
		t.Fatal("after MarkRunStarted: StartedAt is nil, want set")
	}

	pairs := []struct {
		source, dest string
		success      bool
	}{
		{"node-a", "node-b", true},
		{"node-a", "node-c", true},
		{"node-a", "node-d", false},
	}
	for _, p := range pairs {
		res, err := db.UpsertRunResult(ctx, store.RunResultInput{
			RunID:           run.ID,
			SourceNode:      p.source,
			DestinationNode: p.dest,
			Success:         p.success,
			DurationNs:      1_500_000,
			Result:          json.RawMessage(`{"ok":true}`),
		})
		if err != nil {
			t.Fatalf("UpsertRunResult(%s->%s): %v", p.source, p.dest, err)
		}
		if res.RunID != run.ID {
			t.Errorf("UpsertRunResult(%s->%s): RunID = %q, want %q", p.source, p.dest, res.RunID, run.ID)
		}
	}

	results, err := db.GetRunResults(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRunResults: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("GetRunResults: got %d rows, want 3", len(results))
	}

	if err := db.FinishRun(ctx, run.ID, "partial", 2, 1); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	finished, err := db.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun after FinishRun: %v", err)
	}
	if finished.Status != "partial" {
		t.Errorf("after FinishRun: status = %q, want partial", finished.Status)
	}
	if finished.FinishedAt == nil {
		t.Fatal("after FinishRun: FinishedAt is nil, want set")
	}
	if finished.PairOK != 2 || finished.PairFailed != 1 {
		t.Errorf("after FinishRun: counters = (ok=%d,failed=%d), want (2,1)", finished.PairOK, finished.PairFailed)
	}
}

// TestUpsertRunResultTwiceForSamePairOverwrites asserts the retried-pair
// contract: a second UpsertRunResult for the same (run, source, destination)
// updates the existing row's values rather than erroring or creating a
// second row.
func TestUpsertRunResultTwiceForSamePairOverwrites(t *testing.T) {
	db, _ := newChecksDB(t)
	ctx := context.Background()

	run := mustCreateRun(t, ctx, db, 1)

	first, err := db.UpsertRunResult(ctx, store.RunResultInput{
		RunID:           run.ID,
		SourceNode:      "node-a",
		DestinationNode: "node-b",
		Success:         false,
		DurationNs:      5_000_000,
		Error:           "dial timeout",
		Result:          json.RawMessage(`{"attempt":1}`),
	})
	if err != nil {
		t.Fatalf("first UpsertRunResult: %v", err)
	}

	second, err := db.UpsertRunResult(ctx, store.RunResultInput{
		RunID:           run.ID,
		SourceNode:      "node-a",
		DestinationNode: "node-b",
		Success:         true,
		DurationNs:      1_200_000,
		Error:           "",
		Result:          json.RawMessage(`{"attempt":2}`),
	})
	if err != nil {
		t.Fatalf("second UpsertRunResult: %v", err)
	}

	if second.ID != first.ID {
		t.Errorf("second UpsertRunResult: ID = %d, want same row ID %d (overwrite, not a new row)", second.ID, first.ID)
	}
	if !second.Success || second.DurationNs != 1_200_000 || second.Error != "" {
		t.Errorf("second UpsertRunResult: got %+v, want the newer values", second)
	}

	results, err := db.GetRunResults(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRunResults: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("GetRunResults after retried pair: got %d rows, want exactly 1", len(results))
	}
	if !results[0].Success || results[0].DurationNs != 1_200_000 {
		t.Errorf("GetRunResults after retried pair: got %+v, want the newer values", results[0])
	}
}

// TestGetRunNotFoundReturnsErrNotFound asserts GetRun on a well-formed but
// unused UUID reports store.ErrNotFound, not a raw pgx.ErrNoRows.
func TestGetRunNotFoundReturnsErrNotFound(t *testing.T) {
	db, _ := newChecksDB(t)
	ctx := context.Background()

	_, err := db.GetRun(ctx, uuid.NewString())
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRun(unknown id): err = %v, want it to wrap store.ErrNotFound", err)
	}
}

// TestMarkRunStartedNotFoundReturnsErrNotFound mirrors
// TestGetRunNotFoundReturnsErrNotFound for the write path.
func TestMarkRunStartedNotFoundReturnsErrNotFound(t *testing.T) {
	db, _ := newChecksDB(t)
	ctx := context.Background()

	err := db.MarkRunStarted(ctx, uuid.NewString())
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("MarkRunStarted(unknown id): err = %v, want it to wrap store.ErrNotFound", err)
	}
}

// TestFinishRunNotFoundReturnsErrNotFound mirrors
// TestGetRunNotFoundReturnsErrNotFound for FinishRun.
func TestFinishRunNotFoundReturnsErrNotFound(t *testing.T) {
	db, _ := newChecksDB(t)
	ctx := context.Background()

	err := db.FinishRun(ctx, uuid.NewString(), "failed", 0, 1)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("FinishRun(unknown id): err = %v, want it to wrap store.ErrNotFound", err)
	}
}

// TestFinishRunOnPendingRunReturnsErrWrongState asserts FinishRun refuses a
// run that was never started (still "pending", started_at NULL): the
// running->terminal guard on the UPDATE means 0 rows, and the disambiguating
// GetRun lookup finds the run present but in the wrong state.
func TestFinishRunOnPendingRunReturnsErrWrongState(t *testing.T) {
	db, _ := newChecksDB(t)
	ctx := context.Background()

	run := mustCreateRun(t, ctx, db, 1)

	err := db.FinishRun(ctx, run.ID, "succeeded", 1, 0)
	if !errors.Is(err, store.ErrWrongState) {
		t.Fatalf("FinishRun(pending run): err = %v, want it to wrap store.ErrWrongState", err)
	}

	after, gerr := db.GetRun(ctx, run.ID)
	if gerr != nil {
		t.Fatalf("GetRun after failed FinishRun: %v", gerr)
	}
	if after.Status != "pending" {
		t.Errorf("after failed FinishRun: status = %q, want pending (untouched)", after.Status)
	}
	if after.FinishedAt != nil {
		t.Errorf("after failed FinishRun: FinishedAt = %v, want nil (untouched)", after.FinishedAt)
	}
}

// TestFinishRunTwiceReturnsErrWrongStateAndKeepsFirstValues asserts the
// idempotent-retry contract from checks.go's FinishRun doc comment: a second
// FinishRun call on an already-finished run reports ErrWrongState, and the
// row keeps the FIRST call's values -- the second call's (status, counters)
// never land.
func TestFinishRunTwiceReturnsErrWrongStateAndKeepsFirstValues(t *testing.T) {
	db, _ := newChecksDB(t)
	ctx := context.Background()

	run := mustCreateRun(t, ctx, db, 3)
	if err := db.MarkRunStarted(ctx, run.ID); err != nil {
		t.Fatalf("MarkRunStarted: %v", err)
	}

	if err := db.FinishRun(ctx, run.ID, "succeeded", 3, 0); err != nil {
		t.Fatalf("first FinishRun: %v", err)
	}

	err := db.FinishRun(ctx, run.ID, "failed", 0, 3)
	if !errors.Is(err, store.ErrWrongState) {
		t.Fatalf("second FinishRun: err = %v, want it to wrap store.ErrWrongState", err)
	}

	after, gerr := db.GetRun(ctx, run.ID)
	if gerr != nil {
		t.Fatalf("GetRun after second FinishRun: %v", gerr)
	}
	if after.Status != "succeeded" {
		t.Errorf("after second FinishRun: status = %q, want succeeded (first call's value kept)", after.Status)
	}
	if after.PairOK != 3 || after.PairFailed != 0 {
		t.Errorf("after second FinishRun: counters = (ok=%d,failed=%d), want (3,0) (first call's values kept)", after.PairOK, after.PairFailed)
	}
}

// TestMarkRunStartedAfterFinishReturnsErrWrongState asserts MarkRunStarted
// refuses to reopen a finished run: the pending->running guard means 0 rows
// against a terminal-status run, and status stays terminal.
func TestMarkRunStartedAfterFinishReturnsErrWrongState(t *testing.T) {
	db, _ := newChecksDB(t)
	ctx := context.Background()

	run := mustCreateRun(t, ctx, db, 1)
	if err := db.MarkRunStarted(ctx, run.ID); err != nil {
		t.Fatalf("MarkRunStarted: %v", err)
	}
	if err := db.FinishRun(ctx, run.ID, "failed", 0, 1); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	err := db.MarkRunStarted(ctx, run.ID)
	if !errors.Is(err, store.ErrWrongState) {
		t.Fatalf("MarkRunStarted(finished run): err = %v, want it to wrap store.ErrWrongState", err)
	}

	after, gerr := db.GetRun(ctx, run.ID)
	if gerr != nil {
		t.Fatalf("GetRun after reopen attempt: %v", gerr)
	}
	if after.Status != "failed" {
		t.Errorf("after reopen attempt: status = %q, want failed (untouched)", after.Status)
	}
}

// TestCreateRunDuplicateIDReturnsErrAlreadyExists asserts a colliding
// caller-supplied id (the primary key) is reported as store.ErrAlreadyExists,
// not a raw unique-violation PgError.
func TestCreateRunDuplicateIDReturnsErrAlreadyExists(t *testing.T) {
	db, _ := newChecksDB(t)
	ctx := context.Background()

	id := uuid.NewString()
	if _, err := db.CreateRun(ctx, id, "ping", "pod", json.RawMessage(`{}`), "user", "u-1", 1); err != nil {
		t.Fatalf("first CreateRun: %v", err)
	}

	_, err := db.CreateRun(ctx, id, "ping", "pod", json.RawMessage(`{}`), "user", "u-1", 1)
	if !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("second CreateRun(same id): err = %v, want it to wrap store.ErrAlreadyExists", err)
	}
}

// TestListRunsFiltersByCheckTypeAndStatus asserts both filters are exact
// matches, AND-ed together.
func TestListRunsFiltersByCheckTypeAndStatus(t *testing.T) {
	db, _ := newChecksDB(t)
	ctx := context.Background()

	mkRun := func(checkType string, finish string) {
		r, err := db.CreateRun(ctx, uuid.NewString(), checkType, "pod", json.RawMessage(`{}`), "user", "u-1", 1)
		if err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		if err := db.MarkRunStarted(ctx, r.ID); err != nil {
			t.Fatalf("MarkRunStarted: %v", err)
		}
		if err := db.FinishRun(ctx, r.ID, finish, 1, 0); err != nil {
			t.Fatalf("FinishRun: %v", err)
		}
	}
	mkRun("ping", "succeeded")
	mkRun("ping", "failed")
	mkRun("mtr", "succeeded")

	page, err := db.ListRuns(ctx, store.RunFilter{CheckType: "ping", Status: "succeeded"})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(page.Runs) != 1 {
		t.Fatalf("ListRuns(CheckType=ping,Status=succeeded): got %d runs, want 1", len(page.Runs))
	}
	if page.Runs[0].CheckType != "ping" || page.Runs[0].Status != "succeeded" {
		t.Errorf("ListRuns: unexpected run leaked through the filter: %+v", page.Runs[0])
	}
}

// TestListRunsPagesWithoutDuplicatesOrGaps seeds 250 runs and pages through
// them with Limit: 100, asserting 100/100/50 with no duplicate and no
// missing id, and that the final page's NextCursor is empty -- same shape as
// events_integration_test.go's TestListEventsPagesWithoutDuplicatesOrGaps.
func TestListRunsPagesWithoutDuplicatesOrGaps(t *testing.T) {
	db, _ := newChecksDB(t)
	ctx := context.Background()

	const total = 250
	for i := 0; i < total; i++ {
		if _, err := db.CreateRun(ctx, uuid.NewString(), "ping", "pod", json.RawMessage(`{}`), "user", "u-1", 1); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
	}

	var (
		seen      = make(map[string]bool, total)
		pageSizes []int
		cursor    string
	)
	for {
		page, err := db.ListRuns(ctx, store.RunFilter{Limit: 100, Cursor: cursor})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		pageSizes = append(pageSizes, len(page.Runs))
		for _, r := range page.Runs {
			if seen[r.ID] {
				t.Fatalf("ListRuns: duplicate id %s across pages", r.ID)
			}
			seen[r.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if len(pageSizes) > total { // guard against an infinite loop on a bug
			t.Fatal("ListRuns: paging did not terminate")
		}
	}

	if want := []int{100, 100, 50}; len(pageSizes) != len(want) {
		t.Fatalf("page sizes = %v, want %v", pageSizes, want)
	} else {
		for i := range want {
			if pageSizes[i] != want[i] {
				t.Errorf("page %d size = %d, want %d", i, pageSizes[i], want[i])
			}
		}
	}
	if len(seen) != total {
		t.Errorf("saw %d distinct runs across all pages, want %d (gaps present)", len(seen), total)
	}
}

// TestDeleteRunCascadesResults asserts ON DELETE CASCADE: removing a run row
// (via DeleteRunsBefore, the same path the pruner uses) also removes its
// check_results rows, with no orphans left behind.
func TestDeleteRunCascadesResults(t *testing.T) {
	db, dsn := newChecksDB(t)
	ctx := context.Background()

	run := mustCreateRun(t, ctx, db, 1)
	if _, err := db.UpsertRunResult(ctx, store.RunResultInput{
		RunID: run.ID, SourceNode: "a", DestinationNode: "b", Success: true, Result: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("UpsertRunResult: %v", err)
	}

	// A cutoff comfortably in the future so DeleteRunsBefore's "created_at <
	// cutoff" matches this just-created row.
	deleted, err := db.DeleteRunsBefore(ctx, time.Now().Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("DeleteRunsBefore: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("DeleteRunsBefore: deleted = %d, want 1", deleted)
	}

	if _, err := db.GetRun(ctx, run.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRun after delete: err = %v, want store.ErrNotFound", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	var n int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM check_results WHERE run_id = $1`, run.ID).Scan(&n); err != nil {
		t.Fatalf("count check_results: %v", err)
	}
	if n != 0 {
		t.Errorf("check_results rows remaining after run delete = %d, want 0 (cascade)", n)
	}
}

// TestPrunerRemovesOldRunsAndResults is the retention-sweep integration
// point: a run older than the retention horizon (with a result attached) is
// removed by Pruner.PruneOnce, cascading its result away, while a fresh run
// survives.
func TestPrunerRemovesOldRunsAndResults(t *testing.T) {
	db, dsn := newChecksDB(t)
	ctx := context.Background()

	oldRun := mustCreateRun(t, ctx, db, 1)
	if _, err := db.UpsertRunResult(ctx, store.RunResultInput{
		RunID: oldRun.ID, SourceNode: "a", DestinationNode: "b", Success: true, Result: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("UpsertRunResult(old): %v", err)
	}
	freshRun := mustCreateRun(t, ctx, db, 1)

	// Backdate oldRun.created_at well past a 90d retention window; freshRun
	// keeps its real created_at (now), well inside it.
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `UPDATE check_runs SET created_at = now() - interval '200 days' WHERE id = $1`, oldRun.ID); err != nil {
		t.Fatalf("backdate oldRun: %v", err)
	}

	p := store.NewPruner(db, retention90d, newTestMetrics())
	deleted, err := p.PruneOnce(ctx)
	if err != nil {
		t.Fatalf("PruneOnce: %v", err)
	}
	if got := deleted["check_runs"]; got != 1 {
		t.Fatalf("PruneOnce: deleted[check_runs] = %d, want 1", got)
	}

	if _, err := db.GetRun(ctx, oldRun.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRun(oldRun) after prune: err = %v, want store.ErrNotFound", err)
	}
	if _, err := db.GetRun(ctx, freshRun.ID); err != nil {
		t.Fatalf("GetRun(freshRun) after prune: %v, want it to survive", err)
	}

	var n int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM check_results WHERE run_id = $1`, oldRun.ID).Scan(&n); err != nil {
		t.Fatalf("count check_results: %v", err)
	}
	if n != 0 {
		t.Errorf("check_results rows remaining for pruned run = %d, want 0 (cascade)", n)
	}
}

// TestFinishRunAcceptsRunningToCancelled pins the one lifecycle transition M4
// adds: 'cancelled' has been a legal check_runs.status since migration 00003,
// but nothing ever wrote it. FinishRun's `AND status = 'running'` guard must
// accept it exactly like every other terminal status -- and must still refuse
// it from 'pending', so a cancel can never skip the run's own start.
func TestFinishRunAcceptsRunningToCancelled(t *testing.T) {
	db, _ := newChecksDB(t)
	ctx := context.Background()

	run := mustCreateRun(t, ctx, db, 5)
	if err := db.FinishRun(ctx, run.ID, "cancelled", 0, 0); !errors.Is(err, store.ErrWrongState) {
		t.Fatalf("FinishRun(pending -> cancelled) err = %v, want store.ErrWrongState", err)
	}

	if err := db.MarkRunStarted(ctx, run.ID); err != nil {
		t.Fatalf("MarkRunStarted: %v", err)
	}
	if err := db.FinishRun(ctx, run.ID, "cancelled", 2, 1); err != nil {
		t.Fatalf("FinishRun(running -> cancelled): %v", err)
	}

	got, err := db.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Status != "cancelled" {
		t.Errorf("status = %q, want cancelled", got.Status)
	}
	if got.FinishedAt == nil {
		t.Error("FinishedAt is nil, want it stamped")
	}
	if got.PairOK != 2 || got.PairFailed != 1 {
		t.Errorf("pair counts = ok:%d failed:%d, want ok:2 failed:1 (whatever landed before the cancel)", got.PairOK, got.PairFailed)
	}
}

// TestReapStuckRunsForceFinishesOnlyOldRunningRuns is follow-up #6's store
// half: a run left 'running' past the cutoff is force-finished as 'cancelled';
// a healthy running run, a never-started pending run, and an already-terminal
// run are all left untouched. The scheduler that calls this arrives in Task 13
// -- this is only the query.
func TestReapStuckRunsForceFinishesOnlyOldRunningRuns(t *testing.T) {
	db, dsn := newChecksDB(t)
	ctx := context.Background()

	stuck := mustCreateRun(t, ctx, db, 1)
	if err := db.MarkRunStarted(ctx, stuck.ID); err != nil {
		t.Fatalf("MarkRunStarted(stuck): %v", err)
	}
	healthy := mustCreateRun(t, ctx, db, 1)
	if err := db.MarkRunStarted(ctx, healthy.ID); err != nil {
		t.Fatalf("MarkRunStarted(healthy): %v", err)
	}
	pending := mustCreateRun(t, ctx, db, 1)
	finished := mustCreateRun(t, ctx, db, 1)
	if err := db.MarkRunStarted(ctx, finished.ID); err != nil {
		t.Fatalf("MarkRunStarted(finished): %v", err)
	}
	if err := db.FinishRun(ctx, finished.ID, "succeeded", 1, 0); err != nil {
		t.Fatalf("FinishRun(finished): %v", err)
	}

	// Backdate the stuck run AND the already-terminal one past the cutoff:
	// age alone must not be enough to be reaped -- status = 'running' is the
	// other half of the predicate.
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	for _, id := range []string{stuck.ID, pending.ID, finished.ID} {
		if _, err := pool.Exec(ctx, `UPDATE check_runs SET created_at = now() - interval '3 hours' WHERE id = $1`, id); err != nil {
			t.Fatalf("backdate %s: %v", id, err)
		}
	}

	n, err := db.ReapStuckRuns(ctx, time.Now().UTC().Add(-2*time.Hour), 100)
	if err != nil {
		t.Fatalf("ReapStuckRuns: %v", err)
	}
	if n != 1 {
		t.Fatalf("ReapStuckRuns reaped %d rows, want 1", n)
	}

	want := map[string]string{
		stuck.ID: "cancelled", healthy.ID: "running", pending.ID: "pending", finished.ID: "succeeded",
	}
	for id, wantStatus := range want {
		got, err := db.GetRun(ctx, id)
		if err != nil {
			t.Fatalf("GetRun(%s): %v", id, err)
		}
		if got.Status != wantStatus {
			t.Errorf("run %s status = %q, want %q", id, got.Status, wantStatus)
		}
	}

	reaped, err := db.GetRun(ctx, stuck.ID)
	if err != nil {
		t.Fatalf("GetRun(stuck): %v", err)
	}
	if reaped.FinishedAt == nil {
		t.Error("reaped run FinishedAt is nil, want it stamped so the row is genuinely terminal")
	}

	// Idempotent: a second sweep finds nothing left to reap.
	again, err := db.ReapStuckRuns(ctx, time.Now().UTC().Add(-2*time.Hour), 100)
	if err != nil {
		t.Fatalf("second ReapStuckRuns: %v", err)
	}
	if again != 0 {
		t.Errorf("second sweep reaped %d rows, want 0", again)
	}
}

// The reaper's limit bounds one sweep, so a backlog cannot hold a long
// transaction open over thousands of rows -- same posture DeleteRunsBefore's
// own limit takes.
func TestReapStuckRunsHonoursLimit(t *testing.T) {
	db, dsn := newChecksDB(t)
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	for i := 0; i < 5; i++ {
		run := mustCreateRun(t, ctx, db, 1)
		if err := db.MarkRunStarted(ctx, run.ID); err != nil {
			t.Fatalf("MarkRunStarted: %v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE check_runs SET created_at = now() - interval '3 hours' WHERE id = $1`, run.ID); err != nil {
			t.Fatalf("backdate: %v", err)
		}
	}

	n, err := db.ReapStuckRuns(ctx, time.Now().UTC().Add(-2*time.Hour), 2)
	if err != nil {
		t.Fatalf("ReapStuckRuns: %v", err)
	}
	if n != 2 {
		t.Fatalf("reaped %d rows, want 2 (the limit)", n)
	}
}

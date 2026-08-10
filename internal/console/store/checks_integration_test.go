//go:build integration

package store_test

// TestRunStore* / TestRunReader* / TestPruneOnce*CheckRuns require a real PostgreSQL.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// newChecksDB opens a *store.DB with migrations applied, dropping and re-creating the schema first.
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

// TestCreateRunThenMarkStartedThenUpsertResultsThenFinish is the run lifecycle's happy path.
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

// TestUpsertRunResultTwiceForSamePairOverwrites asserts the retried-pair contract.
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

// TestFinishRunOnPendingRunReturnsErrWrongState asserts FinishRun refuses a run that was never
// started (still "pending", started_at NULL).
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

// TestFinishRunTwiceReturnsErrWrongStateAndKeepsFirstValues asserts the idempotent-retry contract
// from checks.go's FinishRun doc comment.
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

// TestListRunsPagesWithoutDuplicatesOrGaps seeds 250 runs and pages through them with Limit: 100.
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

// TestPrunerRemovesOldRunsAndResults is the retention-sweep integration point.
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

// FinishRun's `AND status = 'running'` guard must accept it exactly like every other terminal
// status.
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

// The scheduler that calls this arrives.
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

// ── migration 00010: the pair-count backfill ────────────────────────────────

// upSQLOf reads a migration file and returns the statements between its Up and
// Down markers. The test runs the migration's OWN text rather than a copy of it:
// a copy would keep passing after the file it is meant to pin had drifted.
func upSQLOf(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("migrations", name))
	if err != nil {
		t.Fatalf("read migration %s: %v", name, err)
	}
	body := string(raw)
	start := strings.Index(body, "-- +goose Up")
	end := strings.Index(body, "-- +goose Down")
	if start < 0 || end < 0 || end < start {
		t.Fatalf("migration %s: no Up/Down markers", name)
	}
	return body[start+len("-- +goose Up") : end]
}

// seedSample writes one probe of one pair.
func seedSample(t *testing.T, ctx context.Context, db *store.DB, runID, src, dst string, seq int32, ok bool) {
	t.Helper()
	if _, err := db.UpsertRunResult(ctx, store.RunResultInput{
		RunID: runID, SourceNode: src, DestinationNode: dst,
		Success: ok, DurationNs: 1_000, Result: json.RawMessage(`{}`), SampleSeq: seq,
	}); err != nil {
		t.Fatalf("UpsertRunResult(%s→%s seq %d): %v", src, dst, seq, err)
	}
}

// pairCounts reads one run's two counters straight out of the table, so the
// assertion cannot be satisfied by a projection that fixed things on the way out.
func pairCounts(t *testing.T, ctx context.Context, pool *pgxpool.Pool, runID string) (int32, int32) {
	t.Helper()
	var ok, failed int32
	if err := pool.QueryRow(ctx, `SELECT pair_ok, pair_failed FROM check_runs WHERE id = $1`, runID).Scan(&ok, &failed); err != nil {
		t.Fatalf("read pair counts: %v", err)
	}
	return ok, failed
}

// TestMigration00010RecomputesPairCountsFromLatestSample is QA scope 4, finding
// #2. Before the pair-semantics fix an interval run's FinishRun was handed the
// SAMPLE tallies, so a run over one pair probed twelve times recorded
// pair_ok = 12 against pair_total = 1 and the run list read it back as
// "12/1 successful". The relic rows are still in the database; the migration
// recomputes every terminal run's counters from check_results using the rule the
// runtime uses now -- a pair is OK when its LATEST sample succeeded.
func TestMigration00010RecomputesPairCountsFromLatestSample(t *testing.T) {
	db, dsn := newChecksDB(t)
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	// ── the relic: an interval run over TWO pairs, probed five times between
	// them, finished with the sample tallies in the pair columns.
	relic := mustCreateRun(t, ctx, db, 2)
	if err := db.MarkRunStarted(ctx, relic.ID); err != nil {
		t.Fatalf("MarkRunStarted: %v", err)
	}
	// a→b recovered: its LAST sample is the successful one.
	seedSample(t, ctx, db, relic.ID, "a", "b", 0, false)
	seedSample(t, ctx, db, relic.ID, "a", "b", 1, false)
	seedSample(t, ctx, db, relic.ID, "a", "b", 2, true)
	// a→c broke: its last sample failed, however well it started.
	seedSample(t, ctx, db, relic.ID, "a", "c", 0, true)
	seedSample(t, ctx, db, relic.ID, "a", "c", 1, false)
	if err := db.FinishRun(ctx, relic.ID, "partial", 3, 2); err != nil { // 3 ok + 2 failed SAMPLES
		t.Fatalf("FinishRun: %v", err)
	}

	// ── a run that was always right: one pair, one sample, counted as a pair.
	fine := mustCreateRun(t, ctx, db, 1)
	if err := db.MarkRunStarted(ctx, fine.ID); err != nil {
		t.Fatalf("MarkRunStarted: %v", err)
	}
	seedSample(t, ctx, db, fine.ID, "a", "b", 0, true)
	if err := db.FinishRun(ctx, fine.ID, "succeeded", 1, 0); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	// ── a run still in flight: its counters belong to FinishRun, not to this.
	inFlight := mustCreateRun(t, ctx, db, 4)
	if err := db.MarkRunStarted(ctx, inFlight.ID); err != nil {
		t.Fatalf("MarkRunStarted: %v", err)
	}
	seedSample(t, ctx, db, inFlight.ID, "a", "b", 0, true)
	if _, err := pool.Exec(ctx,
		`UPDATE check_runs SET pair_ok = 7, pair_failed = 7 WHERE id = $1`, inFlight.ID); err != nil {
		t.Fatalf("dirty the in-flight run: %v", err)
	}

	// ── a terminal run that never got a single result back.
	silent := mustCreateRun(t, ctx, db, 3)
	if err := db.MarkRunStarted(ctx, silent.ID); err != nil {
		t.Fatalf("MarkRunStarted: %v", err)
	}
	if err := db.FinishRun(ctx, silent.ID, "cancelled", 5, 0); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	if _, err := pool.Exec(ctx, upSQLOf(t, "00010_backfill_pair_counts.sql")); err != nil {
		t.Fatalf("apply migration 00010: %v", err)
	}

	// The relic now counts PAIRS by their latest sample: a→b ended OK, a→c ended failed.
	if ok, failed := pairCounts(t, ctx, pool, relic.ID); ok != 1 || failed != 1 {
		t.Errorf("relic run: pair_ok/pair_failed = %d/%d, want 1/1", ok, failed)
	}
	if ok, failed := pairCounts(t, ctx, pool, fine.ID); ok != 1 || failed != 0 {
		t.Errorf("already-correct run: pair_ok/pair_failed = %d/%d, want 1/0 (untouched)", ok, failed)
	}
	if ok, failed := pairCounts(t, ctx, pool, inFlight.ID); ok != 7 || failed != 7 {
		t.Errorf("running run: pair_ok/pair_failed = %d/%d, want 7/7 (left to FinishRun)", ok, failed)
	}
	// Not skipped, zeroed: "no pair reported" is a count, and 5 was never one.
	if ok, failed := pairCounts(t, ctx, pool, silent.ID); ok != 0 || failed != 0 {
		t.Errorf("resultless terminal run: pair_ok/pair_failed = %d/%d, want 0/0", ok, failed)
	}
}

// The migration is applied by Open on every start, so running it twice must land
// the same numbers -- and the second pass must not undo the first.
func TestMigration00010IsIdempotent(t *testing.T) {
	db, dsn := newChecksDB(t)
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	run := mustCreateRun(t, ctx, db, 1)
	if err := db.MarkRunStarted(ctx, run.ID); err != nil {
		t.Fatalf("MarkRunStarted: %v", err)
	}
	seedSample(t, ctx, db, run.ID, "a", "b", 0, true)
	seedSample(t, ctx, db, run.ID, "a", "b", 1, false)
	if err := db.FinishRun(ctx, run.ID, "partial", 1, 1); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	up := upSQLOf(t, "00010_backfill_pair_counts.sql")
	for pass := 1; pass <= 2; pass++ {
		if _, err := pool.Exec(ctx, up); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if ok, failed := pairCounts(t, ctx, pool, run.ID); ok != 0 || failed != 1 {
			t.Fatalf("pass %d: pair_ok/pair_failed = %d/%d, want 0/1", pass, ok, failed)
		}
	}
}

// A run recorded BEFORE migration 00009 has one row per pair at the sample_seq
// default of 0, so the recomputation has to reach the right answer through the
// same expression -- the id tiebreak is what makes that true.
func TestMigration00010HandlesPre00009SingleRowRuns(t *testing.T) {
	db, dsn := newChecksDB(t)
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	run := mustCreateRun(t, ctx, db, 2)
	if err := db.MarkRunStarted(ctx, run.ID); err != nil {
		t.Fatalf("MarkRunStarted: %v", err)
	}
	// Exactly what the old unique constraint allowed: one row per pair, seq 0.
	seedSample(t, ctx, db, run.ID, "a", "b", 0, true)
	seedSample(t, ctx, db, run.ID, "a", "c", 0, false)
	if err := db.FinishRun(ctx, run.ID, "partial", 1, 1); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	if _, err := pool.Exec(ctx, upSQLOf(t, "00010_backfill_pair_counts.sql")); err != nil {
		t.Fatalf("apply migration 00010: %v", err)
	}
	if ok, failed := pairCounts(t, ctx, pool, run.ID); ok != 1 || failed != 1 {
		t.Errorf("pre-00009 run: pair_ok/pair_failed = %d/%d, want 1/1", ok, failed)
	}
}

package checks_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/EsDmitrii/kconmon-ng/internal/console/checks"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

func TestMemoryStoreCreateGetRoundTrip(t *testing.T) {
	m := checks.NewMemoryStore()
	ctx := context.Background()

	run, err := m.CreateRun(ctx, "run-1", "tcp", "pod", json.RawMessage(`{"type":"tcp"}`), "user", "u1", 3)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if run.Status != "pending" || run.PairTotal != 3 {
		t.Errorf("created run = %+v, want status=pending pairTotal=3", run)
	}

	got, err := m.GetRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.ID != "run-1" || got.CheckType != "tcp" || got.InitiatorID != "u1" {
		t.Errorf("GetRun = %+v, want matching CreateRun input", got)
	}
}

func TestMemoryStoreCreateRunDuplicateIDIsAlreadyExists(t *testing.T) {
	m := checks.NewMemoryStore()
	ctx := context.Background()
	if _, err := m.CreateRun(ctx, "dup", "tcp", "pod", json.RawMessage(`{}`), "user", "u1", 1); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := m.CreateRun(ctx, "dup", "tcp", "pod", json.RawMessage(`{}`), "user", "u1", 1); !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("err = %v, want ErrAlreadyExists", err)
	}
}

func TestMemoryStoreGetUnknownRunIsNotFound(t *testing.T) {
	m := checks.NewMemoryStore()
	if _, err := m.GetRun(context.Background(), "never-existed"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestMemoryStoreMarkRunStartedLifecycle(t *testing.T) {
	m := checks.NewMemoryStore()
	ctx := context.Background()
	if _, err := m.CreateRun(ctx, "r1", "tcp", "pod", json.RawMessage(`{}`), "user", "u1", 1); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	if err := m.MarkRunStarted(ctx, "r1"); err != nil {
		t.Fatalf("MarkRunStarted: %v", err)
	}
	run, err := m.GetRun(ctx, "r1")
	if err != nil || run.Status != "running" || run.StartedAt == nil {
		t.Fatalf("run after MarkRunStarted = %+v, err=%v, want status=running with StartedAt set", run, err)
	}

	// pending -> running only; a second call on an already-running run is
	// the wrong state.
	if err := m.MarkRunStarted(ctx, "r1"); !errors.Is(err, store.ErrWrongState) {
		t.Fatalf("second MarkRunStarted err = %v, want ErrWrongState", err)
	}

	if err := m.MarkRunStarted(ctx, "never-existed"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("MarkRunStarted(unknown) err = %v, want ErrNotFound", err)
	}
}

func TestMemoryStoreFinishRunLifecycle(t *testing.T) {
	m := checks.NewMemoryStore()
	ctx := context.Background()
	if _, err := m.CreateRun(ctx, "r1", "tcp", "pod", json.RawMessage(`{}`), "user", "u1", 2); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// running required first.
	if err := m.FinishRun(ctx, "r1", "succeeded", 2, 0); !errors.Is(err, store.ErrWrongState) {
		t.Fatalf("FinishRun before start err = %v, want ErrWrongState", err)
	}

	if err := m.MarkRunStarted(ctx, "r1"); err != nil {
		t.Fatalf("MarkRunStarted: %v", err)
	}
	if err := m.FinishRun(ctx, "r1", "partial", 1, 1); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	run, err := m.GetRun(ctx, "r1")
	if err != nil || run.Status != "partial" || run.PairOK != 1 || run.PairFailed != 1 || run.FinishedAt == nil {
		t.Fatalf("run after FinishRun = %+v, err=%v", run, err)
	}

	// A retried FinishRun after the run is already terminal surfaces
	// ErrWrongState -- the caller's documented "treat as already finished"
	// case (store.RunStore.FinishRun's doc comment).
	if err := m.FinishRun(ctx, "r1", "partial", 1, 1); !errors.Is(err, store.ErrWrongState) {
		t.Fatalf("retried FinishRun err = %v, want ErrWrongState", err)
	}

	if err := m.FinishRun(ctx, "never-existed", "failed", 0, 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("FinishRun(unknown) err = %v, want ErrNotFound", err)
	}
}

func TestMemoryStoreUpsertRunResultOverwritesOnRetriedPair(t *testing.T) {
	m := checks.NewMemoryStore()
	ctx := context.Background()
	if _, err := m.CreateRun(ctx, "r1", "tcp", "pod", json.RawMessage(`{}`), "user", "u1", 1); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	in := store.RunResultInput{RunID: "r1", SourceNode: "a", DestinationNode: "b", Success: false, Error: "first try"}
	if _, err := m.UpsertRunResult(ctx, in); err != nil {
		t.Fatalf("UpsertRunResult: %v", err)
	}

	in2 := store.RunResultInput{RunID: "r1", SourceNode: "a", DestinationNode: "b", Success: true, DurationNs: 42}
	if _, err := m.UpsertRunResult(ctx, in2); err != nil {
		t.Fatalf("UpsertRunResult (retry): %v", err)
	}

	results, err := m.GetRunResults(ctx, "r1")
	if err != nil {
		t.Fatalf("GetRunResults: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1 (overwrite, not append)", len(results))
	}
	if !results[0].Success || results[0].DurationNs != 42 || results[0].Error != "" {
		t.Errorf("results[0] = %+v, want the second write's values", results[0])
	}
}

func TestMemoryStoreUpsertRunResultUnknownRunIsNotFound(t *testing.T) {
	m := checks.NewMemoryStore()
	_, err := m.UpsertRunResult(context.Background(), store.RunResultInput{RunID: "never-existed"})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestMemoryStoreGetRunResultsUnknownRunIsEmptyNotError(t *testing.T) {
	m := checks.NewMemoryStore()
	results, err := m.GetRunResults(context.Background(), "never-existed")
	if err != nil {
		t.Fatalf("GetRunResults: %v", err)
	}
	if results == nil || len(results) != 0 {
		t.Errorf("results = %+v, want an empty non-nil slice", results)
	}
}

func TestMemoryStoreRingEvictsOldestAt51(t *testing.T) {
	m := checks.NewMemoryStore()
	ctx := context.Background()

	var ids []string
	for i := 0; i < 51; i++ {
		id := fmt.Sprintf("run-%02d", i)
		if _, err := m.CreateRun(ctx, id, "tcp", "pod", json.RawMessage(`{}`), "user", "u1", 1); err != nil {
			t.Fatalf("CreateRun(%d): %v", i, err)
		}
		ids = append(ids, id)
	}

	if _, err := m.GetRun(ctx, ids[0]); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetRun(first run) err = %v, want ErrNotFound (evicted by the 51st)", err)
	}
	if _, err := m.GetRun(ctx, ids[1]); err != nil {
		t.Errorf("GetRun(second run) err = %v, want the second run still retained", err)
	}
	got, err := m.GetRun(ctx, ids[50])
	if err != nil {
		t.Fatalf("GetRun(last run): %v", err)
	}
	if got.ID != ids[50] {
		t.Errorf("GetRun(last run).ID = %q, want %q", got.ID, ids[50])
	}
}

func TestMemoryStoreListRunsFiltersAndOrdersNewestFirst(t *testing.T) {
	m := checks.NewMemoryStore()
	ctx := context.Background()

	if _, err := m.CreateRun(ctx, "r1", "tcp", "pod", json.RawMessage(`{}`), "user", "u1", 1); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := m.CreateRun(ctx, "r2", "udp", "pod", json.RawMessage(`{}`), "user", "u1", 1); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := m.CreateRun(ctx, "r3", "tcp", "pod", json.RawMessage(`{}`), "user", "u1", 1); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	page, err := m.ListRuns(ctx, store.RunFilter{CheckType: "tcp"})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(page.Runs) != 2 || page.Runs[0].ID != "r3" || page.Runs[1].ID != "r1" {
		t.Errorf("ListRuns(tcp) = %+v, want [r3, r1] newest-first", page.Runs)
	}
}

// ListRuns' Limit clamp must match store/events.go's clampLimit exactly
// (task-22-brief.md minor d): 0 defaults to 100, not *store.DB's own default
// of some other number and not a MemoryStore-only default -- a caller must
// not be able to tell which backend answered a request from its page size.
// 60 runs are created, past memoryRingSize (50), so every case below is
// itself further bounded by "only 50 runs are retained at all" -- that ring
// bound, not the Limit clamp, is what caps the "over 500" and "zero
// defaults to 100" cases at 50 rather than 500/100.
func TestMemoryStoreListRunsLimitClampMirrorsStoreClampLimit(t *testing.T) {
	m := checks.NewMemoryStore()
	ctx := context.Background()
	for i := 0; i < 60; i++ {
		id := fmt.Sprintf("run-%03d", i)
		if _, err := m.CreateRun(ctx, id, "tcp", "pod", json.RawMessage(`{}`), "user", "u1", 1); err != nil {
			t.Fatalf("CreateRun(%d): %v", i, err)
		}
	}

	cases := []struct {
		name      string
		limit     int
		wantCount int
	}{
		{"zero defaults to 100, capped by the 50-entry ring", 0, 50},
		{"negative clamps to 1", -5, 1},
		{"over 500 clamps to 500, capped by the 50-entry ring", 10_000, 50},
		{"in range is used as-is", 7, 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page, err := m.ListRuns(ctx, store.RunFilter{Limit: tc.limit})
			if err != nil {
				t.Fatalf("ListRuns: %v", err)
			}
			if len(page.Runs) != tc.wantCount {
				t.Errorf("len(Runs) = %d, want %d", len(page.Runs), tc.wantCount)
			}
		})
	}
}

// NextCursor must be emitted exactly when len(page) == limit, matching
// *store.DB.ListRuns' own condition (checks.go), not "there is provably
// more" -- both backends share this same imprecise-but-matched heuristic.
// Run ids must be real UUIDs here (unlike this file's other tests' plain
// "r1"/"run-00" ids): store.DecodeRunCursor -- shared with *store.DB, not a
// MemoryStore-only format -- rejects a non-UUID id, and this test actually
// round-trips a cursor ListRuns itself produced.
func TestMemoryStoreListRunsNextCursorMatchesLimitLikeDB(t *testing.T) {
	m := checks.NewMemoryStore()
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		id := uuid.NewString()
		if _, err := m.CreateRun(ctx, id, "tcp", "pod", json.RawMessage(`{}`), "user", "u1", 1); err != nil {
			t.Fatalf("CreateRun(%d): %v", i, err)
		}
	}

	page, err := m.ListRuns(ctx, store.RunFilter{Limit: 2})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(page.Runs) != 2 || page.NextCursor == "" {
		t.Fatalf("page = %+v, want 2 runs with a NextCursor (len(page) == limit)", page)
	}

	page2, err := m.ListRuns(ctx, store.RunFilter{Limit: 2, Cursor: page.NextCursor})
	if err != nil {
		t.Fatalf("ListRuns (page 2): %v", err)
	}
	if len(page2.Runs) != 1 || page2.NextCursor != "" {
		t.Errorf("page 2 = %+v, want 1 run and no NextCursor (len(page) < limit)", page2)
	}
}

// A cursor that decodes fine but names a run no longer in the (bounded,
// 50-entry) ring -- most plausibly evicted between two page requests -- must
// come back as an empty page, not silently restart the caller from the
// beginning of the list (task-22-brief.md minor d).
func TestMemoryStoreListRunsCursorNotFoundIsEmptyPageNotRestart(t *testing.T) {
	m := checks.NewMemoryStore()
	ctx := context.Background()
	if _, err := m.CreateRun(ctx, "only-run", "tcp", "pod", json.RawMessage(`{}`), "user", "u1", 1); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	staleCursor := store.EncodeRunCursor(time.Now().UTC(), uuid.NewString())
	page, err := m.ListRuns(ctx, store.RunFilter{Cursor: staleCursor})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(page.Runs) != 0 {
		t.Errorf("Runs = %+v, want an empty page for an unresolvable cursor, not a restart from the top", page.Runs)
	}
	if page.NextCursor != "" {
		t.Errorf("NextCursor = %q, want empty", page.NextCursor)
	}
}

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store/gen"
)

// ErrWrongState is returned by MarkRunStarted and FinishRun when id names a run that exists but is
// not in the state the transition requires.
var ErrWrongState = errors.New("store: wrong state")

// Run is one persisted check_runs row: a fan-out execution's spec snapshot,
// status, timings, and initiator (DATA.md §5.2). StartedAt/FinishedAt are
// nil until MarkRunStarted / FinishRun set them.
type Run struct {
	ID            string
	CreatedAt     time.Time
	StartedAt     *time.Time
	FinishedAt    *time.Time
	Status        string // pending | running | succeeded | partial | failed | cancelled
	CheckType     string
	Plane         string
	Spec          json.RawMessage
	InitiatorKind string // authz.SubjectKind
	InitiatorID   string
	PairTotal     int32
	PairOK        int32
	PairFailed    int32
}

// RunFilter selects a page of runs. All fields optional; Limit is clamped to
// [1,500] the same way EventFilter.Limit is.
type RunFilter struct {
	CheckType string // exact match; empty = all
	Status    string // exact match; empty = all
	Cursor    string // opaque keyset cursor from a previous page
	Limit     int
}

// RunPage is one page of ListRuns results, same shape as EventPage.
type RunPage struct {
	Runs       []Run
	NextCursor string // "" when the page is the last one
}

// RunResultInput is UpsertRunResult's write payload: one (source,
// destination) pair's outcome within a run.
type RunResultInput struct {
	RunID           string
	SourceNode      string
	DestinationNode string
	Success         bool
	DurationNs      int64
	Error           string
	// Result is the agent's model.CheckResult verbatim, exactly as the
	// controller returned it -- see migration 00003's comment on
	// check_results.result.
	Result json.RawMessage
	// SampleSeq is which PROBE of this pair the row records, 0-based (migration 00009).
	SampleSeq int32
}

// RunResult is one persisted check_results row.
type RunResult struct {
	ID              int64
	RunID           string
	SourceNode      string
	DestinationNode string
	Success         bool
	DurationNs      int64
	Error           string
	Result          json.RawMessage
	RecordedAt      time.Time
	SampleSeq       int32
}

// RunStore is the seam the check runner needs: create a run, mark it
// started, upsert per-pair results as they complete, and finish it. httpapi
// never needs any of these -- it only reads, via RunReader below.
type RunStore interface {
	// CreateRun persists a new run in status "pending", snapshotting spec as JSONB.
	CreateRun(ctx context.Context, id, checkType, plane string, spec json.RawMessage, initiatorKind, initiatorID string, pairTotal int32) (Run, error)
	// MarkRunStarted transitions id from status "pending" to "running" and stamps started_at = now.
	MarkRunStarted(ctx context.Context, id string) error
	// FinishRun transitions id from status "running" to a terminal status.
	FinishRun(ctx context.Context, id, status string, pairOK, pairFailed int32) error
	// UpsertRunResult inserts one (run, source, destination) result, or --
	// on a retried pair -- overwrites the existing row's success, duration,
	// error, result, and recorded_at rather than erroring.
	UpsertRunResult(ctx context.Context, in RunResultInput) (RunResult, error)
	// ReapStuckRuns force-finishes up to limit runs left in status "running" with created_at strictly
	// before before; a run that never started ("pending") and one that already reached a terminal
	// status are both left untouched however old they.
	ReapStuckRuns(ctx context.Context, before time.Time, limit int32) (int64, error)
}

var _ RunStore = (*DB)(nil)

// RunReader is the seam httpapi needs: read one run, page through runs, and
// read one run's results. The runner never needs any of these itself.
type RunReader interface {
	// GetRun returns ErrNotFound when id does not name a run.
	GetRun(ctx context.Context, id string) (Run, error)
	// ListRuns pages newest-first, same keyset cursor shape as
	// EventStore.ListEvents (checks.sql's ListRuns comment has the details on
	// why the cursor's id half is a UUID here rather than a bigint).
	ListRuns(ctx context.Context, f RunFilter) (RunPage, error)
	// GetRunResults returns every result row for id, ordered by insertion (id ascending); an id naming
	// no run returns an empty, non-nil slice rather than an error.
	GetRunResults(ctx context.Context, id string) ([]RunResult, error)
}

var _ RunReader = (*DB)(nil)

// runFromRow maps a gen.CheckRun row (shared by CreateRun/GetRun/ListRuns) to
// a Run.
func runFromRow(r *gen.CheckRun) Run {
	return Run{
		ID:            formatUUID(r.ID),
		CreatedAt:     r.CreatedAt,
		StartedAt:     nullTime(r.StartedAt),
		FinishedAt:    nullTime(r.FinishedAt),
		Status:        r.Status,
		CheckType:     r.CheckType,
		Plane:         r.Plane,
		Spec:          r.Spec,
		InitiatorKind: r.InitiatorKind,
		InitiatorID:   r.InitiatorID,
		PairTotal:     r.PairTotal,
		PairOK:        r.PairOk,
		PairFailed:    r.PairFailed,
	}
}

// runResultFromRow maps a gen.CheckResult row (shared by GetRunResults/
// UpsertRunResult) to a RunResult.
func runResultFromRow(r *gen.CheckResult) RunResult {
	return RunResult{
		ID:              r.ID,
		RunID:           formatUUID(r.RunID),
		SourceNode:      r.SourceNode,
		DestinationNode: r.DestinationNode,
		Success:         r.Success,
		DurationNs:      r.DurationNs,
		Error:           r.Error,
		Result:          r.Result,
		RecordedAt:      r.RecordedAt,
		SampleSeq:       r.SampleSeq,
	}
}

func (db *DB) CreateRun(ctx context.Context, id, checkType, plane string, spec json.RawMessage, initiatorKind, initiatorID string, pairTotal int32) (Run, error) {
	rid, err := parseUUID(id)
	if err != nil {
		return Run{}, fmt.Errorf("store: create run: %w", err)
	}
	r, err := gen.New(db.pool).CreateRun(ctx, gen.CreateRunParams{
		ID:            rid,
		CheckType:     checkType,
		Plane:         plane,
		Spec:          spec,
		InitiatorKind: initiatorKind,
		InitiatorID:   initiatorID,
		PairTotal:     pairTotal,
	})
	if err != nil {
		return Run{}, fmt.Errorf("store: create run: %w", wrapUniqueViolation(err))
	}
	return runFromRow(&r), nil
}

func (db *DB) MarkRunStarted(ctx context.Context, id string) error {
	rid, err := parseUUID(id)
	if err != nil {
		return fmt.Errorf("store: mark run started: %w", err)
	}
	rows, err := gen.New(db.pool).MarkRunStarted(ctx, rid)
	if err != nil {
		return fmt.Errorf("store: mark run started: %w", err)
	}
	if rows == 0 {
		run, gerr := db.GetRun(ctx, id)
		switch {
		case errors.Is(gerr, ErrNotFound):
			return fmt.Errorf("store: mark run started: %w", ErrNotFound)
		case gerr != nil:
			return fmt.Errorf("store: mark run started: %w", gerr)
		default:
			return fmt.Errorf("store: mark run started: %w: run is %q", ErrWrongState, run.Status)
		}
	}
	return nil
}

func (db *DB) FinishRun(ctx context.Context, id, status string, pairOK, pairFailed int32) error {
	rid, err := parseUUID(id)
	if err != nil {
		return fmt.Errorf("store: finish run: %w", err)
	}
	rows, err := gen.New(db.pool).FinishRun(ctx, gen.FinishRunParams{
		ID:         rid,
		Status:     status,
		PairOk:     pairOK,
		PairFailed: pairFailed,
	})
	if err != nil {
		return fmt.Errorf("store: finish run: %w", err)
	}
	if rows == 0 {
		run, gerr := db.GetRun(ctx, id)
		switch {
		case errors.Is(gerr, ErrNotFound):
			return fmt.Errorf("store: finish run: %w", ErrNotFound)
		case gerr != nil:
			return fmt.Errorf("store: finish run: %w", gerr)
		default:
			return fmt.Errorf("store: finish run: %w: run is %q", ErrWrongState, run.Status)
		}
	}
	return nil
}

// GetRun validates id as a UUID BEFORE touching pgx, and reports a malformed one as ErrNotFound
// rather than as a parse failure.
func (db *DB) GetRun(ctx context.Context, id string) (Run, error) {
	rid, err := parseUUID(id)
	if err != nil {
		return Run{}, fmt.Errorf("store: get run: %w: %w", ErrNotFound, err)
	}
	r, err := gen.New(db.pool).GetRun(ctx, rid)
	if err != nil {
		return Run{}, fmt.Errorf("store: get run: %w", wrapNoRows(err))
	}
	return runFromRow(&r), nil
}

func (db *DB) ListRuns(ctx context.Context, f RunFilter) (RunPage, error) { //nolint:gocritic // hugeParam: RunFilter mirrors EventFilter's value semantics (events.go)
	limit := clampLimit(f.Limit)

	var curTime pgtype.Timestamptz
	var curID pgtype.UUID
	if f.Cursor != "" {
		ts, id, ok, err := DecodeRunCursor(f.Cursor)
		if err != nil {
			return RunPage{}, fmt.Errorf("store: list runs: %w", err)
		}
		if ok {
			cid, err := parseUUID(id)
			if err != nil {
				return RunPage{}, fmt.Errorf("store: list runs: %w", err)
			}
			curTime = pgtype.Timestamptz{Time: ts, Valid: true}
			curID = cid
		}
	}

	var checkType, status pgtype.Text
	if f.CheckType != "" {
		checkType = pgtype.Text{String: f.CheckType, Valid: true}
	}
	if f.Status != "" {
		status = pgtype.Text{String: f.Status, Valid: true}
	}

	rows, err := gen.New(db.pool).ListRuns(ctx, gen.ListRunsParams{
		CheckType: checkType,
		Status:    status,
		CurTime:   curTime,
		CurID:     curID,
		Lim:       int32(limit), //nolint:gosec // limit is clamped to [1,500] above
	})
	if err != nil {
		return RunPage{}, fmt.Errorf("store: list runs: %w", err)
	}

	runs := make([]Run, len(rows))
	for i := range rows {
		runs[i] = runFromRow(&rows[i])
	}

	var nextCursor string
	if len(rows) == limit {
		last := runs[len(runs)-1]
		nextCursor = EncodeRunCursor(last.CreatedAt, last.ID)
	}

	return RunPage{Runs: runs, NextCursor: nextCursor}, nil
}

// GetRunResults applies GetRun's UUID pre-check for the same reason and with the same ErrNotFound
// answer.
func (db *DB) GetRunResults(ctx context.Context, id string) ([]RunResult, error) {
	rid, err := parseUUID(id)
	if err != nil {
		return nil, fmt.Errorf("store: get run results: %w: %w", ErrNotFound, err)
	}
	rows, err := gen.New(db.pool).GetRunResults(ctx, rid)
	if err != nil {
		return nil, fmt.Errorf("store: get run results: %w", err)
	}
	results := make([]RunResult, len(rows))
	for i := range rows {
		results[i] = runResultFromRow(&rows[i])
	}
	return results, nil
}

func (db *DB) UpsertRunResult(ctx context.Context, in RunResultInput) (RunResult, error) { //nolint:gocritic // hugeParam: RunResultInput mirrors the other write-payload structs in this package
	rid, err := parseUUID(in.RunID)
	if err != nil {
		return RunResult{}, fmt.Errorf("store: upsert run result: %w", err)
	}
	r, err := gen.New(db.pool).UpsertRunResult(ctx, gen.UpsertRunResultParams{
		RunID:           rid,
		SourceNode:      in.SourceNode,
		DestinationNode: in.DestinationNode,
		Success:         in.Success,
		DurationNs:      in.DurationNs,
		Error:           in.Error,
		Result:          in.Result,
		SampleSeq:       in.SampleSeq,
	})
	if err != nil {
		return RunResult{}, fmt.Errorf("store: upsert run result: %w", err)
	}
	return runResultFromRow(&r), nil
}

// ReapStuckRuns force-finishes runs abandoned in status "running" -- see
// RunStore.ReapStuckRuns for the contract and checks.Runner.ReapStuckRuns for
// where the cutoff comes from.
func (db *DB) ReapStuckRuns(ctx context.Context, before time.Time, limit int32) (int64, error) {
	n, err := gen.New(db.pool).ReapStuckRuns(ctx, gen.ReapStuckRunsParams{
		CreatedAt: before,
		Limit:     limit,
	})
	if err != nil {
		return 0, fmt.Errorf("store: reap stuck runs: %w", err)
	}
	return n, nil
}

// DeleteRunsBefore deletes up to limit runs older than before, oldest first, and reports how many
// were removed.
func (db *DB) DeleteRunsBefore(ctx context.Context, before time.Time, limit int32) (int64, error) {
	n, err := gen.New(db.pool).DeleteRunsBefore(ctx, gen.DeleteRunsBeforeParams{
		CreatedAt: before,
		Limit:     limit,
	})
	if err != nil {
		return 0, fmt.Errorf("store: delete runs before: %w", err)
	}
	return n, nil
}

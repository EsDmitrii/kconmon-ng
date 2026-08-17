package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
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
	/* deadlineAt is the budget the RUNNER gave this run — the same instant its context carries.
	   It is written down because two readers used to reconstruct it and both got it wrong: the
	   scheduler's overrun guard trusted the status column forever (one orphan muted a schedule), and
	   the reaper rebuilt it from the worst shape the build allows. */
	CreateRun(ctx context.Context, id, checkType, plane string, spec json.RawMessage, initiatorKind, initiatorID string, pairTotal int32, deadlineAt time.Time) (Run, error)
	// MarkRunStarted transitions id from status "pending" to "running" and stamps started_at = now.
	MarkRunStarted(ctx context.Context, id string) error
	// FinishRun transitions id from status "running" to a terminal status.
	FinishRun(ctx context.Context, id, status string, pairOK, pairFailed int32) error
	// AbandonRun writes a terminal status onto a run that is still "pending" OR "running"; it is
	// what a run that never started needs, because FinishRun's UPDATE requires "running".
	AbandonRun(ctx context.Context, id, status string) error
	// UpsertRunResult inserts one (run, source, destination) result, or --
	// on a retried pair -- overwrites the existing row's success, duration,
	// error, result, and recorded_at rather than erroring.
	UpsertRunResult(ctx context.Context, in RunResultInput) (RunResult, error)
	// ReapStuckRuns force-finishes up to limit runs abandoned mid-flight, each judged against ITS OWN
	// declared duration and fan-out plus the given budget; a run that already reached a terminal
	// status is left untouched however old it is. A run still in "pending" IS in scope: a replica
	// that died between CreateRun and MarkRunStarted leaves one behind, and nothing else finishes it.
	ReapStuckRuns(ctx context.Context, budget ReapBudget, limit int32) (int64, error)
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
	// ActiveRunsByInitiator counts the runs this initiator has mid-flight ('pending' or 'running')
	// across EVERY replica; it is the replica-independent form of "is my last run still going".
	ActiveRunsByInitiator(ctx context.Context, initiatorKind, initiatorID string) (int64, error)
	// GetRunResults returns the run's result rows in insertion order (id ascending), bounded to the
	// newest RunResultsCap of them; `truncated` says whether the run holds more. An id naming
	// no run returns an empty, non-nil slice rather than an error.
	GetRunResults(ctx context.Context, id string) (results []RunResult, truncated bool, err error)
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

func (db *DB) CreateRun(ctx context.Context, id, checkType, plane string, spec json.RawMessage, initiatorKind, initiatorID string, pairTotal int32, deadlineAt time.Time) (Run, error) {
	rid, err := parseUUID(id)
	if err != nil {
		return Run{}, fmt.Errorf("store: create run: %w", err)
	}
	start := time.Now()
	r, err := gen.New(db.pool).CreateRun(ctx, gen.CreateRunParams{
		ID:            rid,
		CheckType:     checkType,
		Plane:         plane,
		Spec:          spec,
		InitiatorKind: initiatorKind,
		InitiatorID:   initiatorID,
		PairTotal:     pairTotal,
		DeadlineAt:    pgtype.Timestamptz{Time: deadlineAt, Valid: !deadlineAt.IsZero()},
	})
	db.observe(queryCreateRun, start, queryResult(err))
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
	start := time.Now()
	rows, err := gen.New(db.pool).MarkRunStarted(ctx, rid)
	db.observe(queryMarkRunStarted, start, queryResult(err))
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

/*
AbandonRun writes a terminal status onto a run that never reached 'running'.

FinishRun's UPDATE is guarded by `status = 'running'`, which is the right guard for finishing a run
that started. The abandon path is the other case: MarkRunStarted failed transiently, so the row is
still 'pending', FinishRun matched nothing and the caller treated the resulting ErrWrongState as
"already terminal" — leaving the row 'pending' forever, with the run detail page saying the run was
about to start and the stuck-run reaper (which reaps by deadline against 'running') never touching
it.

A run that is already terminal is NOT an error here: the caller is saying "this must not stay
pending", and a run that finished on its own satisfies that.
*/
func (db *DB) AbandonRun(ctx context.Context, id, status string) error {
	rid, err := parseUUID(id)
	if err != nil {
		return fmt.Errorf("store: abandon run: %w", err)
	}
	start := time.Now()
	rows, err := gen.New(db.pool).AbandonRun(ctx, gen.AbandonRunParams{ID: rid, Status: status})
	db.observe(queryAbandonRun, start, queryResult(err))
	if err != nil {
		return fmt.Errorf("store: abandon run: %w", err)
	}
	if rows == 0 {
		if _, gerr := db.GetRun(ctx, id); errors.Is(gerr, ErrNotFound) {
			return fmt.Errorf("store: abandon run: %w", ErrNotFound)
		}
	}
	return nil
}

func (db *DB) FinishRun(ctx context.Context, id, status string, pairOK, pairFailed int32) error {
	rid, err := parseUUID(id)
	if err != nil {
		return fmt.Errorf("store: finish run: %w", err)
	}
	start := time.Now()
	rows, err := gen.New(db.pool).FinishRun(ctx, gen.FinishRunParams{
		ID:         rid,
		Status:     status,
		PairOk:     pairOK,
		PairFailed: pairFailed,
	})
	db.observe(queryFinishRun, start, queryResult(err))
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
	start := time.Now()
	r, err := gen.New(db.pool).GetRun(ctx, rid)
	db.observe(queryGetRun, start, queryResult(err))
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

	start := time.Now()
	rows, err := gen.New(db.pool).ListRuns(ctx, gen.ListRunsParams{
		CheckType: checkType,
		Status:    status,
		CurTime:   curTime,
		CurID:     curID,
		Lim:       int32(limit), //nolint:gosec // limit is clamped to [1,500] above
	})
	db.observe(queryListRuns, start, queryResult(err))
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

// RunResultsCap is the most result rows one read of a run will return.
//
// An interval run is bounded at 400 pairs x 500 samples = 200 000 rows, each carrying the agent's
// verbatim result payload, and the run detail page re-reads them every five seconds while the run is
// alive. Two thousand is the largest tail that stays a sane response; a run that exceeds it is
// reported as truncated rather than quietly served as if it were whole.
const RunResultsCap = 2000

// ActiveRunsByInitiator counts this initiator's mid-flight runs; see RunReader for why it is a
// query rather than a memory.
func (db *DB) ActiveRunsByInitiator(ctx context.Context, initiatorKind, initiatorID string) (int64, error) {
	start := time.Now()
	n, err := gen.New(db.pool).CountActiveRunsByInitiator(ctx, gen.CountActiveRunsByInitiatorParams{
		InitiatorKind: initiatorKind,
		InitiatorID:   initiatorID,
	})
	db.observe(queryCountActiveRunsByInitiator, start, queryResult(err))
	if err != nil {
		return 0, fmt.Errorf("store: count active runs by initiator: %w", err)
	}
	return n, nil
}

// GetRunResults applies GetRun's UUID pre-check for the same reason and with the same ErrNotFound
// answer.
//
// The rows come back in INSERTION order (id ascending), which is what every caller reads them in,
// but they are selected newest-first: a bounded read of a long run has to keep the END of it, which
// is the part the page watching a live run is actually looking at. `truncated` is true when the run
// holds more than RunResultsCap rows, so the caller can say so instead of presenting a tail as the
// whole set.
func (db *DB) GetRunResults(ctx context.Context, id string) (results []RunResult, truncated bool, err error) {
	rid, err := parseUUID(id)
	if err != nil {
		return nil, false, fmt.Errorf("store: get run results: %w: %w", ErrNotFound, err)
	}
	// One row MORE than the cap: its presence is how the truncation is detected, and it is dropped.
	start := time.Now()
	rows, err := gen.New(db.pool).GetRunResults(ctx, gen.GetRunResultsParams{RunID: rid, Lim: RunResultsCap + 1})
	db.observe(queryGetRunResults, start, queryResult(err))
	if err != nil {
		return nil, false, fmt.Errorf("store: get run results: %w", err)
	}
	truncated = len(rows) > RunResultsCap
	if truncated {
		rows = rows[:RunResultsCap]
	}
	results = make([]RunResult, len(rows))
	// Reversed: the query ordered by id DESC to keep the newest, the callers read insertion order.
	for i := range rows {
		results[len(rows)-1-i] = runResultFromRow(&rows[i])
	}
	return results, truncated, nil
}

func (db *DB) UpsertRunResult(ctx context.Context, in RunResultInput) (RunResult, error) { //nolint:gocritic // hugeParam: RunResultInput mirrors the other write-payload structs in this package
	rid, err := parseUUID(in.RunID)
	if err != nil {
		return RunResult{}, fmt.Errorf("store: upsert run result: %w", err)
	}
	start := time.Now()
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
	db.observe(queryUpsertRunResult, start, queryResult(err))
	if err != nil {
		return RunResult{}, fmt.Errorf("store: upsert run result: %w", err)
	}
	return runResultFromRow(&r), nil
}

// ReapStuckRuns force-finishes runs abandoned mid-flight -- see RunStore.ReapStuckRuns for the
// contract and checks.Runner.ReapStuckRuns for where the budget comes from.
//
// budget is the SHAPE of the cutoff rather than a single instant: each row is judged against its own
// spec duration and fan-out (queries/checks.sql), because one fleet-wide cutoff has to be the worst
// run this build accepts and would leave a five-minute run misreporting for a day and a half.
func (db *DB) ReapStuckRuns(ctx context.Context, budget ReapBudget, limit int32) (int64, error) { //nolint:gocritic // hugeParam: mirrors the value semantics of the other filter structs here
	start := time.Now()
	n, err := gen.New(db.pool).ReapStuckRuns(ctx, gen.ReapStuckRunsParams{
		PerSourceConcurrency: numericFromFloat(budget.PerSourceConcurrency),
		PerPairSeconds:       numericFromFloat(budget.PerPairTimeout.Seconds()),
		SlackSeconds:         numericFromFloat(budget.Slack.Seconds()),
		Lim:                  limit,
	})
	db.observe(queryReapStuckRuns, start, queryResult(err))
	if err != nil {
		return 0, fmt.Errorf("store: reap stuck runs: %w", err)
	}
	return n, nil
}

// ReapBudget is what a run is allowed on top of its own declared duration before the reaper may call
// it abandoned: how many pairs one source runs at a time, how long a single pair may take, and a
// flat margin for everything else (dispatch, the store, a slow scrape).
type ReapBudget struct {
	PerSourceConcurrency float64
	PerPairTimeout       time.Duration
	Slack                time.Duration
}

// numericFromFloat builds the pgtype.Numeric the generated params take.
func numericFromFloat(v float64) pgtype.Numeric {
	var n pgtype.Numeric
	// Scan takes the text form, which round-trips a float exactly for the small values used here.
	if err := n.Scan(strconv.FormatFloat(v, 'f', -1, 64)); err != nil {
		return pgtype.Numeric{}
	}
	return n
}

// DeleteRunsBefore deletes up to limit runs older than before, oldest first, and reports how many
// were removed.
func (db *DB) DeleteRunsBefore(ctx context.Context, before time.Time, limit int32) (int64, error) {
	start := time.Now()
	n, err := gen.New(db.pool).DeleteRunsBefore(ctx, gen.DeleteRunsBeforeParams{
		CreatedAt: before,
		Limit:     limit,
	})
	db.observe(queryDeleteRunsBefore, start, queryResult(err))
	if err != nil {
		return 0, fmt.Errorf("store: delete runs before: %w", err)
	}
	return n, nil
}

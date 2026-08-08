package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// memoryRingSize bounds MemoryStore's retained runs (Plan Decision 15): with
// database.mode=disabled there is nowhere durable to put them, so recent runs
// are kept in a small bounded ring instead of unbounded process memory.
const memoryRingSize = 50

// listRunsMinLimit / listRunsMaxLimit / listRunsDefaultLimit mirror
// store/events.go's minLimit/maxLimit/defaultLimit exactly (1/500/100): those
// are unexported, so this is a deliberate copy, not an import, kept in sync
// by hand -- ListRuns must clamp RunFilter.Limit identically regardless of
// which Store implementation (*store.DB or *MemoryStore) answers it, so a
// caller cannot tell which backend served a page from its size alone.
const (
	listRunsMinLimit     = 1
	listRunsMaxLimit     = 500
	listRunsDefaultLimit = 100
)

// memoryRunEntry is one ring slot: a run plus its per-pair results.
type memoryRunEntry struct {
	run     store.Run
	results []store.RunResult
}

// MemoryStore is the database.mode=disabled fallback for store.RunStore and
// store.RunReader (Plan Decision 15): a mutex-guarded, insertion-ordered ring
// of the most recent memoryRingSize (50) runs. It exists so Runner can take
// the Store interface and never branch on "is there a database" -- with the
// database on, Runner is handed a *store.DB; with it off, a *MemoryStore.
// Honestly labelled: runs still work with the database disabled, but only
// the most recent 50 are retrievable.
type MemoryStore struct {
	mu      sync.Mutex
	order   []string // insertion order, oldest first
	runs    map[string]*memoryRunEntry
	nextRes int64
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{runs: make(map[string]*memoryRunEntry)}
}

var (
	_ store.RunStore  = (*MemoryStore)(nil)
	_ store.RunReader = (*MemoryStore)(nil)
)

// CreateRun persists a new run in status "pending". A colliding id returns
// store.ErrAlreadyExists, matching *store.DB's CreateRun contract.
func (m *MemoryStore) CreateRun(_ context.Context, id, checkType, plane string, spec json.RawMessage, initiatorKind, initiatorID string, pairTotal int32) (store.Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.runs[id]; exists {
		return store.Run{}, fmt.Errorf("checks: memory store: create run: %w", store.ErrAlreadyExists)
	}

	run := store.Run{
		ID:            id,
		CreatedAt:     time.Now().UTC(),
		Status:        "pending",
		CheckType:     checkType,
		Plane:         plane,
		Spec:          spec,
		InitiatorKind: initiatorKind,
		InitiatorID:   initiatorID,
		PairTotal:     pairTotal,
	}
	m.runs[id] = &memoryRunEntry{run: run}
	m.order = append(m.order, id)
	if len(m.order) > memoryRingSize {
		evicted := m.order[0]
		m.order = m.order[1:]
		delete(m.runs, evicted)
	}
	return run, nil
}

// MarkRunStarted transitions id from "pending" to "running", matching
// *store.DB's lifecycle guards: store.ErrNotFound when id is unknown (whether
// it never existed or has since been evicted from the ring),
// store.ErrWrongState when id names a run that is not "pending".
func (m *MemoryStore) MarkRunStarted(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.runs[id]
	if !ok {
		return fmt.Errorf("checks: memory store: mark run started: %w", store.ErrNotFound)
	}
	if entry.run.Status != "pending" {
		return fmt.Errorf("checks: memory store: mark run started: %w: run is %q", store.ErrWrongState, entry.run.Status)
	}
	now := time.Now().UTC()
	entry.run.Status = "running"
	entry.run.StartedAt = &now
	return nil
}

// FinishRun transitions id from "running" to a terminal status, matching
// *store.DB's lifecycle guards: store.ErrNotFound when id is unknown,
// store.ErrWrongState when id names a run that is not "running" -- including
// one FinishRun already finished, so a retrying caller can treat
// ErrWrongState as "already finished" exactly as store.DB.FinishRun documents.
func (m *MemoryStore) FinishRun(_ context.Context, id, status string, pairOK, pairFailed int32) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.runs[id]
	if !ok {
		return fmt.Errorf("checks: memory store: finish run: %w", store.ErrNotFound)
	}
	if entry.run.Status != "running" {
		return fmt.Errorf("checks: memory store: finish run: %w: run is %q", store.ErrWrongState, entry.run.Status)
	}
	now := time.Now().UTC()
	entry.run.Status = status
	entry.run.FinishedAt = &now
	entry.run.PairOK = pairOK
	entry.run.PairFailed = pairFailed
	return nil
}

// ReapStuckRuns force-finishes up to limit runs left "running" with CreatedAt
// strictly before before, recording each as "cancelled", and reports how many
// it moved -- the same contract *store.DB implements against SQL
// (store.RunStore.ReapStuckRuns).
//
// It exists here for the reason every other method on this type does: with
// database.mode=disabled the runner is handed a *MemoryStore instead of a
// *store.DB and must not behave differently. A process that dies mid-run
// loses this ring entirely, so the reaper has less to do here than against a
// database -- but a run whose execute goroutine died without reaching
// FinishRun (a panic outside runOneRecovered's reach, say) leaves exactly the
// same stuck row in memory that it would on disk.
//
// Oldest-first, matching the SQL's ORDER BY created_at, so a limit that
// cannot cover the whole backlog makes the same progress either way. The
// insertion-ordered ring means iterating m.order forward IS oldest-first;
// no sort is needed.
func (m *MemoryStore) ReapStuckRuns(_ context.Context, before time.Time, limit int32) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var reaped int64
	now := time.Now().UTC()
	for _, id := range m.order {
		if reaped >= int64(limit) {
			break
		}
		entry := m.runs[id]
		// status "running" is half the predicate: age alone is never enough.
		if entry.run.Status != "running" || !entry.run.CreatedAt.Before(before) {
			continue
		}
		entry.run.Status = "cancelled"
		entry.run.FinishedAt = &now
		reaped++
	}
	return reaped, nil
}

// UpsertRunResult inserts one (run, source, destination) result, or -- on a
// retried pair -- overwrites the existing row, matching *store.DB's upsert
// contract.
func (m *MemoryStore) UpsertRunResult(_ context.Context, in store.RunResultInput) (store.RunResult, error) { //nolint:gocritic // hugeParam: matches store.DB.UpsertRunResult's own signature (store/checks.go)
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.runs[in.RunID]
	if !ok {
		return store.RunResult{}, fmt.Errorf("checks: memory store: upsert run result: %w", store.ErrNotFound)
	}

	for i := range entry.results {
		r := &entry.results[i]
		if r.SourceNode == in.SourceNode && r.DestinationNode == in.DestinationNode {
			r.Success = in.Success
			r.DurationNs = in.DurationNs
			r.Error = in.Error
			r.Result = in.Result
			r.RecordedAt = time.Now().UTC()
			return *r, nil
		}
	}

	m.nextRes++
	res := store.RunResult{
		ID:              m.nextRes,
		RunID:           in.RunID,
		SourceNode:      in.SourceNode,
		DestinationNode: in.DestinationNode,
		Success:         in.Success,
		DurationNs:      in.DurationNs,
		Error:           in.Error,
		Result:          in.Result,
		RecordedAt:      time.Now().UTC(),
	}
	entry.results = append(entry.results, res)
	return res, nil
}

// GetRun returns store.ErrNotFound when id does not name a run -- including
// one the ring has since evicted, which looks identical to "never existed"
// from here, exactly as it would to a caller polling a run that finished
// long enough ago to fall off the end.
func (m *MemoryStore) GetRun(_ context.Context, id string) (store.Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.runs[id]
	if !ok {
		return store.Run{}, fmt.Errorf("checks: memory store: get run: %w", store.ErrNotFound)
	}
	return entry.run, nil
}

// ListRuns pages newest-first, filtered by CheckType/Status, using the same
// opaque cursor encoding *store.DB uses (store.EncodeRunCursor /
// store.DecodeRunCursor) so a caller cannot tell which backend produced a
// page from the cursor shape alone. Limit is clamped exactly like
// *store.DB.ListRuns (clampLimit's contract: 0 defaults to 100, otherwise
// [1, 500]), and NextCursor is emitted under the same condition *store.DB
// uses -- len(page) == limit -- rather than "there really is more" (a
// heuristic *store.DB's own SQL LIMIT-only query cannot improve on either;
// both backends can hand back a NextCursor whose page turns out empty, and
// that is an accepted, matched property, not a bug unique to one side).
func (m *MemoryStore) ListRuns(_ context.Context, f store.RunFilter) (store.RunPage, error) { //nolint:gocritic // hugeParam: RunFilter mirrors store.DB.ListRuns' value semantics
	m.mu.Lock()
	defer m.mu.Unlock()

	runs := make([]store.Run, 0, len(m.order))
	for i := len(m.order) - 1; i >= 0; i-- {
		entry := m.runs[m.order[i]]
		if f.CheckType != "" && entry.run.CheckType != f.CheckType {
			continue
		}
		if f.Status != "" && entry.run.Status != f.Status {
			continue
		}
		runs = append(runs, entry.run)
	}

	limit := clampListRunsLimit(f.Limit)

	start := 0
	if f.Cursor != "" {
		_, id, ok, err := store.DecodeRunCursor(f.Cursor)
		if err != nil {
			return store.RunPage{}, fmt.Errorf("checks: memory store: list runs: %w", err)
		}
		if ok {
			found := false
			for i := range runs {
				if runs[i].ID == id {
					start = i + 1
					found = true
					break
				}
			}
			// The cursor decoded fine but names no run in the (filtered,
			// still-retained) list -- most plausibly the ring (memoryRingSize,
			// 50) evicted it between this page and the last, though a garbage
			// cursor from outside this process looks identical from here.
			// Falling through with start left at 0 would silently restart the
			// caller at page one instead of reporting the page their cursor
			// actually pointed at is gone -- indistinguishable, from the
			// caller's side, from every run simply having been deleted
			// underneath them, so an empty page (not an error) is correct.
			if !found {
				return store.RunPage{Runs: []store.Run{}}, nil
			}
		}
	}
	if start > len(runs) {
		start = len(runs)
	}
	page := runs[start:]
	if len(page) > limit {
		page = page[:limit]
	}

	var next string
	if len(page) == limit {
		last := page[len(page)-1]
		next = store.EncodeRunCursor(last.CreatedAt, last.ID)
	}
	return store.RunPage{Runs: page, NextCursor: next}, nil
}

// clampListRunsLimit applies RunFilter.Limit's documented contract, mirroring
// store/events.go's clampLimit: 0 defaults to listRunsDefaultLimit, everything
// else is clamped into [listRunsMinLimit, listRunsMaxLimit].
func clampListRunsLimit(limit int) int {
	switch {
	case limit == 0:
		return listRunsDefaultLimit
	case limit < listRunsMinLimit:
		return listRunsMinLimit
	case limit > listRunsMaxLimit:
		return listRunsMaxLimit
	default:
		return limit
	}
}

// GetRunResults returns every result row for id, ordered by insertion,
// matching *store.DB's "no rows is not itself a failure" contract: an id
// naming no run returns an empty, non-nil slice rather than an error.
func (m *MemoryStore) GetRunResults(_ context.Context, id string) ([]store.RunResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.runs[id]
	if !ok {
		return []store.RunResult{}, nil
	}
	out := make([]store.RunResult, len(entry.results))
	copy(out, entry.results)
	return out, nil
}

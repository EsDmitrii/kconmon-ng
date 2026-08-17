package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// memoryRingSize bounds MemoryStore's retained runs: with database.mode=disabled there is nowhere
// durable to put them.
const memoryRingSize = 50

// listRunsMinLimit / listRunsMaxLimit / listRunsDefaultLimit mirror store/events.go's
// minLimit/maxLimit/defaultLimit exactly (1/500/100).
const (
	listRunsMinLimit     = 1
	listRunsMaxLimit     = 500
	listRunsDefaultLimit = 100
)

// memorySnapshotRingSize bounds MemoryStore's retained path snapshots; fifty would evict a stable
// cluster's history within a single scheduled sweep.
const memorySnapshotRingSize = 500

// memoryRunEntry is one ring slot: a run plus its per-pair results.
type memoryRunEntry struct {
	run     store.Run
	results []store.RunResult
}

// snapshotKey is one path snapshot's identity, mirroring the SQL table's
// UNIQUE (source_node, destination, path_hash) exactly -- a struct rather than
// a joined string so no separator can ever collide with a destination name.
type snapshotKey struct {
	sourceNode  string
	destination string
	pathHash    string
}

// MemoryStore is the database.mode=disabled fallback for store.RunStore and store.RunReader; it
// exists so Runner can take the Store interface and never branch on "is there a database".
type MemoryStore struct {
	mu      sync.Mutex
	order   []string // insertion order, oldest first
	runs    map[string]*memoryRunEntry
	nextRes int64

	// snapOrder/snaps are the path-history half, kept in their own ring rather than hanging off a run.
	snapOrder []snapshotKey // insertion order, oldest first
	snaps     map[snapshotKey]*store.PathSnapshot
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		runs:  make(map[string]*memoryRunEntry),
		snaps: make(map[snapshotKey]*store.PathSnapshot),
	}
}

var (
	_ store.RunStore          = (*MemoryStore)(nil)
	_ store.RunReader         = (*MemoryStore)(nil)
	_ store.PathSnapshotStore = (*MemoryStore)(nil)
)

// CreateRun persists a new run in status "pending". A colliding id returns
// store.ErrAlreadyExists, matching *store.DB's CreateRun contract.
func (m *MemoryStore) CreateRun(_ context.Context, id, checkType, plane string, spec json.RawMessage, initiatorKind, initiatorID string, pairTotal int32, deadlineAt time.Time) (store.Run, error) {
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

// MarkRunStarted transitions id from "pending" to "running", matching *store.DB's lifecycle guards.
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

// FinishRun transitions id from "running" to a terminal status, matching *store.DB's lifecycle
// guards.
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

/*
AbandonRun writes a terminal status onto a run that is still "pending" OR "running", mirroring
*store.DB's own UPDATE. A run that is already terminal is not an error: the caller is asking for
"must not stay pending", and one that finished on its own satisfies that.
*/
func (m *MemoryStore) AbandonRun(_ context.Context, id, status string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.runs[id]
	if !ok {
		return fmt.Errorf("checks: memory store: abandon run: %w", store.ErrNotFound)
	}
	if entry.run.Status != "pending" && entry.run.Status != "running" {
		return nil
	}
	now := time.Now().UTC()
	entry.run.Status = status
	entry.run.FinishedAt = &now
	entry.run.PairOK = 0
	entry.run.PairFailed = 0
	return nil
}

// ReapStuckRuns force-finishes up to limit runs left "running" with CreatedAt strictly before
// before; it exists here for the reason every other method on this type does.
func (m *MemoryStore) ReapStuckRuns(_ context.Context, budget store.ReapBudget, limit int32) (int64, error) { //nolint:gocritic // hugeParam: mirrors *store.DB's own signature
	m.mu.Lock()
	defer m.mu.Unlock()

	var reaped int64
	now := time.Now().UTC()
	for _, id := range m.order {
		if reaped >= int64(limit) {
			break
		}
		entry := m.runs[id]
		/* Mid-flight is 'running' OR 'pending': a replica that died between CreateRun and
		   MarkRunStarted leaves the latter, and nothing else ever finishes it. A terminal run is
		   never touched however old it is. */
		if entry.run.Status != "running" && entry.run.Status != "pending" {
			continue
		}
		// The row's OWN allowance, the same shape the SQL computes.
		if now.Before(entry.run.CreatedAt.Add(reapAllowance(&entry.run, budget))) {
			continue
		}
		entry.run.Status = "cancelled"
		entry.run.FinishedAt = &now
		reaped++
	}
	return reaped, nil
}

// reapAllowance is how long this run may legitimately take: its declared duration, plus one worst
// round over its own fan-out, plus the flat margin.
func reapAllowance(run *store.Run, budget store.ReapBudget) time.Duration { //nolint:gocritic // hugeParam: see the caller
	var spec struct {
		Duration time.Duration `json:"Duration"`
	}
	_ = json.Unmarshal(run.Spec, &spec)

	pairs := float64(run.PairTotal)
	if pairs < 1 {
		pairs = 1
	}
	batches := math.Ceil(pairs / math.Max(budget.PerSourceConcurrency, 1))
	return spec.Duration + time.Duration(batches)*budget.PerPairTimeout + budget.Slack
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

	// The match key includes SampleSeq, mirroring check_results_pair_unique since migration 00009.
	for i := range entry.results {
		r := &entry.results[i]
		if r.SourceNode == in.SourceNode && r.DestinationNode == in.DestinationNode && r.SampleSeq == in.SampleSeq {
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
		SampleSeq:       in.SampleSeq,
	}
	entry.results = append(entry.results, res)
	return res, nil
}

// UpsertPathSnapshot records one trace in path history.
func (m *MemoryStore) UpsertPathSnapshot(_ context.Context, in store.PathSnapshotInput) (store.PathSnapshot, bool, error) { //nolint:gocritic // hugeParam: matches store.PathSnapshotStore's own signature (store/mtr.go)
	if err := in.Validate(); err != nil {
		return store.PathSnapshot{}, false, err
	}
	// Marshalled up front, outside the lock: PathSnapshot.Hops is the raw
	// JSONB the database hands back, and returning the same shape here is what
	// keeps a caller from having to know which backend answered.
	hops, err := json.Marshal(in.Hops)
	if err != nil {
		return store.PathSnapshot{}, false, fmt.Errorf("checks: memory store: upsert path snapshot: encode hops: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	key := snapshotKey{sourceNode: in.SourceNode, destination: in.Destination, pathHash: in.PathHash}
	if existing, ok := m.snaps[key]; ok {
		existing.LastSeen = in.SeenAt
		existing.TraceCount++
		return *existing, false, nil
	}

	snap := &store.PathSnapshot{
		ID:          uuid.NewString(),
		SourceNode:  in.SourceNode,
		Destination: in.Destination,
		PathHash:    in.PathHash,
		HopCount:    int32(len(in.Hops)), //nolint:gosec // len is bounded by Validate's maxPathHops (64)
		Hops:        hops,
		FirstSeen:   in.SeenAt,
		LastSeen:    in.SeenAt,
		TraceCount:  1,
		RunID:       in.RunID,
	}
	m.snaps[key] = snap
	m.snapOrder = append(m.snapOrder, key)
	if len(m.snapOrder) > memorySnapshotRingSize {
		evicted := m.snapOrder[0]
		m.snapOrder = m.snapOrder[1:]
		delete(m.snaps, evicted)
	}
	return *snap, true, nil
}

// GetRun returns store.ErrNotFound when id does not name a run -- including one the ring has since
// evicted.
func (m *MemoryStore) GetRun(_ context.Context, id string) (store.Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.runs[id]
	if !ok {
		return store.Run{}, fmt.Errorf("checks: memory store: get run: %w", store.ErrNotFound)
	}
	return entry.run, nil
}

// ListRuns pages newest-first, filtered by CheckType/Status.
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
			// The cursor decoded fine but names no run in the (filtered, still-retained) list.
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

// ActiveRunsByInitiator counts this initiator's mid-flight runs, matching *store.DB's own contract.
func (m *MemoryStore) ActiveRunsByInitiator(_ context.Context, initiatorKind, initiatorID string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, id := range m.order {
		run := m.runs[id].run
		if run.InitiatorKind != initiatorKind || run.InitiatorID != initiatorID {
			continue
		}
		if run.Status == "pending" || run.Status == "running" {
			n++
		}
	}
	return n, nil
}

// GetRunResults returns the run's result rows in insertion order, bounded to the newest
// store.RunResultsCap of them exactly as the database does, matching *store.DB's "no rows is not
// itself a failure" contract: an id naming no run returns an empty, non-nil slice rather than an
// error.
func (m *MemoryStore) GetRunResults(_ context.Context, id string) ([]store.RunResult, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.runs[id]
	if !ok {
		return []store.RunResult{}, false, nil
	}
	rows := entry.results
	truncated := len(rows) > store.RunResultsCap
	if truncated {
		// The NEWEST cap, same as the query: a bounded read of a long run keeps its end.
		rows = rows[len(rows)-store.RunResultsCap:]
	}
	out := make([]store.RunResult, len(rows))
	copy(out, rows)
	return out, truncated, nil
}

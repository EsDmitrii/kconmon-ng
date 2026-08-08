package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store/gen"
)

// The RetentionDeleted "table" labels for the nine tables this sweep prunes.
// This is a closed set enforced only by this file's own usage. check_runs'
// own results (check_results) never get a label or a deleteBatches call of
// their own: ON DELETE CASCADE on check_results.run_id means deleting the run
// row is enough to also drop its results.
//
// The three M5 tables age out on the column that means "still relevant", not
// on when the row was written:
//
//   - mtr_path_snapshots by last_seen -- a route the pair still takes is
//     current however long ago it was first observed. (Its run_id is
//     ON DELETE SET NULL, so a snapshot deliberately outlives the check_runs
//     row that produced it; the two sweeps are independent by design.)
//   - mtr_hop_enrichment by resolved_at -- a TTL cache whose every row is
//     re-derivable, so this sweep costs at most one lookup to be wrong.
//   - annotations by start_at -- a mark ages out with the data it annotates,
//     not with when it was typed.
//
// The three M6 tables follow the same rule:
//
//   - k8s_events by event_time -- a capture ages out with the window it
//     describes, exactly as topology_events does.
//   - incidents by resolved_at, WHICH MEANS AN OPEN INCIDENT IS NEVER PRUNED:
//     an open incident has a NULL resolved_at, and `NULL < cutoff` is NULL,
//     never true, so no cutoff can ever select one. That is deliberate. An
//     investigation nobody closed is not stale data, it is unfinished work,
//     and a retention sweep that deleted it would be closing it on the
//     operator's behalf. An operator who wants an old open incident gone
//     resolves it (and it then ages out normally) or deletes it.
//   - maintenance_windows by end_at -- a window that is still open is still
//     current however long ago it began.
//
// WEBHOOKS ARE NEVER SWEPT, and that is why there is no tableWebhooks label
// and no fourth M6 entry in PruneOnce's list. A webhook row is CONFIGURATION,
// not observation: it is what an operator typed, it does not accumulate with
// time (one row per endpoint, bounded by how many were configured), and its
// only time column, created_at, records when the endpoint was set up rather
// than when it last mattered. Ageing rows out of it would silently switch off
// notifications for a still-wanted endpoint whose only crime was being
// configured before the retention horizon -- a retention policy is not a
// deconfiguration policy. Endpoints leave this table exactly one way: an
// operator deletes them.
const (
	tableTopologyEvents = "topology_events"
	tableAuditLog       = "audit_log"
	tableCheckRuns      = "check_runs"
	tableMTRSnapshots   = "mtr_path_snapshots"
	tableMTREnrichment  = "mtr_hop_enrichment"
	tableAnnotations    = "annotations"
	tableK8sEvents      = "k8s_events"
	tableIncidents      = "incidents"
	tableMaintenance    = "maintenance_windows"
)

// pruneInterval is the steady-state sweep cadence.
const pruneInterval = 24 * time.Hour

// pruneJitterMax bounds the random delay before a replica's very first sweep.
// Several console replicas restarting together (a rollout) would otherwise all
// call pg_try_advisory_lock in the same instant; harmless since only one can
// ever win, but needless simultaneous connection use against the pool for no
// benefit.
const pruneJitterMax = 30 * time.Second

// pruneBatchSize bounds one DELETE statement: a first sweep against a
// long-neglected database can have far more expired rows than is safe to
// remove in one statement, so a sweep loops in bounded batches instead of
// taking one table-wide lock or risking the statement timeout.
const pruneBatchSize = 5000

// pruneBatchPause separates consecutive batches within one sweep, so a long
// catch-up run does not hold the connection -- and with it the advisory lock
// -- in a tight loop against the database.
const pruneBatchPause = 50 * time.Millisecond

// unlockTimeout bounds pg_advisory_unlock, run on a context deliberately
// independent of the sweep's own ctx -- see PruneOnce.
const unlockTimeout = 5 * time.Second

// pruneLockKey is the pg_try_advisory_lock key Pruner uses to serialize
// sweeps across replicas. It is
// crc32.Checksum([]byte("kconmon-ng.store.Pruner"), crc32.MakeTable(crc32.IEEE)):
// the same derivation goose's own DefaultLockID uses (4097083626, see
// github.com/pressly/goose/v3/lock.DefaultLockID), computed from a different
// input string so a migration run and a prune sweep never contend for the
// same key.
const pruneLockKey int64 = 3698486424

// Pruner deletes rows past the retention horizon. It runs in every replica but
// does work in at most one at a time: each sweep takes a PostgreSQL
// session-level advisory lock (pg_try_advisory_lock) and returns immediately
// if another replica holds it. That is the same primitive goose uses for
// migrations (migrate.go), so no new concurrency mechanism enters the
// codebase.
type Pruner struct {
	db        *DB
	retention time.Duration
	m         *metrics.Metrics
}

// NewPruner returns a Pruner that deletes rows older than retention from db's
// tables on every sweep. m must not be nil.
func NewPruner(db *DB, retention time.Duration, m *metrics.Metrics) *Pruner {
	return &Pruner{db: db, retention: retention, m: m}
}

// Run sweeps once at start (after a short jitter) and every 24h until ctx is
// cancelled. A zero (or negative) retention makes Run a no-op that returns
// immediately, without ever touching db's pool: database.retentionDays=0 is
// the documented "keep everything" setting (internal/console/config/config.go),
// not merely an unconfigured edge case.
func (p *Pruner) Run(ctx context.Context) {
	if p.retention <= 0 {
		return
	}

	if !pruneSleep(ctx, pruneJitter()) {
		return
	}
	p.sweep(ctx)

	ticker := time.NewTicker(pruneInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.sweep(ctx)
		}
	}
}

// sweep runs one PruneOnce and logs the outcome. A failed sweep must never
// stop Run's loop -- a transient database error simply gets retried on the
// next tick, 24h later.
func (p *Pruner) sweep(ctx context.Context) {
	deleted, err := p.PruneOnce(ctx)
	if err != nil {
		slog.Warn("prune sweep failed", "error", err)
		return
	}
	if len(deleted) > 0 {
		slog.Info("prune sweep completed", "deleted", deleted)
	}
}

// PruneOnce performs one bounded sweep and reports rows deleted per table. It
// returns an empty (non-nil), zero-value map with a nil error when another
// replica already holds the advisory lock: that is the expected steady-state
// outcome on every replica but one, not a failure. Every table's sweep always
// runs, even when an earlier one fails: the returned map holds the counts for
// the sweeps that ran, alongside a joined error for the ones that did not
// finish clean — a partial result, not all-or-nothing.
func (p *Pruner) PruneOnce(ctx context.Context) (map[string]int64, error) {
	// pg_try_advisory_lock and its matching pg_advisory_unlock must run on the
	// exact same backend connection: it is a session-level lock, and p.db.pool's
	// own Exec/Query/QueryRow each borrow a possibly-different pooled connection
	// per call. conn, acquired once here, is reused below for the lock check,
	// every delete batch, and the unlock.
	conn, err := p.db.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: prune: acquire connection: %w", err)
	}
	defer conn.Release()

	var locked bool
	if err = conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", pruneLockKey).Scan(&locked); err != nil {
		return nil, fmt.Errorf("store: prune: acquire advisory lock: %w", err)
	}
	if !locked {
		return map[string]int64{}, nil
	}
	defer p.releaseLock(conn)

	cutoff := time.Now().Add(-p.retention)
	q := gen.New(conn)

	return runSweeps(ctx, p.m, []sweep{
		{
			table: tableTopologyEvents,
			del: func(ctx context.Context, limit int32) (int64, error) {
				return q.DeleteTopologyEventsBefore(ctx, gen.DeleteTopologyEventsBeforeParams{
					EventTime: cutoff,
					Limit:     limit,
				})
			},
		},
		{
			table: tableAuditLog,
			del: func(ctx context.Context, limit int32) (int64, error) {
				return q.DeleteAuditEntriesBefore(ctx, gen.DeleteAuditEntriesBeforeParams{
					At:    cutoff,
					Limit: limit,
				})
			},
		},
		{
			table: tableCheckRuns,
			del: func(ctx context.Context, limit int32) (int64, error) {
				return q.DeleteRunsBefore(ctx, gen.DeleteRunsBeforeParams{
					CreatedAt: cutoff,
					Limit:     limit,
				})
			},
		},
		{
			table: tableMTRSnapshots,
			del: func(ctx context.Context, limit int32) (int64, error) {
				return q.DeletePathSnapshotsBefore(ctx, gen.DeletePathSnapshotsBeforeParams{
					LastSeen: cutoff,
					Limit:    limit,
				})
			},
		},
		{
			table: tableMTREnrichment,
			del: func(ctx context.Context, limit int32) (int64, error) {
				return q.DeleteEnrichmentBefore(ctx, gen.DeleteEnrichmentBeforeParams{
					ResolvedAt: cutoff,
					Limit:      limit,
				})
			},
		},
		{
			table: tableAnnotations,
			del: func(ctx context.Context, limit int32) (int64, error) {
				return q.DeleteAnnotationsBefore(ctx, gen.DeleteAnnotationsBeforeParams{
					StartAt: cutoff,
					Limit:   limit,
				})
			},
		},
		{
			table: tableK8sEvents,
			del: func(ctx context.Context, limit int32) (int64, error) {
				return q.DeleteK8sEventsBefore(ctx, gen.DeleteK8sEventsBeforeParams{
					EventTime: cutoff,
					Limit:     limit,
				})
			},
		},
		{
			// Open incidents are excluded by the query's own NULL semantics,
			// not by anything here -- see the block comment on the table
			// labels above, and the query's own comment.
			table: tableIncidents,
			del: func(ctx context.Context, limit int32) (int64, error) {
				return q.DeleteIncidentsBefore(ctx, gen.DeleteIncidentsBeforeParams{
					ResolvedAt: pgtype.Timestamptz{Time: cutoff, Valid: true},
					Limit:      limit,
				})
			},
		},
		{
			table: tableMaintenance,
			del: func(ctx context.Context, limit int32) (int64, error) {
				return q.DeleteMaintenanceWindowsBefore(ctx, gen.DeleteMaintenanceWindowsBeforeParams{
					EndAt: cutoff,
					Limit: limit,
				})
			},
		},
		// NO webhooks sweep. See the block comment on the table labels above:
		// a webhook row is configuration, and retention is not a
		// deconfiguration policy.
	})
}

// sweep is one prune target: table is the RetentionDeleted metric label and
// the returned per-table map key, del is the deleteBatches-shaped closure
// that drives one table's DELETEs.
type sweep struct {
	table string
	del   func(ctx context.Context, limit int32) (int64, error)
}

// runSweeps runs every sweep in sweeps, unconditionally: an error from one
// sweep never skips the next, so a topology_events failure (say, a statement
// timeout on an unusually large backlog) still leaves the unrelated
// audit_log sweep a chance to make its own progress on the same lock/conn.
// Rows a sweep deleted before failing (deleteBatches's partial total on a
// mid-loop error, or a ctx cancel between batches) are still committed rows,
// so they are credited to that sweep's own RetentionDeleted metric and its
// entry in the returned map either way. Every sweep's error, if any, is
// combined into the single returned error with errors.Join.
func runSweeps(ctx context.Context, m *metrics.Metrics, sweeps []sweep) (map[string]int64, error) {
	deleted := make(map[string]int64, len(sweeps))
	var errs []error
	for _, s := range sweeps {
		n, err := deleteBatches(ctx, s.del)
		m.RetentionDeleted.WithLabelValues(s.table).Add(float64(n))
		deleted[s.table] = n
		if err != nil {
			errs = append(errs, fmt.Errorf("store: prune: %s: %w", s.table, err))
		}
	}
	return deleted, errors.Join(errs...)
}

// releaseLock releases pruneLockKey on conn. The fresh-context reasoning --
// the unlock must outlive a cancelled sweep or this pooled connection carries
// an orphaned lock for the rest of its life -- lives with the shared
// implementation in lock.go.
func (p *Pruner) releaseLock(conn *pgxpool.Conn) {
	releaseAdvisoryLock(conn, pruneLockKey)
}

// deleteBatches calls del repeatedly with limit=pruneBatchSize, pausing
// pruneBatchPause between calls, until del reports fewer rows than requested
// -- proof nothing further is left to delete -- or ctx is cancelled. It
// returns the running total either way.
func deleteBatches(ctx context.Context, del func(ctx context.Context, limit int32) (int64, error)) (int64, error) {
	var total int64
	for {
		n, err := del(ctx, pruneBatchSize)
		if err != nil {
			return total, err
		}
		total += n
		if n < pruneBatchSize {
			return total, nil
		}
		if !pruneSleep(ctx, pruneBatchPause) {
			return total, ctx.Err()
		}
	}
}

// ---------------------------------------------------------------------------
// Connection pool gauge
// ---------------------------------------------------------------------------

// StorePoolConns "state" labels: a closed set of three, enforced only by this
// file's own usage (metrics.go's Help string names the same three). Never
// widened with a pool name, a DSN, or a host.
const (
	poolStateAcquired = "acquired"
	poolStateIdle     = "idle"
	poolStateTotal    = "total"
)

// poolStatInterval is how often PoolStatsPoller resamples. pgxpool.Pool.Stat
// is a cheap in-process read (it touches no connection and issues no query),
// so the cadence is set by how fresh a scrape needs the gauge to be, not by
// cost: 15s keeps it under the shortest scrape interval anyone realistically
// configures.
const poolStatInterval = 15 * time.Second

// poolStats is the three-integer view of pgxpool.Pool.Stat() the gauge needs.
// It is deliberately NOT *pgxpool.Stat: that type's only field is an
// unexported *puddle.Stat with no exported constructor, so a test could not
// fabricate one to assert against -- every method call on a hand-built
// &pgxpool.Stat{} panics on the nil embedded pointer. Reducing the pool to
// three numbers at the seam is what makes the poller unit-testable without a
// database.
type poolStats struct {
	acquired int32
	idle     int32
	total    int32
}

// PoolStatsPoller samples a pool's connection counts into StorePoolConns.
// Without it the gauge is declared and registered but never written -- a
// permanently-zero series that reads, on a dashboard, exactly like a pool
// with no connections.
type PoolStatsPoller struct {
	stats    func() poolStats
	m        *metrics.Metrics
	interval time.Duration
}

// NewPoolStatsPoller returns a poller over db's pool. m must not be nil.
func NewPoolStatsPoller(db *DB, m *metrics.Metrics) *PoolStatsPoller {
	return &PoolStatsPoller{
		stats: func() poolStats {
			s := db.pool.Stat()
			return poolStats{acquired: s.AcquiredConns(), idle: s.IdleConns(), total: s.TotalConns()}
		},
		m:        m,
		interval: poolStatInterval,
	}
}

// Run samples once immediately, then every poolStatInterval until ctx is
// cancelled. The immediate first sample matters: without it a replica scraped
// in its first 15 seconds reports zeros, which is indistinguishable from a
// pool that failed to open.
func (p *PoolStatsPoller) Run(ctx context.Context) {
	p.observe()

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.observe()
		}
	}
}

// observe writes one sample to all three gauge series.
func (p *PoolStatsPoller) observe() {
	s := p.stats()
	p.m.StorePoolConns.WithLabelValues(poolStateAcquired).Set(float64(s.acquired))
	p.m.StorePoolConns.WithLabelValues(poolStateIdle).Set(float64(s.idle))
	p.m.StorePoolConns.WithLabelValues(poolStateTotal).Set(float64(s.total))
}

// pruneJitter returns a random delay in [0, pruneJitterMax). Split out from
// Run so a unit test can assert its bound without waiting out a real sleep.
func pruneJitter() time.Duration {
	return time.Duration(rand.Int64N(int64(pruneJitterMax))) //nolint:gosec // G404: non-security jitter
}

// pruneSleep waits for d or ctx cancellation, whichever comes first, and
// reports whether d elapsed (true) or ctx was done first (false). A
// non-positive d returns immediately, true, unless ctx is already done.
func pruneSleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

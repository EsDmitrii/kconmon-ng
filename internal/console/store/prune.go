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

// The RetentionDeleted "table" labels for the tables this sweep prunes; this is a closed set
// enforced only by this file's own usage.
const (
	tableTopologyEvents = "topology_events"
	tableAuditLog       = "audit_log"
	tableCheckRuns      = "check_runs"
	tableCheckResults   = "check_results"
	tableMTRSnapshots   = "mtr_path_snapshots"
	tableMTREnrichment  = "mtr_hop_enrichment"
	tableAnnotations    = "annotations"
	tableK8sEvents      = "k8s_events"
	tableIncidents      = "incidents"
	tableMaintenance    = "maintenance_windows"
)

// pruneInterval is the steady-state sweep cadence.
const pruneInterval = 24 * time.Hour

// pruneJitterMax bounds the random delay before a replica's very first sweep; several console
// replicas restarting together (a rollout) would otherwise all call pg_try_advisory_lock in the
// same instant.
const pruneJitterMax = 30 * time.Second

// pruneBatchSize bounds one DELETE statement.
const pruneBatchSize = 5000

// pruneBatchPause separates consecutive batches within one sweep, so a long
// catch-up run does not hold the connection -- and with it the advisory lock
// -- in a tight loop against the database.
const pruneBatchPause = 50 * time.Millisecond

// unlockTimeout bounds pg_advisory_unlock, run on a context deliberately
// independent of the sweep's own ctx -- see PruneOnce.
const unlockTimeout = 5 * time.Second

// pruneLockKey is the pg_try_advisory_lock key Pruner uses to serialize sweeps across replicas; it
// is crc32.Checksum([]byte("kconmon-ng.store.Pruner"), crc32.MakeTable(crc32.IEEE)).
const pruneLockKey int64 = 3698486424

// Pruner deletes rows past the retention horizon.
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

// Run sweeps once at start (after a short jitter) and every 24h until ctx is cancelled.
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

// PruneOnce performs one bounded sweep and reports rows deleted per table.
func (p *Pruner) PruneOnce(ctx context.Context) (map[string]int64, error) {
	// pg_try_advisory_lock and its matching pg_advisory_unlock must run on the exact same backend
	// connection.
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
			/* The CHILDREN first, and in batches of their own: deleting a run cascades to every
			   sample it owns, so a batch counted in runs was unbounded in rows. See the query. */
			table: tableCheckResults,
			del: func(ctx context.Context, limit int32) (int64, error) {
				return q.DeleteResultsForRunsBefore(ctx, gen.DeleteResultsForRunsBeforeParams{
					CreatedAt: cutoff,
					Limit:     limit,
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
					EndAt: pgtype.Timestamptz{Time: cutoff, Valid: true},
					Limit: limit,
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
		// NO webhooks sweep and NO alert_rules sweep. See the block comment on
		// the table labels above: both rows are configuration, and retention
		// is not a deconfiguration policy.
	})
}

// sweep is one prune target: table is the RetentionDeleted metric label and
// the returned per-table map key, del is the deleteBatches-shaped closure
// that drives one table's DELETEs.
type sweep struct {
	table string
	del   func(ctx context.Context, limit int32) (int64, error)
}

// runSweeps runs every sweep in sweeps, unconditionally: an error from one sweep never skips the
// next.
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

// releaseLock releases pruneLockKey on conn; the fresh-context reasoning -- the unlock must outlive
// a cancelled sweep or this pooled connection carries an orphaned lock for the rest of its life.
func (p *Pruner) releaseLock(conn *pgxpool.Conn) {
	releaseAdvisoryLock(conn, pruneLockKey)
}

// deleteBatches calls del repeatedly with limit=pruneBatchSize, pausing pruneBatchPause between
// calls.
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

// poolStatInterval is how often PoolStatsPoller resamples.
const poolStatInterval = 15 * time.Second

// poolStats is the three-integer view of pgxpool.Pool.Stat the gauge needs; it is deliberately NOT
// *pgxpool.Stat: that type's only field is an unexported *puddle.Stat with no exported constructor.
type poolStats struct {
	acquired int32
	idle     int32
	total    int32
}

// PoolStatsPoller samples a pool's connection counts into StorePoolConns; without it the gauge is
// declared and registered but never written.
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

// Run samples once immediately, then every poolStatInterval until ctx is cancelled.
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

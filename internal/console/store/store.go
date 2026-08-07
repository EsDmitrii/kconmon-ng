// Package store is the Console's PostgreSQL persistence layer. Per ADR-001 it is
// the ONLY package in this module that imports pgx: every consumer takes one of
// the narrow interfaces declared here, so nothing else grows a database import.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
)

// DB is an open pool plus the migration state that was applied to it.
type DB struct {
	pool *pgxpool.Pool
	// m meters the auth-path queries (auth.go). It is nil for a DB nobody
	// called SetMetrics on -- every integration test, and any embedding with
	// no registry -- and observe is a no-op in that case. eventStore
	// (events.go) demands a non-nil Metrics in its constructor instead; *DB
	// cannot, because Open has no Metrics parameter and is called from
	// contexts (migrations, tests) that have no registry to hand it.
	m *metrics.Metrics
}

// SetMetrics attaches m to db, so every auth-path query it serves is counted
// and timed. Call it once, immediately after Open and before db is shared
// with anything else: it is a plain field write with no synchronization, and
// the only safe moment to do it is while db is still owned by one goroutine.
func (db *DB) SetMetrics(m *metrics.Metrics) {
	db.m = m
}

// observe records the StoreQueryDuration / StoreQueries pair for one query,
// success or failure, which is what makes StoreQueries a true call count. A
// no-op when no Metrics was attached -- see DB.m.
func (db *DB) observe(query string, start time.Time, result string) {
	if db.m == nil {
		return
	}
	db.m.StoreQueryDuration.WithLabelValues(query).Observe(time.Since(start).Seconds())
	db.m.StoreQueries.WithLabelValues(query, result).Inc()
}

// queryResult classifies one query's outcome into the closed {ok, conflict,
// error} label set. A unique-constraint conflict is "conflict", matching
// InsertEvent's use of it for ON CONFLICT DO NOTHING. A miss -- ErrNotFound,
// whether from pgx.ErrNoRows or from a zero-row UPDATE -- is "ok", NOT
// "error": an unknown username or an unrecognized token hash is a normal
// outcome of an authentication attempt, and counting it as a store error
// would make store_queries_total{result="error"} alarm on nothing more than
// someone mistyping a login. Whether that miss mattered is the authn layer's
// question, and AuthRequests{result="invalid"} is where it is answered.
func queryResult(err error) string {
	switch {
	case err == nil, errors.Is(err, ErrNotFound):
		return resultOK
	case errors.Is(err, ErrAlreadyExists):
		return resultConflict
	default:
		return resultError
	}
}

// Open dials dsn, applies migrations when migrate is true, and returns a DB.
// The caller owns Close. A dial failure is returned, NOT swallowed: unlike
// Valkey (cache.NewValkeyBus, whose failure degrades to an in-process bus)
// a half-configured database is not a working state — an operator who set
// database.mode must get a loud boot failure, not silent data loss.
//
// connectTimeout bounds only the initial dial (config parse + the first
// successful connection, proven with a Ping): pgxpool.NewWithConfig itself
// never blocks on the network, so without an explicit Ping an unroutable
// host would return a healthy-looking pool immediately and only fail later,
// mid-request. migrate, when true, runs under ctx (the caller's own
// deadline, if any) rather than connectTimeout, since applying migrations
// can legitimately take longer than a single dial.
func Open(ctx context.Context, dsn string, maxConns int32, connectTimeout time.Duration, migrate bool) (*DB, error) {
	if dsn == "" {
		return nil, errors.New("store: dsn must not be empty")
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("store: parse dsn: %w", err)
	}
	cfg.MaxConns = maxConns

	dialCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(dialCtx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: configure pool: %w", err)
	}

	if err := pool.Ping(dialCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: connect to %s: %w", cfg.ConnConfig.Host, err)
	}

	if migrate {
		if err := runMigrations(ctx, pool); err != nil {
			pool.Close()
			return nil, fmt.Errorf("store: migrate: %w", err)
		}
	}

	return &DB{pool: pool}, nil
}

// Close releases the underlying connection pool. Safe to call more than
// once: it delegates to pgxpool.Pool.Close, which guards itself with a
// sync.Once.
func (db *DB) Close() {
	db.pool.Close()
}

// Ping verifies connectivity to the underlying PostgreSQL server.
func (db *DB) Ping(ctx context.Context) error {
	return db.pool.Ping(ctx)
}

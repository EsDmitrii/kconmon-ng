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
	// m meters the auth-path queries (auth.go).
	m *metrics.Metrics
}

// SetMetrics attaches m to db, so every auth-path query it serves is counted and timed; call it
// once, immediately after Open and before db is shared with anything else.
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

// queryResult classifies one query's outcome into the closed {ok, conflict, error} label set; a
// miss -- ErrNotFound, whether from pgx.ErrNoRows or from a zero-row UPDATE -- is "ok".
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

// Open dials dsn, applies migrations when migrate is true, and returns a DB; a dial failure is
// returned.
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

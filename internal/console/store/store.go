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
)

// DB is an open pool plus the migration state that was applied to it.
type DB struct {
	pool *pgxpool.Pool
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

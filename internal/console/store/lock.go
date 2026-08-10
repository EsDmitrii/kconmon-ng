package store

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
)

// WithAdvisoryLock runs fn while this process holds the PostgreSQL session-level advisory lock
// named by key.
func (db *DB) WithAdvisoryLock(ctx context.Context, key int64, fn func(context.Context) error) (bool, error) {
	conn, err := db.pool.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("store: advisory lock %d: acquire connection: %w", key, err)
	}
	defer conn.Release()

	var locked bool
	if scanErr := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&locked); scanErr != nil {
		return false, fmt.Errorf("store: advisory lock %d: %w", key, scanErr)
	}
	if !locked {
		return false, nil
	}
	defer releaseAdvisoryLock(conn, key)

	return true, fn(ctx)
}

// releaseAdvisoryLock runs pg_advisory_unlock on conn using a fresh, short-lived context rather
// than the caller's.
func releaseAdvisoryLock(conn *pgxpool.Conn, key int64) {
	ctx, cancel := context.WithTimeout(context.Background(), unlockTimeout)
	defer cancel()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", key); err != nil {
		slog.Warn("store: release advisory lock failed", "key", key, "error", err)
	}
}

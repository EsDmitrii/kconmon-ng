package store

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
)

// WithAdvisoryLock runs fn while this process holds the PostgreSQL
// session-level advisory lock named by key, and reports whether the lock was
// actually taken.
//
// (false, nil) is the ORDINARY outcome, not a failure: another replica holds
// key, so fn was not run at all. Callers whose whole point is "at most one
// replica does this" -- the schedule loop, the retention sweep -- treat that
// as a silent skip. (true, err) means fn itself failed; the lock was still
// held and has already been released by the time this returns.
//
// The lock is taken on ONE connection acquired here and released on that same
// connection, because pg_try_advisory_lock is per SESSION and db.pool hands a
// possibly-different pooled connection to every Exec/Query call. fn's own work
// is deliberately NOT confined to that connection: it goes through ordinary
// store methods on the pool, which is correct because the lock's lifetime is a
// property of the session holding it, not of the sessions doing the work.
// (store.Pruner keeps its own inline copy of this dance instead of calling
// here -- see PruneOnce -- because it needs the locked *pgxpool.Conn itself,
// to run its DELETE batches through gen.New(conn); exposing that connection
// through this signature would leak pgx into every caller, which ADR-001
// forbids.)
//
// key must be distinct per purpose. The keys in use in this module are
// goose's DefaultLockID (4097083626, migrations), pruneLockKey (3698486424,
// the retention sweep) and the schedule loop's own, declared in
// internal/console/scheduler.
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

// releaseAdvisoryLock runs pg_advisory_unlock on conn using a fresh,
// short-lived context rather than the caller's: the unlock must still happen
// when that context is already cancelled, or this pooled connection would keep
// holding key for the rest of its life in the pool and every future attempt by
// THIS replica would fail against its own orphaned lock.
func releaseAdvisoryLock(conn *pgxpool.Conn, key int64) {
	ctx, cancel := context.WithTimeout(context.Background(), unlockTimeout)
	defer cancel()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", key); err != nil {
		slog.Warn("store: release advisory lock failed", "key", key, "error", err)
	}
}

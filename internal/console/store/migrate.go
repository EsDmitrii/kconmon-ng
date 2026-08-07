package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

// migrationsFS embeds every migration this binary knows about. A file in
// this directory is immutable once merged (see 00001_topology_events.sql's
// header): a later schema change is a new numbered migration, never an edit
// to an existing one.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// runMigrations applies every embedded migration to pool via goose used as a
// library (not the goose CLI). lock.NewPostgresSessionLocker wraps the run in
// a PostgreSQL session-level advisory lock (pg_advisory_lock), which is what
// ADR-001 means by "advisory-locked": several console replicas starting at
// once serialize their migration attempts instead of racing each other over
// the same schema.
//
// goose.WithSessionLocker is the real provider option for this — the plan
// this package was built from named a goose.WithSessionLocking(bool) helper,
// but no such function exists in github.com/pressly/goose/v3@v3.27.3 (verified
// against the pinned module's source, provider_options.go and lock/postgres.go);
// WithSessionLocker(lock.SessionLocker) is the one that actually enables
// session-level advisory locking, so it is used here instead.
//
// goose needs a database/sql handle: stdlib.OpenDBFromPool wraps pool without
// opening a second connection pool, and closing that handle here releases only
// the thin stdlib adapter, not pool itself — pool is what Open is about to
// hand back to the caller, and must stay alive after this function returns.
func runMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()

	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("create session locker: %w", err)
	}

	// goose scans fsys at its OWN root for migration files, not for a
	// "migrations/" subdirectory — migrationsFS embeds the "migrations/"
	// prefix (needed so //go:embed can name a directory), so it must be
	// re-rooted with fs.Sub before goose will find anything in it.
	migrationsRoot, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("root migrations fs: %w", err)
	}

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrationsRoot, goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("create migration provider: %w", err)
	}

	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

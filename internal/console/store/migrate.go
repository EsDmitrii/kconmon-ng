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

// migrationsFS embeds every migration this binary knows about; a file in this directory is
// immutable once merged (see 00001_topology_events.sql's header).
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// runMigrations applies every embedded migration to pool via goose used as a library (not the goose
// CLI).
func runMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()

	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("create session locker: %w", err)
	}

	// goose scans fsys at its OWN root for migration files, not for a "migrations/" subdirectory.
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

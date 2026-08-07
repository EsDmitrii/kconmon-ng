//go:build integration

package store_test

// TestOpenAppliesMigrations requires a real PostgreSQL.
// Run: docker run --rm -d -p 5432:5432 -e POSTGRES_PASSWORD=test -e POSTGRES_DB=kconmon postgres:17-alpine
// Then: KCONMON_TEST_DATABASE_DSN='postgres://postgres:test@127.0.0.1:5432/kconmon?sslmode=disable' \
//       go test -tags=integration ./internal/console/store/... -v

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

const connectTimeout = 10 * time.Second

// testDSN returns the DSN from KCONMON_TEST_DATABASE_DSN, skipping the test
// when it is unset.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("KCONMON_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("KCONMON_TEST_DATABASE_DSN not set; see docker command in this test's comment")
	}
	return dsn
}

// dropSchema wipes every table this package's migrations create (plus
// goose_db_version) so the file is re-runnable: every test in this file
// shares one database (there is no per-test schema), so each must leave it
// as it found it.
func dropSchema(t *testing.T, dsn string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("dropSchema: connect: %v", err)
	}
	defer pool.Close()

	const tables = `check_results, check_runs, topology_events, audit_log, api_tokens, role_bindings, roles, users, goose_db_version`
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS `+tables); err != nil {
		t.Fatalf("dropSchema: %v", err)
	}
}

// tableExists reports whether name is present in information_schema.tables
// for the current (public) schema.
func tableExists(ctx context.Context, t *testing.T, dsn, name string) bool {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("tableExists: connect: %v", err)
	}
	defer pool.Close()

	var exists bool
	err = pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name = $1
	)`, name).Scan(&exists)
	if err != nil {
		t.Fatalf("tableExists: query: %v", err)
	}
	return exists
}

// TestOpenAppliesMigrations asserts Open(migrate=true) creates topology_events.
func TestOpenAppliesMigrations(t *testing.T) {
	dsn := testDSN(t)
	dropSchema(t, dsn)
	t.Cleanup(func() { dropSchema(t, dsn) })

	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()

	db, err := store.Open(ctx, dsn, 5, connectTimeout, true)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if !tableExists(ctx, t, dsn, "topology_events") {
		t.Error("Open(migrate=true) did not create topology_events")
	}
}

// TestOpenMigrateIsIdempotent asserts calling Open twice with migrate=true
// against the same database does not error the second time (goose_db_version
// already records 00001 as applied).
func TestOpenMigrateIsIdempotent(t *testing.T) {
	dsn := testDSN(t)
	dropSchema(t, dsn)
	t.Cleanup(func() { dropSchema(t, dsn) })

	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()

	db1, err := store.Open(ctx, dsn, 5, connectTimeout, true)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	db1.Close()

	db2, err := store.Open(ctx, dsn, 5, connectTimeout, true)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer db2.Close()

	if !tableExists(ctx, t, dsn, "topology_events") {
		t.Error("topology_events missing after second Open")
	}
}

// TestOpenWithoutMigrateDoesNotCreateSchema asserts Open(migrate=false)
// against a fresh database leaves it untouched.
func TestOpenWithoutMigrateDoesNotCreateSchema(t *testing.T) {
	dsn := testDSN(t)
	dropSchema(t, dsn)
	t.Cleanup(func() { dropSchema(t, dsn) })

	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()

	db, err := store.Open(ctx, dsn, 5, connectTimeout, false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if tableExists(ctx, t, dsn, "topology_events") {
		t.Error("Open(migrate=false) created topology_events on a fresh database")
	}
}

//go:build integration

package scheduler_test

// These tests require a real PostgreSQL: the whole point is the ADVISORY LOCK,
// which has no in-process equivalent to fake.
// Run: docker run --rm -d -p 5432:5432 -e POSTGRES_PASSWORD=test -e POSTGRES_DB=kconmon postgres:17-alpine
// Then: KCONMON_TEST_DATABASE_DSN='postgres://postgres:test@127.0.0.1:5432/kconmon?sslmode=disable' \
//       go test -tags=integration -race ./internal/console/scheduler/... -v

import (
	"context"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/checks"
	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/scheduler"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

const connectTimeout = 10 * time.Second

// testDSN returns the DSN from KCONMON_TEST_DATABASE_DSN, skipping when it is
// unset -- same convention as package store's own integration tests -- and
// then swaps the database name for a scheduler-owned one it creates on the
// same server. `go test ./internal/console/...` runs PACKAGES in parallel,
// and package store's integration tests dropSchema the shared database
// exactly as this file does: two packages resetting one database race each
// other into spurious failures (caught by the M4 final gate). Isolation by
// database, not by -p 1, so the whole-tree integration invocation stays
// parallel and honest.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("KCONMON_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("KCONMON_TEST_DATABASE_DSN not set; see docker command in this file's comment")
	}

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse KCONMON_TEST_DATABASE_DSN: %v", err)
	}
	const ownDB = "kconmon_scheduler_test"
	if strings.Trim(u.Path, "/") == ownDB {
		return dsn // already pointed at our database
	}

	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect for database create: %v", err)
	}
	defer admin.Close()
	// No IF NOT EXISTS for CREATE DATABASE; a duplicate is the fine case.
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+ownDB); err != nil &&
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("create %s: %v", ownDB, err)
	}

	u.Path = "/" + ownDB
	return u.String()
}

// dropSchema wipes every table the migrations create so this file is
// re-runnable and shares the database cleanly with the other suites.
//
// This is the one deliberate pgx use outside internal/console/store: the
// "store is the sole pgx importer" boundary (ADR-001) protects PRODUCTION
// data paths, and store's public API rightly has no drop-every-table
// operation for a test harness to call. Integration-tagged test code
// resetting its own schema is outside that boundary's blast radius.
func dropSchema(t *testing.T, dsn string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("dropSchema: connect: %v", err)
	}
	defer pool.Close()

	_, err = pool.Exec(ctx, `DROP TABLE IF EXISTS
		webhooks, maintenance_windows, incidents, k8s_events,
		mtr_path_snapshots, mtr_hop_enrichment, annotations,
		check_results, check_runs, check_schedules, check_definitions, targets,
		audit_log, role_bindings, roles, api_tokens, users, topology_events,
		goose_db_version CASCADE`)
	if err != nil {
		t.Fatalf("dropSchema: %v", err)
	}
}

// openDB opens a migrated *store.DB against a freshly dropped schema.
func openDB(t *testing.T, dsn string) *store.DB {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()

	db, err := store.Open(ctx, dsn, 5, connectTimeout, true)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

// countingRunner counts Start calls across BOTH scheduler instances (one
// shared instance, deliberately: the assertion is fleet-wide, "exactly one
// fire", not per-replica). Get and ReapStuckRuns come from the embedded real
// *checks.Runner, so the reaper test exercises Task 12's actual sweep against
// the actual table.
type countingRunner struct {
	*checks.Runner
	mu      sync.Mutex
	started int
}

func (c *countingRunner) Start(_ context.Context, _ checks.Spec, _ authz.Subject) (string, error) { //nolint:gocritic // Subject is a value type by design
	c.mu.Lock()
	defer c.mu.Unlock()
	c.started++
	return uuid.NewString(), nil
}

func (c *countingRunner) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.started
}

// fixedTopology is a stand-in for the controller. The definitions seeded below
// use selection "all", which needs no snapshot at all, so this only exists to
// satisfy Deps.
type fixedTopology struct{}

func (fixedTopology) Topology(context.Context) (*controllerclient.Topology, error) {
	return &controllerclient.Topology{}, nil
}

func newScheduler(t *testing.T, db *store.DB, runner scheduler.Runner) *scheduler.Scheduler {
	t.Helper()
	return scheduler.New(scheduler.Deps{
		Lock: db, Store: db, Runner: runner, Topology: fixedTopology{},
		Metrics:  metrics.New("kconmon_ng_sched_it_"+uuid.NewString()[:8], prometheus.NewRegistry()),
		Interval: time.Second,
	})
}

// seedDueInterval creates an enabled definition plus an enabled interval
// schedule that is already due, and returns the schedule id.
func seedDueInterval(t *testing.T, db *store.DB, interval time.Duration) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()

	def, err := db.CreateDefinition(ctx, store.DefinitionInput{
		Name: "sched-it-" + uuid.NewString()[:8], SourceSelection: "all",
		DestinationKind: "adhoc", DestinationAddress: "10.0.0.1:53",
		CheckType: "tcp", Plane: "pod", Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateDefinition: %v", err)
	}

	past := time.Now().UTC().Add(-time.Minute)
	sched, err := db.CreateSchedule(ctx, store.ScheduleInput{
		DefinitionID: def.ID, Kind: "interval", IntervalNs: int64(interval),
		Enabled: true, NextFireAt: &past,
	})
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	return sched.ID
}

// TestTwoInstancesFireADueScheduleExactlyOnce is the reason this test needs a
// real database: the advisory lock is what makes N replicas behave like one
// scheduler, and nothing in-process can stand in for it.
//
// Two Scheduler instances, two independent *store.DB pools (two sets of
// PostgreSQL sessions -- one pool would still prove the lock works, but two
// is what a real deployment looks like), one shared runner, several
// overlapping ticks. A schedule with a long interval is due exactly once, so
// any leak in the mutual exclusion shows up as a second Start.
func TestTwoInstancesFireADueScheduleExactlyOnce(t *testing.T) {
	dsn := testDSN(t)
	dropSchema(t, dsn)
	t.Cleanup(func() { dropSchema(t, dsn) })

	dbA := openDB(t, dsn)
	dbB := openDB(t, dsn)

	schedID := seedDueInterval(t, dbA, time.Hour)

	runner := &countingRunner{Runner: checks.NewRunner(nil, nil, nil, dbA, metrics.New("kconmon_ng_it_runner", prometheus.NewRegistry()))}
	a := newScheduler(t, dbA, runner)
	b := newScheduler(t, dbB, runner)

	// Several rounds of simultaneous ticks: each round both instances race for
	// the same key, and only one may ever get through to the schedule.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for range 5 {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); a.Tick(ctx) }()
		go func() { defer wg.Done(); b.Tick(ctx) }()
		wg.Wait()
	}

	if got := runner.count(); got != 1 {
		t.Fatalf("Start called %d times across two instances and five rounds, want exactly 1", got)
	}

	// And the row itself moved on: fired once, next fire an hour out.
	sched, err := dbA.GetSchedule(ctx, schedID)
	if err != nil {
		t.Fatalf("GetSchedule: %v", err)
	}
	if sched.LastFiredAt == nil {
		t.Error("lastFiredAt is nil, want the fire recorded")
	}
	if sched.NextFireAt == nil || !sched.NextFireAt.After(time.Now().UTC().Add(30*time.Minute)) {
		t.Errorf("nextFireAt = %v, want roughly an hour out", sched.NextFireAt)
	}
}

// TestReaperFinishesAStuckRunUnderTheLock covers the other half of a tick:
// a run row left "running" by a console that died mid-fan-out is force-
// finished as "cancelled", and it happens on the leader only.
func TestReaperFinishesAStuckRunUnderTheLock(t *testing.T) {
	dsn := testDSN(t)
	dropSchema(t, dsn)
	t.Cleanup(func() { dropSchema(t, dsn) })

	db := openDB(t, dsn)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	run, err := db.CreateRun(ctx, uuid.NewString(), "tcp", "pod", []byte(`{}`), "user", "u1", 1)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err = db.MarkRunStarted(ctx, run.ID); err != nil {
		t.Fatalf("MarkRunStarted: %v", err)
	}

	// Back-date it well past checks' own maxRunLifetime ceiling (the longest
	// deadline Start can hand a run, plus its reap slack -- under two hours);
	// four hours is comfortably beyond it without this test needing to know
	// the exact number.
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if _, err = pool.Exec(ctx, `UPDATE check_runs SET created_at = now() - interval '4 hours' WHERE id = $1`, run.ID); err != nil {
		t.Fatalf("back-date run: %v", err)
	}

	realRunner := checks.NewRunner(nil, nil, nil, db, metrics.New("kconmon_ng_it_reaper", prometheus.NewRegistry()))
	s := newScheduler(t, db, realRunner)

	s.Tick(ctx)

	after, err := db.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if after.Status != "cancelled" {
		t.Fatalf("status = %q, want cancelled after the reaper swept", after.Status)
	}
	if after.FinishedAt == nil {
		t.Error("finishedAt is nil, want it stamped by the reaper")
	}
}

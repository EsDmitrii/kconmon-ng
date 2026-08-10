//go:build integration

package scheduler_test

// These tests require a real PostgreSQL: the whole point is the ADVISORY LOCK, which has no
// in-process equivalent to fake.

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

// testDSN returns the DSN from KCONMON_TEST_DATABASE_DSN, skipping when it is unset.
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

// dropSchema wipes every table the migrations create so this file is re-runnable and shares the
// database cleanly with the other suites.
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
		alert_rules, webhooks, maintenance_windows, incidents, k8s_events,
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

// countingRunner counts Start calls across BOTH scheduler instances (one shared instance,
// deliberately: the assertion is fleet-wide, "exactly one fire", not per-replica).
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

// TestTwoInstancesFireADueScheduleExactlyOnce is the reason this test needs a real database; two
// Scheduler instances, two independent *store.DB pools (two sets of PostgreSQL sessions -- one pool
// would still prove the lock works, but two is what a real deployment looks like).
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

	// Back-date it well past checks' own maxRunLifetime ceiling (the longest deadline Start can hand a
	// run, plus its reap slack -- under two hours).
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

func TestAFailingScheduleRecordsItAgainstPostgres(t *testing.T) {
	dsn := testDSN(t)
	dropSchema(t, dsn)
	t.Cleanup(func() { dropSchema(t, dsn) })

	db := openDB(t, dsn)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	schedID := seedDueInterval(t, db, time.Hour)

	// Delete the definition out from under the schedule -- the exact shape the finding describes.
	sched, err := db.GetSchedule(ctx, schedID)
	if err != nil {
		t.Fatalf("GetSchedule: %v", err)
	}
	target, err := db.CreateTarget(ctx, store.TargetInput{
		Name: "gone-" + uuid.NewString()[:8], Kind: "host", Address: "10.0.0.9",
	})
	if err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	if _, err = db.UpdateDefinition(ctx, sched.DefinitionID, store.DefinitionInput{
		Name: "sched-it-broken-" + uuid.NewString()[:8], SourceSelection: "all",
		DestinationKind: "target", DestinationTargetID: target.ID,
		CheckType: "tcp", Plane: "pod", Enabled: true,
	}); err != nil {
		t.Fatalf("UpdateDefinition: %v", err)
	}
	// Drop the target row directly: DeleteTarget is refused by ON DELETE RESTRICT while the definition
	// points at it.
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if _, err = pool.Exec(ctx,
		`ALTER TABLE check_definitions DROP CONSTRAINT check_definitions_destination_target_id_fkey`); err != nil {
		t.Fatalf("drop fk: %v", err)
	}
	if _, err = pool.Exec(ctx, `DELETE FROM targets WHERE id = $1`, target.ID); err != nil {
		t.Fatalf("delete target: %v", err)
	}

	// The failure half never gets that far -- specFor refuses before Start is called, which is the
	// point.
	runner := &countingRunner{Runner: checks.NewRunner(nil, nil, nil, db, metrics.New("kconmon_ng_it_lasterr", prometheus.NewRegistry()))}
	newScheduler(t, db, runner).Tick(ctx)

	after, err := db.GetSchedule(ctx, schedID)
	if err != nil {
		t.Fatalf("GetSchedule after the tick: %v", err)
	}
	if after.LastError == "" {
		t.Fatal("LastError is empty after a fire that could not resolve its target")
	}
	if !strings.Contains(after.LastError, "target") {
		t.Errorf("LastError = %q, want it to name the destination target failure", after.LastError)
	}
	if after.LastErrorAt == nil {
		t.Error("LastErrorAt is nil beside a non-empty LastError")
	}
	// The cadence advanced anyway -- which is the whole reason the error column
	// had to exist rather than the row simply staying due.
	if after.LastFiredAt == nil || after.NextFireAt == nil {
		t.Errorf("a failed fire left LastFiredAt=%v NextFireAt=%v, want both stamped",
			after.LastFiredAt, after.NextFireAt)
	}

	// Point the definition back at something that works and let it fire again:
	// the row goes green, and NOTHING but a real fire could have done that.
	if _, err = db.UpdateDefinition(ctx, sched.DefinitionID, store.DefinitionInput{
		Name: "sched-it-fixed-" + uuid.NewString()[:8], SourceSelection: "all",
		DestinationKind: "adhoc", DestinationAddress: "10.0.0.1:53",
		CheckType: "tcp", Plane: "pod", Enabled: true,
	}); err != nil {
		t.Fatalf("UpdateDefinition (repair): %v", err)
	}
	past := time.Now().UTC().Add(-time.Minute)
	if _, err = db.UpdateSchedule(ctx, schedID, store.ScheduleInput{
		DefinitionID: sched.DefinitionID, Kind: "interval", IntervalNs: int64(time.Hour),
		Enabled: true, NextFireAt: &past,
	}); err != nil {
		t.Fatalf("UpdateSchedule: %v", err)
	}
	// The edit alone must NOT have cleared it.
	edited, err := db.GetSchedule(ctx, schedID)
	if err != nil {
		t.Fatalf("GetSchedule after the edit: %v", err)
	}
	if edited.LastError == "" {
		t.Error("the edit cleared LastError; only a fire may do that")
	}

	newScheduler(t, db, runner).Tick(ctx)

	healthy, err := db.GetSchedule(ctx, schedID)
	if err != nil {
		t.Fatalf("GetSchedule after the good fire: %v", err)
	}
	if healthy.LastError != "" || healthy.LastErrorAt != nil {
		t.Errorf("after a good fire = %q/%v, want the empty pair", healthy.LastError, healthy.LastErrorAt)
	}
}

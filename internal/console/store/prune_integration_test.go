//go:build integration

package store_test

// TestPruneOnce* / TestRun* require a real PostgreSQL.

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

const retention90d = 90 * 24 * time.Hour

// newPrunerDB opens a *store.DB with migrations applied, dropping and re-creating the schema first.
func newPrunerDB(t *testing.T) (*store.DB, string) {
	t.Helper()
	dsn := testDSN(t)
	dropSchema(t, dsn)
	t.Cleanup(func() { dropSchema(t, dsn) })

	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()

	db, err := store.Open(ctx, dsn, 5, connectTimeout, true)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(db.Close)
	return db, dsn
}

// seedTopologyEventsAtAges inserts one row per element of agesDays; it bulk-inserts via UNNEST in a
// single round trip rather than one store.EventStore.InsertEvent call per row.
func seedTopologyEventsAtAges(t *testing.T, dsn string, seqBase int64, agesDays []int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("seedTopologyEventsAtAges: connect: %v", err)
	}
	defer pool.Close()

	n := len(agesDays)
	seqs := make([]int64, n)
	times := make([]time.Time, n)
	now := time.Now()
	for i, age := range agesDays {
		seqs[i] = seqBase + int64(i)
		times[i] = now.Add(-time.Duration(age) * 24 * time.Hour)
	}

	_, err = pool.Exec(ctx, `
		INSERT INTO topology_events (event_seq, event_time, type, severity, scope, summary, details)
		SELECT seq, ts, 'check_observed', 'info', 'seed', 'seed', '{}'::jsonb
		FROM UNNEST($1::bigint[], $2::timestamptz[]) AS u(seq, ts)
	`, seqs, times)
	if err != nil {
		t.Fatalf("seedTopologyEventsAtAges: insert: %v", err)
	}
}

// countTopologyEvents returns the current row count of topology_events.
func countTopologyEvents(t *testing.T, dsn string) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("countTopologyEvents: connect: %v", err)
	}
	defer pool.Close()

	var n int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM topology_events`).Scan(&n); err != nil {
		t.Fatalf("countTopologyEvents: query: %v", err)
	}
	return n
}

// agesSpanning200Days returns 10 ages just inside a 90d retention window and 10 just outside it.
func agesSpanning200Days() []int {
	ages := make([]int, 0, 20)
	for d := 1; d <= 10; d++ {
		ages = append(ages, d) // well inside 90d: kept
	}
	for d := 191; d <= 200; d++ {
		ages = append(ages, d) // well past 90d: deleted
	}
	return ages
}

// TestPruneOnceDeletesRowsPastRetention seeds 20 rows spanning roughly 200
// days and asserts a 90d-retention PruneOnce deletes exactly the 10 older
// ones, and that a second sweep against the now-clean table deletes 0.
func TestPruneOnceDeletesRowsPastRetention(t *testing.T) {
	db, dsn := newPrunerDB(t)
	p := store.NewPruner(db, retention90d, newTestMetrics())
	seedTopologyEventsAtAges(t, dsn, 1, agesSpanning200Days())

	ctx := context.Background()

	deleted, err := p.PruneOnce(ctx)
	if err != nil {
		t.Fatalf("PruneOnce: %v", err)
	}
	if got := deleted["topology_events"]; got != 10 {
		t.Fatalf("PruneOnce: deleted[topology_events] = %d, want 10", got)
	}
	if got := countTopologyEvents(t, dsn); got != 10 {
		t.Fatalf("rows remaining after PruneOnce = %d, want 10", got)
	}

	second, err := p.PruneOnce(ctx)
	if err != nil {
		t.Fatalf("second PruneOnce: %v", err)
	}
	if got := second["topology_events"]; got != 0 {
		t.Fatalf("second PruneOnce: deleted[topology_events] = %d, want 0", got)
	}
}

// TestPruneOnceSweepsEveryTable is the whole sweep list in one call.
func TestPruneOnceSweepsEveryTable(t *testing.T) {
	db, dsn := newPrunerDB(t)
	p := store.NewPruner(db, retention90d, newTestMetrics())
	ctx := context.Background()

	expired := time.Now().UTC().Add(-200 * 24 * time.Hour)
	current := time.Now().UTC()

	// topology_events (by event_time) and audit_log are covered by the other
	// tests in this file; seed topology_events here too so the returned map
	// carries a non-zero entry for it as well.
	seedTopologyEventsAtAges(t, dsn, 1, []int{200, 1})

	// check_runs (by created_at). created_at has a column DEFAULT of now(), so
	// the expired run is aged with one direct UPDATE -- the store API has no
	// way to backdate a run, and inventing one for a test would be worse.
	oldRun := uuid.NewString()
	if _, err := db.CreateRun(ctx, oldRun, "mtr", "pod", json.RawMessage(`{}`), "user", "admin", 1, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("CreateRun(old, time.Now().Add(time.Hour)): %v", err)
	}
	if _, err := db.CreateRun(ctx, uuid.NewString(), "mtr", "pod", json.RawMessage(`{}`), "user", "admin", 1, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("CreateRun(new, time.Now().Add(time.Hour)): %v", err)
	}
	pool, poolErr := pgxpool.New(ctx, dsn)
	if poolErr != nil {
		t.Fatalf("connect: %v", poolErr)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `UPDATE check_runs SET created_at = $1 WHERE id = $2`, expired, oldRun); err != nil {
		t.Fatalf("backdate the old run: %v", err)
	}

	// mtr_path_snapshots (by last_seen).
	for _, tc := range []struct {
		hops []store.PathHop
		at   time.Time
	}{{pathAB(), expired}, {pathAC(), current}} {
		if _, _, err := db.UpsertPathSnapshot(ctx, snapshotInput("node-a", "edge-gw", tc.hops, tc.at)); err != nil {
			t.Fatalf("UpsertPathSnapshot: %v", err)
		}
	}

	// mtr_hop_enrichment (by resolved_at).
	if err := db.PutEnrichment(ctx, []store.Enrichment{
		{IP: "10.0.0.1", ResolvedAt: expired},
		{IP: "10.0.0.2", ResolvedAt: current},
	}); err != nil {
		t.Fatalf("PutEnrichment: %v", err)
	}

	// annotations (by start_at).
	for _, at := range []time.Time{expired, current} {
		if _, err := db.CreateAnnotation(ctx, store.AnnotationInput{
			StartAt: at, Text: "mark", CreatedBy: "user:admin",
		}); err != nil {
			t.Fatalf("CreateAnnotation: %v", err)
		}
	}

	// k8s_events (by event_time).
	for i, at := range []time.Time{expired, current} {
		if _, err := db.InsertK8sEvent(ctx, store.K8sEventInput{
			UID:             "prune-uid-" + strconv.Itoa(i),
			ResourceVersion: "1",
			EventTime:       at,
			Kind:            "Node",
			Name:            "node-a",
			Reason:          "NodeNotReady",
			Type:            "Warning",
			Message:         "seeded",
		}); err != nil {
			t.Fatalf("InsertK8sEvent: %v", err)
		}
	}

	// incidents (by resolved_at); THREE rows, not two: one resolved past the horizon (swept), one
	// resolved inside it (kept).
	for i, tc := range []struct {
		title      string
		resolvedAt *time.Time
	}{
		{"resolved-long-ago", &expired},
		{"resolved-recently", &current},
		{"never-closed", nil},
	} {
		created, err := db.CreateIncident(ctx, store.IncidentInput{
			Title: tc.title, FromAt: expired, CreatedBy: "user:admin",
		})
		if err != nil {
			t.Fatalf("CreateIncident(%d): %v", i, err)
		}
		if tc.resolvedAt != nil {
			if _, _, err := db.UpdateIncidentStatus(ctx, created.ID,
				store.IncidentStatusResolved, tc.resolvedAt); err != nil {
				t.Fatalf("resolve %s: %v", tc.title, err)
			}
		}
	}

	// maintenance_windows (by end_at).
	for _, tc := range []struct {
		scope string
		start time.Time
		end   time.Time
	}{
		{"expired-window", expired, expired.Add(time.Hour)},
		{"current-window", current, current.Add(time.Hour)},
	} {
		if _, err := db.CreateMaintenanceWindow(ctx, store.MaintenanceInput{
			Scope: tc.scope, StartAt: tc.start, EndAt: tc.end, Reason: "seeded", CreatedBy: "user:admin",
		}); err != nil {
			t.Fatalf("CreateMaintenanceWindow(%s): %v", tc.scope, err)
		}
	}

	// webhooks: configuration, NEVER swept. Created before the sweep and
	// asserted present after it.
	hook, err := db.CreateWebhook(ctx, store.WebhookInput{
		Name:      "ops-slack",
		URL:       "https://hooks.example.test/x",
		Events:    []string{store.WebhookEventIncidentCreated},
		SecretEnc: []byte{0x01, 0x02, 0x03},
		Enabled:   true,
	})
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}

	// alert_rules: configuration, NEVER swept -- aged past the horizon on both
	// of its time columns so no plausible sweep could have spared it by
	// accident.
	rule, err := db.CreateAlertRule(ctx, alertRuleInput("PairLossHigh"))
	if err != nil {
		t.Fatalf("CreateAlertRule: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE alert_rules SET created_at = $1, updated_at = $1, last_synced_at = $1 WHERE id = $2`,
		expired, rule.ID); err != nil {
		t.Fatalf("backdate the alert rule: %v", err)
	}

	deleted, err := p.PruneOnce(ctx)
	if err != nil {
		t.Fatalf("PruneOnce: %v", err)
	}

	for table, want := range map[string]int64{
		"topology_events":     1,
		"check_runs":          1,
		"mtr_path_snapshots":  1,
		"mtr_hop_enrichment":  1,
		"annotations":         1,
		"k8s_events":          1,
		"incidents":           1,
		"maintenance_windows": 1,
	} {
		got, ok := deleted[table]
		if !ok {
			t.Errorf("PruneOnce reported no entry for %s: the sweep is missing from the list", table)
			continue
		}
		if got != want {
			t.Errorf("PruneOnce: deleted[%s] = %d, want %d", table, got, want)
		}
	}
	// audit_log has no expired rows here, but its sweep must still have run
	// and reported zero rather than being absent.
	if _, ok := deleted["audit_log"]; !ok {
		t.Error("PruneOnce reported no entry for audit_log")
	}
	if _, ok := deleted["webhooks"]; ok {
		t.Error("PruneOnce reported a webhooks entry: that table has no sweep and must not gain one")
	}
	if _, ok := deleted["alert_rules"]; ok {
		t.Error("PruneOnce reported an alert_rules entry: that table has no sweep and must not gain one")
	}
	/* check_results has a sweep OF ITS OWN, ahead of check_runs: a batch counted in runs was
	   unbounded in the rows it cascaded (one interval run owns up to 200 000 samples). */
	if _, ok := deleted["check_results"]; !ok {
		t.Error("PruneOnce reported no entry for check_results: the cascade is unbounded again")
	}
	if len(deleted) != 10 {
		t.Errorf("PruneOnce reported %d tables, want 10: %v", len(deleted), deleted)
	}

	// The survivors, read back through the store rather than counted in SQL.
	snaps, err := db.ListPathSnapshots(ctx, store.SnapshotFilter{})
	if err != nil {
		t.Fatalf("ListPathSnapshots: %v", err)
	}
	if len(snaps.Snapshots) != 1 || !snaps.Snapshots[0].LastSeen.Equal(current.Truncate(time.Microsecond)) {
		t.Errorf("after the sweep %d snapshots remain, want just the current one", len(snaps.Snapshots))
	}
	cache, err := db.GetEnrichment(ctx, []string{"10.0.0.1", "10.0.0.2"})
	if err != nil {
		t.Fatalf("GetEnrichment: %v", err)
	}
	if _, gone := cache["10.0.0.1"]; gone {
		t.Error("the expired enrichment row survived the sweep")
	}
	if _, kept := cache["10.0.0.2"]; !kept {
		t.Error("the fresh enrichment row was swept")
	}
	anns, err := db.ListAnnotations(ctx, store.AnnotationFilter{})
	if err != nil {
		t.Fatalf("ListAnnotations: %v", err)
	}
	if len(anns.Annotations) != 1 {
		t.Errorf("after the sweep %d annotations remain, want 1", len(anns.Annotations))
	}
	k8s, err := db.ListK8sEvents(ctx, store.K8sEventFilter{})
	if err != nil {
		t.Fatalf("ListK8sEvents: %v", err)
	}
	if len(k8s.Events) != 1 {
		t.Errorf("after the sweep %d k8s events remain, want 1", len(k8s.Events))
	}
	// The recently-resolved one AND the open one: two survivors, which is the
	// open-incidents rule stated as a row count.
	incidents, err := db.ListIncidents(ctx, store.IncidentFilter{})
	if err != nil {
		t.Fatalf("ListIncidents: %v", err)
	}
	if len(incidents.Incidents) != 2 {
		t.Errorf("after the sweep %d incidents remain, want 2 (the recently-resolved one and the open one)",
			len(incidents.Incidents))
	}
	openOnes, err := db.ListIncidents(ctx, store.IncidentFilter{Status: store.IncidentStatusOpen})
	if err != nil {
		t.Fatalf("ListIncidents(open): %v", err)
	}
	if len(openOnes.Incidents) != 1 || openOnes.Incidents[0].Title != "never-closed" {
		t.Errorf("the 200-day-old OPEN incident did not survive the sweep: %+v", openOnes.Incidents)
	}
	windows, err := db.ListMaintenanceWindows(ctx, store.MaintenanceFilter{})
	if err != nil {
		t.Fatalf("ListMaintenanceWindows: %v", err)
	}
	if len(windows.Windows) != 1 || windows.Windows[0].Scope != "current-window" {
		t.Errorf("after the sweep %+v remain, want just the current window", windows.Windows)
	}
	// The whole point of the webhooks exclusion, as a read-back.
	if _, err := db.GetWebhook(ctx, hook.ID); err != nil {
		t.Errorf("the configured webhook was swept: %v -- webhooks have no retention sweep", err)
	}
	// And of the alert_rules exclusion, on a row 200 days stale on every
	// column a sweep could have keyed off.
	if _, err := db.GetAlertRule(ctx, rule.ID); err != nil {
		t.Errorf("the configured alert rule was swept: %v -- alert_rules has no retention sweep", err)
	}
}

// TestPruneOnceCrossesBatchBoundary seeds 12000 rows, all past retention -- more than
// pruneBatchSize (5000).
func TestPruneOnceCrossesBatchBoundary(t *testing.T) {
	db, dsn := newPrunerDB(t)
	p := store.NewPruner(db, retention90d, newTestMetrics())

	const n = 12000
	ages := make([]int, n)
	for i := range ages {
		ages[i] = 200 // comfortably past the 90d cutoff
	}
	seedTopologyEventsAtAges(t, dsn, 1, ages)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	deleted, err := p.PruneOnce(ctx)
	if err != nil {
		t.Fatalf("PruneOnce: %v", err)
	}
	if got := deleted["topology_events"]; got != n {
		t.Fatalf("PruneOnce: deleted[topology_events] = %d, want %d", got, n)
	}
	if got := countTopologyEvents(t, dsn); got != 0 {
		t.Fatalf("rows remaining = %d, want 0", got)
	}
}

// TestPruneOnceConcurrentReplicasDoNotDoubleWork is the multi-replica guarantee; exactly one must
// do the deleting.
func TestPruneOnceConcurrentReplicasDoNotDoubleWork(t *testing.T) {
	dsn := testDSN(t)
	dropSchema(t, dsn)
	t.Cleanup(func() { dropSchema(t, dsn) })

	openCtx, openCancel := context.WithTimeout(context.Background(), connectTimeout)
	db1, err := store.Open(openCtx, dsn, 5, connectTimeout, true)
	openCancel()
	if err != nil {
		t.Fatalf("Open db1: %v", err)
	}
	t.Cleanup(db1.Close)

	// migrate=false: db1's Open call above already applied every migration,
	// and running goose's own advisory-locked migration path a second time
	// here would just be redundant.
	openCtx2, openCancel2 := context.WithTimeout(context.Background(), connectTimeout)
	db2, err := store.Open(openCtx2, dsn, 5, connectTimeout, false)
	openCancel2()
	if err != nil {
		t.Fatalf("Open db2: %v", err)
	}
	t.Cleanup(db2.Close)

	const n = 12000
	ages := make([]int, n)
	for i := range ages {
		ages[i] = 200
	}
	seedTopologyEventsAtAges(t, dsn, 1, ages)

	p1 := store.NewPruner(db1, retention90d, newTestMetrics())
	p2 := store.NewPruner(db2, retention90d, newTestMetrics())

	type outcome struct {
		deleted map[string]int64
		err     error
	}
	runCtx, runCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer runCancel()

	resultCh := make(chan outcome, 1)
	go func() {
		d, err := p1.PruneOnce(runCtx)
		resultCh <- outcome{d, err}
	}()

	// Give p1 a head start: 12000 rows over a 5000-row batch size means at least one pruneBatchPause
	// between batches.
	time.Sleep(30 * time.Millisecond)

	r2, err2 := p2.PruneOnce(runCtx)
	if err2 != nil {
		t.Fatalf("p2.PruneOnce: %v", err2)
	}

	var r1 outcome
	select {
	case r1 = <-resultCh:
	case <-time.After(60 * time.Second):
		t.Fatal("p1.PruneOnce did not complete in time")
	}
	if r1.err != nil {
		t.Fatalf("p1.PruneOnce: %v", r1.err)
	}

	if got := r2["topology_events"]; got != 0 {
		t.Errorf("p2 (the late starter) deleted %d rows, want 0: pruneLockKey should already have been held by p1", got)
	}

	total := r1.deleted["topology_events"] + r2["topology_events"]
	if total != n {
		t.Fatalf("total rows deleted across both replicas = %d, want %d", total, n)
	}
	if got := countTopologyEvents(t, dsn); got != 0 {
		t.Fatalf("rows remaining = %d, want 0", got)
	}
}

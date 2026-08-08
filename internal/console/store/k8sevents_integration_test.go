//go:build integration

package store_test

// TestK8sEvent* / TestListK8sEvents* require a real PostgreSQL.
// Run: docker run --rm -d -p 5432:5432 -e POSTGRES_PASSWORD=test -e POSTGRES_DB=kconmon postgres:17-alpine
// Then: KCONMON_TEST_DATABASE_DSN='postgres://postgres:test@127.0.0.1:5432/kconmon?sslmode=disable' \
//       go test -tags=integration ./internal/console/store/... -v

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// newK8sEventsDB opens a *store.DB with migrations applied, dropping and
// re-creating the schema first -- same convention as newAnnotationsDB; this
// file shares one database with every other file in package store_test, so
// each test must leave it clean.
func newK8sEventsDB(t *testing.T) (*store.DB, string) {
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

// k8sEventInput builds one plausible captured node event.
func k8sEventInput(uid, rv, name string, at time.Time) store.K8sEventInput {
	return store.K8sEventInput{
		UID:             uid,
		ResourceVersion: rv,
		EventTime:       at,
		Kind:            "Node",
		Name:            name,
		Reason:          "NodeNotReady",
		Type:            "Warning",
		Message:         "Node " + name + " status is now: NodeNotReady",
		Count:           1,
	}
}

// TestInsertK8sEventDedupesOnUIDAndResourceVersion is the capture's whole
// idempotence claim (M6 Decision 3). Three inserts, one uid:
//
//   - the first revision inserts;
//   - re-offering the SAME (uid, resourceVersion) -- what a relist after a
//     watch expiry does for every current object -- inserts nothing and is not
//     an error;
//   - a NEW resourceVersion for the same uid DOES insert, because a recurring
//     event is a new revision with a bumped count and the timeline has to be
//     able to show the recurrence. That is why the key is the pair.
func TestInsertK8sEventDedupesOnUIDAndResourceVersion(t *testing.T) {
	db, _ := newK8sEventsDB(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Microsecond)

	const uid = "5f8d1e2a-0000-4000-8000-000000000001"

	inserted, err := db.InsertK8sEvent(ctx, k8sEventInput(uid, "10241", "node-a", at))
	if err != nil {
		t.Fatalf("first InsertK8sEvent: %v", err)
	}
	if !inserted {
		t.Fatal("first InsertK8sEvent reported inserted=false, want true")
	}

	inserted, err = db.InsertK8sEvent(ctx, k8sEventInput(uid, "10241", "node-a", at))
	if err != nil {
		t.Fatalf("relist InsertK8sEvent: %v", err)
	}
	if inserted {
		t.Error("re-offering the same (uid, resourceVersion) reported inserted=true; the relist must dedupe")
	}

	recur := k8sEventInput(uid, "10999", "node-a", at.Add(time.Minute))
	recur.Count = 4
	inserted, err = db.InsertK8sEvent(ctx, recur)
	if err != nil {
		t.Fatalf("recurrence InsertK8sEvent: %v", err)
	}
	if !inserted {
		t.Error("a new resourceVersion for the same uid reported inserted=false; a recurrence is a new revision")
	}

	page, err := db.ListK8sEvents(ctx, store.K8sEventFilter{})
	if err != nil {
		t.Fatalf("ListK8sEvents: %v", err)
	}
	if len(page.Events) != 2 {
		t.Fatalf("the table holds %d rows, want 2 (one per revision)", len(page.Events))
	}
	// Newest first: the recurrence, then the original.
	if page.Events[0].ResourceVersion != "10999" || page.Events[0].Count != 4 {
		t.Errorf("newest row = %+v, want the resourceVersion 10999 / count 4 recurrence", page.Events[0])
	}
	if page.Events[1].ResourceVersion != "10241" || page.Events[1].Count != 1 {
		t.Errorf("older row = %+v, want the original revision", page.Events[1])
	}
}

// TestInsertK8sEventRoundTripsEveryField asserts nothing is dropped or
// transposed between the input struct, the ten INSERT parameters and the read
// back -- ten same-typed columns in a row is exactly the shape a mis-ordered
// parameter list hides in.
func TestInsertK8sEventRoundTripsEveryField(t *testing.T) {
	db, _ := newK8sEventsDB(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Microsecond)

	in := store.K8sEventInput{
		UID:             "9d2b7c10-0000-4000-8000-00000000abcd",
		ResourceVersion: "77123",
		EventTime:       at,
		Kind:            "Pod",
		Name:            "kconmon-agent-7fz9x",
		Namespace:       "kconmon",
		Reason:          "BackOff",
		Type:            "Warning",
		Message:         "Back-off restarting failed container",
		Count:           12,
	}
	if _, err := db.InsertK8sEvent(ctx, in); err != nil {
		t.Fatalf("InsertK8sEvent: %v", err)
	}

	page, err := db.ListK8sEvents(ctx, store.K8sEventFilter{})
	if err != nil {
		t.Fatalf("ListK8sEvents: %v", err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("ListK8sEvents returned %d rows, want 1", len(page.Events))
	}
	got := page.Events[0]
	if got.ID == 0 {
		t.Error("ID is 0, want the BIGSERIAL value")
	}
	if got.UID != in.UID || got.ResourceVersion != in.ResourceVersion {
		t.Errorf("key round trip: got (%q, %q), want (%q, %q)",
			got.UID, got.ResourceVersion, in.UID, in.ResourceVersion)
	}
	if !got.EventTime.Equal(at) {
		t.Errorf("EventTime = %v, want %v", got.EventTime, at)
	}
	if got.Kind != in.Kind || got.Name != in.Name || got.Namespace != in.Namespace {
		t.Errorf("identity round trip: got (%q, %q, %q), want (%q, %q, %q)",
			got.Kind, got.Name, got.Namespace, in.Kind, in.Name, in.Namespace)
	}
	if got.Reason != in.Reason || got.Type != in.Type || got.Message != in.Message {
		t.Errorf("payload round trip: got (%q, %q, %q), want (%q, %q, %q)",
			got.Reason, got.Type, got.Message, in.Reason, in.Type, in.Message)
	}
	if got.Count != in.Count {
		t.Errorf("Count = %d, want %d", got.Count, in.Count)
	}
}

// TestInsertK8sEventDefaultsCountToOne pins the "unset means one occurrence"
// rule: a caller that leaves Count at its zero value must not write "this
// happened zero times".
func TestInsertK8sEventDefaultsCountToOne(t *testing.T) {
	db, _ := newK8sEventsDB(t)
	ctx := context.Background()

	in := k8sEventInput("uid-count", "1", "node-a", time.Now().UTC())
	in.Count = 0
	if _, err := db.InsertK8sEvent(ctx, in); err != nil {
		t.Fatalf("InsertK8sEvent: %v", err)
	}

	page, err := db.ListK8sEvents(ctx, store.K8sEventFilter{})
	if err != nil {
		t.Fatalf("ListK8sEvents: %v", err)
	}
	if len(page.Events) != 1 || page.Events[0].Count != 1 {
		t.Errorf("stored count = %+v, want 1", page.Events)
	}
}

// TestK8sEventInvalidInputNeverReachesTheDatabase asserts validation runs
// before the INSERT: a rejected input leaves no row.
func TestK8sEventInvalidInputNeverReachesTheDatabase(t *testing.T) {
	db, _ := newK8sEventsDB(t)
	ctx := context.Background()

	bad := k8sEventInput("uid-bad", "1", "node-a", time.Now().UTC())
	bad.Kind = "Deployment"
	if _, err := db.InsertK8sEvent(ctx, bad); err == nil {
		t.Fatal("InsertK8sEvent(kind=Deployment) succeeded, want a validation error")
	}

	page, err := db.ListK8sEvents(ctx, store.K8sEventFilter{})
	if err != nil {
		t.Fatalf("ListK8sEvents: %v", err)
	}
	if len(page.Events) != 0 {
		t.Errorf("a rejected insert left %d rows behind", len(page.Events))
	}
}

// TestListK8sEventsFiltersAndWindow covers the filter set in one seeded table:
// name, kind, type, and the half-open [from, to) window.
func TestListK8sEventsFiltersAndWindow(t *testing.T) {
	db, _ := newK8sEventsDB(t)
	ctx := context.Background()

	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	seed := []struct {
		uid, name, kind, evType string
		at                      time.Time
	}{
		{"u1", "node-a", "Node", "Warning", base},
		{"u2", "node-a", "Node", "Normal", base.Add(10 * time.Minute)},
		{"u3", "node-b", "Node", "Warning", base.Add(20 * time.Minute)},
		{"u4", "kconmon-agent-1", "Pod", "Warning", base.Add(30 * time.Minute)},
	}
	for _, s := range seed {
		in := k8sEventInput(s.uid, "1", s.name, s.at)
		in.Kind = s.kind
		in.Type = s.evType
		if s.kind == "Pod" {
			in.Namespace = "kconmon"
		}
		if _, err := db.InsertK8sEvent(ctx, in); err != nil {
			t.Fatalf("seed %s: %v", s.uid, err)
		}
	}

	count := func(f store.K8sEventFilter) int {
		t.Helper()
		page, err := db.ListK8sEvents(ctx, f)
		if err != nil {
			t.Fatalf("ListK8sEvents(%+v): %v", f, err)
		}
		return len(page.Events)
	}

	if got := count(store.K8sEventFilter{}); got != 4 {
		t.Errorf("no filter returned %d rows, want 4", got)
	}
	if got := count(store.K8sEventFilter{Name: "node-a"}); got != 2 {
		t.Errorf("name=node-a returned %d rows, want 2", got)
	}
	if got := count(store.K8sEventFilter{Kind: "Pod"}); got != 1 {
		t.Errorf("kind=Pod returned %d rows, want 1", got)
	}
	if got := count(store.K8sEventFilter{Type: "Warning"}); got != 3 {
		t.Errorf("type=Warning returned %d rows, want 3", got)
	}
	if got := count(store.K8sEventFilter{Name: "node-a", Type: "Warning"}); got != 1 {
		t.Errorf("name+type returned %d rows, want 1", got)
	}

	// The window is half-open: an event exactly at From is in, one exactly at
	// To belongs to the next window.
	if got := count(store.K8sEventFilter{From: base, To: base.Add(20 * time.Minute)}); got != 2 {
		t.Errorf("[base, base+20m) returned %d rows, want 2 (u1 at the inclusive lower bound, u2)", got)
	}
	if got := count(store.K8sEventFilter{From: base.Add(20 * time.Minute)}); got != 2 {
		t.Errorf("an open-ended window returned %d rows, want 2", got)
	}
	if got := count(store.K8sEventFilter{To: base.Add(10 * time.Minute)}); got != 1 {
		t.Errorf("an open-started window returned %d rows, want 1", got)
	}
}

// TestListK8sEventsPagesNewestFirst covers the bigint keyset: a full page
// hands back a cursor, the next page continues strictly after it with no
// repeats and no gaps, and the last page's cursor is empty.
func TestListK8sEventsPagesNewestFirst(t *testing.T) {
	db, _ := newK8sEventsDB(t)
	ctx := context.Background()

	const total = 7
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	for i := 0; i < total; i++ {
		in := k8sEventInput("uid-"+strconv.Itoa(i), "1", "node-a", base.Add(time.Duration(i)*time.Minute))
		if _, err := db.InsertK8sEvent(ctx, in); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	seen := make(map[int64]bool, total)
	var last time.Time
	cursor := ""
	pages := 0
	for {
		page, err := db.ListK8sEvents(ctx, store.K8sEventFilter{Cursor: cursor, Limit: 3})
		if err != nil {
			t.Fatalf("ListK8sEvents(page %d): %v", pages, err)
		}
		pages++
		for _, e := range page.Events {
			if seen[e.ID] {
				t.Fatalf("event %d appeared on two pages", e.ID)
			}
			seen[e.ID] = true
			if !last.IsZero() && e.EventTime.After(last) {
				t.Fatalf("page order broke: %v came after %v", e.EventTime, last)
			}
			last = e.EventTime
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if pages > total {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != total {
		t.Errorf("paged through %d events, want %d", len(seen), total)
	}
}

// TestDeleteK8sEventsBeforeUsesEventTime pins which column retention reads: a
// capture ages out with the window it describes.
func TestDeleteK8sEventsBeforeUsesEventTime(t *testing.T) {
	db, _ := newK8sEventsDB(t)
	ctx := context.Background()

	ancient := time.Now().UTC().Add(-200 * 24 * time.Hour)
	recent := time.Now().UTC()
	cutoff := time.Now().UTC().Add(-90 * 24 * time.Hour)

	for i, at := range []time.Time{ancient, recent} {
		if _, err := db.InsertK8sEvent(ctx, k8sEventInput("uid-"+strconv.Itoa(i), "1", "node-a", at)); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	n, err := db.DeleteK8sEventsBefore(ctx, cutoff, 100)
	if err != nil {
		t.Fatalf("DeleteK8sEventsBefore: %v", err)
	}
	if n != 1 {
		t.Fatalf("DeleteK8sEventsBefore deleted %d rows, want 1", n)
	}

	page, err := db.ListK8sEvents(ctx, store.K8sEventFilter{})
	if err != nil {
		t.Fatalf("ListK8sEvents: %v", err)
	}
	if len(page.Events) != 1 || !page.Events[0].EventTime.Equal(recent.Truncate(time.Microsecond)) {
		t.Errorf("after the sweep %d rows remain, want just the recent one", len(page.Events))
	}
}

// k8sIdxSeedRows is how many capture rows the index test seeds. Large enough
// that a seq scan is genuinely the more expensive plan for a single-name page,
// so the index wins on its own merits and no planner knob is touched -- the
// same reasoning snapshotIdxSeedRows carries.
const k8sIdxSeedRows = 20000

// listK8sEventsSQL returns the exact SQL text sqlc generated for
// ListK8sEvents, read out of the generated file at test time. EXPLAINing a
// hand-copied duplicate would prove nothing -- see listPathSnapshotsSQL's
// comment for the whole of that reasoning.
func listK8sEventsSQL(t *testing.T) string {
	t.Helper()
	return generatedSQL(t, "gen/k8s_events.sql.go", "listK8sEvents")
}

// TestListK8sEventsUsesNameTimeIndex asserts the REAL shipped scoped timeline
// -- store.DB.ListK8sEvents, not a copy of its SQL -- is answered by
// k8s_events_name_time_idx, which is the whole reason that index leads with
// name and trails with (event_time DESC, id DESC).
//
// Two independent halves, the pattern TestListPathSnapshotsUsesPairSeenIndex
// established:
//
//   - The counter half calls db.ListK8sEvents and watches
//     pg_stat_user_indexes.idx_scan for the index move. Nothing about the query
//     text is assumed; if the shipped query stopped matching the index the
//     counter would stay put.
//   - The plan half EXPLAINs the SQL extracted from the generated code and
//     asserts the plan names the index AND carries no Sort node. Both trailing
//     index columns are DESC, so ORDER BY event_time DESC, id DESC is supposed
//     to come out of the scan for free -- an index scan followed by a sort
//     would satisfy the "uses the index" half while quietly breaking the
//     promise that a long-running capture stays cheap to page per node.
func TestListK8sEventsUsesNameTimeIndex(t *testing.T) {
	db, dsn := newK8sEventsDB(t)
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	// Seeded in a single statement rather than through InsertK8sEvent: 20k
	// round trips would dominate the suite's runtime, and the rows only need
	// to be plausible. 200 names share the table so the leading index column
	// has real selectivity to exploit, which is precisely what a seq scan
	// cannot.
	if _, err = conn.Exec(ctx, `
INSERT INTO k8s_events (uid, resource_ver, event_time, kind, name, namespace, reason, type, message, count)
SELECT gen_random_uuid()::text,
       g::text,
       now() - make_interval(secs => $1::int - g),
       'Node',
       'node-' || (g % 200)::text,
       '',
       'NodeNotReady',
       'Warning',
       'seeded',
       1
FROM generate_series(1, $1::int) AS g`, k8sIdxSeedRows); err != nil {
		t.Fatalf("seed %d k8s events: %v", k8sIdxSeedRows, err)
	}
	if _, err = conn.Exec(ctx, "ANALYZE k8s_events"); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}

	// --- half one: the shipped call moves the index's scan counter ---------

	before := idxScans(t, ctx, conn, "k8s_events_name_time_idx")

	page, err := db.ListK8sEvents(ctx, store.K8sEventFilter{Name: "node-7", Limit: 50})
	if err != nil {
		t.Fatalf("ListK8sEvents: %v", err)
	}
	if len(page.Events) != 50 {
		t.Fatalf("ListK8sEvents returned %d rows, want 50", len(page.Events))
	}
	for i := 1; i < len(page.Events); i++ {
		if page.Events[i-1].EventTime.Before(page.Events[i].EventTime) {
			t.Fatalf("row %d is newer than row %d: %v < %v", i, i-1,
				page.Events[i-1].EventTime, page.Events[i].EventTime)
		}
	}

	// The counter does not move the instant the query returns -- see
	// TestListPathSnapshotsUsesPairSeenIndex's comment on the stats flush. The
	// nudge here is an unrelated single-row listing that touches no index this
	// test measures.
	deadline := time.Now().Add(30 * time.Second)
	var after int64
	for {
		after = idxScans(t, ctx, conn, "k8s_events_name_time_idx")
		if after > before {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("k8s_events_name_time_idx idx_scan stayed at %d after ListK8sEvents (was %d before): "+
				"the shipped query is not being answered by the index", after, before)
		}
		time.Sleep(300 * time.Millisecond)
		if _, err = db.ListK8sEvents(ctx, store.K8sEventFilter{Limit: 1}); err != nil {
			t.Fatalf("stats-flush nudge: ListK8sEvents: %v", err)
		}
	}

	// --- half two: the plan names the index and carries no sort ------------

	sql := listK8sEventsSQL(t)
	if !strings.Contains(sql, "k8s_events") {
		t.Fatalf("extracted SQL does not look like the k8s listing:\n%s", sql)
	}

	rows, err := conn.Query(ctx, "EXPLAIN\n"+sql, "node-7", nil, nil, nil, nil, nil, nil, int32(50))
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()

	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("EXPLAIN scan: %v", err)
		}
		plan.WriteString(line)
		plan.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("EXPLAIN rows: %v", err)
	}

	if !strings.Contains(plan.String(), "k8s_events_name_time_idx") {
		t.Errorf("the scoped timeline does not use k8s_events_name_time_idx; plan was:\n%s", plan.String())
	}
	if strings.Contains(plan.String(), "Sort") {
		t.Errorf("the scoped timeline sorts instead of reading ORDER BY event_time DESC, id DESC out of "+
			"k8s_events_name_time_idx; plan was:\n%s", plan.String())
	}
}

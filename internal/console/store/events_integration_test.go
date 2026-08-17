//go:build integration

package store_test

// TestEventStore* require a real PostgreSQL.

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// newTestMetrics returns Metrics on a fresh registry: metrics.New(nil) targets
// prometheus.DefaultRegisterer.
func newTestMetrics() *metrics.Metrics {
	return metrics.New("kconmon_ng", prometheus.NewRegistry())
}

// newEventStoreDB opens a *store.DB with migrations applied.
func newEventStoreDB(t *testing.T) *store.DB {
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
	return db
}

// rec builds a minimal, valid EventRecord for seq at ts.
func rec(seq int64, ts time.Time, typ, scope string) store.EventRecord {
	return store.EventRecord{
		EventSeq:  seq,
		EventTime: ts,
		Type:      typ,
		Severity:  "info",
		Scope:     scope,
		Summary:   fmt.Sprintf("event %d", seq),
		Details:   json.RawMessage(`{}`),
	}
}

// TestInsertEventThenListRoundTrip asserts the basic insert -> list path
// returns the row back with every field intact.
func TestInsertEventThenListRoundTrip(t *testing.T) {
	db := newEventStoreDB(t)
	m := newTestMetrics()
	es := store.NewEventStore(db, m)
	ctx := context.Background()

	ts := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	ev := rec(1, ts, "check_observed", "node-a→node-b")
	ev.Details = json.RawMessage(`{"taskId":"abc"}`)

	inserted, err := es.InsertEvent(ctx, ev)
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}
	if !inserted {
		t.Fatal("InsertEvent: inserted = false on first insert, want true")
	}

	page, err := es.ListEvents(ctx, store.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("ListEvents: got %d events, want 1", len(page.Events))
	}
	got := page.Events[0]
	if got.EventSeq != ev.EventSeq || !got.EventTime.Equal(ev.EventTime) || got.Type != ev.Type ||
		got.Severity != ev.Severity || got.Scope != ev.Scope || got.Summary != ev.Summary {
		t.Errorf("ListEvents: got %+v, want fields matching %+v", got, ev)
	}
	// jsonb round-trips through Postgres's own (re-)serialization -- it may
	// reformat whitespace -- so Details is compared as parsed JSON, not as
	// raw bytes.
	if !jsonEqual(t, got.Details, ev.Details) {
		t.Errorf("ListEvents: Details = %s, want %s", got.Details, ev.Details)
	}
	if page.NextCursor != "" {
		t.Errorf("ListEvents: NextCursor = %q on a short page, want empty", page.NextCursor)
	}
}

// TestInsertEventDuplicateIsIdempotent is the single most important assertion in Phase A.
func TestInsertEventDuplicateIsIdempotent(t *testing.T) {
	db := newEventStoreDB(t)
	m := newTestMetrics()
	es := store.NewEventStore(db, m)
	ctx := context.Background()

	ts := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	ev := rec(1, ts, "check_observed", "node-a→node-b")

	first, err := es.InsertEvent(ctx, ev)
	if err != nil {
		t.Fatalf("first InsertEvent: %v", err)
	}
	if !first {
		t.Fatal("first InsertEvent: inserted = false, want true")
	}

	second, err := es.InsertEvent(ctx, ev)
	if err != nil {
		t.Fatalf("second InsertEvent: %v", err)
	}
	if second {
		t.Fatal("second InsertEvent: inserted = true on a duplicate (event_seq, event_time), want false")
	}

	page, err := es.ListEvents(ctx, store.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("ListEvents after duplicate insert: got %d rows, want exactly 1", len(page.Events))
	}
}

// TestListEventsFiltersByType asserts the type filter is OR-ed across the
// given values and excludes everything else.
func TestListEventsFiltersByType(t *testing.T) {
	db := newEventStoreDB(t)
	m := newTestMetrics()
	es := store.NewEventStore(db, m)
	ctx := context.Background()

	base := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	mustInsert(t, ctx, es, rec(1, base, "check_observed", "a→b"))
	mustInsert(t, ctx, es, rec(2, base.Add(time.Second), "mtr_triggered", "a→b"))
	mustInsert(t, ctx, es, rec(3, base.Add(2*time.Second), "topology_changed", "cluster"))

	page, err := es.ListEvents(ctx, store.EventFilter{Types: []string{"check_observed", "topology_changed"}})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(page.Events) != 2 {
		t.Fatalf("ListEvents(Types=[check_observed,topology_changed]): got %d events, want 2", len(page.Events))
	}
	for _, e := range page.Events {
		if e.Type != "check_observed" && e.Type != "topology_changed" {
			t.Errorf("ListEvents: unexpected type %q leaked through the filter", e.Type)
		}
	}
}

// TestListEventsFiltersByScope asserts an exact scope match.
func TestListEventsFiltersByScope(t *testing.T) {
	db := newEventStoreDB(t)
	m := newTestMetrics()
	es := store.NewEventStore(db, m)
	ctx := context.Background()

	base := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	mustInsert(t, ctx, es, rec(1, base, "check_observed", "a→b"))
	mustInsert(t, ctx, es, rec(2, base.Add(time.Second), "check_observed", "c→d"))

	page, err := es.ListEvents(ctx, store.EventFilter{Scope: "a→b"})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("ListEvents(Scope=a→b): got %d events, want 1", len(page.Events))
	}
	if page.Events[0].Scope != "a→b" {
		t.Errorf("ListEvents(Scope=a→b): got scope %q", page.Events[0].Scope)
	}
}

// TestListEventsFiltersByScopeNode asserts the pair-aware filter a node card needs; the near-miss
// rows ("a-x→b", "b→x-a") are the whole point.
func TestListEventsFiltersByScopeNode(t *testing.T) {
	db := newEventStoreDB(t)
	m := newTestMetrics()
	es := store.NewEventStore(db, m)
	ctx := context.Background()

	base := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	mustInsert(t, ctx, es, rec(1, base, "topology_changed", "a"))                      // the node's own scope
	mustInsert(t, ctx, es, rec(2, base.Add(time.Second), "check_observed", "a→b"))     // pair, source side
	mustInsert(t, ctx, es, rec(3, base.Add(2*time.Second), "check_observed", "c→a"))   // pair, destination side
	mustInsert(t, ctx, es, rec(4, base.Add(3*time.Second), "check_observed", "c→d"))   // unrelated pair
	mustInsert(t, ctx, es, rec(5, base.Add(4*time.Second), "check_observed", "a-x→b")) // near miss on the source side
	mustInsert(t, ctx, es, rec(6, base.Add(5*time.Second), "check_observed", "b→x-a")) // near miss on the destination side
	mustInsert(t, ctx, es, rec(7, base.Add(6*time.Second), "topology_changed", "ab"))  // near miss on the bare scope

	page, err := es.ListEvents(ctx, store.EventFilter{ScopeNode: "a"})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	got := map[int64]string{}
	for _, e := range page.Events {
		got[e.EventSeq] = e.Scope
	}
	want := map[int64]string{1: "a", 2: "a→b", 3: "c→a"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListEvents(ScopeNode=a): got %v, want %v", got, want)
	}
}

/*
The pair-aware filter is a UNION ALL of two arms now, and a UNION ALL can return a row twice.

A NON-pair scope has scope_left = scope_right = the scope itself, so "a" matches both arms; a
self-pair "a→a" does too. The second arm excludes what the first already returned, and this asserts
it — the map-keyed assertion in TestListEventsFiltersByScopeNode would silently collapse a duplicate.
*/
func TestListEventsScopeNodeReturnsEachEventOnce(t *testing.T) {
	db := newEventStoreDB(t)
	m := newTestMetrics()
	es := store.NewEventStore(db, m)
	ctx := context.Background()

	base := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	mustInsert(t, ctx, es, rec(1, base, "topology_changed", "a"))                    // both sides are "a"
	mustInsert(t, ctx, es, rec(2, base.Add(time.Second), "check_observed", "a→a"))   // ... and so are these
	mustInsert(t, ctx, es, rec(3, base.Add(2*time.Second), "check_observed", "a→b")) // one side
	mustInsert(t, ctx, es, rec(4, base.Add(3*time.Second), "check_observed", "b→a")) // the other

	page, err := es.ListEvents(ctx, store.EventFilter{ScopeNode: "a"})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	seen := map[int64]int{}
	for _, e := range page.Events {
		seen[e.EventSeq]++
	}
	for seq, n := range seen {
		if n != 1 {
			t.Errorf("event_seq %d came back %d times: the two UNION arms overlap", seq, n)
		}
	}
	if len(page.Events) != 4 {
		t.Errorf("got %d events, want the 4 seeded (all of them name node a)", len(page.Events))
	}
}

/*
And the whole reason the filter was rewritten: it has to be answered by an index.

The old shape was three ORed predicates, one of them `scope LIKE '%→' || $node`. A leading wildcard
is unindexable, and one such arm makes PostgreSQL walk the time index and filter every row — the
LIMIT bounds the response, not the work. The sharp case is a scope with FEW or NO events: a drained
node, a typo, a stale permalink. Splitting the scope into indexed columns was not enough on its own,
because under `ORDER BY event_time DESC LIMIT n` the planner still preferred the time index; the
UNION ALL is what gives each arm its own ordered index scan.
*/
func TestListEventsByScopeNodeUsesTheScopeSideIndexes(t *testing.T) {
	db := newEventStoreDB(t)
	m := newTestMetrics()
	es := store.NewEventStore(db, m)
	ctx := context.Background()

	// Enough rows, spread over enough scopes, that a full scan is plainly the wrong plan.
	base := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	for i := range 5000 {
		scope := fmt.Sprintf("node-%d→node-%d", i%200, (i+7)%200)
		mustInsert(t, ctx, es, rec(int64(i+1), base.Add(time.Duration(i)*time.Second), "check_observed", scope))
	}

	pool, err := pgxpool.New(ctx, testDSN(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "ANALYZE topology_events"); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}

	sql := generatedSQL(t, "gen/topology_events.sql.go", "listTopologyEventsByScopeNode")
	rows, qerr := conn.Query(ctx, "EXPLAIN\n"+sql, int32(50), "node-does-not-exist", nil, nil, nil, nil, nil)
	if qerr != nil {
		t.Fatalf("EXPLAIN: %v", qerr)
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

	for _, idx := range []string{"topology_events_scope_left_time_idx", "topology_events_scope_right_time_idx"} {
		if !strings.Contains(plan.String(), idx) {
			t.Errorf("the pair-aware filter does not use %s; plan was:\n%s", idx, plan.String())
		}
	}
	if strings.Contains(plan.String(), "topology_events_time_idx") {
		t.Errorf("the pair-aware filter fell back to walking the time index and filtering; plan was:\n%s", plan.String())
	}
}

// Node names cannot contain _ or %, but a scope is not always a node name.
func TestListEventsScopeNodeEscapesLIKEMetacharacters(t *testing.T) {
	db := newEventStoreDB(t)
	m := newTestMetrics()
	es := store.NewEventStore(db, m)
	ctx := context.Background()

	base := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	mustInsert(t, ctx, es, rec(1, base, "check_observed", "abc→b"))                    // the wildcard victim
	mustInsert(t, ctx, es, rec(2, base.Add(time.Second), "check_observed", "b→abc"))   // ... on the other side
	mustInsert(t, ctx, es, rec(3, base.Add(2*time.Second), "check_observed", "a_c→b")) // the literal match

	page, err := es.ListEvents(ctx, store.EventFilter{ScopeNode: "a_c"})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(page.Events) != 1 || page.Events[0].Scope != "a_c→b" {
		t.Fatalf("ListEvents(ScopeNode=a_c): got %+v, want exactly the a_c→b row", page.Events)
	}

	// '%' is the other metacharacter, and a bare '%' would otherwise match
	// every pair scope in the table.
	page, err = es.ListEvents(ctx, store.EventFilter{ScopeNode: "%"})
	if err != nil {
		t.Fatalf("ListEvents(ScopeNode=%%): %v", err)
	}
	if len(page.Events) != 0 {
		t.Fatalf("ListEvents(ScopeNode=%%): got %d events, want 0 -- the wildcard was not escaped", len(page.Events))
	}
}

// TestListEventsFiltersByTimeWindow asserts From is inclusive and To is
// exclusive.
func TestListEventsFiltersByTimeWindow(t *testing.T) {
	db := newEventStoreDB(t)
	m := newTestMetrics()
	es := store.NewEventStore(db, m)
	ctx := context.Background()

	base := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	mustInsert(t, ctx, es, rec(1, base, "check_observed", "a→b"))                    // in window (From, inclusive)
	mustInsert(t, ctx, es, rec(2, base.Add(time.Minute), "check_observed", "a→b"))   // in window
	mustInsert(t, ctx, es, rec(3, base.Add(2*time.Minute), "check_observed", "a→b")) // == To, excluded
	mustInsert(t, ctx, es, rec(4, base.Add(-time.Minute), "check_observed", "a→b"))  // before From, excluded

	page, err := es.ListEvents(ctx, store.EventFilter{
		From: base,
		To:   base.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(page.Events) != 2 {
		t.Fatalf("ListEvents(time window): got %d events, want 2", len(page.Events))
	}
	for _, e := range page.Events {
		if e.EventSeq != 1 && e.EventSeq != 2 {
			t.Errorf("ListEvents(time window): unexpected seq %d leaked through the filter", e.EventSeq)
		}
	}
}

// TestListEventsPagesWithoutDuplicatesOrGaps seeds 250 rows and pages through
// them with Limit: 100, asserting 100/100/50 with no duplicate and no missing
// EventSeq, and that the final page's NextCursor is empty.
func TestListEventsPagesWithoutDuplicatesOrGaps(t *testing.T) {
	db := newEventStoreDB(t)
	m := newTestMetrics()
	es := store.NewEventStore(db, m)
	ctx := context.Background()

	const total = 250
	base := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	for i := 0; i < total; i++ {
		mustInsert(t, ctx, es, rec(int64(i), base.Add(time.Duration(i)*time.Second), "check_observed", "a→b"))
	}

	var (
		seen      = make(map[int64]bool, total)
		pageSizes []int
		cursor    string
	)
	for {
		page, err := es.ListEvents(ctx, store.EventFilter{Limit: 100, Cursor: cursor})
		if err != nil {
			t.Fatalf("ListEvents: %v", err)
		}
		pageSizes = append(pageSizes, len(page.Events))
		for _, e := range page.Events {
			if seen[e.EventSeq] {
				t.Fatalf("ListEvents: duplicate EventSeq %d across pages", e.EventSeq)
			}
			seen[e.EventSeq] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if len(pageSizes) > total { // guard against an infinite loop on a bug
			t.Fatal("ListEvents: paging did not terminate")
		}
	}

	if want := []int{100, 100, 50}; len(pageSizes) != len(want) {
		t.Fatalf("page sizes = %v, want %v", pageSizes, want)
	} else {
		for i := range want {
			if pageSizes[i] != want[i] {
				t.Errorf("page %d size = %d, want %d", i, pageSizes[i], want[i])
			}
		}
	}

	if len(seen) != total {
		t.Errorf("saw %d distinct events across all pages, want %d (gaps present)", len(seen), total)
	}
	for i := 0; i < total; i++ {
		if !seen[int64(i)] {
			t.Errorf("EventSeq %d missing from paged results", i)
		}
	}
}

// mustInsert is a small helper: most of these tests only care that seeding
// succeeded, not about the returned inserted flag.
func mustInsert(t *testing.T, ctx context.Context, es store.EventStore, ev store.EventRecord) {
	t.Helper()
	if _, err := es.InsertEvent(ctx, ev); err != nil {
		t.Fatalf("InsertEvent(seq=%d): %v", ev.EventSeq, err)
	}
}

// jsonEqual compares a and b as parsed JSON values rather than raw bytes.
func jsonEqual(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var va, vb any
	if err := json.Unmarshal(a, &va); err != nil {
		t.Fatalf("jsonEqual: unmarshal %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &vb); err != nil {
		t.Fatalf("jsonEqual: unmarshal %s: %v", b, err)
	}
	return reflect.DeepEqual(va, vb)
}

// topoRec builds one topology_changed row whose details carry the exact four keys
// events.topologyChangedDetails marshals; the zone travels through real JSONB storage here.
func topoRec(seq int64, ts time.Time, reason, node, agent, zone string) store.EventRecord {
	scope := node
	if scope == "" {
		scope = "cluster"
	}
	ev := rec(seq, ts, "topology_changed", scope)
	ev.Summary = fmt.Sprintf("topology changed: %s", reason)
	ev.Details = json.RawMessage(fmt.Sprintf(
		`{"reason":%q,"nodeName":%q,"agentId":%q,"zone":%q}`, reason, node, agent, zone))
	return ev
}

// TestTopologyAtThreeInstantsGiveThreeSets is the fold's end-to-end proof: real
// rows written through InsertEvent, read back through the real query, folded at
// three instants that each fall between two changes.
func TestTopologyAtThreeInstantsGiveThreeSets(t *testing.T) {
	db := newEventStoreDB(t)
	es := store.NewEventStore(db, newTestMetrics())
	ctx := context.Background()

	t0 := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	mustInsert(t, ctx, es, topoRec(1, t0, "agent_registered", "node-a", "agent-a", "zone-a"))
	mustInsert(t, ctx, es, topoRec(2, t0.Add(time.Hour), "agent_registered", "node-b", "agent-b", "zone-b"))
	mustInsert(t, ctx, es, topoRec(3, t0.Add(2*time.Hour), "agent_evicted", "node-a", "agent-a", "zone-a"))
	// A non-topology row at the same times must never reach the fold.
	mustInsert(t, ctx, es, topoRecNoise(4, t0.Add(90*time.Minute)))

	for _, tc := range []struct {
		name  string
		at    time.Time
		nodes []string
		zones []string
	}{
		{"after the first registration", t0.Add(30 * time.Minute), []string{"node-a"}, []string{"zone-a"}},
		{"after the second", t0.Add(90 * time.Minute), []string{"node-a", "node-b"}, []string{"zone-a", "zone-b"}},
		{"after the eviction", t0.Add(3 * time.Hour), []string{"node-b"}, []string{"zone-b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap, err := es.TopologyAt(ctx, tc.at)
			if err != nil {
				t.Fatalf("TopologyAt: %v", err)
			}
			got := make([]string, 0, len(snap.Nodes))
			gotZones := make([]string, 0, len(snap.Nodes))
			for _, n := range snap.Nodes {
				got = append(got, n.Name)
				gotZones = append(gotZones, n.Zone)
			}
			if !reflect.DeepEqual(got, tc.nodes) {
				t.Errorf("nodes at %v = %v, want %v", tc.at, got, tc.nodes)
			}
			// The zone survives the JSONB round trip, so a reconstructed map
			// can draw the same zone lanes the live one does.
			if !reflect.DeepEqual(gotZones, tc.zones) {
				t.Errorf("zones at %v = %v, want %v", tc.at, gotZones, tc.zones)
			}
			if snap.UnfoldableEvents != 0 {
				t.Errorf("UnfoldableEvents = %d, want 0: every seeded event names a node", snap.UnfoldableEvents)
			}
			if snap.Truncated {
				t.Error("Truncated = true on a four-row table")
			}
			if !snap.OldestRetained.Equal(t0) {
				t.Errorf("OldestRetained = %v, want the first row's time %v", snap.OldestRetained, t0)
			}
		})
	}
}

// topoRecNoise is a non-topology event: the fold's WHERE type= clause must
// skip it, and OldestRetained must still count it (it is a retained row).
func topoRecNoise(seq int64, ts time.Time) store.EventRecord {
	ev := rec(seq, ts, "check_observed", "node-a→node-b")
	ev.Details = json.RawMessage(`{"taskId":"noise"}`)
	return ev
}

// TestTopologyAtBeforeAnyEventIsEmptyButRetentionAware pins the two facts the
// handler's 422 decision rests on: an instant before the oldest row folds to
// nothing, and OldestRetained still reports where history actually starts.
func TestTopologyAtBeforeAnyEventIsEmptyButRetentionAware(t *testing.T) {
	db := newEventStoreDB(t)
	es := store.NewEventStore(db, newTestMetrics())
	ctx := context.Background()

	t0 := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	mustInsert(t, ctx, es, topoRec(1, t0, "agent_registered", "node-a", "agent-a", "zone-a"))

	snap, err := es.TopologyAt(ctx, t0.Add(-time.Hour))
	if err != nil {
		t.Fatalf("TopologyAt: %v", err)
	}
	if len(snap.Nodes) != 0 || len(snap.Agents) != 0 {
		t.Errorf("pre-history fold = %+v, want empty", snap)
	}
	if snap.EventsFolded != 0 {
		t.Errorf("EventsFolded = %d, want 0", snap.EventsFolded)
	}
	if !snap.OldestRetained.Equal(t0) {
		t.Errorf("OldestRetained = %v, want %v -- the handler needs this to answer 422", snap.OldestRetained, t0)
	}
}

// TestTopologyAtOnAnEmptyTableReportsNoRetention is the database-configured
// but never-ingested case: no error, and a zero OldestRetained the handler
// reads as "there is no history at all".
func TestTopologyAtOnAnEmptyTableReportsNoRetention(t *testing.T) {
	db := newEventStoreDB(t)
	es := store.NewEventStore(db, newTestMetrics())

	snap, err := es.TopologyAt(context.Background(), time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("TopologyAt on an empty table: %v", err)
	}
	if !snap.OldestRetained.IsZero() {
		t.Errorf("OldestRetained = %v, want the zero time on an empty table", snap.OldestRetained)
	}
	if len(snap.Nodes) != 0 || snap.Nodes == nil {
		t.Errorf("Nodes = %v, want a non-nil empty slice", snap.Nodes)
	}
}

// TestTopologyAtTieBreaksOnID pins the (event_time, id) order against real
// Postgres: two rows in the SAME microsecond must fold in insertion order, so
// the later-inserted removal wins.
func TestTopologyAtTieBreaksOnID(t *testing.T) {
	db := newEventStoreDB(t)
	es := store.NewEventStore(db, newTestMetrics())
	ctx := context.Background()

	ts := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	mustInsert(t, ctx, es, topoRec(1, ts, "agent_registered", "node-a", "agent-a", "zone-a"))
	// Same event_time, different event_seq, so the natural key lets both in.
	mustInsert(t, ctx, es, topoRec(2, ts, "agent_deregistered", "node-a", "agent-a", "zone-a"))

	snap, err := es.TopologyAt(ctx, ts)
	if err != nil {
		t.Fatalf("TopologyAt: %v", err)
	}
	if len(snap.Nodes) != 0 {
		t.Errorf("nodes = %+v, want empty: the deregistration has the higher id and folds last", snap.Nodes)
	}
	if snap.EventsFolded != 2 {
		t.Errorf("EventsFolded = %d, want 2 -- event_time <= at is INCLUSIVE", snap.EventsFolded)
	}
}

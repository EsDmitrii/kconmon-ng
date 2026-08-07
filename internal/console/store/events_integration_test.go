//go:build integration

package store_test

// TestEventStore* require a real PostgreSQL.
// Run: docker run --rm -d -p 5432:5432 -e POSTGRES_PASSWORD=test -e POSTGRES_DB=kconmon postgres:17-alpine
// Then: KCONMON_TEST_DATABASE_DSN='postgres://postgres:test@127.0.0.1:5432/kconmon?sslmode=disable' \
//       go test -tags=integration ./internal/console/store/... -v

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// newTestMetrics returns Metrics on a fresh registry: metrics.New(nil) targets
// prometheus.DefaultRegisterer, which every test function in this binary
// would share and collide on ("duplicate metrics collector registration")
// the second time any test called it.
func newTestMetrics() *metrics.Metrics {
	return metrics.New("kconmon_ng", prometheus.NewRegistry())
}

// newEventStoreDB opens a *store.DB with migrations applied, dropping and
// re-creating the schema first so every test starts from a clean
// topology_events table -- there is no per-test schema, this file shares one
// database with store_integration_test.go.
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

// TestInsertEventDuplicateIsIdempotent is the single most important
// assertion in Phase A: two console replicas racing to ingest the same
// controller event (same EventSeq, same EventTime) must not duplicate the
// row. The second insert reports inserted=false and the table still holds
// exactly one row.
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

// jsonEqual compares a and b as parsed JSON values rather than raw bytes:
// Postgres's jsonb type re-serializes on read (e.g. adding a space after a
// key's colon), so a byte-exact comparison of a value that survived a round
// trip through the database is not a valid test.
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

//go:build integration

package store_test

// TestAnnotation* require a real PostgreSQL.
// Run: docker run --rm -d -p 5432:5432 -e POSTGRES_PASSWORD=test -e POSTGRES_DB=kconmon postgres:17-alpine
// Then: KCONMON_TEST_DATABASE_DSN='postgres://postgres:test@127.0.0.1:5432/kconmon?sslmode=disable' \
//       go test -tags=integration ./internal/console/store/... -v

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// newAnnotationsDB opens a *store.DB with migrations applied, dropping and
// re-creating the schema first -- same convention as newTargetsDB; this file
// shares one database with every other file in package store_test, so each
// test must leave it clean.
func newAnnotationsDB(t *testing.T) *store.DB {
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

func scopePtr(s string) *string { return &s }

// TestAnnotationLifecycle is the happy path M5 supports: create -> get ->
// delete, with the delete asserted through an independent read. There is no
// update by design (Decision 10), so this is the whole of the CRUD.
func TestAnnotationLifecycle(t *testing.T) {
	db := newAnnotationsDB(t)
	ctx := context.Background()

	start := time.Now().UTC().Truncate(time.Microsecond)
	end := start.Add(30 * time.Minute)

	created, err := db.CreateAnnotation(ctx, store.AnnotationInput{
		StartAt:   start,
		EndAt:     &end,
		Scope:     "node-a",
		Text:      "rolled the CNI upgrade",
		CreatedBy: "user:admin",
	})
	if err != nil {
		t.Fatalf("CreateAnnotation: %v", err)
	}
	if created.ID == "" {
		t.Fatal("CreateAnnotation: ID is empty, want a minted UUID")
	}
	if !created.StartAt.Equal(start) {
		t.Errorf("CreateAnnotation: StartAt = %v, want %v", created.StartAt, start)
	}
	if created.EndAt == nil || !created.EndAt.Equal(end) {
		t.Errorf("CreateAnnotation: EndAt = %v, want %v", created.EndAt, end)
	}
	if created.Scope != "node-a" || created.Text != "rolled the CNI upgrade" || created.CreatedBy != "user:admin" {
		t.Errorf("CreateAnnotation: got %+v, want the input's fields back", created)
	}
	if created.CreatedAt.IsZero() {
		t.Error("CreateAnnotation: CreatedAt is zero, want the column default")
	}

	got, err := db.GetAnnotation(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetAnnotation: %v", err)
	}
	if got.ID != created.ID || got.Text != created.Text {
		t.Errorf("GetAnnotation = %+v, want %+v", got, created)
	}

	if err := db.DeleteAnnotation(ctx, created.ID); err != nil {
		t.Fatalf("DeleteAnnotation: %v", err)
	}
	if _, err := db.GetAnnotation(ctx, created.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetAnnotation after the delete = %v, want ErrNotFound", err)
	}
	if err := db.DeleteAnnotation(ctx, created.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second DeleteAnnotation = %v, want ErrNotFound: the caller asked about a specific mark", err)
	}
}

// TestAnnotationInstantMarkRoundTrips pins Decision 10's NULL end_at: an
// instant mark reads back with a nil EndAt, not with EndAt == StartAt, so a
// consumer can tell "a moment" from "a zero-length span" without guessing.
func TestAnnotationInstantMarkRoundTrips(t *testing.T) {
	db := newAnnotationsDB(t)
	ctx := context.Background()
	start := time.Now().UTC().Truncate(time.Microsecond)

	created, err := db.CreateAnnotation(ctx, store.AnnotationInput{
		StartAt: start, Text: "restarted the controller", CreatedBy: "user:admin",
	})
	if err != nil {
		t.Fatalf("CreateAnnotation: %v", err)
	}
	if created.EndAt != nil {
		t.Errorf("CreateAnnotation: EndAt = %v for an instant mark, want nil", created.EndAt)
	}
	if created.Scope != "" {
		t.Errorf("CreateAnnotation: Scope = %q, want \"\" (global)", created.Scope)
	}

	got, err := db.GetAnnotation(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetAnnotation: %v", err)
	}
	if got.EndAt != nil {
		t.Errorf("GetAnnotation: EndAt = %v, want nil", got.EndAt)
	}
}

// TestAnnotationInvalidInputNeverReachesTheDatabase asserts validation runs
// before the INSERT: a rejected input leaves no row.
func TestAnnotationInvalidInputNeverReachesTheDatabase(t *testing.T) {
	db := newAnnotationsDB(t)
	ctx := context.Background()

	bad := store.AnnotationInput{
		StartAt: time.Now().UTC(),
		Text:    strings.Repeat("x", 1025),
	}
	if _, err := db.CreateAnnotation(ctx, bad); err == nil {
		t.Fatal("CreateAnnotation with 1025 bytes of text succeeded, want a validation error")
	}
	page, err := db.ListAnnotations(ctx, store.AnnotationFilter{})
	if err != nil {
		t.Fatalf("ListAnnotations: %v", err)
	}
	if len(page.Annotations) != 0 {
		t.Errorf("a rejected create left %d rows behind", len(page.Annotations))
	}
}

// TestListAnnotationsWindowIsOverlapNotContainment is the range filter's real
// claim, and the one a naive "start_at BETWEEN from AND to" gets wrong: a span
// that began BEFORE the window and is still running inside it is exactly the
// annotation a chart needs to draw, and it must come back.
func TestListAnnotationsWindowIsOverlapNotContainment(t *testing.T) {
	db := newAnnotationsDB(t)
	ctx := context.Background()

	// The window under test.
	from := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	to := from.Add(time.Hour)

	spanEnd := from.Add(10 * time.Minute)
	mustCreateAnnotation := func(text string, start time.Time, end *time.Time) {
		t.Helper()
		if _, err := db.CreateAnnotation(ctx, store.AnnotationInput{
			StartAt: start, EndAt: end, Text: text, CreatedBy: "user:admin",
		}); err != nil {
			t.Fatalf("CreateAnnotation(%s): %v", text, err)
		}
	}

	// Straddles the lower bound: starts before "from", ends inside. Must match.
	mustCreateAnnotation("straddles-start", from.Add(-30*time.Minute), &spanEnd)
	// Wholly inside. Must match.
	mustCreateAnnotation("inside", from.Add(20*time.Minute), nil)
	// Instant mark exactly at "from". Must match -- the lower bound is
	// inclusive, and an instant mark's end is its start.
	mustCreateAnnotation("at-from", from, nil)
	// Instant mark exactly at "to". Must NOT match -- the upper bound is
	// exclusive, so a mark at "to" belongs to the next window.
	mustCreateAnnotation("at-to", to, nil)
	// Entirely before the window, ending before it opens. Must NOT match.
	beforeEnd := from.Add(-time.Minute)
	mustCreateAnnotation("before", from.Add(-2*time.Hour), &beforeEnd)
	// Entirely after. Must NOT match.
	mustCreateAnnotation("after", to.Add(time.Hour), nil)

	page, err := db.ListAnnotations(ctx, store.AnnotationFilter{From: from, To: to})
	if err != nil {
		t.Fatalf("ListAnnotations: %v", err)
	}
	got := make(map[string]bool, len(page.Annotations))
	for _, a := range page.Annotations {
		got[a.Text] = true
	}
	for _, want := range []string{"straddles-start", "inside", "at-from"} {
		if !got[want] {
			t.Errorf("the window dropped %q, which overlaps it", want)
		}
	}
	for _, unwanted := range []string{"at-to", "before", "after"} {
		if got[unwanted] {
			t.Errorf("the window returned %q, which does not overlap it", unwanted)
		}
	}
	if len(page.Annotations) != 3 {
		t.Errorf("the window returned %d annotations, want 3: %v", len(page.Annotations), got)
	}
}

// TestListAnnotationsUnboundedWindow asserts a zero From/To really is
// unbounded on that side, rather than folding to the zero time and dropping
// everything.
func TestListAnnotationsUnboundedWindow(t *testing.T) {
	db := newAnnotationsDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	for i, at := range []time.Time{now.Add(-48 * time.Hour), now, now.Add(48 * time.Hour)} {
		if _, err := db.CreateAnnotation(ctx, store.AnnotationInput{
			StartAt: at, Text: "mark-" + strconv.Itoa(i), CreatedBy: "user:admin",
		}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	all, err := db.ListAnnotations(ctx, store.AnnotationFilter{})
	if err != nil {
		t.Fatalf("ListAnnotations(unbounded): %v", err)
	}
	if len(all.Annotations) != 3 {
		t.Errorf("an unbounded window returned %d annotations, want 3", len(all.Annotations))
	}

	fromOnly, err := db.ListAnnotations(ctx, store.AnnotationFilter{From: now.Add(-time.Hour)})
	if err != nil {
		t.Fatalf("ListAnnotations(from only): %v", err)
	}
	if len(fromOnly.Annotations) != 2 {
		t.Errorf("an open-ended window returned %d annotations, want 2", len(fromOnly.Annotations))
	}

	toOnly, err := db.ListAnnotations(ctx, store.AnnotationFilter{To: now.Add(time.Hour)})
	if err != nil {
		t.Fatalf("ListAnnotations(to only): %v", err)
	}
	if len(toOnly.Annotations) != 2 {
		t.Errorf("an open-started window returned %d annotations, want 2", len(toOnly.Annotations))
	}
}

// TestListAnnotationsScopeFilterCanSelectTheGlobalOnes is why
// AnnotationFilter.Scope is a pointer: "" is the GLOBAL scope, a real value,
// so "no filter" and "the global ones" are two different requests and both
// have to be expressible. Every other filter in this package uses "" for "no
// filter", which here would make the global marks unaddressable.
func TestListAnnotationsScopeFilterCanSelectTheGlobalOnes(t *testing.T) {
	db := newAnnotationsDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	for i, scope := range []string{"", "node-a", "node-b", ""} {
		if _, err := db.CreateAnnotation(ctx, store.AnnotationInput{
			StartAt: now.Add(time.Duration(i) * time.Minute),
			Scope:   scope,
			Text:    "mark-" + strconv.Itoa(i),
		}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	all, err := db.ListAnnotations(ctx, store.AnnotationFilter{})
	if err != nil {
		t.Fatalf("ListAnnotations(nil scope): %v", err)
	}
	if len(all.Annotations) != 4 {
		t.Errorf("a nil scope returned %d annotations, want all 4", len(all.Annotations))
	}

	global, err := db.ListAnnotations(ctx, store.AnnotationFilter{Scope: scopePtr("")})
	if err != nil {
		t.Fatalf("ListAnnotations(global scope): %v", err)
	}
	if len(global.Annotations) != 2 {
		t.Fatalf("scope=\"\" returned %d annotations, want the 2 global ones", len(global.Annotations))
	}
	for _, a := range global.Annotations {
		if a.Scope != "" {
			t.Errorf("scope=\"\" returned a %q-scoped annotation", a.Scope)
		}
	}

	scoped, err := db.ListAnnotations(ctx, store.AnnotationFilter{Scope: scopePtr("node-a")})
	if err != nil {
		t.Fatalf("ListAnnotations(node-a): %v", err)
	}
	if len(scoped.Annotations) != 1 || scoped.Annotations[0].Scope != "node-a" {
		t.Errorf("scope=node-a returned %+v, want exactly the node-a mark", scoped.Annotations)
	}
}

// TestListAnnotationsPagesNewestFirst covers the keyset: a full page hands
// back a cursor, the next page continues strictly after it with no repeats and
// no gaps, and the last page's cursor is empty.
func TestListAnnotationsPagesNewestFirst(t *testing.T) {
	db := newAnnotationsDB(t)
	ctx := context.Background()

	const total = 7
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	for i := 0; i < total; i++ {
		if _, err := db.CreateAnnotation(ctx, store.AnnotationInput{
			StartAt: base.Add(time.Duration(i) * time.Minute),
			Text:    "mark-" + strconv.Itoa(i),
		}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	seen := make(map[string]bool, total)
	var last time.Time
	cursor := ""
	pages := 0
	for {
		page, err := db.ListAnnotations(ctx, store.AnnotationFilter{Cursor: cursor, Limit: 3})
		if err != nil {
			t.Fatalf("ListAnnotations(page %d): %v", pages, err)
		}
		pages++
		for _, a := range page.Annotations {
			if seen[a.ID] {
				t.Fatalf("annotation %s appeared on two pages", a.ID)
			}
			seen[a.ID] = true
			if !last.IsZero() && a.StartAt.After(last) {
				t.Fatalf("page order broke: %v came after %v", a.StartAt, last)
			}
			last = a.StartAt
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
		t.Errorf("paged through %d annotations, want %d", len(seen), total)
	}
}

// TestGetAnnotationUnknownIDIsNotFound is the seam's miss contract.
func TestGetAnnotationUnknownIDIsNotFound(t *testing.T) {
	db := newAnnotationsDB(t)
	ctx := context.Background()

	if _, err := db.GetAnnotation(ctx, uuid.NewString()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetAnnotation(unknown) = %v, want ErrNotFound", err)
	}
	if err := db.DeleteAnnotation(ctx, uuid.NewString()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DeleteAnnotation(unknown) = %v, want ErrNotFound", err)
	}
}

// TestDeleteAnnotationsBeforeUsesStartAt pins which column retention reads: an
// annotation ages out with the data it annotates, not with when it was typed.
func TestDeleteAnnotationsBeforeUsesStartAt(t *testing.T) {
	db := newAnnotationsDB(t)
	ctx := context.Background()

	ancient := time.Now().UTC().Add(-200 * 24 * time.Hour)
	recent := time.Now().UTC()
	cutoff := time.Now().UTC().Add(-90 * 24 * time.Hour)

	// Both rows get created_at = now() from the column default; only start_at
	// differs, which is exactly what makes this test about the right column.
	for _, tc := range []struct {
		text string
		at   time.Time
	}{{"old", ancient}, {"new", recent}} {
		if _, err := db.CreateAnnotation(ctx, store.AnnotationInput{
			StartAt: tc.at, Text: tc.text, CreatedBy: "user:admin",
		}); err != nil {
			t.Fatalf("seed %s: %v", tc.text, err)
		}
	}

	n, err := db.DeleteAnnotationsBefore(ctx, cutoff, 100)
	if err != nil {
		t.Fatalf("DeleteAnnotationsBefore: %v", err)
	}
	if n != 1 {
		t.Fatalf("DeleteAnnotationsBefore deleted %d rows, want 1", n)
	}
	page, err := db.ListAnnotations(ctx, store.AnnotationFilter{})
	if err != nil {
		t.Fatalf("ListAnnotations: %v", err)
	}
	if len(page.Annotations) != 1 || page.Annotations[0].Text != "new" {
		t.Errorf("after the sweep: %+v, want just the recent mark", page.Annotations)
	}
}

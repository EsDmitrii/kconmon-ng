//go:build integration

package store_test

// TestMaintenance* require a real PostgreSQL.

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// newMaintenanceDB opens a *store.DB with migrations applied, dropping and
// re-creating the schema first -- same convention as newAnnotationsDB.
func newMaintenanceDB(t *testing.T) (*store.DB, string) {
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

func maintenanceInput(scope string, start time.Time, d time.Duration) store.MaintenanceInput {
	return store.MaintenanceInput{
		Scope:     scope,
		StartAt:   start,
		EndAt:     start.Add(d),
		Reason:    "kernel upgrade, rolling reboot",
		CreatedBy: "user:admin",
	}
}

// TestMaintenanceLifecycle is the whole of the maintenance CRUD: create -> list -> delete.
func TestMaintenanceLifecycle(t *testing.T) {
	db, _ := newMaintenanceDB(t)
	ctx := context.Background()

	start := time.Now().UTC().Truncate(time.Microsecond)
	created, err := db.CreateMaintenanceWindow(ctx, maintenanceInput("node-a", start, 2*time.Hour))
	if err != nil {
		t.Fatalf("CreateMaintenanceWindow: %v", err)
	}
	if created.ID == "" {
		t.Fatal("CreateMaintenanceWindow: ID is empty, want a minted UUID")
	}
	if !created.StartAt.Equal(start) || !created.EndAt.Equal(start.Add(2*time.Hour)) {
		t.Errorf("range round trip = (%v, %v), want (%v, %v)",
			created.StartAt, created.EndAt, start, start.Add(2*time.Hour))
	}
	if created.Scope != "node-a" || created.Reason != "kernel upgrade, rolling reboot" ||
		created.CreatedBy != "user:admin" {
		t.Errorf("CreateMaintenanceWindow: got %+v, want the input's fields back", created)
	}
	if created.CreatedAt.IsZero() {
		t.Error("CreateMaintenanceWindow: CreatedAt is zero, want the column default")
	}

	page, err := db.ListMaintenanceWindows(ctx, store.MaintenanceFilter{})
	if err != nil {
		t.Fatalf("ListMaintenanceWindows: %v", err)
	}
	if len(page.Windows) != 1 || page.Windows[0].ID != created.ID {
		t.Fatalf("ListMaintenanceWindows = %+v, want the one created window", page.Windows)
	}

	if err := db.DeleteMaintenanceWindow(ctx, created.ID); err != nil {
		t.Fatalf("DeleteMaintenanceWindow: %v", err)
	}
	page, err = db.ListMaintenanceWindows(ctx, store.MaintenanceFilter{})
	if err != nil {
		t.Fatalf("ListMaintenanceWindows after the delete: %v", err)
	}
	if len(page.Windows) != 0 {
		t.Errorf("%d windows remain after the delete, want 0", len(page.Windows))
	}
	if err := db.DeleteMaintenanceWindow(ctx, created.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second DeleteMaintenanceWindow = %v, want ErrNotFound", err)
	}
	if err := db.DeleteMaintenanceWindow(ctx, uuid.NewString()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DeleteMaintenanceWindow(unknown) = %v, want ErrNotFound", err)
	}
}

// TestMaintenanceCheckConstraintRejectsAnInvertedWindow is the DB half of the end-after-start rule.
func TestMaintenanceCheckConstraintRejectsAnInvertedWindow(t *testing.T) {
	db, dsn := newMaintenanceDB(t)
	ctx := context.Background()

	// The store rejects it before the round trip.
	start := time.Now().UTC()
	bad := maintenanceInput("node-a", start, -time.Hour)
	if _, err := db.CreateMaintenanceWindow(ctx, bad); err == nil {
		t.Fatal("CreateMaintenanceWindow(end before start) succeeded, want a validation error")
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	for _, tc := range []struct {
		name string
		end  time.Time
	}{
		{"end before start", start.Add(-time.Hour)},
		{"end equal to start", start},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pool.Exec(ctx, `
				INSERT INTO maintenance_windows (scope, start_at, end_at, reason, created_by)
				VALUES ('node-a', $1, $2, 'raw', 'test')`, start, tc.end)
			if err == nil {
				t.Fatal("the raw INSERT succeeded; maintenance_end_after_start is not enforcing end_at > start_at")
			}
		})
	}

	page, err := db.ListMaintenanceWindows(ctx, store.MaintenanceFilter{})
	if err != nil {
		t.Fatalf("ListMaintenanceWindows: %v", err)
	}
	if len(page.Windows) != 0 {
		t.Errorf("rejected inserts left %d rows behind", len(page.Windows))
	}
}

// TestListMaintenanceWindowsWindowIsOverlapNotContainment is the markArea query's real claim.
func TestListMaintenanceWindowsWindowIsOverlapNotContainment(t *testing.T) {
	db, _ := newMaintenanceDB(t)
	ctx := context.Background()

	from := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	to := from.Add(time.Hour)

	mustCreate := func(reason string, start time.Time, d time.Duration) {
		t.Helper()
		in := maintenanceInput("node-a", start, d)
		in.Reason = reason
		if _, err := db.CreateMaintenanceWindow(ctx, in); err != nil {
			t.Fatalf("CreateMaintenanceWindow(%s): %v", reason, err)
		}
	}

	// Straddles the lower bound. Must match.
	mustCreate("straddles-start", from.Add(-30*time.Minute), 40*time.Minute)
	// Wholly inside. Must match.
	mustCreate("inside", from.Add(20*time.Minute), 10*time.Minute)
	// Ends exactly at "from". Must match -- the lower bound is inclusive on
	// the window's end.
	mustCreate("ends-at-from", from.Add(-time.Hour), time.Hour)
	// Starts exactly at "to". Must NOT match -- the upper bound is exclusive.
	mustCreate("at-to", to, time.Hour)
	// Entirely before. Must NOT match.
	mustCreate("before", from.Add(-3*time.Hour), time.Hour)

	page, err := db.ListMaintenanceWindows(ctx, store.MaintenanceFilter{From: from, To: to})
	if err != nil {
		t.Fatalf("ListMaintenanceWindows: %v", err)
	}
	got := make(map[string]bool, len(page.Windows))
	for _, w := range page.Windows {
		got[w.Reason] = true
	}
	for _, want := range []string{"straddles-start", "inside", "ends-at-from"} {
		if !got[want] {
			t.Errorf("the range dropped %q, which overlaps it", want)
		}
	}
	for _, unwanted := range []string{"at-to", "before"} {
		if got[unwanted] {
			t.Errorf("the range returned %q, which does not overlap it", unwanted)
		}
	}
	if len(page.Windows) != 3 {
		t.Errorf("the range returned %d windows, want 3: %v", len(page.Windows), got)
	}
}

// TestListMaintenanceWindowsScopeFilterCanSelectTheGlobalOnes is why
// MaintenanceFilter.Scope is a pointer -- "" is the GLOBAL scope, a real
// value, so "no filter" and "the global ones" are two different requests.
func TestListMaintenanceWindowsScopeFilterCanSelectTheGlobalOnes(t *testing.T) {
	db, _ := newMaintenanceDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	for i, scope := range []string{"", "node-a", "node-b", ""} {
		if _, err := db.CreateMaintenanceWindow(ctx,
			maintenanceInput(scope, now.Add(time.Duration(i)*time.Minute), time.Hour)); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	all, err := db.ListMaintenanceWindows(ctx, store.MaintenanceFilter{})
	if err != nil {
		t.Fatalf("ListMaintenanceWindows(nil scope): %v", err)
	}
	if len(all.Windows) != 4 {
		t.Errorf("a nil scope returned %d windows, want all 4", len(all.Windows))
	}

	global, err := db.ListMaintenanceWindows(ctx, store.MaintenanceFilter{Scope: scopePtr("")})
	if err != nil {
		t.Fatalf("ListMaintenanceWindows(global): %v", err)
	}
	if len(global.Windows) != 2 {
		t.Fatalf("scope=\"\" returned %d windows, want the 2 global ones", len(global.Windows))
	}
	for _, w := range global.Windows {
		if w.Scope != "" {
			t.Errorf("scope=\"\" returned a %q-scoped window", w.Scope)
		}
	}

	scoped, err := db.ListMaintenanceWindows(ctx, store.MaintenanceFilter{Scope: scopePtr("node-a")})
	if err != nil {
		t.Fatalf("ListMaintenanceWindows(node-a): %v", err)
	}
	if len(scoped.Windows) != 1 || scoped.Windows[0].Scope != "node-a" {
		t.Errorf("scope=node-a returned %+v, want exactly the node-a window", scoped.Windows)
	}
}

// TestListMaintenanceWindowsPagesNewestFirst covers the keyset over start_at.
func TestListMaintenanceWindowsPagesNewestFirst(t *testing.T) {
	db, _ := newMaintenanceDB(t)
	ctx := context.Background()

	const total = 7
	base := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Microsecond)
	for i := 0; i < total; i++ {
		in := maintenanceInput("node-"+strconv.Itoa(i), base.Add(time.Duration(i)*time.Hour), time.Minute)
		if _, err := db.CreateMaintenanceWindow(ctx, in); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	seen := make(map[string]bool, total)
	var last time.Time
	cursor := ""
	pages := 0
	for {
		page, err := db.ListMaintenanceWindows(ctx, store.MaintenanceFilter{Cursor: cursor, Limit: 3})
		if err != nil {
			t.Fatalf("ListMaintenanceWindows(page %d): %v", pages, err)
		}
		pages++
		for _, w := range page.Windows {
			if seen[w.ID] {
				t.Fatalf("window %s appeared on two pages", w.ID)
			}
			seen[w.ID] = true
			if !last.IsZero() && w.StartAt.After(last) {
				t.Fatalf("page order broke: %v came after %v", w.StartAt, last)
			}
			last = w.StartAt
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
		t.Errorf("paged through %d windows, want %d", len(seen), total)
	}
}

// TestDeleteMaintenanceWindowsBeforeUsesEndAt pins which column retention
// reads: a window that is still open is still current however long ago it
// began, so an ancient window that has not yet ended must survive the sweep.
func TestDeleteMaintenanceWindowsBeforeUsesEndAt(t *testing.T) {
	db, _ := newMaintenanceDB(t)
	ctx := context.Background()

	ancient := time.Now().UTC().Add(-200 * 24 * time.Hour).Truncate(time.Microsecond)
	cutoff := time.Now().UTC().Add(-90 * 24 * time.Hour)

	// Began 200 days ago, ended 199 days ago: expired.
	if _, err := db.CreateMaintenanceWindow(ctx, maintenanceInput("expired", ancient, 24*time.Hour)); err != nil {
		t.Fatalf("seed expired: %v", err)
	}
	// Began 200 days ago and runs for a year: STILL OPEN, must survive.
	if _, err := db.CreateMaintenanceWindow(ctx,
		maintenanceInput("long-runner", ancient, 365*24*time.Hour)); err != nil {
		t.Fatalf("seed long-runner: %v", err)
	}

	n, err := db.DeleteMaintenanceWindowsBefore(ctx, cutoff, 100)
	if err != nil {
		t.Fatalf("DeleteMaintenanceWindowsBefore: %v", err)
	}
	if n != 1 {
		t.Fatalf("DeleteMaintenanceWindowsBefore deleted %d rows, want 1", n)
	}

	page, err := db.ListMaintenanceWindows(ctx, store.MaintenanceFilter{})
	if err != nil {
		t.Fatalf("ListMaintenanceWindows: %v", err)
	}
	if len(page.Windows) != 1 || page.Windows[0].Scope != "long-runner" {
		t.Errorf("after the sweep %+v remain, want just the still-open window", page.Windows)
	}
}

//go:build integration

package store_test

// TestIncident* / TestListIncidents* require a real PostgreSQL.

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// newIncidentsDB opens a *store.DB with migrations applied, dropping and
// re-creating the schema first -- same convention as newAnnotationsDB.
func newIncidentsDB(t *testing.T) *store.DB {
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

func incidentInput(title string, from time.Time) store.IncidentInput {
	return store.IncidentInput{
		Title:     title,
		Scope:     "node-a",
		FromAt:    from,
		CreatedBy: "user:admin",
	}
}

func TestIncidentLifecycle(t *testing.T) {
	db := newIncidentsDB(t)
	ctx := context.Background()

	from := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	to := from.Add(30 * time.Minute)

	in := incidentInput("loss spike on node-a", from)
	in.ToAt = &to
	in.Notes = "began after the CNI rollout"

	created, err := db.CreateIncident(ctx, in)
	if err != nil {
		t.Fatalf("CreateIncident: %v", err)
	}
	if created.ID == "" {
		t.Fatal("CreateIncident: ID is empty, want a minted UUID")
	}
	if created.Status != store.IncidentStatusOpen {
		t.Errorf("CreateIncident: Status = %q, want %q (the column default's Go twin)",
			created.Status, store.IncidentStatusOpen)
	}
	if created.ResolvedAt != nil {
		t.Errorf("CreateIncident: ResolvedAt = %v for a new incident, want nil", created.ResolvedAt)
	}
	if !created.FromAt.Equal(from) || created.ToAt == nil || !created.ToAt.Equal(to) {
		t.Errorf("CreateIncident: range = (%v, %v), want (%v, %v)", created.FromAt, created.ToAt, from, to)
	}
	if string(created.Pinned) != "[]" {
		t.Errorf("CreateIncident: Pinned = %s, want [] (an ARRAY, not {})", created.Pinned)
	}
	if created.CreatedAt.IsZero() {
		t.Error("CreateIncident: CreatedAt is zero, want the column default")
	}

	got, err := db.GetIncident(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetIncident: %v", err)
	}
	if got.ID != created.ID || got.Title != created.Title || got.Notes != created.Notes {
		t.Errorf("GetIncident = %+v, want %+v", got, created)
	}

	// Notes: the narrow update touches notes and nothing else.
	updated, err := db.UpdateIncidentNotes(ctx, created.ID, "root cause: a flapping uplink")
	if err != nil {
		t.Fatalf("UpdateIncidentNotes: %v", err)
	}
	if updated.Notes != "root cause: a flapping uplink" {
		t.Errorf("UpdateIncidentNotes: Notes = %q, want the new value", updated.Notes)
	}
	if updated.Title != created.Title || updated.Scope != created.Scope || updated.Status != created.Status {
		t.Errorf("UpdateIncidentNotes changed a field it must not: %+v vs %+v", updated, created)
	}

	// Pinned: same claim.
	pins := json.RawMessage(`[{"kind":"k8s","id":"901","note":"NodeNotReady"},{"kind":"event","id":"12"}]`)
	updated, err = db.UpdateIncidentPinned(ctx, created.ID, pins)
	if err != nil {
		t.Fatalf("UpdateIncidentPinned: %v", err)
	}
	refs, err := store.DecodePinned(updated.Pinned)
	if err != nil {
		t.Fatalf("DecodePinned: %v", err)
	}
	if len(refs) != 2 || refs[0].Kind != "k8s" || refs[0].ID != "901" || refs[0].Note != "NodeNotReady" {
		t.Errorf("UpdateIncidentPinned round trip = %+v, want the two pins in order", refs)
	}
	if updated.Notes != "root cause: a flapping uplink" {
		t.Error("UpdateIncidentPinned clobbered notes: the updates must stay narrow")
	}

	// Resolve, then reopen.
	resolvedAt := time.Now().UTC().Truncate(time.Microsecond)
	resolved, err := db.UpdateIncidentStatus(ctx, created.ID, store.IncidentStatusResolved, &resolvedAt)
	if err != nil {
		t.Fatalf("UpdateIncidentStatus(resolved): %v", err)
	}
	if resolved.Status != store.IncidentStatusResolved {
		t.Errorf("Status = %q, want resolved", resolved.Status)
	}
	if resolved.ResolvedAt == nil || !resolved.ResolvedAt.Equal(resolvedAt) {
		t.Errorf("ResolvedAt = %v, want %v", resolved.ResolvedAt, resolvedAt)
	}

	reopened, err := db.UpdateIncidentStatus(ctx, created.ID, store.IncidentStatusOpen, nil)
	if err != nil {
		t.Fatalf("UpdateIncidentStatus(reopen): %v", err)
	}
	if reopened.Status != store.IncidentStatusOpen {
		t.Errorf("Status = %q, want open", reopened.Status)
	}
	if reopened.ResolvedAt != nil {
		t.Errorf("ResolvedAt = %v after a reopen, want nil: a reopened incident must not keep a resolution time",
			reopened.ResolvedAt)
	}

	if err := db.DeleteIncident(ctx, created.ID); err != nil {
		t.Fatalf("DeleteIncident: %v", err)
	}
	if _, err := db.GetIncident(ctx, created.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetIncident after the delete = %v, want ErrNotFound", err)
	}
	if err := db.DeleteIncident(ctx, created.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second DeleteIncident = %v, want ErrNotFound", err)
	}
}

// TestIncidentOpenEndedRangeRoundTrips pins the NULL to_at: an open-ended
// incident reads back with a nil ToAt, and the listing's own overlap filter
// treats it as extending to infinity (see the window test below).
func TestIncidentOpenEndedRangeRoundTrips(t *testing.T) {
	db := newIncidentsDB(t)
	ctx := context.Background()
	from := time.Now().UTC().Truncate(time.Microsecond)

	created, err := db.CreateIncident(ctx, incidentInput("still going", from))
	if err != nil {
		t.Fatalf("CreateIncident: %v", err)
	}
	if created.ToAt != nil {
		t.Errorf("ToAt = %v for an open-ended incident, want nil", created.ToAt)
	}

	got, err := db.GetIncident(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetIncident: %v", err)
	}
	if got.ToAt != nil {
		t.Errorf("GetIncident: ToAt = %v, want nil", got.ToAt)
	}
}

// TestIncidentInvalidInputNeverReachesTheDatabase asserts validation runs
// before the INSERT: a rejected input leaves no row.
func TestIncidentInvalidInputNeverReachesTheDatabase(t *testing.T) {
	db := newIncidentsDB(t)
	ctx := context.Background()

	bad := incidentInput(strings.Repeat("t", 256), time.Now().UTC())
	if _, err := db.CreateIncident(ctx, bad); err == nil {
		t.Fatal("CreateIncident with a 256-byte title succeeded, want a validation error")
	}

	worse := incidentInput("bad pins", time.Now().UTC())
	worse.Pinned = json.RawMessage(`[{"kind":"metric","id":"up"}]`)
	if _, err := db.CreateIncident(ctx, worse); err == nil {
		t.Fatal("CreateIncident with an unknown pinned kind succeeded, want a validation error")
	}

	page, err := db.ListIncidents(ctx, store.IncidentFilter{})
	if err != nil {
		t.Fatalf("ListIncidents: %v", err)
	}
	if len(page.Incidents) != 0 {
		t.Errorf("rejected creates left %d rows behind", len(page.Incidents))
	}
}

// TestIncidentPinnedIsStoredAsAJSONArray is the orEmptyPinnedArray claim checked against real
// jsonb.
func TestIncidentPinnedIsStoredAsAJSONArray(t *testing.T) {
	db := newIncidentsDB(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		in   json.RawMessage
	}{
		{"nil", nil},
		{"empty", json.RawMessage{}},
		{"json null", json.RawMessage(`null`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := incidentInput("pins-"+tc.name, time.Now().UTC())
			in.Pinned = tc.in
			created, err := db.CreateIncident(ctx, in)
			if err != nil {
				t.Fatalf("CreateIncident: %v", err)
			}
			refs, err := store.DecodePinned(created.Pinned)
			if err != nil {
				t.Fatalf("DecodePinned(%s): %v", created.Pinned, err)
			}
			if len(refs) != 0 {
				t.Errorf("Pinned = %s, want an empty array", created.Pinned)
			}

			// And on the UPDATE path, which has no column DEFAULT to fall back
			// on -- the case orEmptyPinnedArray exists for.
			updated, err := db.UpdateIncidentPinned(ctx, created.ID, tc.in)
			if err != nil {
				t.Fatalf("UpdateIncidentPinned: %v", err)
			}
			if string(updated.Pinned) != "[]" {
				t.Errorf("UpdateIncidentPinned(%s) stored %s, want []", tc.in, updated.Pinned)
			}
		})
	}
}

// TestListIncidentsStatusAndScopeFilters covers both exact-match filters,
// including AnnotationFilter.Scope's pointer distinction: nil is "every
// scope", a pointer to "" is "the global ones only".
func TestListIncidentsStatusAndScopeFilters(t *testing.T) {
	db := newIncidentsDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	for i, scope := range []string{"", "node-a", "node-b", ""} {
		in := incidentInput("incident-"+strconv.Itoa(i), now.Add(time.Duration(i)*time.Minute))
		in.Scope = scope
		created, err := db.CreateIncident(ctx, in)
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		if i == 1 {
			at := now
			if _, err := db.UpdateIncidentStatus(ctx, created.ID, store.IncidentStatusResolved, &at); err != nil {
				t.Fatalf("resolve seed %d: %v", i, err)
			}
		}
	}

	count := func(f store.IncidentFilter) int {
		t.Helper()
		page, err := db.ListIncidents(ctx, f)
		if err != nil {
			t.Fatalf("ListIncidents(%+v): %v", f, err)
		}
		return len(page.Incidents)
	}

	if got := count(store.IncidentFilter{}); got != 4 {
		t.Errorf("no filter returned %d incidents, want 4", got)
	}
	if got := count(store.IncidentFilter{Status: store.IncidentStatusOpen}); got != 3 {
		t.Errorf("status=open returned %d incidents, want 3", got)
	}
	if got := count(store.IncidentFilter{Status: store.IncidentStatusResolved}); got != 1 {
		t.Errorf("status=resolved returned %d incidents, want 1", got)
	}
	if got := count(store.IncidentFilter{Scope: scopePtr("")}); got != 2 {
		t.Errorf("scope=\"\" returned %d incidents, want the 2 global ones", got)
	}
	if got := count(store.IncidentFilter{Scope: scopePtr("node-a")}); got != 1 {
		t.Errorf("scope=node-a returned %d incidents, want 1", got)
	}
	if got := count(store.IncidentFilter{Status: store.IncidentStatusOpen, Scope: scopePtr("node-b")}); got != 1 {
		t.Errorf("status+scope returned %d incidents, want 1", got)
	}
}

// TestListIncidentsWindowIsOverlapNotContainment is the range filter's real claim; it also pins the
// open-ended case: a nil ToAt extends to infinity.
func TestListIncidentsWindowIsOverlapNotContainment(t *testing.T) {
	db := newIncidentsDB(t)
	ctx := context.Background()

	from := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	to := from.Add(time.Hour)

	mustCreate := func(title string, start time.Time, end *time.Time) {
		t.Helper()
		in := incidentInput(title, start)
		in.ToAt = end
		if _, err := db.CreateIncident(ctx, in); err != nil {
			t.Fatalf("CreateIncident(%s): %v", title, err)
		}
	}

	spanEnd := from.Add(10 * time.Minute)
	// Straddles the lower bound. Must match.
	mustCreate("straddles-start", from.Add(-30*time.Minute), &spanEnd)
	// Wholly inside. Must match.
	insideEnd := from.Add(30 * time.Minute)
	mustCreate("inside", from.Add(20*time.Minute), &insideEnd)
	// Open-ended, started long before the window. Must match -- nil to_at is
	// infinity, not from_at.
	mustCreate("still-open", from.Add(-10*24*time.Hour), nil)
	// Starts exactly at "to". Must NOT match -- the upper bound is exclusive.
	atToEnd := to.Add(time.Hour)
	mustCreate("at-to", to, &atToEnd)
	// Entirely before, closed before the window opens. Must NOT match.
	beforeEnd := from.Add(-time.Minute)
	mustCreate("before", from.Add(-2*time.Hour), &beforeEnd)

	page, err := db.ListIncidents(ctx, store.IncidentFilter{From: from, To: to})
	if err != nil {
		t.Fatalf("ListIncidents: %v", err)
	}
	got := make(map[string]bool, len(page.Incidents))
	for _, i := range page.Incidents {
		got[i.Title] = true
	}
	for _, want := range []string{"straddles-start", "inside", "still-open"} {
		if !got[want] {
			t.Errorf("the window dropped %q, which overlaps it", want)
		}
	}
	for _, unwanted := range []string{"at-to", "before"} {
		if got[unwanted] {
			t.Errorf("the window returned %q, which does not overlap it", unwanted)
		}
	}
	if len(page.Incidents) != 3 {
		t.Errorf("the window returned %d incidents, want 3: %v", len(page.Incidents), got)
	}
}

// TestListIncidentsPagesNewestFirst covers the UUID keyset over created_at.
func TestListIncidentsPagesNewestFirst(t *testing.T) {
	db := newIncidentsDB(t)
	ctx := context.Background()

	const total = 7
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	for i := 0; i < total; i++ {
		if _, err := db.CreateIncident(ctx, incidentInput("incident-"+strconv.Itoa(i),
			base.Add(time.Duration(i)*time.Minute))); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	seen := make(map[string]bool, total)
	var last time.Time
	cursor := ""
	pages := 0
	for {
		page, err := db.ListIncidents(ctx, store.IncidentFilter{Cursor: cursor, Limit: 3})
		if err != nil {
			t.Fatalf("ListIncidents(page %d): %v", pages, err)
		}
		pages++
		for _, i := range page.Incidents {
			if seen[i.ID] {
				t.Fatalf("incident %s appeared on two pages", i.ID)
			}
			seen[i.ID] = true
			if !last.IsZero() && i.CreatedAt.After(last) {
				t.Fatalf("page order broke: %v came after %v", i.CreatedAt, last)
			}
			last = i.CreatedAt
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
		t.Errorf("paged through %d incidents, want %d", len(seen), total)
	}
}

// TestGetIncidentUnknownIDIsNotFound is the seam's miss contract, across every
// id-taking method.
func TestGetIncidentUnknownIDIsNotFound(t *testing.T) {
	db := newIncidentsDB(t)
	ctx := context.Background()
	id := uuid.NewString()
	at := time.Now().UTC()

	if _, err := db.GetIncident(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetIncident(unknown) = %v, want ErrNotFound", err)
	}
	if err := db.DeleteIncident(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DeleteIncident(unknown) = %v, want ErrNotFound", err)
	}
	if _, err := db.UpdateIncidentNotes(ctx, id, "x"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UpdateIncidentNotes(unknown) = %v, want ErrNotFound", err)
	}
	if _, err := db.UpdateIncidentPinned(ctx, id, nil); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UpdateIncidentPinned(unknown) = %v, want ErrNotFound", err)
	}
	if _, err := db.UpdateIncidentStatus(ctx, id, store.IncidentStatusResolved, &at); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UpdateIncidentStatus(unknown) = %v, want ErrNotFound", err)
	}
}

// TestDeleteIncidentsBeforeNeverPrunesAnOpenIncident is the rule the whole retention story for this
// table turns on; three incidents, all ancient: - resolved long ago -> deleted.
func TestDeleteIncidentsBeforeNeverPrunesAnOpenIncident(t *testing.T) {
	db := newIncidentsDB(t)
	ctx := context.Background()

	ancient := time.Now().UTC().Add(-200 * 24 * time.Hour).Truncate(time.Microsecond)
	recent := time.Now().UTC().Truncate(time.Microsecond)
	cutoff := time.Now().UTC().Add(-90 * 24 * time.Hour)

	resolvedOld, err := db.CreateIncident(ctx, incidentInput("resolved-long-ago", ancient))
	if err != nil {
		t.Fatalf("CreateIncident(resolved-long-ago): %v", err)
	}
	if _, err := db.UpdateIncidentStatus(ctx, resolvedOld.ID, store.IncidentStatusResolved, &ancient); err != nil {
		t.Fatalf("resolve resolved-long-ago: %v", err)
	}

	resolvedNew, err := db.CreateIncident(ctx, incidentInput("resolved-recently", ancient))
	if err != nil {
		t.Fatalf("CreateIncident(resolved-recently): %v", err)
	}
	if _, err := db.UpdateIncidentStatus(ctx, resolvedNew.ID, store.IncidentStatusResolved, &recent); err != nil {
		t.Fatalf("resolve resolved-recently: %v", err)
	}

	stillOpen, err := db.CreateIncident(ctx, incidentInput("never-closed", ancient))
	if err != nil {
		t.Fatalf("CreateIncident(never-closed): %v", err)
	}

	n, err := db.DeleteIncidentsBefore(ctx, cutoff, 100)
	if err != nil {
		t.Fatalf("DeleteIncidentsBefore: %v", err)
	}
	if n != 1 {
		t.Fatalf("DeleteIncidentsBefore deleted %d rows, want exactly 1 (the long-resolved one)", n)
	}

	if _, err := db.GetIncident(ctx, resolvedOld.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the long-resolved incident survived the sweep: %v", err)
	}
	if _, err := db.GetIncident(ctx, resolvedNew.ID); err != nil {
		t.Errorf("the recently-resolved incident was swept: %v", err)
	}
	if _, err := db.GetIncident(ctx, stillOpen.ID); err != nil {
		t.Errorf("an OPEN incident was pruned: %v -- open incidents are never pruned, at any age", err)
	}
}

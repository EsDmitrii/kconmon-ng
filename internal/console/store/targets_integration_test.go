//go:build integration

package store_test

// TestTarget* / TestDefinition* / TestSchedule* require a real PostgreSQL.

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// newTargetsDB opens a *store.DB with migrations applied, dropping and re-creating the schema
// first.
func newTargetsDB(t *testing.T) (*store.DB, string) {
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

func mustCreateTarget(t *testing.T, ctx context.Context, db *store.DB, name string) store.Target {
	t.Helper()
	tgt, err := db.CreateTarget(ctx, store.TargetInput{Name: name, Kind: "host", Address: "10.0.0.1"})
	if err != nil {
		t.Fatalf("CreateTarget(%q): %v", name, err)
	}
	return tgt
}

func mustCreateDefinition(t *testing.T, ctx context.Context, db *store.DB, name, targetID string) store.Definition {
	t.Helper()
	in := store.DefinitionInput{
		Name:            name,
		SourceSelection: "all",
		DestinationKind: "node",
		CheckType:       "tcp",
		Plane:           "pod",
	}
	if targetID != "" {
		in.DestinationKind = "target"
		in.DestinationTargetID = targetID
	}
	def, err := db.CreateDefinition(ctx, in)
	if err != nil {
		t.Fatalf("CreateDefinition(%q): %v", name, err)
	}
	return def
}

// TestTargetLifecycle is the happy path: create -> get -> update -> delete,
// asserting each step's effect through an independent read.
func TestTargetLifecycle(t *testing.T) {
	db, _ := newTargetsDB(t)
	ctx := context.Background()

	created, err := db.CreateTarget(ctx, store.TargetInput{
		Name:    "edge-gw",
		Kind:    "host",
		Address: "10.0.0.1",
		Labels:  json.RawMessage(`{"env": "prod"}`),
	})
	if err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	if created.ID == "" {
		t.Fatal("CreateTarget: ID is empty, want a minted UUID")
	}
	if created.Name != "edge-gw" || created.Kind != "host" || created.Address != "10.0.0.1" {
		t.Errorf("CreateTarget: got %+v, want the input's fields back", created)
	}
	if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Error("CreateTarget: timestamps are zero, want the column defaults")
	}

	got, err := db.GetTarget(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetTarget: %v", err)
	}
	if got.ID != created.ID || got.Name != created.Name {
		t.Errorf("GetTarget = %+v, want %+v", got, created)
	}
	var labels map[string]string
	if err := json.Unmarshal(got.Labels, &labels); err != nil {
		t.Fatalf("GetTarget: labels are not a JSON object: %v", err)
	}
	if labels["env"] != "prod" {
		t.Errorf("GetTarget: labels = %v, want env=prod", labels)
	}

	updated, err := db.UpdateTarget(ctx, created.ID, store.TargetInput{
		Name:    "edge-gw-2",
		Kind:    "url",
		Address: "https://example.test/health",
	})
	if err != nil {
		t.Fatalf("UpdateTarget: %v", err)
	}
	if updated.Name != "edge-gw-2" || updated.Kind != "url" {
		t.Errorf("UpdateTarget: got %+v, want the new name and kind", updated)
	}
	if string(updated.Labels) != `{}` {
		t.Errorf("UpdateTarget: labels = %s, want {} for a nil input (a full replace, not a patch)", updated.Labels)
	}
	if !updated.UpdatedAt.After(created.UpdatedAt) {
		t.Errorf("UpdateTarget: updated_at = %v, want it after the create's %v", updated.UpdatedAt, created.UpdatedAt)
	}
	if !updated.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("UpdateTarget: created_at moved from %v to %v", created.CreatedAt, updated.CreatedAt)
	}

	if err := db.DeleteTarget(ctx, created.ID); err != nil {
		t.Fatalf("DeleteTarget: %v", err)
	}
	if _, err := db.GetTarget(ctx, created.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetTarget after delete: err = %v, want ErrNotFound", err)
	}
	if err := db.DeleteTarget(ctx, created.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second DeleteTarget: err = %v, want ErrNotFound", err)
	}
}

// TestCreateTargetDuplicateNameIsAlreadyExists pins the UNIQUE(name)
// constraint's mapping: a second target under a taken name is
// ErrAlreadyExists, never a raw SQLSTATE reaching the caller.
func TestCreateTargetDuplicateNameIsAlreadyExists(t *testing.T) {
	db, _ := newTargetsDB(t)
	ctx := context.Background()

	mustCreateTarget(t, ctx, db, "edge-gw")

	_, err := db.CreateTarget(ctx, store.TargetInput{Name: "edge-gw", Kind: "url", Address: "https://other.test"})
	if !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("CreateTarget with a taken name: err = %v, want ErrAlreadyExists", err)
	}

	// The same rule on the update path: renaming onto a taken name.
	other := mustCreateTarget(t, ctx, db, "core-gw")
	_, err = db.UpdateTarget(ctx, other.ID, store.TargetInput{Name: "edge-gw", Kind: "host", Address: "10.0.0.2"})
	if !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("UpdateTarget onto a taken name: err = %v, want ErrAlreadyExists", err)
	}
}

// TestDeleteTargetReferencedByDefinitionIsInUse is the ON DELETE RESTRICT contract.
func TestDeleteTargetReferencedByDefinitionIsInUse(t *testing.T) {
	db, _ := newTargetsDB(t)
	ctx := context.Background()

	tgt := mustCreateTarget(t, ctx, db, "edge-gw")
	def := mustCreateDefinition(t, ctx, db, "edge-tcp", tgt.ID)
	if def.DestinationTargetID != tgt.ID {
		t.Fatalf("CreateDefinition: DestinationTargetID = %q, want %q", def.DestinationTargetID, tgt.ID)
	}

	err := db.DeleteTarget(ctx, tgt.ID)
	if !errors.Is(err, store.ErrInUse) {
		t.Fatalf("DeleteTarget while referenced: err = %v, want ErrInUse", err)
	}

	if _, err := db.GetTarget(ctx, tgt.ID); err != nil {
		t.Errorf("GetTarget after the refused delete: %v", err)
	}
	survived, err := db.GetDefinition(ctx, def.ID)
	if err != nil {
		t.Fatalf("GetDefinition after the refused delete: %v", err)
	}
	if survived.DestinationTargetID != tgt.ID {
		t.Errorf("definition's DestinationTargetID = %q, want %q -- the reference was not preserved", survived.DestinationTargetID, tgt.ID)
	}

	// Removing the definition unblocks the delete.
	if err := db.DeleteDefinition(ctx, def.ID); err != nil {
		t.Fatalf("DeleteDefinition: %v", err)
	}
	if err := db.DeleteTarget(ctx, tgt.ID); err != nil {
		t.Fatalf("DeleteTarget after the definition went away: %v", err)
	}
}

// TestCreateDefinitionUnknownTargetIsNotFound pins the other direction of the
// same SQLSTATE: an INSERT naming a target that does not exist is ErrNotFound
// (the reference is missing), never ErrInUse.
func TestCreateDefinitionUnknownTargetIsNotFound(t *testing.T) {
	db, _ := newTargetsDB(t)
	ctx := context.Background()

	_, err := db.CreateDefinition(ctx, store.DefinitionInput{
		Name:                "edge-tcp",
		SourceSelection:     "all",
		DestinationKind:     "target",
		DestinationTargetID: "0f1d1a2f-6f8e-4a3a-9a0e-7f3f9d0f1c22",
		CheckType:           "tcp",
		Plane:               "pod",
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("CreateDefinition with an unknown target: err = %v, want ErrNotFound", err)
	}
	if errors.Is(err, store.ErrInUse) {
		t.Error("CreateDefinition with an unknown target reported ErrInUse; that is the DELETE direction")
	}
}

// The probe goes through CreateDefinition and UpdateDefinition rather than through Validate
// directly (targets_test.go covers the rule itself).
func TestCreateDefinitionRejectsUndialableAdhocAddress(t *testing.T) {
	db, _ := newTargetsDB(t)
	ctx := context.Background()

	bad := store.DefinitionInput{
		Name:               "edge-adhoc",
		SourceSelection:    "all",
		DestinationKind:    "adhoc",
		DestinationAddress: "sdfsdfsdf !!",
		CheckType:          "tcp",
		Plane:              "pod",
	}
	if _, err := db.CreateDefinition(ctx, bad); err == nil {
		t.Fatal("CreateDefinition with an undialable adhoc address = nil, want an error")
	} else if !strings.Contains(err.Error(), "destination address") {
		t.Errorf("CreateDefinition error %q does not name the destination address field", err)
	}

	// Nothing was written: the refusal happens before the INSERT.
	page, err := db.ListDefinitions(ctx, store.DefinitionFilter{})
	if err != nil {
		t.Fatalf("ListDefinitions: %v", err)
	}
	if len(page.Definitions) != 0 {
		t.Fatalf("ListDefinitions after the refused create = %d rows, want 0", len(page.Definitions))
	}

	// The shapes the agent CAN dial still go in, unchanged.
	good := bad
	good.DestinationAddress = "example.test:8443"
	def, err := db.CreateDefinition(ctx, good)
	if err != nil {
		t.Fatalf("CreateDefinition with host:port: %v", err)
	}
	if def.DestinationAddress != "example.test:8443" {
		t.Errorf("DestinationAddress = %q, want it stored verbatim", def.DestinationAddress)
	}

	// And an update cannot walk a good row into a bad one.
	broken := good
	broken.DestinationAddress = "example.test:http"
	if _, err := db.UpdateDefinition(ctx, def.ID, broken); err == nil {
		t.Fatal("UpdateDefinition to a non-numeric port = nil, want an error")
	}
	after, err := db.GetDefinition(ctx, def.ID)
	if err != nil {
		t.Fatalf("GetDefinition: %v", err)
	}
	if after.DestinationAddress != "example.test:8443" {
		t.Errorf("DestinationAddress after the refused update = %q, want the original", after.DestinationAddress)
	}
}

// TestDeleteDefinitionCascadesSchedules pins check_schedules.definition_id's
// ON DELETE CASCADE: the definition's schedules go with it, and no orphan row
// is left behind.
func TestDeleteDefinitionCascadesSchedules(t *testing.T) {
	db, _ := newTargetsDB(t)
	ctx := context.Background()

	def := mustCreateDefinition(t, ctx, db, "edge-tcp", "")
	keep := mustCreateDefinition(t, ctx, db, "core-tcp", "")

	next := time.Now().Add(time.Minute).UTC()
	var scheduleIDs []string
	for range 3 {
		s, err := db.CreateSchedule(ctx, store.ScheduleInput{
			DefinitionID: def.ID,
			Kind:         "interval",
			IntervalNs:   int64(time.Minute),
			Enabled:      true,
			NextFireAt:   &next,
		})
		if err != nil {
			t.Fatalf("CreateSchedule: %v", err)
		}
		scheduleIDs = append(scheduleIDs, s.ID)
	}
	survivor, err := db.CreateSchedule(ctx, store.ScheduleInput{
		DefinitionID: keep.ID,
		Kind:         "continuous",
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateSchedule for the untouched definition: %v", err)
	}

	page, err := db.ListSchedules(ctx, store.ScheduleFilter{DefinitionID: def.ID})
	if err != nil {
		t.Fatalf("ListSchedules: %v", err)
	}
	if len(page.Schedules) != 3 {
		t.Fatalf("ListSchedules(definition): got %d, want 3", len(page.Schedules))
	}

	if err := db.DeleteDefinition(ctx, def.ID); err != nil {
		t.Fatalf("DeleteDefinition: %v", err)
	}
	for _, id := range scheduleIDs {
		if _, err := db.GetSchedule(ctx, id); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("GetSchedule(%s) after the cascade: err = %v, want ErrNotFound", id, err)
		}
	}
	if _, err := db.GetSchedule(ctx, survivor.ID); err != nil {
		t.Errorf("the other definition's schedule was collateral damage: %v", err)
	}
}

// TestCreateScheduleUnknownDefinitionIsNotFound pins the schedule side of the
// foreign key.
func TestCreateScheduleUnknownDefinitionIsNotFound(t *testing.T) {
	db, _ := newTargetsDB(t)
	ctx := context.Background()

	_, err := db.CreateSchedule(ctx, store.ScheduleInput{
		DefinitionID: "0f1d1a2f-6f8e-4a3a-9a0e-7f3f9d0f1c22",
		Kind:         "continuous",
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("CreateSchedule with an unknown definition: err = %v, want ErrNotFound", err)
	}
}

// TestListTargetsKeysetPagesWithoutDuplicates walks every page of a 25-row table two at a time and
// asserts each id is seen exactly once.
func TestListTargetsKeysetPagesWithoutDuplicates(t *testing.T) {
	db, _ := newTargetsDB(t)
	ctx := context.Background()

	const total = 25
	want := make(map[string]bool, total)
	for i := range total {
		// Distinct names, and created_at values that mostly collide (the
		// inserts are milliseconds apart), so the cursor's id half is doing
		// real tie-breaking work rather than being decorative.
		tgt := mustCreateTarget(t, ctx, db, "gw-"+string(rune('a'+i%26))+"-"+strings.Repeat("x", i%5+1))
		want[tgt.ID] = true
	}

	seen := make(map[string]int, total)
	var cursor string
	var order []time.Time
	for page := 0; ; page++ {
		if page > total {
			t.Fatal("ListTargets did not terminate; cursor is not advancing")
		}
		got, err := db.ListTargets(ctx, store.TargetFilter{Cursor: cursor, Limit: 2})
		if err != nil {
			t.Fatalf("ListTargets(page %d): %v", page, err)
		}
		for _, tgt := range got.Targets {
			seen[tgt.ID]++
			order = append(order, tgt.CreatedAt)
		}
		if got.NextCursor == "" {
			break
		}
		cursor = got.NextCursor
	}

	if len(seen) != total {
		t.Errorf("saw %d distinct targets across pages, want %d", len(seen), total)
	}
	for id := range want {
		switch seen[id] {
		case 1:
		case 0:
			t.Errorf("target %s never appeared on any page", id)
		default:
			t.Errorf("target %s appeared on %d pages, want exactly 1", id, seen[id])
		}
	}
	for i := 1; i < len(order); i++ {
		if order[i].After(order[i-1]) {
			t.Fatalf("results are not newest-first at index %d: %v came after %v", i, order[i], order[i-1])
		}
	}
}

// TestListTargetsFiltersByKind asserts the kind filter and that it composes
// with the cursor rather than resetting it.
func TestListTargetsFiltersByKind(t *testing.T) {
	db, _ := newTargetsDB(t)
	ctx := context.Background()

	for i := range 4 {
		mustCreateTarget(t, ctx, db, "host-"+string(rune('a'+i)))
	}
	for i := range 3 {
		if _, err := db.CreateTarget(ctx, store.TargetInput{
			Name: "url-" + string(rune('a'+i)), Kind: "url", Address: "https://example.test",
		}); err != nil {
			t.Fatalf("CreateTarget(url): %v", err)
		}
	}

	page, err := db.ListTargets(ctx, store.TargetFilter{Kind: "url"})
	if err != nil {
		t.Fatalf("ListTargets(kind=url): %v", err)
	}
	if len(page.Targets) != 3 {
		t.Fatalf("ListTargets(kind=url): got %d, want 3", len(page.Targets))
	}
	for _, tgt := range page.Targets {
		if tgt.Kind != "url" {
			t.Errorf("ListTargets(kind=url) returned a %q target", tgt.Kind)
		}
	}
	if page.NextCursor != "" {
		t.Errorf("NextCursor = %q on a short page, want empty", page.NextCursor)
	}
}

// TestListDefinitionsFiltersByTarget covers the check_definitions_target_idx
// query: "which definitions would this target's deletion break?".
func TestListDefinitionsFiltersByTarget(t *testing.T) {
	db, _ := newTargetsDB(t)
	ctx := context.Background()

	tgt := mustCreateTarget(t, ctx, db, "edge-gw")
	other := mustCreateTarget(t, ctx, db, "core-gw")
	mustCreateDefinition(t, ctx, db, "edge-tcp", tgt.ID)
	mustCreateDefinition(t, ctx, db, "edge-http", tgt.ID)
	mustCreateDefinition(t, ctx, db, "core-tcp", other.ID)
	mustCreateDefinition(t, ctx, db, "node-tcp", "")

	page, err := db.ListDefinitions(ctx, store.DefinitionFilter{TargetID: tgt.ID})
	if err != nil {
		t.Fatalf("ListDefinitions(target): %v", err)
	}
	if len(page.Definitions) != 2 {
		t.Fatalf("ListDefinitions(target): got %d, want 2", len(page.Definitions))
	}
	for _, def := range page.Definitions {
		if def.DestinationTargetID != tgt.ID {
			t.Errorf("ListDefinitions(target) returned a definition pointing at %q", def.DestinationTargetID)
		}
	}

	all, err := db.ListDefinitions(ctx, store.DefinitionFilter{})
	if err != nil {
		t.Fatalf("ListDefinitions(unfiltered): %v", err)
	}
	if len(all.Definitions) != 4 {
		t.Errorf("ListDefinitions(unfiltered): got %d, want 4", len(all.Definitions))
	}
	// The node-scoped definition reads its NULL destination_target_id back as
	// "", never as a zero UUID.
	var nodeScoped int
	for _, def := range all.Definitions {
		if def.DestinationKind == "node" {
			nodeScoped++
			if def.DestinationTargetID != "" {
				t.Errorf("node-scoped definition's DestinationTargetID = %q, want empty", def.DestinationTargetID)
			}
		}
	}
	if nodeScoped != 1 {
		t.Errorf("got %d node-scoped definitions, want 1", nodeScoped)
	}
}

// TestListDueSchedulesReturnsOnlyEnabledPastDue covers the scheduler's poll:
// disabled schedules and future ones stay out, and the result is soonest
// first.
func TestListDueSchedulesReturnsOnlyEnabledPastDue(t *testing.T) {
	db, _ := newTargetsDB(t)
	ctx := context.Background()

	def := mustCreateDefinition(t, ctx, db, "edge-tcp", "")
	now := time.Now().UTC()
	past := now.Add(-2 * time.Minute)
	older := now.Add(-5 * time.Minute)
	future := now.Add(time.Hour)

	mkSchedule := func(enabled bool, next *time.Time) store.Schedule {
		t.Helper()
		s, err := db.CreateSchedule(ctx, store.ScheduleInput{
			DefinitionID: def.ID,
			Kind:         "interval",
			IntervalNs:   int64(time.Minute),
			Enabled:      enabled,
			NextFireAt:   next,
		})
		if err != nil {
			t.Fatalf("CreateSchedule: %v", err)
		}
		return s
	}

	dueSoonest := mkSchedule(true, &older)
	dueLater := mkSchedule(true, &past)
	mkSchedule(false, &past)  // disabled
	mkSchedule(true, &future) // not yet due
	mkSchedule(true, nil)     // retired from the due index

	due, err := db.ListDueSchedules(ctx, now, 10)
	if err != nil {
		t.Fatalf("ListDueSchedules: %v", err)
	}
	if len(due) != 2 {
		t.Fatalf("ListDueSchedules: got %d, want 2", len(due))
	}
	if due[0].ID != dueSoonest.ID || due[1].ID != dueLater.ID {
		t.Errorf("ListDueSchedules order = [%s %s], want [%s %s] (soonest next_fire_at first)",
			due[0].ID, due[1].ID, dueSoonest.ID, dueLater.ID)
	}

	// MarkScheduleFired with a nil next retires the schedule from the poll.
	if err := db.MarkScheduleFired(ctx, dueSoonest.ID, now, nil, ""); err != nil {
		t.Fatalf("MarkScheduleFired: %v", err)
	}
	after, err := db.GetSchedule(ctx, dueSoonest.ID)
	if err != nil {
		t.Fatalf("GetSchedule: %v", err)
	}
	if after.LastFiredAt == nil {
		t.Error("MarkScheduleFired left LastFiredAt nil")
	}
	if after.NextFireAt != nil {
		t.Errorf("MarkScheduleFired(nil next) left NextFireAt = %v, want nil", after.NextFireAt)
	}
	due, err = db.ListDueSchedules(ctx, now, 10)
	if err != nil {
		t.Fatalf("ListDueSchedules after the fire: %v", err)
	}
	if len(due) != 1 || due[0].ID != dueLater.ID {
		t.Errorf("ListDueSchedules after the fire returned %d rows, want just %s", len(due), dueLater.ID)
	}

	if err := db.MarkScheduleFired(ctx, "0f1d1a2f-6f8e-4a3a-9a0e-7f3f9d0f1c22", now, nil, ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("MarkScheduleFired on an unknown id: err = %v, want ErrNotFound", err)
	}
}

// TestScheduleLastErrorRoundTrips is migration 00008's boundary test: the two columns exist, they
// default to the healthy pair.
func TestScheduleLastErrorRoundTrips(t *testing.T) {
	db, _ := newTargetsDB(t)
	ctx := context.Background()

	def := mustCreateDefinition(t, ctx, db, "edge-tcp", "")
	now := time.Now().UTC().Truncate(time.Microsecond) // pg stores microseconds
	next := now.Add(time.Minute)

	created, err := db.CreateSchedule(ctx, store.ScheduleInput{
		DefinitionID: def.ID, Kind: "interval", IntervalNs: int64(time.Minute),
		Enabled: true, NextFireAt: &next,
	})
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	// The column DEFAULT is the healthy pair, so every row that predates the
	// migration reads as "nothing wrong" rather than as an unknown.
	if created.LastError != "" || created.LastErrorAt != nil {
		t.Fatalf("a new schedule = %q/%v, want the empty pair", created.LastError, created.LastErrorAt)
	}

	const boom = "get destination target 0f1d1a2f-6f8e-4a3a-9a0e-7f3f9d0f1c22: store: not found"
	if err := db.MarkScheduleFired(ctx, created.ID, now, &next, boom); err != nil {
		t.Fatalf("MarkScheduleFired(failure): %v", err)
	}
	failed, err := db.GetSchedule(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetSchedule: %v", err)
	}
	if failed.LastError != boom {
		t.Errorf("LastError = %q, want %q", failed.LastError, boom)
	}
	if failed.LastErrorAt == nil || !failed.LastErrorAt.UTC().Equal(now) {
		t.Errorf("LastErrorAt = %v, want the fire's own stamp %v", failed.LastErrorAt, now)
	}
	// The failure did NOT cost the row its cadence: a broken schedule still
	// advances, which is exactly why the error column had to exist.
	if failed.LastFiredAt == nil || failed.NextFireAt == nil {
		t.Errorf("a failed fire left LastFiredAt=%v NextFireAt=%v, want both stamped",
			failed.LastFiredAt, failed.NextFireAt)
	}

	// An EDIT is not a fire: the failure survives a full-replace update, so an
	// operator cannot turn a broken row green by pressing Save.
	if _, err := db.UpdateSchedule(ctx, created.ID, store.ScheduleInput{
		DefinitionID: def.ID, Kind: "interval", IntervalNs: int64(2 * time.Minute),
		Enabled: true, NextFireAt: &next,
	}); err != nil {
		t.Fatalf("UpdateSchedule: %v", err)
	}
	edited, err := db.GetSchedule(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetSchedule after update: %v", err)
	}
	if edited.LastError != boom {
		t.Errorf("LastError after an edit = %q, want it untouched", edited.LastError)
	}

	// The next fire that goes through clears BOTH halves.
	later := now.Add(time.Minute)
	if err := db.MarkScheduleFired(ctx, created.ID, later, &next, ""); err != nil {
		t.Fatalf("MarkScheduleFired(success): %v", err)
	}
	healthy, err := db.GetSchedule(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetSchedule after recovery: %v", err)
	}
	if healthy.LastError != "" || healthy.LastErrorAt != nil {
		t.Errorf("after a good fire = %q/%v, want the empty pair", healthy.LastError, healthy.LastErrorAt)
	}
}

// A pathological error must not become a pathological row: the store caps what
// one fire may write, on a rune boundary, and marks the cut.
func TestScheduleLastErrorIsTruncated(t *testing.T) {
	db, _ := newTargetsDB(t)
	ctx := context.Background()

	def := mustCreateDefinition(t, ctx, db, "edge-tcp", "")
	now := time.Now().UTC()
	created, err := db.CreateSchedule(ctx, store.ScheduleInput{
		DefinitionID: def.ID, Kind: "interval", IntervalNs: int64(time.Minute), Enabled: true, NextFireAt: &now,
	})
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	// Multi-byte on purpose: a byte-sliced UTF-8 message would put a
	// replacement character on the operator's screen.
	huge := strings.Repeat("плохо ", 400)
	if err := db.MarkScheduleFired(ctx, created.ID, now, &now, huge); err != nil {
		t.Fatalf("MarkScheduleFired: %v", err)
	}
	got, err := db.GetSchedule(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetSchedule: %v", err)
	}
	if len(got.LastError) > 500 {
		t.Errorf("LastError is %d bytes, want at most 500", len(got.LastError))
	}
	if !strings.HasSuffix(got.LastError, "…") {
		t.Errorf("LastError = %q, want the cut marked", got.LastError)
	}
	if !utf8.ValidString(got.LastError) {
		t.Errorf("LastError is not valid UTF-8: %q", got.LastError)
	}
}

func TestTargetAddressIsValidatedByKindAgainstPostgres(t *testing.T) {
	db, _ := newTargetsDB(t)
	ctx := context.Background()

	rejected := []struct{ name, kind, address string }{
		{"whitespace-only host", "host", "   "},
		{"garbage host", "host", "sdfsdfsdf !!"},
		{"a URL filed as a host", "host", "https://example.test"},
		{"whitespace-only url", "url", "  "},
		{"garbage url", "url", "sdfsdfsdf !!"},
		{"a bare hostname filed as a url", "url", "example.test"},
	}
	for _, tc := range rejected {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			_, err := db.CreateTarget(ctx, store.TargetInput{Name: "t-" + strings.ReplaceAll(tc.kind, " ", ""), Kind: tc.kind, Address: tc.address})
			if err == nil {
				t.Fatalf("CreateTarget(kind=%s, address=%q) = nil, want an error", tc.kind, tc.address)
			}
		})
	}

	// …and the accepted one is stored TRIMMED, so what the agent dials is what
	// was validated.
	got, err := db.CreateTarget(ctx, store.TargetInput{Name: "edge-gw", Kind: "host", Address: "  10.0.0.1:8443  "})
	if err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	if got.Address != "10.0.0.1:8443" {
		t.Errorf("stored address = %q, want it trimmed", got.Address)
	}
	updated, err := db.UpdateTarget(ctx, got.ID, store.TargetInput{Name: "edge-gw", Kind: "url", Address: "  https://example.test/health  "})
	if err != nil {
		t.Fatalf("UpdateTarget: %v", err)
	}
	if updated.Address != "https://example.test/health" {
		t.Errorf("updated address = %q, want it trimmed", updated.Address)
	}
}

// dueIdxSeedRows is how many enabled, already-due schedules the index test seeds; it has to be
// large enough that an index range scan is genuinely the cheaper plan.
const dueIdxSeedRows = 50000

// listDueSchedulesSQL returns the exact SQL text sqlc generated for ListDueSchedules; this is the
// whole point of the test: EXPLAINing a hand-copied duplicate of the query proves nothing.
func listDueSchedulesSQL(t *testing.T) string {
	t.Helper()
	const (
		file  = "gen/targets.sql.go"
		ident = "listDueSchedules"
	)
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if name.Name != ident || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("%s.%s is not a string literal", file, ident)
				}
				sql, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s.%s: %v", file, ident, err)
				}
				return sql
			}
		}
	}
	t.Fatalf("no const %q in %s -- did sqlc's naming change?", ident, file)
	return ""
}

// idxScans reads pg_stat_user_indexes.idx_scan for one index.
func idxScans(t *testing.T, ctx context.Context, conn *pgxpool.Conn, index string) int64 {
	t.Helper()
	if _, err := conn.Exec(ctx, "SELECT pg_stat_clear_snapshot()"); err != nil {
		t.Fatalf("pg_stat_clear_snapshot: %v", err)
	}
	var n int64
	err := conn.QueryRow(ctx,
		`SELECT coalesce(idx_scan, 0) FROM pg_stat_user_indexes WHERE indexrelname = $1`,
		index).Scan(&n)
	if err != nil {
		t.Fatalf("read idx_scan for %s: %v", index, err)
	}
	return n
}

// TestListDueSchedulesUsesPartialIndex asserts the REAL shipped due poll; nothing about the query
// text is assumed.
func TestListDueSchedulesUsesPartialIndex(t *testing.T) {
	db, dsn := newTargetsDB(t)
	ctx := context.Background()

	def := mustCreateDefinition(t, ctx, db, "edge-tcp", "")

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	// One pinned connection for every statement below: stats reads and the
	// snapshot clearing that precedes them have to happen on the same backend.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	// Seeded in a single statement rather than through CreateSchedule: 50k round trips would dominate
	// the suite's runtime.
	if _, err := conn.Exec(ctx, `
INSERT INTO check_schedules (id, definition_id, kind, interval_ns, enabled, next_fire_at)
SELECT gen_random_uuid(), $1::uuid, 'interval', 60000000000, true,
       now() - make_interval(secs => $2::int - g)
FROM generate_series(1, $2::int) AS g`, def.ID, dueIdxSeedRows); err != nil {
		t.Fatalf("seed %d schedules: %v", dueIdxSeedRows, err)
	}
	if _, err := conn.Exec(ctx, "ANALYZE check_schedules"); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}

	// --- half one: the shipped call moves the index's scan counter ---------

	before := idxScans(t, ctx, conn, "check_schedules_due_idx")

	due, err := db.ListDueSchedules(ctx, time.Now().UTC(), 100)
	if err != nil {
		t.Fatalf("ListDueSchedules: %v", err)
	}
	if len(due) != 100 {
		t.Fatalf("ListDueSchedules returned %d rows, want 100", len(due))
	}
	for i := 1; i < len(due); i++ {
		if due[i-1].NextFireAt.After(*due[i].NextFireAt) {
			t.Fatalf("ListDueSchedules row %d fires before row %d: %v > %v",
				i, i-1, due[i-1].NextFireAt, due[i].NextFireAt)
		}
	}

	// The counter does not move the instant the query returns; left alone they would surface only when
	// the idle-stats timeout fires ten seconds later.
	deadline := time.Now().Add(30 * time.Second)
	var after int64
	for {
		after = idxScans(t, ctx, conn, "check_schedules_due_idx")
		if after > before {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("check_schedules_due_idx idx_scan stayed at %d after ListDueSchedules "+
				"(was %d before): the shipped query is not being answered by the index", after, before)
		}
		time.Sleep(300 * time.Millisecond)
		if _, err := db.GetTarget(ctx, "0f1d1a2f-6f8e-4a3a-9a0e-7f3f9d0f1c22"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("stats-flush nudge: GetTarget err = %v, want ErrNotFound", err)
		}
	}

	// --- half two: the plan names the index and carries no sort ------------

	sql := listDueSchedulesSQL(t)
	if !strings.Contains(sql, "check_schedules") {
		t.Fatalf("extracted SQL does not look like the due poll:\n%s", sql)
	}

	rows, err := conn.Query(ctx, "EXPLAIN\n"+sql, time.Now().UTC(), int32(100))
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

	if !strings.Contains(plan.String(), "check_schedules_due_idx") {
		t.Errorf("the due-schedule query does not use check_schedules_due_idx; plan was:\n%s", plan.String())
	}
	if strings.Contains(plan.String(), "Sort") {
		t.Errorf("the due-schedule query sorts instead of reading ORDER BY next_fire_at out of "+
			"check_schedules_due_idx; plan was:\n%s", plan.String())
	}
}

// TestTargetInvalidInputNeverReachesTheDatabase asserts validation runs
// before the INSERT/UPDATE: a rejected input leaves no row and produces no
// constraint violation from the driver.
func TestTargetInvalidInputNeverReachesTheDatabase(t *testing.T) {
	db, _ := newTargetsDB(t)
	ctx := context.Background()

	bad := store.TargetInput{Name: "edge gw", Kind: "host", Address: "10.0.0.1"}
	if _, err := db.CreateTarget(ctx, bad); err == nil {
		t.Fatal("CreateTarget with a space in the name succeeded, want a validation error")
	}
	page, err := db.ListTargets(ctx, store.TargetFilter{})
	if err != nil {
		t.Fatalf("ListTargets: %v", err)
	}
	if len(page.Targets) != 0 {
		t.Errorf("a rejected create left %d rows behind", len(page.Targets))
	}
}

// TestTargetMalformedIDIsNotErrNotFound pins the house rule the auth store
// established: a non-UUID id is its own distinct error, never silently folded
// into ErrNotFound.
func TestTargetMalformedIDIsNotErrNotFound(t *testing.T) {
	db, _ := newTargetsDB(t)
	ctx := context.Background()

	_, err := db.GetTarget(ctx, "not-a-uuid")
	if err == nil {
		t.Fatal("GetTarget(non-uuid) = nil error, want one")
	}
	if errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetTarget(non-uuid) reported ErrNotFound: %v", err)
	}
}

// TestEmptyJSONShapesReachTheDatabaseAsEmptyObject is orEmptyJSON's claim checked where it actually
// matters; only nil used to be substituted, and the other two shapes failed in different ways
// against a live server.
func TestEmptyJSONShapesReachTheDatabaseAsEmptyObject(t *testing.T) {
	db, _ := newTargetsDB(t)
	ctx := context.Background()

	shapes := []struct {
		name string
		raw  json.RawMessage
	}{
		{"nil", nil},
		{"empty", json.RawMessage{}},
		{"json null", json.RawMessage(`null`)},
	}

	for i, sh := range shapes {
		t.Run(sh.name, func(t *testing.T) {
			tgt, err := db.CreateTarget(ctx, store.TargetInput{
				Name:    "gw-" + strconv.Itoa(i),
				Kind:    "host",
				Address: "10.0.0.1",
				Labels:  sh.raw,
			})
			if err != nil {
				t.Fatalf("CreateTarget with %s labels: %v", sh.name, err)
			}
			if got := string(tgt.Labels); got != `{}` {
				t.Errorf("CreateTarget with %s labels stored %s, want {}", sh.name, got)
			}

			// The UPDATE path has no column DEFAULT to fall back on, so it is
			// the one that would break first.
			updated, err := db.UpdateTarget(ctx, tgt.ID, store.TargetInput{
				Name:    "gw-" + strconv.Itoa(i),
				Kind:    "host",
				Address: "10.0.0.2",
				Labels:  sh.raw,
			})
			if err != nil {
				t.Fatalf("UpdateTarget with %s labels: %v", sh.name, err)
			}
			if got := string(updated.Labels); got != `{}` {
				t.Errorf("UpdateTarget with %s labels stored %s, want {}", sh.name, got)
			}

			def, err := db.CreateDefinition(ctx, store.DefinitionInput{
				Name:            "def-" + strconv.Itoa(i),
				SourceSelection: "all",
				DestinationKind: "node",
				CheckType:       "tcp",
				Plane:           "pod",
				Params:          sh.raw,
			})
			if err != nil {
				t.Fatalf("CreateDefinition with %s params: %v", sh.name, err)
			}
			if got := string(def.Params); got != `{}` {
				t.Errorf("CreateDefinition with %s params stored %s, want {}", sh.name, got)
			}
		})
	}

	// And the rejection half: a payload no jsonb column would accept is
	// refused by name, before any statement is sent.
	_, err := db.CreateTarget(ctx, store.TargetInput{
		Name: "gw-bad", Kind: "host", Address: "10.0.0.1",
		Labels: json.RawMessage(`{"env":`),
	})
	if err == nil {
		t.Fatal("CreateTarget with malformed labels succeeded, want a validation error")
	}
	if !strings.Contains(err.Error(), "labels") {
		t.Errorf("CreateTarget error %q does not name the labels field", err)
	}
}

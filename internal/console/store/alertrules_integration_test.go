//go:build integration

package store_test

// TestAlertRule* / TestListAlertRules* require a real PostgreSQL.
// Run: docker run --rm -d -p 5432:5432 -e POSTGRES_PASSWORD=test -e POSTGRES_DB=kconmon postgres:17-alpine
// Then: KCONMON_TEST_DATABASE_DSN='postgres://postgres:test@127.0.0.1:5432/kconmon?sslmode=disable' \
//       go test -tags=integration ./internal/console/store/... -v

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// newAlertRulesDB opens a *store.DB with migrations applied, dropping and
// re-creating the schema first -- same convention as newIncidentsDB. The dsn
// comes back alongside it because the CHECK-constraint test below has to write
// this table WITHOUT going through the store.
func newAlertRulesDB(t *testing.T) (*store.DB, string) {
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

func alertRuleInput(name string) store.AlertRuleInput {
	return store.AlertRuleInput{
		Name:         name,
		Kind:         store.AlertRuleKindPairLoss,
		Params:       json.RawMessage(`{"threshold":0.05,"window":"5m"}`),
		Severity:     store.AlertSeverityWarning,
		ForNs:        int64(5 * time.Minute),
		Labels:       json.RawMessage(`{"team":"net"}`),
		Annotations:  json.RawMessage(`{"summary":"pair loss above threshold"}`),
		Enabled:      true,
		RenderedExpr: `kconmon_ng_pair_loss_ratio > 0.05`,
	}
}

// TestAlertRuleLifecycle is the whole M7 Task 1 flow through the store:
// create -> get -> list -> the two narrow updates -> delete, with the delete
// asserted through an independent read.
func TestAlertRuleLifecycle(t *testing.T) {
	db, _ := newAlertRulesDB(t)
	ctx := context.Background()

	created, err := db.CreateAlertRule(ctx, alertRuleInput("PairLossHigh"))
	if err != nil {
		t.Fatalf("CreateAlertRule: %v", err)
	}
	if created.ID == "" {
		t.Fatal("CreateAlertRule: ID is empty, want a minted UUID")
	}
	if created.Name != "PairLossHigh" || created.Kind != store.AlertRuleKindPairLoss {
		t.Errorf("CreateAlertRule: got name=%q kind=%q, want the input's back", created.Name, created.Kind)
	}
	if created.Severity != store.AlertSeverityWarning {
		t.Errorf("CreateAlertRule: Severity = %q, want warning", created.Severity)
	}
	if created.ForNs != int64(5*time.Minute) {
		t.Errorf("CreateAlertRule: ForNs = %d, want %d -- the store must not convert durations",
			created.ForNs, int64(5*time.Minute))
	}
	if !created.Enabled {
		t.Error("CreateAlertRule: Enabled = false, want the input's true")
	}
	if created.RenderedExpr != `kconmon_ng_pair_loss_ratio > 0.05` {
		t.Errorf("CreateAlertRule: RenderedExpr = %q, want the input's back", created.RenderedExpr)
	}
	// A freshly typed rule has never been applied: that is the column DEFAULT,
	// not something the caller passed.
	if created.SyncStatus != store.AlertSyncStatusUnsynced {
		t.Errorf("CreateAlertRule: SyncStatus = %q, want unsynced", created.SyncStatus)
	}
	if created.SyncMessage != "" {
		t.Errorf("CreateAlertRule: SyncMessage = %q, want empty", created.SyncMessage)
	}
	if created.LastSyncedAt != nil {
		t.Errorf("CreateAlertRule: LastSyncedAt = %v, want nil -- nothing has been applied yet", created.LastSyncedAt)
	}
	assertJSONEqual(t, "Params", created.Params, `{"threshold":0.05,"window":"5m"}`)
	assertJSONEqual(t, "Labels", created.Labels, `{"team":"net"}`)
	assertJSONEqual(t, "Annotations", created.Annotations, `{"summary":"pair loss above threshold"}`)

	got, err := db.GetAlertRule(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetAlertRule: %v", err)
	}
	if got.ID != created.ID || got.Name != created.Name {
		t.Errorf("GetAlertRule: got %+v, want the created row", got)
	}

	rules, err := db.ListAlertRules(ctx, false)
	if err != nil {
		t.Fatalf("ListAlertRules: %v", err)
	}
	if len(rules) != 1 || rules[0].ID != created.ID {
		t.Errorf("ListAlertRules: got %d rules, want just the created one", len(rules))
	}

	// The builder update.
	edited := alertRuleInput("PairLossHigh")
	edited.Kind = store.AlertRuleKindRaw
	edited.Params = json.RawMessage(`{"expr":"kconmon_ng_up == 0"}`)
	edited.Severity = store.AlertSeverityCritical
	edited.ForNs = int64(90 * time.Second)
	edited.Enabled = false
	edited.RenderedExpr = `kconmon_ng_up == 0`
	updated, err := db.UpdateAlertRule(ctx, created.ID, edited)
	if err != nil {
		t.Fatalf("UpdateAlertRule: %v", err)
	}
	if updated.Kind != store.AlertRuleKindRaw || updated.Severity != store.AlertSeverityCritical {
		t.Errorf("UpdateAlertRule: got kind=%q severity=%q, want raw/critical", updated.Kind, updated.Severity)
	}
	if updated.ForNs != int64(90*time.Second) || updated.Enabled {
		t.Errorf("UpdateAlertRule: got ForNs=%d Enabled=%v, want 90s/false", updated.ForNs, updated.Enabled)
	}
	if updated.CreatedAt != created.CreatedAt {
		t.Errorf("UpdateAlertRule moved CreatedAt from %v to %v", created.CreatedAt, updated.CreatedAt)
	}

	// The sync update.
	syncedAt := time.Now().UTC().Truncate(time.Microsecond)
	synced, err := db.UpdateAlertRuleSyncStatus(ctx, created.ID, store.AlertSyncStatusSynced, "applied", &syncedAt)
	if err != nil {
		t.Fatalf("UpdateAlertRuleSyncStatus: %v", err)
	}
	if synced.SyncStatus != store.AlertSyncStatusSynced || synced.SyncMessage != "applied" {
		t.Errorf("UpdateAlertRuleSyncStatus: got status=%q message=%q, want synced/applied",
			synced.SyncStatus, synced.SyncMessage)
	}
	if synced.LastSyncedAt == nil || !synced.LastSyncedAt.Equal(syncedAt) {
		t.Errorf("UpdateAlertRuleSyncStatus: LastSyncedAt = %v, want %v", synced.LastSyncedAt, syncedAt)
	}

	if err := db.DeleteAlertRule(ctx, created.ID); err != nil {
		t.Fatalf("DeleteAlertRule: %v", err)
	}
	if _, err := db.GetAlertRule(ctx, created.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetAlertRule after delete: err = %v, want ErrNotFound", err)
	}
	if err := db.DeleteAlertRule(ctx, created.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second DeleteAlertRule: err = %v, want ErrNotFound", err)
	}
}

// TestAlertRuleNameUniquenessIsCaseInsensitive is what the lower(name) UNIQUE
// INDEX buys over the plain column constraint targets uses: two rules named
// `PairLoss` and `pairloss` would render into two alerts an operator reads as
// one. Go cannot enforce this -- it cannot see the other rows -- so the index
// is the only place it can live, and the conflict must surface as
// ErrAlreadyExists rather than a raw driver error.
func TestAlertRuleNameUniquenessIsCaseInsensitive(t *testing.T) {
	db, _ := newAlertRulesDB(t)
	ctx := context.Background()

	first, err := db.CreateAlertRule(ctx, alertRuleInput("PairLossHigh"))
	if err != nil {
		t.Fatalf("CreateAlertRule: %v", err)
	}

	for _, name := range []string{"PairLossHigh", "pairlosshigh", "PAIRLOSSHIGH", "pAiRlOsShIgH"} {
		t.Run(name, func(t *testing.T) {
			_, dupErr := db.CreateAlertRule(ctx, alertRuleInput(name))
			if !errors.Is(dupErr, store.ErrAlreadyExists) {
				t.Errorf("CreateAlertRule(%q) err = %v, want ErrAlreadyExists", name, dupErr)
			}
		})
	}

	// The same rule on the UPDATE path: renaming rule B onto A's name in a
	// different case must conflict too.
	second, err := db.CreateAlertRule(ctx, alertRuleInput("ZoneLatencyHigh"))
	if err != nil {
		t.Fatalf("CreateAlertRule(second): %v", err)
	}
	rename := alertRuleInput("PAIRLOSSHIGH")
	_, renameErr := db.UpdateAlertRule(ctx, second.ID, rename)
	if !errors.Is(renameErr, store.ErrAlreadyExists) {
		t.Errorf("UpdateAlertRule(rename onto a case variant) err = %v, want ErrAlreadyExists", renameErr)
	}

	// Renaming a rule onto its OWN name in a different case is not a conflict:
	// the index sees one row, the row it is already on.
	if _, recaseErr := db.UpdateAlertRule(ctx, first.ID, alertRuleInput("pairlosshigh")); recaseErr != nil {
		t.Errorf("UpdateAlertRule(re-casing its own name): %v, want nil", recaseErr)
	}

	rules, err := db.ListAlertRules(ctx, false)
	if err != nil {
		t.Fatalf("ListAlertRules: %v", err)
	}
	if len(rules) != 2 {
		t.Errorf("the rejected creates left %d rows behind, want 2", len(rules))
	}
}

// TestAlertRuleCheckConstraintsRejectTheClosedSets is the DB half of the three
// closed vocabularies and the for_ns >= 0 rule. Validate catches every one of
// them first for anything going through the store, so the CHECKs are exercised
// here with raw SQL -- they are the backstop for anything that ever writes
// this table without going through this package, and a backstop nobody ever
// fires is a backstop nobody knows is missing.
func TestAlertRuleCheckConstraintsRejectTheClosedSets(t *testing.T) {
	db, dsn := newAlertRulesDB(t)
	ctx := context.Background()

	// The store rejects each of them before the round trip.
	for _, tc := range []struct {
		name   string
		mutate func(*store.AlertRuleInput)
	}{
		{"kind", func(in *store.AlertRuleInput) { in.Kind = "pair-latency" }},
		{"severity", func(in *store.AlertRuleInput) { in.Severity = "page" }},
		{"for_ns", func(in *store.AlertRuleInput) { in.ForNs = -1 }},
	} {
		in := alertRuleInput("Rejected")
		tc.mutate(&in)
		if _, err := db.CreateAlertRule(ctx, in); err == nil {
			t.Fatalf("CreateAlertRule(bad %s) succeeded, want a validation error", tc.name)
		}
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"unknown kind", `INSERT INTO alert_rules (name, kind, severity)
			VALUES ('RawKindCheck', 'pair-latency', 'warning')`},
		{"unknown severity", `INSERT INTO alert_rules (name, kind, severity)
			VALUES ('RawSeverityCheck', 'pair-loss', 'page')`},
		{"negative for_ns", `INSERT INTO alert_rules (name, kind, severity, for_ns)
			VALUES ('RawForCheck', 'pair-loss', 'warning', -1)`},
		{"unknown sync status", `INSERT INTO alert_rules (name, kind, severity, sync_status)
			VALUES ('RawSyncCheck', 'pair-loss', 'warning', 'syncing')`},
		{"empty name", `INSERT INTO alert_rules (name, kind, severity)
			VALUES ('', 'pair-loss', 'warning')`},
		{"name over 63 bytes", `INSERT INTO alert_rules (name, kind, severity)
			VALUES (repeat('n', 64), 'pair-loss', 'warning')`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, rawErr := pool.Exec(ctx, tc.sql); rawErr == nil {
				t.Fatalf("the raw INSERT succeeded; the %s CHECK is not enforcing its closed set", tc.name)
			}
		})
	}

	rules, err := db.ListAlertRules(ctx, false)
	if err != nil {
		t.Fatalf("ListAlertRules: %v", err)
	}
	if len(rules) != 0 {
		t.Errorf("rejected inserts left %d rows behind", len(rules))
	}
}

// TestAlertRuleUpdatesAreTwoDisjointHalves is the whole point of the narrow
// split (M7 Decision 5): the builder update must reset the rule to unsynced --
// a changed rule is by definition not the rule that was applied -- and the
// sync update must never touch a single builder field. Asserted in both
// directions in one test, because either half alone would pass with a single
// full-replace UPDATE.
func TestAlertRuleUpdatesAreTwoDisjointHalves(t *testing.T) {
	db, _ := newAlertRulesDB(t)
	ctx := context.Background()

	created, err := db.CreateAlertRule(ctx, alertRuleInput("PairLossHigh"))
	if err != nil {
		t.Fatalf("CreateAlertRule: %v", err)
	}

	syncedAt := time.Now().UTC().Truncate(time.Microsecond)
	synced, err := db.UpdateAlertRuleSyncStatus(ctx, created.ID,
		store.AlertSyncStatusSynced, "applied to kconmon-ng-console", &syncedAt)
	if err != nil {
		t.Fatalf("UpdateAlertRuleSyncStatus: %v", err)
	}

	// Direction 1: the sync update touched NOTHING the operator typed.
	if synced.Name != created.Name || synced.Kind != created.Kind || synced.Severity != created.Severity ||
		synced.ForNs != created.ForNs || synced.Enabled != created.Enabled ||
		synced.RenderedExpr != created.RenderedExpr {
		t.Errorf("UpdateAlertRuleSyncStatus changed a builder field: got %+v, want %+v", synced, created)
	}
	assertJSONEqual(t, "Params", synced.Params, string(created.Params))
	assertJSONEqual(t, "Labels", synced.Labels, string(created.Labels))
	assertJSONEqual(t, "Annotations", synced.Annotations, string(created.Annotations))
	// Not even updated_at: a 60s reconcile loop bumping it would make every
	// rule look freshly edited every minute.
	if !synced.UpdatedAt.Equal(created.UpdatedAt) {
		t.Errorf("UpdateAlertRuleSyncStatus moved UpdatedAt from %v to %v", created.UpdatedAt, synced.UpdatedAt)
	}

	// Direction 2: the builder update resets the sync verdict, and keeps
	// last_synced_at -- a historical fact about the last apply, not a claim
	// about the current row.
	edited := alertRuleInput("PairLossHigh")
	edited.Severity = store.AlertSeverityCritical
	updated, err := db.UpdateAlertRule(ctx, created.ID, edited)
	if err != nil {
		t.Fatalf("UpdateAlertRule: %v", err)
	}
	if updated.SyncStatus != store.AlertSyncStatusUnsynced {
		t.Errorf("UpdateAlertRule: SyncStatus = %q, want unsynced -- an edited rule is not a synced rule",
			updated.SyncStatus)
	}
	if updated.SyncMessage != "" {
		t.Errorf("UpdateAlertRule: SyncMessage = %q, want empty -- the message explained the old status",
			updated.SyncMessage)
	}
	if updated.LastSyncedAt == nil || !updated.LastSyncedAt.Equal(syncedAt) {
		t.Errorf("UpdateAlertRule: LastSyncedAt = %v, want %v kept", updated.LastSyncedAt, syncedAt)
	}

	// And an error outcome with a nil lastSyncedAt writes SQL NULL rather than
	// year 1.
	errored, err := db.UpdateAlertRuleSyncStatus(ctx, created.ID,
		store.AlertSyncStatusError, "PrometheusRule CRD is absent", nil)
	if err != nil {
		t.Fatalf("UpdateAlertRuleSyncStatus(error): %v", err)
	}
	if errored.LastSyncedAt != nil {
		t.Errorf("UpdateAlertRuleSyncStatus(nil lastSyncedAt): LastSyncedAt = %v, want nil", errored.LastSyncedAt)
	}
	if errored.SyncStatus != store.AlertSyncStatusError || errored.SyncMessage != "PrometheusRule CRD is absent" {
		t.Errorf("UpdateAlertRuleSyncStatus(error): got status=%q message=%q",
			errored.SyncStatus, errored.SyncMessage)
	}
}

// TestAlertRuleUpdatedAtIsMonotonic pins the column's meaning: an operator
// edit moves it forward, and nothing else moves it at all.
func TestAlertRuleUpdatedAtIsMonotonic(t *testing.T) {
	db, _ := newAlertRulesDB(t)
	ctx := context.Background()

	created, err := db.CreateAlertRule(ctx, alertRuleInput("PairLossHigh"))
	if err != nil {
		t.Fatalf("CreateAlertRule: %v", err)
	}
	if created.UpdatedAt.Before(created.CreatedAt) {
		t.Errorf("CreateAlertRule: UpdatedAt %v is before CreatedAt %v", created.UpdatedAt, created.CreatedAt)
	}

	prev := created.UpdatedAt
	for i, sev := range []string{
		store.AlertSeverityInfo,
		store.AlertSeverityCritical,
		store.AlertSeverityWarning,
	} {
		// now() is the transaction's start time, so two UPDATEs inside the
		// same clock tick could otherwise land on the same instant.
		time.Sleep(2 * time.Millisecond)
		in := alertRuleInput("PairLossHigh")
		in.Severity = sev
		row, err := db.UpdateAlertRule(ctx, created.ID, in)
		if err != nil {
			t.Fatalf("UpdateAlertRule(%d): %v", i, err)
		}
		if !row.UpdatedAt.After(prev) {
			t.Errorf("UpdateAlertRule(%d): UpdatedAt = %v, want strictly after %v", i, row.UpdatedAt, prev)
		}
		if !row.CreatedAt.Equal(created.CreatedAt) {
			t.Errorf("UpdateAlertRule(%d) moved CreatedAt to %v", i, row.CreatedAt)
		}
		prev = row.UpdatedAt
	}
}

// TestListAlertRulesOrdersByLowerNameAndFiltersEnabled covers both halves of
// the one listing this table has. The order is asserted with names whose
// byte-order and case-folded order DISAGREE -- a plain ORDER BY name would put
// every uppercase name before every lowercase one, which is exactly the
// jumbled list the lower(name) index exists to prevent.
func TestListAlertRulesOrdersByLowerNameAndFiltersEnabled(t *testing.T) {
	db, _ := newAlertRulesDB(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name    string
		enabled bool
	}{
		{"zeta", true},
		{"Alpha", true},
		{"beta", false},
		{"Gamma", false},
		{"delta", true},
	} {
		in := alertRuleInput(tc.name)
		in.Enabled = tc.enabled
		if _, err := db.CreateAlertRule(ctx, in); err != nil {
			t.Fatalf("CreateAlertRule(%s): %v", tc.name, err)
		}
	}

	all, err := db.ListAlertRules(ctx, false)
	if err != nil {
		t.Fatalf("ListAlertRules(false): %v", err)
	}
	wantAll := []string{"Alpha", "beta", "delta", "Gamma", "zeta"}
	assertRuleNames(t, "ListAlertRules(false)", all, wantAll)

	enabled, err := db.ListAlertRules(ctx, true)
	if err != nil {
		t.Fatalf("ListAlertRules(true): %v", err)
	}
	assertRuleNames(t, "ListAlertRules(true)", enabled, []string{"Alpha", "delta", "zeta"})
}

// TestListAlertRulesOnAnEmptyTableIsEmptyNotNil pins the emit_empty_slices
// contract the rest of the package leans on: a caller ranges over the result
// without a nil check.
func TestListAlertRulesOnAnEmptyTableIsEmptyNotNil(t *testing.T) {
	db, _ := newAlertRulesDB(t)

	for _, enabledOnly := range []bool{false, true} {
		rules, err := db.ListAlertRules(context.Background(), enabledOnly)
		if err != nil {
			t.Fatalf("ListAlertRules(%v): %v", enabledOnly, err)
		}
		if rules == nil {
			t.Errorf("ListAlertRules(%v) = nil, want an empty slice", enabledOnly)
		}
		if len(rules) != 0 {
			t.Errorf("ListAlertRules(%v) returned %d rules on an empty table", enabledOnly, len(rules))
		}
	}
}

// TestAlertRuleUnknownIDIsNotFound is the seam's miss contract across every
// id-taking method, with a well-formed UUID that names nothing (the malformed
// case is the unit test's, since it never reaches the database).
func TestAlertRuleUnknownIDIsNotFound(t *testing.T) {
	db, _ := newAlertRulesDB(t)
	ctx := context.Background()
	missing := uuid.NewString()

	if _, err := db.GetAlertRule(ctx, missing); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetAlertRule(unknown) = %v, want ErrNotFound", err)
	}
	if _, err := db.UpdateAlertRule(ctx, missing, alertRuleInput("PairLossHigh")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UpdateAlertRule(unknown) = %v, want ErrNotFound", err)
	}
	_, err := db.UpdateAlertRuleSyncStatus(ctx, missing, store.AlertSyncStatusSynced, "", nil)
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UpdateAlertRuleSyncStatus(unknown) = %v, want ErrNotFound", err)
	}
	if err := db.DeleteAlertRule(ctx, missing); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DeleteAlertRule(unknown) = %v, want ErrNotFound", err)
	}
}

// TestAlertRuleInvalidInputNeverReachesTheDatabase is the layering claim as a
// row count: every rejected payload must leave the table exactly as it was.
func TestAlertRuleInvalidInputNeverReachesTheDatabase(t *testing.T) {
	db, _ := newAlertRulesDB(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name   string
		mutate func(*store.AlertRuleInput)
	}{
		{"empty name", func(in *store.AlertRuleInput) { in.Name = "" }},
		{"name with a space", func(in *store.AlertRuleInput) { in.Name = "pair loss" }},
		{"unknown kind", func(in *store.AlertRuleInput) { in.Kind = "pair-latency" }},
		{"unknown severity", func(in *store.AlertRuleInput) { in.Severity = "page" }},
		{"negative for", func(in *store.AlertRuleInput) { in.ForNs = -1 }},
		{"params as an array", func(in *store.AlertRuleInput) { in.Params = json.RawMessage(`[]`) }},
		{"labels as a scalar", func(in *store.AlertRuleInput) { in.Labels = json.RawMessage(`3`) }},
		{"raw without an expr", func(in *store.AlertRuleInput) {
			in.Kind = store.AlertRuleKindRaw
			in.Params = json.RawMessage(`{"window":"5m"}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := alertRuleInput("Rejected")
			tc.mutate(&in)
			if _, err := db.CreateAlertRule(ctx, in); err == nil {
				t.Fatalf("CreateAlertRule(%s) succeeded, want a validation error", tc.name)
			}
		})
	}

	rules, err := db.ListAlertRules(ctx, false)
	if err != nil {
		t.Fatalf("ListAlertRules: %v", err)
	}
	if len(rules) != 0 {
		t.Errorf("rejected inputs left %d rows behind", len(rules))
	}
}

// TestAlertRuleJSONColumnsDefaultToEmptyObjects is orEmptyJSON's claim over
// this table's three JSONB columns: a nil, an empty slice and a literal JSON
// null must all read back as {} -- the column's own DEFAULT -- so a rule
// written without params and one written with null are indistinguishable to
// every reader.
func TestAlertRuleJSONColumnsDefaultToEmptyObjects(t *testing.T) {
	db, _ := newAlertRulesDB(t)
	ctx := context.Background()

	for i, tc := range []struct {
		name string
		raw  json.RawMessage
	}{
		{"NilJSON", nil},
		{"EmptyJSON", json.RawMessage{}},
		{"NullJSON", json.RawMessage(`null`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := alertRuleInput(tc.name)
			in.Params, in.Labels, in.Annotations = tc.raw, tc.raw, tc.raw
			created, err := db.CreateAlertRule(ctx, in)
			if err != nil {
				t.Fatalf("CreateAlertRule(%d): %v", i, err)
			}
			assertJSONEqual(t, "Params", created.Params, `{}`)
			assertJSONEqual(t, "Labels", created.Labels, `{}`)
			assertJSONEqual(t, "Annotations", created.Annotations, `{}`)
		})
	}
}

// assertRuleNames compares a listing's names, in order, against want.
func assertRuleNames(t *testing.T, what string, got []store.AlertRule, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s returned %d rules, want %d: %v", what, len(got), len(want), ruleNames(got))
	}
	for i := range want {
		if got[i].Name != want[i] {
			t.Errorf("%s[%d] = %q, want %q (full order %v)", what, i, got[i].Name, want[i], ruleNames(got))
		}
	}
}

func ruleNames(rules []store.AlertRule) []string {
	names := make([]string, len(rules))
	for i := range rules {
		names[i] = rules[i].Name
	}
	return names
}

// assertJSONEqual compares two JSONB payloads by VALUE, not by bytes: jsonb is
// a parsed representation and Postgres is free to re-order keys and normalize
// whitespace on the way back out.
func assertJSONEqual(t *testing.T, field string, got json.RawMessage, want string) {
	t.Helper()
	var gotVal, wantVal any
	if err := json.Unmarshal(got, &gotVal); err != nil {
		t.Fatalf("%s: unmarshal stored value %q: %v", field, got, err)
	}
	if err := json.Unmarshal([]byte(want), &wantVal); err != nil {
		t.Fatalf("%s: unmarshal want %q: %v", field, want, err)
	}
	gotJSON, _ := json.Marshal(gotVal)
	wantJSON, _ := json.Marshal(wantVal)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("%s = %s, want %s", field, gotJSON, wantJSON)
	}
}

package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// validAlertRuleInput is the baseline every AlertRuleInput case below mutates
// one field of, so a failure names exactly the field under test -- the same
// shape validWebhookInput takes.
func validAlertRuleInput() AlertRuleInput {
	return AlertRuleInput{
		Name:        "PairLossHigh",
		Kind:        AlertRuleKindPairLoss,
		Params:      json.RawMessage(`{"threshold":0.05}`),
		Severity:    AlertSeverityWarning,
		ForNs:       int64(5 * time.Minute),
		Labels:      json.RawMessage(`{"team":"net"}`),
		Annotations: json.RawMessage(`{"summary":"loss is up"}`),
		Enabled:     true,
	}
}

func TestAlertRuleInputValidateAcceptsWellFormed(t *testing.T) {
	cases := []struct {
		name string
		in   AlertRuleInput
	}{
		{"the baseline", validAlertRuleInput()},
		{"every template kind", validAlertRuleInput()}, // widened per-kind below
		{"disabled", func() AlertRuleInput { in := validAlertRuleInput(); in.Enabled = false; return in }()},
		{"zero for", func() AlertRuleInput { in := validAlertRuleInput(); in.ForNs = 0; return in }()},
		{"nil params, labels and annotations", func() AlertRuleInput {
			in := validAlertRuleInput()
			in.Params, in.Labels, in.Annotations = nil, nil, nil
			return in
		}()},
		{"empty json objects", func() AlertRuleInput {
			in := validAlertRuleInput()
			in.Params, in.Labels, in.Annotations = json.RawMessage(`{}`), json.RawMessage(`{}`), json.RawMessage(`{}`)
			return in
		}()},
		{"literal json null folds to the column default", func() AlertRuleInput {
			in := validAlertRuleInput()
			in.Params, in.Labels, in.Annotations = json.RawMessage(`null`), json.RawMessage(`null`), json.RawMessage(`null`)
			return in
		}()},
		{"raw kind with an expr", func() AlertRuleInput {
			in := validAlertRuleInput()
			in.Kind = AlertRuleKindRaw
			in.Params = json.RawMessage(`{"expr":"up == 0"}`)
			return in
		}()},
		{"dotted and underscored name", func() AlertRuleInput {
			in := validAlertRuleInput()
			in.Name = "kconmon.pair_loss-9"
			return in
		}()},
		{"max name", func() AlertRuleInput {
			in := validAlertRuleInput()
			in.Name = strings.Repeat("n", alertRuleNameMaxLen)
			return in
		}()},
		{"max rendered expr", func() AlertRuleInput {
			in := validAlertRuleInput()
			in.RenderedExpr = strings.Repeat("x", alertRuleExprMaxLen)
			return in
		}()},
		{"info severity", func() AlertRuleInput { in := validAlertRuleInput(); in.Severity = AlertSeverityInfo; return in }()},
		{"critical severity", func() AlertRuleInput {
			in := validAlertRuleInput()
			in.Severity = AlertSeverityCritical
			return in
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			if err := in.Validate(); err != nil {
				t.Errorf("Validate(%+v) = %v, want nil", tc.in, err)
			}
		})
	}

	// Every template kind must validate with the baseline's params: params are
	// validated closed by the RENDERER (M7 Task 2), not here, so this layer
	// must not reject a kind it does not yet know how to render.
	for kind := range alertRuleKinds {
		if kind == AlertRuleKindRaw {
			continue // raw carries its own expr requirement, asserted above
		}
		t.Run("kind "+kind, func(t *testing.T) {
			in := validAlertRuleInput()
			in.Kind = kind
			if err := in.Validate(); err != nil {
				t.Errorf("Validate(kind=%s) = %v, want nil", kind, err)
			}
		})
	}
}

func TestAlertRuleInputValidateRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*AlertRuleInput)
	}{
		{"empty name", func(in *AlertRuleInput) { in.Name = "" }},
		{"name over 63 bytes", func(in *AlertRuleInput) { in.Name = strings.Repeat("n", alertRuleNameMaxLen+1) }},
		{"space in the name", func(in *AlertRuleInput) { in.Name = "pair loss" }},
		{"slash in the name", func(in *AlertRuleInput) { in.Name = "pair/loss" }},
		{"name starting with a hyphen", func(in *AlertRuleInput) { in.Name = "-pairloss" }},
		{"empty kind", func(in *AlertRuleInput) { in.Kind = "" }},
		{"unknown kind", func(in *AlertRuleInput) { in.Kind = "pair-latency" }},
		{"kind differing only in case", func(in *AlertRuleInput) { in.Kind = "Pair-Loss" }},
		{"empty severity", func(in *AlertRuleInput) { in.Severity = "" }},
		{"unknown severity", func(in *AlertRuleInput) { in.Severity = "page" }},
		{"severity differing only in case", func(in *AlertRuleInput) { in.Severity = "Warning" }},
		{"negative for", func(in *AlertRuleInput) { in.ForNs = -1 }},
		{"malformed params", func(in *AlertRuleInput) { in.Params = json.RawMessage(`{"threshold":`) }},
		{"params as an array", func(in *AlertRuleInput) { in.Params = json.RawMessage(`[1,2]`) }},
		{"params as a scalar", func(in *AlertRuleInput) { in.Params = json.RawMessage(`7`) }},
		{"params as a string", func(in *AlertRuleInput) { in.Params = json.RawMessage(`"threshold"`) }},
		{"labels as an array", func(in *AlertRuleInput) { in.Labels = json.RawMessage(`["team"]`) }},
		{"labels as a scalar", func(in *AlertRuleInput) { in.Labels = json.RawMessage(`true`) }},
		{"malformed labels", func(in *AlertRuleInput) { in.Labels = json.RawMessage(`{team}`) }},
		{"annotations as an array", func(in *AlertRuleInput) { in.Annotations = json.RawMessage(`[]`) }},
		{"annotations as a scalar", func(in *AlertRuleInput) { in.Annotations = json.RawMessage(`1.5`) }},
		{"malformed annotations", func(in *AlertRuleInput) { in.Annotations = json.RawMessage(`{"a":}`) }},
		{"raw kind with no params at all", func(in *AlertRuleInput) {
			in.Kind = AlertRuleKindRaw
			in.Params = nil
		}},
		{"raw kind with empty params", func(in *AlertRuleInput) {
			in.Kind = AlertRuleKindRaw
			in.Params = json.RawMessage(`{}`)
		}},
		{"raw kind with an empty expr", func(in *AlertRuleInput) {
			in.Kind = AlertRuleKindRaw
			in.Params = json.RawMessage(`{"expr":""}`)
		}},
		{"raw kind with a blank expr", func(in *AlertRuleInput) {
			in.Kind = AlertRuleKindRaw
			in.Params = json.RawMessage(`{"expr":"   "}`)
		}},
		{"raw kind with a non-string expr", func(in *AlertRuleInput) {
			in.Kind = AlertRuleKindRaw
			in.Params = json.RawMessage(`{"expr":42}`)
		}},
		{"raw kind with a null expr", func(in *AlertRuleInput) {
			in.Kind = AlertRuleKindRaw
			in.Params = json.RawMessage(`{"expr":null}`)
		}},
		{"raw kind with an over-long expr", func(in *AlertRuleInput) {
			in.Kind = AlertRuleKindRaw
			expr, _ := json.Marshal(strings.Repeat("u", alertRuleExprMaxLen+1))
			in.Params = json.RawMessage(`{"expr":` + string(expr) + `}`)
		}},
		{"rendered expr over 4096 bytes", func(in *AlertRuleInput) {
			in.RenderedExpr = strings.Repeat("x", alertRuleExprMaxLen+1)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validAlertRuleInput()
			tc.mutate(&in)
			if err := in.Validate(); err == nil {
				t.Errorf("Validate(%+v) = nil, want an error", in)
			}
		})
	}
}

// TestAlertRuleKindsAreTheClosedM7Set pins the VALUES, not just the count, and
// pins them against the LITERAL strings the migration's CHECK spells -- a test
// that compares a constant to itself pins nothing. A kind that drifts out of
// step with the CHECK is a row the store accepts and Postgres rejects.
func TestAlertRuleKindsAreTheClosedM7Set(t *testing.T) {
	want := map[string]bool{
		"pair-loss":            true,
		"zone-latency":         true,
		"dns-failures":         true,
		"http-ttfb":            true,
		"cert-expiry":          true,
		"agent-missing":        true,
		"external-target-down": true,
		"raw":                  true,
	}
	if len(alertRuleKinds) != len(want) {
		t.Fatalf("alertRuleKinds has %d entries, want %d: %v", len(alertRuleKinds), len(want), alertRuleKinds)
	}
	for kind := range want {
		if !alertRuleKinds[kind] {
			t.Errorf("alertRuleKinds is missing %q", kind)
		}
	}
	for _, got := range []string{
		AlertRuleKindPairLoss,
		AlertRuleKindZoneLatency,
		AlertRuleKindDNSFailures,
		AlertRuleKindHTTPTTFB,
		AlertRuleKindCertExpiry,
		AlertRuleKindAgentMissing,
		AlertRuleKindExternalTargetDown,
		AlertRuleKindRaw,
	} {
		if !want[got] {
			t.Errorf("exported constant %q is not one of the eight M7 kinds", got)
		}
	}
}

// TestAlertSeveritiesAreTheClosedSet is the same pin for severity.
func TestAlertSeveritiesAreTheClosedSet(t *testing.T) {
	want := map[string]bool{"info": true, "warning": true, "critical": true}
	if len(alertSeverities) != len(want) {
		t.Fatalf("alertSeverities has %d entries, want %d: %v", len(alertSeverities), len(want), alertSeverities)
	}
	for sev := range want {
		if !alertSeverities[sev] {
			t.Errorf("alertSeverities is missing %q", sev)
		}
	}
	for _, got := range []string{AlertSeverityInfo, AlertSeverityWarning, AlertSeverityCritical} {
		if !want[got] {
			t.Errorf("exported constant %q is not one of the three severities", got)
		}
	}
}

// TestAlertSyncStatusesAreTheClosedSet is the same pin for sync_status, whose
// four values the reconciler (M7 Decision 5) writes and the migration's CHECK
// backstops.
func TestAlertSyncStatusesAreTheClosedSet(t *testing.T) {
	want := map[string]bool{"unsynced": true, "synced": true, "drift": true, "error": true}
	if len(alertSyncStatuses) != len(want) {
		t.Fatalf("alertSyncStatuses has %d entries, want %d: %v", len(alertSyncStatuses), len(want), alertSyncStatuses)
	}
	for st := range want {
		if !alertSyncStatuses[st] {
			t.Errorf("alertSyncStatuses is missing %q", st)
		}
	}
	for _, got := range []string{
		AlertSyncStatusUnsynced,
		AlertSyncStatusSynced,
		AlertSyncStatusDrift,
		AlertSyncStatusError,
	} {
		if !want[got] {
			t.Errorf("exported constant %q is not one of the four sync statuses", got)
		}
	}
}

// TestCreateAlertRuleValidatesBeforeTouchingPgx asserts validation runs before
// the INSERT. NIL pool, so a clean return proves no round trip was attempted.
func TestCreateAlertRuleValidatesBeforeTouchingPgx(t *testing.T) {
	db := &DB{}
	if _, err := db.CreateAlertRule(context.Background(), AlertRuleInput{}); err == nil {
		t.Error("CreateAlertRule(zero input) = nil error, want a validation error")
	}
}

// TestAlertRuleMalformedIDIsNotFoundWithoutTouchingPgx mirrors the webhook
// readers' pre-check across every id-taking method. NIL pool.
func TestAlertRuleMalformedIDIsNotFoundWithoutTouchingPgx(t *testing.T) {
	db := &DB{}
	ctx := context.Background()
	now := time.Now()

	for _, id := range []string{"", "not-a-uuid", "../../etc/passwd", "1234", "%00"} {
		t.Run(id, func(t *testing.T) {
			if _, err := db.GetAlertRule(ctx, id); !errors.Is(err, ErrNotFound) {
				t.Errorf("GetAlertRule(%q) err = %v, want ErrNotFound", id, err)
			}
			if err := db.DeleteAlertRule(ctx, id); !errors.Is(err, ErrNotFound) {
				t.Errorf("DeleteAlertRule(%q) err = %v, want ErrNotFound", id, err)
			}
			if _, err := db.UpdateAlertRule(ctx, id, validAlertRuleInput()); !errors.Is(err, ErrNotFound) {
				t.Errorf("UpdateAlertRule(%q) err = %v, want ErrNotFound", id, err)
			}
			_, err := db.UpdateAlertRuleSyncStatus(ctx, id, AlertSyncStatusSynced, "", &now)
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("UpdateAlertRuleSyncStatus(%q) err = %v, want ErrNotFound", id, err)
			}
		})
	}
}

// TestUpdateAlertRuleValidatesBeforeTheIDPreCheck asserts a bad payload is
// reported as a validation error rather than as a miss: an operator who typed
// an unknown severity must be told that, not told the rule does not exist.
// NIL pool.
func TestUpdateAlertRuleValidatesBeforeTheIDPreCheck(t *testing.T) {
	db := &DB{}
	in := validAlertRuleInput()
	in.Severity = "page"

	_, err := db.UpdateAlertRule(context.Background(), "not-a-uuid", in)
	if err == nil {
		t.Fatal("UpdateAlertRule(bad severity) = nil, want a validation error")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateAlertRule reported %v, want the severity error rather than a miss", err)
	}
}

// TestUpdateAlertRuleSyncStatusValidatesBeforeTouchingPgx asserts the sync
// half's own bounds -- the closed status set and the message length -- are
// applied before the id pre-check, for the same reason. NIL pool.
func TestUpdateAlertRuleSyncStatusValidatesBeforeTouchingPgx(t *testing.T) {
	db := &DB{}
	ctx := context.Background()
	id := "3f1d1a2f-6f8e-4a3a-9a0e-7f3f9d0f1c22"

	for _, tc := range []struct {
		name    string
		status  string
		message string
	}{
		{"empty status", "", ""},
		{"unknown status", "syncing", ""},
		{"status differing only in case", "Synced", ""},
		{"over-long message", AlertSyncStatusError, strings.Repeat("m", alertRuleSyncMessageMaxLen+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.UpdateAlertRuleSyncStatus(ctx, id, tc.status, tc.message, nil)
			if err == nil {
				t.Fatalf("UpdateAlertRuleSyncStatus(%q, %d-byte message) = nil, want a validation error",
					tc.status, len(tc.message))
			}
			if errors.Is(err, ErrNotFound) {
				t.Errorf("UpdateAlertRuleSyncStatus reported %v, want a validation error rather than a miss", err)
			}
		})
	}
}

// TestValidateJSONObjectRejectsNonObjects is the rule params, labels and
// annotations all lean on, asserted directly: jsonb would happily store an
// array or a scalar, and every reader of these three columns -- the renderer,
// the builder UI -- assumes a map. The check belongs at the only layer that
// can name which field carried it.
func TestValidateJSONObjectRejectsNonObjects(t *testing.T) {
	for _, ok := range []string{"", "null", "  null  ", "{}", `{"a":1}`, "\n\t{\"a\":[1,2]}\n"} {
		if err := validateJSONObject("params", json.RawMessage(ok)); err != nil {
			t.Errorf("validateJSONObject(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"[]", "[1]", "1", `"s"`, "true", "{", `{"a":}`, "nul"} {
		if err := validateJSONObject("params", json.RawMessage(bad)); err == nil {
			t.Errorf("validateJSONObject(%q) = nil, want an error", bad)
		}
	}
}

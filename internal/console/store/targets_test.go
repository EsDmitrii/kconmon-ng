package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// validTargetInput is the baseline every TargetInput case below mutates one
// field of, so a failure names exactly the field under test.
func validTargetInput() TargetInput {
	return TargetInput{Name: "edge-gw", Kind: "host", Address: "10.0.0.1"}
}

func TestTargetInputValidateAcceptsWellFormed(t *testing.T) {
	cases := []struct {
		name string
		in   TargetInput
	}{
		{"host", TargetInput{Name: "edge-gw", Kind: "host", Address: "10.0.0.1"}},
		{"url", TargetInput{Name: "status.page", Kind: "url", Address: "https://example.test/health"}},
		{"single character name", TargetInput{Name: "a", Kind: "host", Address: "a"}},
		{"underscores and dots", TargetInput{Name: "eu_west.gw-1", Kind: "host", Address: "a"}},
		{"63 byte name", TargetInput{Name: strings.Repeat("a", nameMaxLen), Kind: "host", Address: "a"}},
		{"labels set", TargetInput{Name: "a", Kind: "host", Address: "a", Labels: json.RawMessage(`{"env":"prod"}`)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			if err := in.Validate(); err != nil {
				t.Errorf("Validate(%+v) = %v, want nil", tc.in, err)
			}
		})
	}
}

func TestTargetInputValidateRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*TargetInput)
	}{
		{"empty name", func(in *TargetInput) { in.Name = "" }},
		{"64 byte name", func(in *TargetInput) { in.Name = strings.Repeat("a", nameMaxLen+1) }},
		{"leading dash", func(in *TargetInput) { in.Name = "-gw" }},
		{"trailing dot", func(in *TargetInput) { in.Name = "gw." }},
		{"space in name", func(in *TargetInput) { in.Name = "edge gw" }},
		{"quote in name", func(in *TargetInput) { in.Name = `gw"1` }},
		{"newline in name", func(in *TargetInput) { in.Name = "gw\n1" }},
		{"non-ascii name", func(in *TargetInput) { in.Name = "шлюз" }},
		{"empty kind", func(in *TargetInput) { in.Kind = "" }},
		{"unknown kind", func(in *TargetInput) { in.Kind = "tcp" }},
		{"kind case mismatch", func(in *TargetInput) { in.Kind = "Host" }},
		{"empty address", func(in *TargetInput) { in.Address = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validTargetInput()
			tc.mutate(&in)
			if err := in.Validate(); err == nil {
				t.Errorf("Validate(%+v) = nil, want an error", in)
			}
		})
	}
}

// TestTargetNameLengthRuleMatchesMigration pins the Go-side length bound to
// the CHECK (length(name) BETWEEN 1 AND 63) migration 00004 declares: if one
// moves without the other, a name Postgres accepts starts getting rejected in
// Go (or worse, the reverse -- a raw constraint violation reaching a caller).
func TestTargetNameLengthRuleMatchesMigration(t *testing.T) {
	if nameMaxLen != 63 {
		t.Fatalf("nameMaxLen = %d, want 63 to match migration 00004's CHECK", nameMaxLen)
	}
}

func validDefinitionInput() DefinitionInput {
	return DefinitionInput{
		Name:            "edge-tcp",
		SourceSelection: "all",
		DestinationKind: "node",
		CheckType:       "tcp",
		Plane:           "pod",
	}
}

func TestDefinitionInputValidateAcceptsWellFormed(t *testing.T) {
	target := validDefinitionInput()
	target.DestinationKind = "target"
	target.DestinationTargetID = "0f1d1a2f-6f8e-4a3a-9a0e-7f3f9d0f1c22"

	adhoc := validDefinitionInput()
	adhoc.DestinationKind = "adhoc"
	adhoc.DestinationAddress = "10.0.0.9:443"

	for _, in := range []DefinitionInput{validDefinitionInput(), target, adhoc} {
		if err := in.Validate(); err != nil {
			t.Errorf("Validate(%+v) = %v, want nil", in, err)
		}
	}
}

func TestDefinitionInputValidateRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*DefinitionInput)
	}{
		{"bad name", func(in *DefinitionInput) { in.Name = "edge tcp" }},
		{"unknown source selection", func(in *DefinitionInput) { in.SourceSelection = "everywhere" }},
		{"unknown destination kind", func(in *DefinitionInput) { in.DestinationKind = "pod" }},
		{"unknown check type", func(in *DefinitionInput) { in.CheckType = "ping" }},
		{"empty plane", func(in *DefinitionInput) { in.Plane = "" }},
		{"target kind without target id", func(in *DefinitionInput) { in.DestinationKind = "target" }},
		{"node kind with target id", func(in *DefinitionInput) {
			in.DestinationTargetID = "0f1d1a2f-6f8e-4a3a-9a0e-7f3f9d0f1c22"
		}},
		{"adhoc kind without address", func(in *DefinitionInput) { in.DestinationKind = "adhoc" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validDefinitionInput()
			tc.mutate(&in)
			if err := in.Validate(); err == nil {
				t.Errorf("Validate(%+v) = nil, want an error", in)
			}
		})
	}
}

func TestScheduleInputValidate(t *testing.T) {
	const defID = "0f1d1a2f-6f8e-4a3a-9a0e-7f3f9d0f1c22"
	runAt := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name    string
		in      ScheduleInput
		wantErr bool
	}{
		{"once with run at", ScheduleInput{DefinitionID: defID, Kind: "once", RunAt: &runAt}, false},
		{"interval with positive interval", ScheduleInput{DefinitionID: defID, Kind: "interval", IntervalNs: int64(time.Minute)}, false},
		{"continuous", ScheduleInput{DefinitionID: defID, Kind: "continuous"}, false},
		{"non-uuid definition id", ScheduleInput{DefinitionID: "not-a-uuid", Kind: "continuous"}, true},
		{"empty definition id", ScheduleInput{Kind: "continuous"}, true},
		{"cron is not a kind yet", ScheduleInput{DefinitionID: defID, Kind: "cron"}, true},
		{"once without run at", ScheduleInput{DefinitionID: defID, Kind: "once"}, true},
		{"once with interval", ScheduleInput{DefinitionID: defID, Kind: "once", RunAt: &runAt, IntervalNs: 1}, true},
		{"interval without interval", ScheduleInput{DefinitionID: defID, Kind: "interval"}, true},
		{"interval with negative interval", ScheduleInput{DefinitionID: defID, Kind: "interval", IntervalNs: -1}, true},
		{"interval with run at", ScheduleInput{DefinitionID: defID, Kind: "interval", IntervalNs: 1, RunAt: &runAt}, true},
		{"continuous with run at", ScheduleInput{DefinitionID: defID, Kind: "continuous", RunAt: &runAt}, true},
		// continuous has no cadence, so an interval on it is as wrong as an
		// interval on "once" -- rejected symmetrically rather than silently
		// stored and never read.
		{"continuous with interval", ScheduleInput{DefinitionID: defID, Kind: "continuous", IntervalNs: int64(time.Minute)}, true},
		{"continuous with negative interval", ScheduleInput{DefinitionID: defID, Kind: "continuous", IntervalNs: -1}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			err := in.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("Validate(%+v) = nil, want an error", tc.in)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate(%+v) = %v, want nil", tc.in, err)
			}
		})
	}
}

// TestOrEmptyJSONSubstitutesObject pins the three empty shapes -> {}
// substitution. Each one fails differently without it: a nil binds as SQL
// NULL, which the NOT NULL labels/params columns reject; a len-0 slice
// reaches the driver as a zero-length jsonb literal and comes back as a raw
// "invalid input syntax for type json"; and a literal null is accepted by
// jsonb but stores a JSON null, a second spelling of "no labels" that every
// reader would then have to handle.
func TestOrEmptyJSONSubstitutesObject(t *testing.T) {
	empty := []struct {
		name string
		in   json.RawMessage
	}{
		{"nil", nil},
		{"empty non-nil slice", json.RawMessage{}},
		{"empty literal", json.RawMessage(``)},
		{"json null", json.RawMessage(`null`)},
		{"json null with whitespace", json.RawMessage(" null\n")},
	}
	for _, tc := range empty {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(orEmptyJSON(tc.in)); got != `{}` {
				t.Errorf("orEmptyJSON(%q) = %q, want {}", tc.in, got)
			}
		})
	}

	kept := []json.RawMessage{
		json.RawMessage(`{"env":"prod"}`),
		json.RawMessage(`{}`),
		json.RawMessage(`{"nested":{"a":null}}`), // only a TOP-LEVEL null is folded
		json.RawMessage(`[1,2]`),
	}
	for _, raw := range kept {
		if got := string(orEmptyJSON(raw)); got != string(raw) {
			t.Errorf("orEmptyJSON(%q) = %q, want it unchanged", raw, got)
		}
	}
}

// TestValidateAcceptsEveryEmptyJSONShape pins the other half of the contract:
// the three shapes orEmptyJSON folds must all get PAST Validate, or the fold
// never runs.
func TestValidateAcceptsEveryEmptyJSONShape(t *testing.T) {
	shapes := map[string]json.RawMessage{
		"nil":                 nil,
		"empty non-nil slice": {},
		"json null":           json.RawMessage(`null`),
	}
	for name, raw := range shapes {
		t.Run(name, func(t *testing.T) {
			tgt := validTargetInput()
			tgt.Labels = raw
			if err := tgt.Validate(); err != nil {
				t.Errorf("TargetInput.Validate() with labels %q = %v, want nil", raw, err)
			}
			def := validDefinitionInput()
			def.Params = raw
			if err := def.Validate(); err != nil {
				t.Errorf("DefinitionInput.Validate() with params %q = %v, want nil", raw, err)
			}
		})
	}
}

// TestValidateRejectsMalformedJSON asserts a payload Postgres would refuse is
// named by its field here instead of surfacing as the driver's opaque
// "invalid input syntax for type json".
func TestValidateRejectsMalformedJSON(t *testing.T) {
	bad := map[string]json.RawMessage{
		"unterminated object": json.RawMessage(`{"env":`),
		"bare word":           json.RawMessage(`prod`),
		"trailing comma":      json.RawMessage(`{"a":1,}`),
		"whitespace only":     json.RawMessage("   "),
	}
	for name, raw := range bad {
		t.Run(name, func(t *testing.T) {
			tgt := validTargetInput()
			tgt.Labels = raw
			err := tgt.Validate()
			if err == nil {
				t.Fatalf("TargetInput.Validate() with labels %q = nil, want an error", raw)
			}
			if !strings.Contains(err.Error(), "labels") {
				t.Errorf("TargetInput.Validate() error %q does not name the labels field", err)
			}

			def := validDefinitionInput()
			def.Params = raw
			err = def.Validate()
			if err == nil {
				t.Fatalf("DefinitionInput.Validate() with params %q = nil, want an error", raw)
			}
			if !strings.Contains(err.Error(), "params") {
				t.Errorf("DefinitionInput.Validate() error %q does not name the params field", err)
			}
		})
	}
}

// TestOptionalUUIDRoundTrip pins the ""<->SQL NULL mapping both the
// destination_target_id column and the ListDefinitions/ListSchedules filters
// depend on.
func TestOptionalUUIDRoundTrip(t *testing.T) {
	null, err := optionalUUID("")
	if err != nil {
		t.Fatalf(`optionalUUID(""): %v`, err)
	}
	if null.Valid {
		t.Error(`optionalUUID("") is Valid, want SQL NULL`)
	}
	if got := optionalUUIDString(null); got != "" {
		t.Errorf("optionalUUIDString(NULL) = %q, want empty", got)
	}

	const id = "0f1d1a2f-6f8e-4a3a-9a0e-7f3f9d0f1c22"
	set, err := optionalUUID(id)
	if err != nil {
		t.Fatalf("optionalUUID(%q): %v", id, err)
	}
	if got := optionalUUIDString(set); got != id {
		t.Errorf("optionalUUIDString round trip = %q, want %q", got, id)
	}

	if _, err := optionalUUID("not-a-uuid"); err == nil {
		t.Error(`optionalUUID("not-a-uuid") = nil error, want one`)
	}
}

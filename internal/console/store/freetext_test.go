package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The Console sends incident notes and annotation text from multi-line textareas, so line breaks
// and tabs are ordinary input there; NUL and the other control characters stay refused.
func TestFreeTextFieldsAcceptLineBreaksAndTabs(t *testing.T) {
	const multiline = "line one\nline two\r\n\tindented"

	in := validIncidentInput()
	in.Notes = multiline
	if err := in.Validate(); err != nil {
		t.Errorf("incident notes with line breaks: %v", err)
	}

	a := validAnnotationInput()
	a.Text = multiline
	if err := a.Validate(); err != nil {
		t.Errorf("annotation text with line breaks: %v", err)
	}
}

func TestFreeTextFieldsStillRefuseOtherControlCharacters(t *testing.T) {
	for _, bad := range []string{"a\x00b", "a\x1bb", "a\x7fb"} {
		in := validIncidentInput()
		in.Notes = bad
		if err := in.Validate(); err == nil {
			t.Errorf("incident notes %q accepted, want a control-character error", bad)
		}

		a := validAnnotationInput()
		a.Text = bad
		if err := a.Validate(); err == nil {
			t.Errorf("annotation text %q accepted, want a control-character error", bad)
		}
	}

	// Single-line fields keep the strict rule.
	in := validIncidentInput()
	in.Title = "two\nlines"
	if err := in.Validate(); err == nil {
		t.Error("incident title with a newline accepted, want the strict rule")
	}
	a := validAnnotationInput()
	a.Scope = "node-a\n"
	if err := a.Validate(); err == nil {
		t.Error("annotation scope with a newline accepted, want the strict rule")
	}
}

// PATCH notes must apply the same rule as create and fail as a validation error before the
// database, which cannot store a NUL and would otherwise surface as 502.
func TestUpdateIncidentNotesRefusesNULBeforeTheDatabase(t *testing.T) {
	db := &DB{}
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("UpdateIncidentNotes reached the database with a NUL in the notes: %v", r)
			}
		}()
		_, err = db.UpdateIncidentNotes(context.Background(), "3f1d1a2f-6f8e-4a3a-9a0e-7f3f9d0f1c22", "bad\x00byte")
	}()
	if err == nil || !strings.HasPrefix(err.Error(), "store: incident: ") {
		t.Errorf("UpdateIncidentNotes(NUL) = %v, want a \"store: incident: \" validation error", err)
	}
}

func TestValidatePinnedRefusesAnEscapedNUL(t *testing.T) {
	err := ValidatePinned(json.RawMessage(`[{"kind":"event","id":"1","note":"a\u0000b"}]`))
	if err == nil || !strings.HasPrefix(err.Error(), "store: incident: ") {
		t.Errorf("ValidatePinned(\\u0000 in a note) = %v, want a \"store: incident: \" validation error", err)
	}
}

// The maintenance reason is typed into a textarea like incident notes: line breaks and tabs pass,
// NUL and the other control characters do not, and scope stays single-line.
func TestMaintenanceReasonFollowsTheFreeTextRule(t *testing.T) {
	in := validMaintenanceInput()
	in.Reason = "line one\nline two\r\n\ttab"
	if err := in.Validate(); err != nil {
		t.Errorf("maintenance reason with line breaks: %v", err)
	}
	for _, bad := range []string{"a\x00b", "a\x1bb"} {
		in = validMaintenanceInput()
		in.Reason = bad
		if err := in.Validate(); err == nil {
			t.Errorf("maintenance reason %q accepted, want a control-character error", bad)
		}
	}
	in = validMaintenanceInput()
	in.Scope = "node-a\n"
	if err := in.Validate(); err == nil {
		t.Error("maintenance scope with a newline accepted, want the strict rule")
	}
}

// PostgreSQL's jsonb input refuses an unpaired UTF-16 surrogate escape (22P02) and text that is not
// UTF-8, though json.Valid and json.Unmarshal accept both. Every JSONB validator must refuse them as
// a validation error, or the client's input comes back as 502 "<subsystem> unavailable".
func TestJSONBValidatorsRefuseWhatJSONBCannotStore(t *testing.T) {
	bad := map[string]string{
		"lone high surrogate":           `"\ud800"`,
		"lone low surrogate":            `"\uDC00"`,
		"high surrogate then a letter":  `"\ud83dx"`,
		"high surrogate at string end":  `"a\uD83D"`,
		"two high surrogates":           `"\ud83d\ud83d"`,
		"reversed pair":                 `"\ude00\ud83d"`,
		"surrogate in a key":            `{"\ud800":1}`,
		"escaped NUL":                   `"a\u0000b"`,
		"invalid UTF-8 byte":            "\"a\xffb\"",
		"surrogate after an escaped \\": `"\\\ud800"`,
	}
	// escaped builds the \u escapes from bs: a tool that decodes escapes on write would otherwise
	// turn these cases into copies of "literal UTF-8".
	const bs = `\`
	escaped := func(units ...string) string { return `"` + bs + "u" + strings.Join(units, bs+"u") + `"` }
	good := map[string]string{
		"surrogate pair":                    escaped("d83d", "de00"),
		"upper-case surrogate pair":         escaped("D83D", "DE00"),
		"escaped backslash then u0000 text": `"\\u0000"`,
		"escaped backslash then ud800 text": `"\\ud800"`,
		"plain BMP escape":                  escaped("00e9", "4e2d"),
		"literal UTF-8":                     `"é中😀"`,
	}
	wrap := func(v string) (pinned, object string) {
		return `[{"kind":"event","id":"1","note":` + v + `}]`, `{"k":` + v + `}`
	}
	for name, v := range bad {
		if name == "surrogate in a key" {
			if err := validateJSON("params", json.RawMessage(v)); err == nil {
				t.Errorf("validateJSON(%s) = nil, want an error", name)
			}
			if err := validateJSONObject("annotations", json.RawMessage(v)); err == nil {
				t.Errorf("validateJSONObject(%s) = nil, want an error", name)
			}
			continue
		}
		pinned, object := wrap(v)
		if err := ValidatePinned(json.RawMessage(pinned)); err == nil || !strings.HasPrefix(err.Error(), "store: incident: ") {
			t.Errorf("ValidatePinned(%s) = %v, want a \"store: incident: \" validation error", name, err)
		}
		if err := validateJSON("params", json.RawMessage(object)); err == nil {
			t.Errorf("validateJSON(%s) = nil, want an error", name)
		}
		if err := validateJSONObject("annotations", json.RawMessage(object)); err == nil {
			t.Errorf("validateJSONObject(%s) = nil, want an error", name)
		}
	}
	for name, v := range good {
		pinned, object := wrap(v)
		if err := ValidatePinned(json.RawMessage(pinned)); err != nil {
			t.Errorf("ValidatePinned(%s) = %v, want nil", name, err)
		}
		if err := validateJSON("params", json.RawMessage(object)); err != nil {
			t.Errorf("validateJSON(%s) = %v, want nil", name, err)
		}
		if err := validateJSONObject("annotations", json.RawMessage(object)); err != nil {
			t.Errorf("validateJSONObject(%s) = %v, want nil", name, err)
		}
	}
}

/*
jsonb keeps a number as numeric, which holds at most 131072 digits before the decimal point and
16383 after it, the written scale included; PostgreSQL refuses anything beyond with 22003. Go
decodes such a literal without complaint (1e-20000 as 0), so every JSONB validator has to look at
the number tokens themselves. The bounds were measured on PostgreSQL 18; the integration suites run
the same literals through the database.
*/
func TestJSONBValidatorsRefuseNumbersNumericCannotHold(t *testing.T) {
	bad := []string{
		"1e131072", "1e200000", "-1e131072", "100e131070", "0.001e131075", "1E+131072",
		"1e-16384", "1e-20000", "0e-16384", "1.0e-16383", "0.0e-16383",
		"0e1073741824", "1e99999999999999999999", "1e-99999999999999999999",
	}
	good := []string{
		"0", "-0.0", "1.5", "12345678901234567890", "1e131071", "-1e131071", "100e131069",
		"0.001e131074", "1e-16383", "0e200000", "1E+5",
	}
	wrap := func(n string) (pinned, object string) {
		return `[{"kind":"event","id":"1","x":` + n + `}]`, `{"k":[true,{"n":` + n + `}],"s":"1e200000"}`
	}
	for _, n := range bad {
		pinned, object := wrap(n)
		if err := ValidatePinned(json.RawMessage(pinned)); err == nil || !strings.HasPrefix(err.Error(), "store: incident: ") {
			t.Errorf("ValidatePinned(%s) = %v, want a \"store: incident: \" validation error", n, err)
		}
		if err := validateJSON("params", json.RawMessage(object)); err == nil {
			t.Errorf("validateJSON(%s) = nil, want an error", n)
		}
		if err := validateJSONObject("annotations", json.RawMessage(object)); err == nil {
			t.Errorf("validateJSONObject(%s) = nil, want an error", n)
		}
	}
	for _, n := range good {
		pinned, object := wrap(n)
		if err := ValidatePinned(json.RawMessage(pinned)); err != nil {
			t.Errorf("ValidatePinned(%s) = %v, want nil", n, err)
		}
		if err := validateJSON("params", json.RawMessage(object)); err != nil {
			t.Errorf("validateJSON(%s) = %v, want nil", n, err)
		}
		if err := validateJSONObject("annotations", json.RawMessage(object)); err != nil {
			t.Errorf("validateJSONObject(%s) = %v, want nil", n, err)
		}
	}
}

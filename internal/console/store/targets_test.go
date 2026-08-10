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
		{"whitespace-only address", func(in *TargetInput) { in.Address = "   " }},
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

// TestTargetNameLengthRuleMatchesMigration pins the Go-side length bound to the CHECK (length(name)
// BETWEEN 1 AND 63) migration 00004 declares.
func TestTargetNameLengthRuleMatchesMigration(t *testing.T) {
	if nameMaxLen != 63 {
		t.Fatalf("nameMaxLen = %d, want 63 to match migration 00004's CHECK", nameMaxLen)
	}
}

// The rule is ValidateAdhocAddress's, SPLIT BY KIND rather than widened.
func TestTargetAddressIsValidatedByKind(t *testing.T) {
	cases := []struct {
		name    string
		kind    string
		address string
		wantOK  bool
	}{
		// host: everything ValidateAdhocAddress takes MINUS the URL form.
		{"host bare name", "host", "example.test", true},
		{"host fully-qualified", "host", "example.test.", true},
		{"host single label", "host", "gateway", true},
		{"host IPv4", "host", "10.0.0.1", true},
		{"host bracketed IPv6", "host", "[::1]", true},
		{"host with a port", "host", "example.test:8443", true},
		{"host surrounded by whitespace", "host", "  10.0.0.1  ", true},
		{"host that is whitespace only", "host", "   ", false},
		{"host that is the finding's garbage", "host", "sdfsdfsdf !!", false},
		{"host with a non-numeric port", "host", "example.test:http", false},
		{"host with a doubled dot", "host", "example..test", false},
		// A URL in the host slot is the wrong SHAPE for the kind, not a
		// generous synonym: ResolveAllowed would send the whole string to DNS.
		{"host carrying a URL", "host", "https://example.test", false},

		// url: checker.validateExternalHTTP's rule, verbatim. It reads
		// u.Port() and uses u.Path, so a port and a path are both legal --
		// derived from that function, not assumed.
		{"url plain", "url", "https://example.test", true},
		{"url with a path", "url", "https://example.test/health", true},
		{"url with a port and a path", "url", "https://example.test:8443/health", true},
		{"url with a query", "url", "https://example.test/health?deep=1", true},
		{"url plain http", "url", "http://example.test", true},
		{"url with an IP host", "url", "http://10.0.0.1:8080/", true},
		{"url surrounded by whitespace", "url", "  https://example.test  ", true},
		{"url that is whitespace only", "url", "   ", false},
		{"url that is the finding's garbage", "url", "sdfsdfsdf !!", false},
		{"url with no scheme", "url", "example.test", false},
		{"url with the wrong scheme", "url", "ftp://example.test", false},
		{"url with no host", "url", "http://", false},
		{"url that is a bare path", "url", "/health", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := TargetInput{Name: "edge-gw", Kind: tc.kind, Address: tc.address}
			err := in.Validate()
			switch {
			case tc.wantOK && err != nil:
				t.Fatalf("Validate(kind=%s, address=%q) = %v, want nil", tc.kind, tc.address, err)
			case !tc.wantOK && err == nil:
				t.Fatalf("Validate(kind=%s, address=%q) = nil, want an error", tc.kind, tc.address)
			}
			if err == nil {
				return
			}
			// The 422 detail is routed to a form field by phrase
			// (web/src/pages/targets.tsx's TARGET_FIELD_PHRASES), so every
			// message here has to keep naming the address field.
			if !strings.Contains(strings.ToLower(err.Error()), "address") {
				t.Errorf("error %q does not name the address field", err)
			}
		})
	}
}

// Validate NORMALISES the address it accepts, so what is stored is the string that was checked;
// without this, " 10.0.0.1 " passed validation (every sender trims) and was then written verbatim.
func TestTargetInputValidateTrimsTheAddressItAccepts(t *testing.T) {
	in := TargetInput{Name: "edge-gw", Kind: "host", Address: "  10.0.0.1  "}
	if err := in.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
	if in.Address != "10.0.0.1" {
		t.Errorf("Address = %q after Validate, want the trimmed %q", in.Address, "10.0.0.1")
	}
}

// The two address failures are different operator mistakes with different
// fixes, and the console routes both to the same field -- so they must not
// collapse into one message.
func TestTargetInputValidateAddressMessages(t *testing.T) {
	blank := TargetInput{Name: "edge-gw", Kind: "host", Address: "   "}
	if err := blank.Validate(); err == nil || !strings.Contains(err.Error(), "address must not be empty") {
		t.Errorf("Validate() with a blank address = %v, want the required-field message", err)
	}

	badHost := TargetInput{Name: "edge-gw", Kind: "host", Address: "sdfsdfsdf !!"}
	if err := badHost.Validate(); err == nil || !strings.Contains(err.Error(), "must be a host, an IP, or host:port") {
		t.Errorf("Validate() with a malformed host address = %v, want the host shape message", err)
	}

	badURL := TargetInput{Name: "edge-gw", Kind: "url", Address: "example.test"}
	if err := badURL.Validate(); err == nil || !strings.Contains(err.Error(), "must be an http(s) URL with a host") {
		t.Errorf("Validate() with a malformed url address = %v, want the url shape message", err)
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

// The accepted set is DERIVED FROM THE AGENT.
func TestValidateAdhocAddress(t *testing.T) {
	accepted := []struct{ name, address string }{
		// allowlist.ResolveAllowed sends a non-literal host to LookupNetIP.
		{"bare host", "example.test"},
		{"fully-qualified host", "example.test."},
		{"host with an underscore, which net.isDomainName permits", "my_svc.example.test"},
		{"single-label host, which is legal inside a cluster", "gateway"},
		// allowlist.parseLiteral takes these verbatim and never asks DNS.
		{"IPv4 literal", "10.0.0.1"},
		{"bare IPv6 literal", "2001:db8::1"},
		{"bracketed IPv6 literal", "[::1]"},
		// checks.externalTarget / agent.approveExternalTarget split the port.
		{"host:port", "example.test:8443"},
		{"ip:port", "10.0.0.1:8080"},
		{"bracketed IPv6 with a port", "[::1]:443"},
		{"lowest port", "10.0.0.1:1"},
		{"highest port", "10.0.0.1:65535"},
		// checker.validateExternalHTTP parses exactly these two schemes.
		{"http URL", "http://example.test"},
		{"https URL with a port and a path", "https://example.test:8443/health"},
		// Every sender trims before resolving.
		{"surrounding whitespace", "  10.0.0.1  "},
		// A refusal by the allowlist is a POLICY answer belonging to the
		// agent: this rule must not become a second, quietly diverging
		// arbiter of what an operator may point a check at.
		{"loopback, which the allowlist will very likely deny at probe time", "127.0.0.1"},
		{"zone-scoped literal, which the allowlist ALWAYS denies at probe time", "fe80::1%eth0"},
	}
	for _, tc := range accepted {
		t.Run("accepts "+tc.name, func(t *testing.T) {
			if err := ValidateAdhocAddress(tc.address); err != nil {
				t.Errorf("ValidateAdhocAddress(%q) = %v, want nil", tc.address, err)
			}
		})
	}

	rejected := []struct{ name, address string }{
		{"the finding's own garbage", "sdfsdfsdf !!"},
		{"blank", "   "},
		{"an inner space", "example test"},
		{"a doubled dot", "example..test"},
		{"a leading-hyphen label", "-example.test"},
		{"a trailing-hyphen label", "example-.test"},
		// SplitHostPort accepts these, both senders' own port parse does not,
		// so the whole string travels to DNS as a name and cannot resolve.
		{"a non-numeric port", "example.test:http"},
		{"port zero", "example.test:0"},
		{"a port out of range", "example.test:99999"},
		{"a trailing colon", "example.test:"},
		// Not one of the two schemes validateExternalHTTP parses, and not a
		// resolvable name either.
		{"an ftp URL", "ftp://example.test"},
		{"a scheme with no host", "http://"},
		{"a bare path", "/health"},
		{"a quote, which would have to survive a metric label", `exa"mple.test`},
	}
	for _, tc := range rejected {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			err := ValidateAdhocAddress(tc.address)
			if err == nil {
				t.Fatalf("ValidateAdhocAddress(%q) = nil, want an error", tc.address)
			}
			// The console's 422 detail is routed to a form field by phrase
			// (web/src/pages/targets.tsx's DEFINITION_FIELD_PHRASES), so the
			// message has to keep naming the field.
			if !strings.Contains(strings.ToLower(err.Error()), "destination address") {
				t.Errorf("ValidateAdhocAddress(%q) error %q does not name the destination address field",
					tc.address, err)
			}
		})
	}
}

// A blank address reaching Validate must still get the REQUIRED-field message,
// not the shape one: they are two different operator mistakes and the console
// routes them to the same field with different fixes.
func TestDefinitionInputValidateAdhocAddressMessages(t *testing.T) {
	missing := validDefinitionInput()
	missing.DestinationKind = "adhoc"
	err := missing.Validate()
	if err == nil || !strings.Contains(err.Error(), "requires a destination address") {
		t.Errorf("Validate() with an empty adhoc address = %v, want the required-field message", err)
	}

	malformed := validDefinitionInput()
	malformed.DestinationKind = "adhoc"
	malformed.DestinationAddress = "sdfsdfsdf !!"
	err = malformed.Validate()
	if err == nil || !strings.Contains(err.Error(), "must be a host, an IP, host:port, or an http(s) URL") {
		t.Errorf("Validate() with a malformed adhoc address = %v, want the shape message", err)
	}
}

// The rule applies ONLY to the kind that carries a literal address. A node or
// target definition is untouched by it -- destination_address is not its
// field, and a stray value there is the pre-existing (unrelated) case.
func TestValidateAdhocAddressAppliesOnlyToAdhoc(t *testing.T) {
	in := validDefinitionInput()
	in.DestinationAddress = "sdfsdfsdf !!"
	if err := in.Validate(); err != nil {
		t.Errorf("Validate() on a NODE definition carrying a stray address = %v, want nil", err)
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

// TestOrEmptyJSONSubstitutesObject pins the three empty shapes -> {} substitution; each one fails
// differently without it: a nil binds as SQL NULL.
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

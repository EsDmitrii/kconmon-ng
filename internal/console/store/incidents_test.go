package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// validIncidentInput is the baseline every IncidentInput case below mutates
// one field of, so a failure names exactly the field under test.
func validIncidentInput() IncidentInput {
	return IncidentInput{
		Title:     "loss spike between node-a and node-b",
		Scope:     "node-a->node-b",
		FromAt:    time.Now().UTC().Add(-time.Hour),
		Notes:     "started right after the CNI rollout",
		CreatedBy: "user:admin",
	}
}

func TestIncidentInputValidateAcceptsWellFormed(t *testing.T) {
	to := time.Now().UTC()
	resolved := time.Now().UTC()

	cases := []struct {
		name string
		in   IncidentInput
	}{
		{"open-ended range", validIncidentInput()},
		{"closed range", func() IncidentInput { in := validIncidentInput(); in.ToAt = &to; return in }()},
		{"zero-length range", func() IncidentInput {
			in := validIncidentInput()
			in.ToAt = &in.FromAt
			return in
		}()},
		{"global scope", func() IncidentInput { in := validIncidentInput(); in.Scope = ""; return in }()},
		{"explicit open status", func() IncidentInput {
			in := validIncidentInput()
			in.Status = IncidentStatusOpen
			return in
		}()},
		{"resolved with a resolved at", func() IncidentInput {
			in := validIncidentInput()
			in.Status = IncidentStatusResolved
			in.ResolvedAt = &resolved
			return in
		}()},
		{"no notes", func() IncidentInput { in := validIncidentInput(); in.Notes = ""; return in }()},
		{"max title", func() IncidentInput {
			in := validIncidentInput()
			in.Title = strings.Repeat("t", incidentTitleMaxLen)
			return in
		}()},
		{"max notes", func() IncidentInput {
			in := validIncidentInput()
			in.Notes = strings.Repeat("n", incidentNotesMaxLen)
			return in
		}()},
		{"nil pinned", validIncidentInput()},
		{"empty pinned array", func() IncidentInput {
			in := validIncidentInput()
			in.Pinned = json.RawMessage(`[]`)
			return in
		}()},
		{"json null pinned", func() IncidentInput {
			in := validIncidentInput()
			in.Pinned = json.RawMessage(`null`)
			return in
		}()},
		{"pinned with every kind", func() IncidentInput {
			in := validIncidentInput()
			in.Pinned = json.RawMessage(`[
				{"kind":"event","id":"12"},
				{"kind":"audit","id":"3f1d1a2f-6f8e-4a3a-9a0e-7f3f9d0f1c22"},
				{"kind":"annotation","id":"3f1d1a2f-6f8e-4a3a-9a0e-7f3f9d0f1c23","note":"the mark"},
				{"kind":"snapshot","id":"3f1d1a2f-6f8e-4a3a-9a0e-7f3f9d0f1c24"},
				{"kind":"run","id":"3f1d1a2f-6f8e-4a3a-9a0e-7f3f9d0f1c25"},
				{"kind":"k8s","id":"901"}
			]`)
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
}

func TestIncidentInputValidateRejects(t *testing.T) {
	before := time.Now().UTC().Add(-2 * time.Hour)
	resolved := time.Now().UTC()
	var zeroTime time.Time

	cases := []struct {
		name   string
		mutate func(*IncidentInput)
	}{
		{"empty title", func(in *IncidentInput) { in.Title = "" }},
		{"title over 255 bytes", func(in *IncidentInput) {
			in.Title = strings.Repeat("t", incidentTitleMaxLen+1)
		}},
		{"scope over 255 bytes", func(in *IncidentInput) {
			in.Scope = strings.Repeat("s", incidentScopeMaxLen+1)
		}},
		{"created by over 255 bytes", func(in *IncidentInput) {
			in.CreatedBy = strings.Repeat("u", incidentScopeMaxLen+1)
		}},
		{"zero from at", func(in *IncidentInput) { in.FromAt = time.Time{} }},
		{"to at before from at", func(in *IncidentInput) { in.ToAt = &before }},
		{"notes over 16384 bytes", func(in *IncidentInput) {
			in.Notes = strings.Repeat("n", incidentNotesMaxLen+1)
		}},
		{"unknown status", func(in *IncidentInput) { in.Status = "acknowledged" }},
		{"resolved without a resolved at", func(in *IncidentInput) { in.Status = IncidentStatusResolved }},
		{"open with a resolved at", func(in *IncidentInput) {
			in.Status = IncidentStatusOpen
			in.ResolvedAt = &resolved
		}},
		{"resolved with a zero resolved at", func(in *IncidentInput) {
			in.Status = IncidentStatusResolved
			in.ResolvedAt = &zeroTime
		}},
		{"pinned is an object", func(in *IncidentInput) { in.Pinned = json.RawMessage(`{"kind":"event","id":"1"}`) }},
		{"pinned is malformed json", func(in *IncidentInput) { in.Pinned = json.RawMessage(`[{`) }},
		{"pinned kind unknown", func(in *IncidentInput) {
			in.Pinned = json.RawMessage(`[{"kind":"metric","id":"1"}]`)
		}},
		{"pinned id empty", func(in *IncidentInput) { in.Pinned = json.RawMessage(`[{"kind":"event","id":""}]`) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validIncidentInput()
			tc.mutate(&in)
			if err := in.Validate(); err == nil {
				t.Errorf("Validate(%+v) = nil, want an error", in)
			}
		})
	}
}

// TestIncidentEmptyStatusMeansOpen pins the column DEFAULT's Go twin: a caller
// that does not name a status creates an OPEN incident, not one with an empty
// status string that no filter would ever match.
func TestIncidentEmptyStatusMeansOpen(t *testing.T) {
	in := validIncidentInput()
	if in.Status != "" {
		t.Fatalf("the baseline already names a status (%q); the test would prove nothing", in.Status)
	}
	if err := in.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := in.effectiveStatus(); got != IncidentStatusOpen {
		t.Errorf("effectiveStatus() = %q, want %q", got, IncidentStatusOpen)
	}
}

// TestIncidentNotesLengthIsBytesNotRunes pins which unit the 16384 bound is
// in, for annotationTextMaxLen's reason: the column stores bytes.
func TestIncidentNotesLengthIsBytesNotRunes(t *testing.T) {
	in := validIncidentInput()
	in.Notes = strings.Repeat("ы", incidentNotesMaxLen) // 2 bytes per rune
	if err := in.Validate(); err == nil {
		t.Error("Validate accepted 16384 two-byte runes of notes, want the byte bound to reject it")
	}
}

// TestValidatePinnedBoundsTheArray covers the three numeric bounds Decision 7
// names: at most 64 entries, ids at most 128 bytes, notes at most 512.
func TestValidatePinnedBoundsTheArray(t *testing.T) {
	build := func(n int) json.RawMessage {
		refs := make([]PinnedRef, n)
		for i := range refs {
			refs[i] = PinnedRef{Kind: "event", ID: "1"}
		}
		raw, err := json.Marshal(refs)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return raw
	}

	if err := ValidatePinned(build(pinnedMaxEntries)); err != nil {
		t.Errorf("ValidatePinned(%d entries) = %v, want nil", pinnedMaxEntries, err)
	}
	if err := ValidatePinned(build(pinnedMaxEntries + 1)); err == nil {
		t.Errorf("ValidatePinned(%d entries) = nil, want an error", pinnedMaxEntries+1)
	}

	marshalRef := func(ref PinnedRef) json.RawMessage {
		t.Helper()
		raw, err := json.Marshal([]PinnedRef{ref})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return raw
	}

	longID := marshalRef(PinnedRef{Kind: "event", ID: strings.Repeat("i", pinnedIDMaxLen+1)})
	if err := ValidatePinned(longID); err == nil {
		t.Error("ValidatePinned accepted a 129-byte id, want an error")
	}

	okID := marshalRef(PinnedRef{Kind: "event", ID: strings.Repeat("i", pinnedIDMaxLen)})
	if err := ValidatePinned(okID); err != nil {
		t.Errorf("ValidatePinned(128-byte id) = %v, want nil", err)
	}

	longNote := marshalRef(PinnedRef{Kind: "event", ID: "1", Note: strings.Repeat("n", pinnedNoteMaxLen+1)})
	if err := ValidatePinned(longNote); err == nil {
		t.Error("ValidatePinned accepted a 513-byte note, want an error")
	}
}

// TestValidatePinnedChecksTheDecodedValueNotJustTheBytes is the whole reason
// pinned is not simply run through validateJSON like labels and params are: a
// pin whose kind nothing can resolve is well-formed JSON and a dangling
// reference, and it must be rejected at write time rather than render as
// nothing forever.
func TestValidatePinnedChecksTheDecodedValueNotJustTheBytes(t *testing.T) {
	raw := json.RawMessage(`[{"kind":"prometheus-series","id":"up"}]`)
	if !json.Valid(raw) {
		t.Fatal("the fixture is not valid JSON; the test would prove nothing")
	}
	if err := ValidatePinned(raw); err == nil {
		t.Error("ValidatePinned accepted a well-formed pin with an unresolvable kind")
	}
}

// TestDecodePinnedRoundTrips is DecodePinned's contract for the consumers
// (httpapi, the UI's server-side twin) that want the typed list.
func TestDecodePinnedRoundTrips(t *testing.T) {
	want := []PinnedRef{
		{Kind: "k8s", ID: "901", Note: "NodeNotReady"},
		{Kind: "run", ID: "3f1d1a2f-6f8e-4a3a-9a0e-7f3f9d0f1c25"},
	}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	got, err := DecodePinned(raw)
	if err != nil {
		t.Fatalf("DecodePinned: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("DecodePinned returned %d refs, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("DecodePinned[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	for _, empty := range []json.RawMessage{nil, {}, json.RawMessage(`null`)} {
		refs, err := DecodePinned(empty)
		if err != nil {
			t.Errorf("DecodePinned(%q) = %v, want nil", empty, err)
		}
		if refs != nil {
			t.Errorf("DecodePinned(%q) = %+v, want nil", empty, refs)
		}
	}
}

// TestOrEmptyPinnedArrayStoresAnArrayNotAnObject is the bug this helper exists
// to prevent: orEmptyJSON substitutes {}, which is right for labels and params
// and would put an OBJECT in a column every reader unmarshals into a slice.
func TestOrEmptyPinnedArrayStoresAnArrayNotAnObject(t *testing.T) {
	for _, empty := range []json.RawMessage{nil, {}, json.RawMessage(`  `), json.RawMessage(`null`)} {
		if got := string(orEmptyPinnedArray(empty)); got != "[]" {
			t.Errorf("orEmptyPinnedArray(%q) = %q, want \"[]\"", empty, got)
		}
	}
	if got := string(orEmptyJSON(nil)); got != "{}" {
		t.Fatalf("orEmptyJSON(nil) = %q, want \"{}\": the two helpers must stay distinct", got)
	}

	passthrough := json.RawMessage(`[{"kind":"event","id":"1"}]`)
	if got := string(orEmptyPinnedArray(passthrough)); got != string(passthrough) {
		t.Errorf("orEmptyPinnedArray passed %q through as %q", passthrough, got)
	}
}

// TestIncidentStatusInvariantHoldsOnUpdateToo asserts the status/resolved_at
// rule is not create-only: UpdateIncidentStatus checks it BEFORE the UUID
// pre-check, so a reopen that forgot to clear resolved_at fails on the
// invariant rather than reaching the database. The *DB has a NIL pool, so a
// clean return is itself proof no round trip was attempted.
func TestIncidentStatusInvariantHoldsOnUpdateToo(t *testing.T) {
	db := &DB{}
	ctx := context.Background()
	at := time.Now().UTC()
	id := "3f1d1a2f-6f8e-4a3a-9a0e-7f3f9d0f1c22"

	if _, err := db.UpdateIncidentStatus(ctx, id, IncidentStatusOpen, &at); err == nil {
		t.Error("UpdateIncidentStatus(open, resolvedAt) = nil, want the reopen to have to clear resolved_at")
	}
	if _, err := db.UpdateIncidentStatus(ctx, id, IncidentStatusResolved, nil); err == nil {
		t.Error("UpdateIncidentStatus(resolved, nil) = nil, want a resolution to carry its time")
	}
	if _, err := db.UpdateIncidentStatus(ctx, id, "closed", nil); err == nil {
		t.Error("UpdateIncidentStatus(closed) = nil, want the closed status set to reject it")
	}
}

// TestIncidentMalformedIDIsNotFoundWithoutTouchingPgx mirrors the annotation
// readers' pre-check: every id-taking method on the incident seams answers
// ErrNotFound for an id that cannot name a row, so the edge answers 404 rather
// than "incident store unavailable". NIL pool.
func TestIncidentMalformedIDIsNotFoundWithoutTouchingPgx(t *testing.T) {
	db := &DB{}
	ctx := context.Background()
	at := time.Now().UTC()

	for _, id := range []string{"", "not-a-uuid", "../../etc/passwd", "1234", "%00"} {
		t.Run(id, func(t *testing.T) {
			if _, err := db.GetIncident(ctx, id); !errors.Is(err, ErrNotFound) {
				t.Errorf("GetIncident(%q) err = %v, want ErrNotFound", id, err)
			}
			if err := db.DeleteIncident(ctx, id); !errors.Is(err, ErrNotFound) {
				t.Errorf("DeleteIncident(%q) err = %v, want ErrNotFound", id, err)
			}
			if _, err := db.UpdateIncidentNotes(ctx, id, "x"); !errors.Is(err, ErrNotFound) {
				t.Errorf("UpdateIncidentNotes(%q) err = %v, want ErrNotFound", id, err)
			}
			if _, err := db.UpdateIncidentPinned(ctx, id, nil); !errors.Is(err, ErrNotFound) {
				t.Errorf("UpdateIncidentPinned(%q) err = %v, want ErrNotFound", id, err)
			}
			if _, err := db.UpdateIncidentStatus(ctx, id, IncidentStatusResolved, &at); !errors.Is(err, ErrNotFound) {
				t.Errorf("UpdateIncidentStatus(%q) err = %v, want ErrNotFound", id, err)
			}
		})
	}
}

// TestUpdateIncidentNotesBoundsBeforeTheDatabase asserts the notes bound is
// enforced on the narrow update too, not only at create. NIL pool.
func TestUpdateIncidentNotesBoundsBeforeTheDatabase(t *testing.T) {
	db := &DB{}
	_, err := db.UpdateIncidentNotes(context.Background(),
		"3f1d1a2f-6f8e-4a3a-9a0e-7f3f9d0f1c22", strings.Repeat("n", incidentNotesMaxLen+1))
	if err == nil {
		t.Error("UpdateIncidentNotes(16385 bytes) = nil, want the bound to reject it")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateIncidentNotes reported %v, want a length error rather than a miss", err)
	}
}

// TestUpdateIncidentPinnedValidatesBeforeTheDatabase is the same claim for
// pinned. NIL pool.
func TestUpdateIncidentPinnedValidatesBeforeTheDatabase(t *testing.T) {
	db := &DB{}
	_, err := db.UpdateIncidentPinned(context.Background(),
		"3f1d1a2f-6f8e-4a3a-9a0e-7f3f9d0f1c22", json.RawMessage(`[{"kind":"nope","id":"1"}]`))
	if err == nil {
		t.Error("UpdateIncidentPinned(unknown kind) = nil, want a validation error")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateIncidentPinned reported %v, want a validation error rather than a miss", err)
	}
}

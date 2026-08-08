package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// hopsAB is the baseline path every hashing case below permutes: two hops,
// distinct IPs, in a fixed order.
func hopsAB() []PathHop {
	return []PathHop{
		{Number: 1, IP: "10.0.0.1", Hostname: "gw-a", RTTNs: 1_000_000, LossRatio: 0},
		{Number: 2, IP: "10.0.0.2", Hostname: "gw-b", RTTNs: 2_000_000, LossRatio: 0.5},
	}
}

// TestHashPathIsStable is the dedupe key's first requirement (Decision 2): the
// same ordered IP list always hashes to the same value, because an unstable
// hash would make every trace look like a new path and defeat the whole table.
func TestHashPathIsStable(t *testing.T) {
	first := HashPath(hopsAB())
	second := HashPath(hopsAB())
	if first != second {
		t.Errorf("HashPath is not deterministic: %q != %q", first, second)
	}
	if len(first) != pathHashLen {
		t.Errorf("HashPath returned %d chars, want %d (hex sha256)", len(first), pathHashLen)
	}
	if strings.ToLower(first) != first {
		t.Errorf("HashPath returned %q, want lowercase hex", first)
	}
}

// TestHashPathIgnoresEverythingButTheIP is Decision 2's exclusion list, one
// field at a time. RTTs jitter on every trace and hostnames come from
// enrichment (mutable, resolved long after the trace); folding either into the
// key would either defeat dedupe outright or make a path "change" when nothing
// about the route did.
func TestHashPathIgnoresEverythingButTheIP(t *testing.T) {
	base := HashPath(hopsAB())

	cases := []struct {
		name   string
		mutate func([]PathHop)
	}{
		{"rtt", func(h []PathHop) { h[0].RTTNs = 999_999_999 }},
		{"hostname", func(h []PathHop) { h[1].Hostname = "renamed.example.test" }},
		{"loss ratio", func(h []PathHop) { h[1].LossRatio = 1 }},
		{"hop number", func(h []PathHop) { h[0].Number = 42 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hops := hopsAB()
			tc.mutate(hops)
			if got := HashPath(hops); got != base {
				t.Errorf("HashPath changed when only %s changed: %q != %q", tc.name, got, base)
			}
		})
	}
}

// TestHashPathIsOrderSensitive is the other half of the key: a route that
// visits the same routers in a different order IS a different path, and the
// hash has to say so.
func TestHashPathIsOrderSensitive(t *testing.T) {
	forward := HashPath(hopsAB())
	hops := hopsAB()
	hops[0], hops[1] = hops[1], hops[0]
	if got := HashPath(hops); got == forward {
		t.Errorf("HashPath(reversed) = HashPath(forward) = %q, want them to differ", got)
	}
}

// TestHashPathDistinguishesConcatenations pins the separator's job: without
// one, ["1.2.3", "4.5"] and ["1.2", "34.5"] would both hash the byte sequence
// "1.2.34.5" and two genuinely different routes would dedupe into one row.
func TestHashPathDistinguishesConcatenations(t *testing.T) {
	a := HashPath([]PathHop{{IP: "1.2.3"}, {IP: "4.5"}})
	b := HashPath([]PathHop{{IP: "1.2"}, {IP: "34.5"}})
	if a == b {
		t.Errorf("HashPath does not separate hops: both concatenations hashed to %q", a)
	}
}

// TestHashPathEmptyIsEmpty keeps a hopless trace out of the table by giving it
// no key at all: the projector (Task 2) reports false for a hopless result,
// and this is the store-side backstop that makes a caller that ignored that
// fail validation instead of writing a snapshot of nothing.
func TestHashPathEmptyIsEmpty(t *testing.T) {
	if got := HashPath(nil); got != "" {
		t.Errorf("HashPath(nil) = %q, want \"\"", got)
	}
	if got := HashPath([]PathHop{}); got != "" {
		t.Errorf("HashPath(empty) = %q, want \"\"", got)
	}
}

// validSnapshotInput is the baseline every PathSnapshotInput case mutates one
// field of, so a failure names exactly the field under test.
func validSnapshotInput() PathSnapshotInput {
	return PathSnapshotInput{
		SourceNode:  "node-a",
		Destination: "edge-gw",
		Hops:        hopsAB(),
		SeenAt:      time.Now().UTC(),
	}
}

func TestPathSnapshotInputValidateAcceptsWellFormed(t *testing.T) {
	cases := []struct {
		name string
		in   PathSnapshotInput
	}{
		{"hash derived from hops", validSnapshotInput()},
		{"hash supplied by the projector", func() PathSnapshotInput {
			in := validSnapshotInput()
			in.PathHash = HashPath(in.Hops)
			return in
		}()},
		{"run id set", func() PathSnapshotInput {
			in := validSnapshotInput()
			in.RunID = "0f1d1a2f-6f8e-4a3a-9a0e-7f3f9d0f1c22"
			return in
		}()},
		{"single hop", func() PathSnapshotInput {
			in := validSnapshotInput()
			in.Hops = []PathHop{{Number: 1, IP: "10.0.0.1"}}
			return in
		}()},
		{"max hops", func() PathSnapshotInput {
			in := validSnapshotInput()
			in.Hops = make([]PathHop, maxPathHops)
			for i := range in.Hops {
				in.Hops[i] = PathHop{Number: i + 1, IP: "10.0.0.1"}
			}
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

func TestPathSnapshotInputValidateRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*PathSnapshotInput)
	}{
		{"empty source node", func(in *PathSnapshotInput) { in.SourceNode = "" }},
		{"empty destination", func(in *PathSnapshotInput) { in.Destination = "" }},
		{"no hops", func(in *PathSnapshotInput) { in.Hops = nil }},
		{"hop with no ip", func(in *PathSnapshotInput) { in.Hops[1].IP = "" }},
		{"too many hops", func(in *PathSnapshotInput) {
			in.Hops = make([]PathHop, maxPathHops+1)
			for i := range in.Hops {
				in.Hops[i] = PathHop{Number: i + 1, IP: "10.0.0.1"}
			}
		}},
		{"zero seen at", func(in *PathSnapshotInput) { in.SeenAt = time.Time{} }},
		{"hash disagrees with hops", func(in *PathSnapshotInput) {
			in.PathHash = strings.Repeat("a", pathHashLen)
		}},
		{"hash is not hex", func(in *PathSnapshotInput) { in.PathHash = strings.Repeat("z", pathHashLen) }},
		{"hash is too short", func(in *PathSnapshotInput) { in.PathHash = "abc" }},
		{"run id is not a uuid", func(in *PathSnapshotInput) { in.RunID = "not-a-uuid" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validSnapshotInput()
			tc.mutate(&in)
			if err := in.Validate(); err == nil {
				t.Errorf("Validate(%+v) = nil, want an error", in)
			}
		})
	}
}

// TestPathSnapshotInputValidateFillsTheHash pins the convenience the projector
// relies on and the guard that keeps it honest: an unset PathHash is derived
// from the hops, and a set one has to be the same value.
func TestPathSnapshotInputValidateFillsTheHash(t *testing.T) {
	in := validSnapshotInput()
	if err := in.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if in.PathHash != HashPath(hopsAB()) {
		t.Errorf("Validate left PathHash = %q, want it derived from the hops", in.PathHash)
	}
}

// validEnrichment is the baseline every Enrichment case mutates one field of.
func validEnrichment() Enrichment {
	return Enrichment{
		IP:         "10.0.0.1",
		RDNS:       "gw-a.example.test",
		ASN:        64512,
		Provider:   "Example Transit",
		Geo:        json.RawMessage(`{"country":"NL"}`),
		ResolvedAt: time.Now().UTC(),
	}
}

func TestEnrichmentValidateAcceptsWellFormed(t *testing.T) {
	cases := []struct {
		name string
		in   Enrichment
	}{
		{"full", validEnrichment()},
		{"bare ip only", Enrichment{IP: "10.0.0.1", ResolvedAt: time.Now().UTC()}},
		{"ipv6", func() Enrichment { e := validEnrichment(); e.IP = "2001:db8::1"; return e }()},
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

func TestEnrichmentValidateRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Enrichment)
	}{
		{"empty ip", func(e *Enrichment) { e.IP = "" }},
		{"over-long ip", func(e *Enrichment) { e.IP = strings.Repeat("a", enrichmentIPMaxLen+1) }},
		{"zero resolved at", func(e *Enrichment) { e.ResolvedAt = time.Time{} }},
		{"malformed geo", func(e *Enrichment) { e.Geo = json.RawMessage(`{`) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validEnrichment()
			tc.mutate(&in)
			if err := in.Validate(); err == nil {
				t.Errorf("Validate(%+v) = nil, want an error", in)
			}
		})
	}
}

// TestGetPathSnapshotMalformedIDIsNotFoundWithoutTouchingPgx mirrors
// TestGetRunMalformedIDIsNotFoundWithoutTouchingPgx (checks_test.go): the *DB
// here has a NIL pool, so a test that returns cleanly is itself proof no
// round trip was attempted, and an id that cannot name a row in a UUID-keyed
// table gets the truthful "not found" rather than a database error the edge
// would report as 502.
func TestGetPathSnapshotMalformedIDIsNotFoundWithoutTouchingPgx(t *testing.T) {
	db := &DB{}
	ctx := context.Background()

	for _, id := range []string{"", "not-a-uuid", "../../etc/passwd", "1234", "%00"} {
		t.Run(id, func(t *testing.T) {
			_, err := db.GetPathSnapshot(ctx, id)
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("GetPathSnapshot(%q) err = %v, want ErrNotFound", id, err)
			}
		})
	}
}

// TestGetEnrichmentEmptyInputSkipsTheDatabase pins the short-circuit: asking
// for no IPs is a normal read-path outcome (a snapshot whose hops were all
// resolved from the in-request cache), and it must not cost a round trip. The
// nil pool proves it did not take one.
func TestGetEnrichmentEmptyInputSkipsTheDatabase(t *testing.T) {
	db := &DB{}
	got, err := db.GetEnrichment(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetEnrichment(nil) = %v, want nil error", err)
	}
	if got == nil {
		t.Error("GetEnrichment(nil) returned a nil map, want an empty non-nil one")
	}
	if len(got) != 0 {
		t.Errorf("GetEnrichment(nil) = %v, want empty", got)
	}
}

// TestPutEnrichmentEmptyInputSkipsTheDatabase is GetEnrichment's write-side
// twin, same nil-pool proof.
func TestPutEnrichmentEmptyInputSkipsTheDatabase(t *testing.T) {
	db := &DB{}
	if err := db.PutEnrichment(context.Background(), nil); err != nil {
		t.Fatalf("PutEnrichment(nil) = %v, want nil error", err)
	}
}

// TestPutEnrichmentValidatesEveryRow asserts one bad row rejects the whole
// batch before any of it reaches pgx -- a partially-written cache refresh is
// worse than a rejected one, since the caller would have no way to tell which
// half landed.
func TestPutEnrichmentValidatesEveryRow(t *testing.T) {
	db := &DB{}
	rows := []Enrichment{validEnrichment(), {IP: "", ResolvedAt: time.Now()}}
	if err := db.PutEnrichment(context.Background(), rows); err == nil {
		t.Fatal("PutEnrichment with a malformed second row = nil, want a validation error")
	}
}

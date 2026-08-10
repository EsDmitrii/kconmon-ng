package checks_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/checks"
)

// Pair.Destination is a typed checks.Destination.
func nodePair(src, dst string) checks.Pair {
	return checks.Pair{Source: src, Destination: checks.NodeDestination(dst)}
}

func equalPairs(t *testing.T, got, want []checks.Pair) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("pairs = %+v, want %+v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("pairs = %+v, want %+v", got, want)
		}
	}
}

func TestPlanFullMeshFromEmptySpec(t *testing.T) {
	pairs, err := checks.Plan(checks.Spec{Type: "tcp", Plane: "pod"}, []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	want := []checks.Pair{
		nodePair("a", "b"), nodePair("a", "c"),
		nodePair("b", "a"), nodePair("b", "c"),
		nodePair("c", "a"), nodePair("c", "b"),
	}
	equalPairs(t, pairs, want)
}

func TestPlanExplicitSourcesAndDestinations(t *testing.T) {
	pairs, err := checks.Plan(checks.Spec{
		Sources: []string{"a", "b"}, Destinations: []string{"c"}, Type: "tcp",
	}, []string{"a", "b", "c", "d"})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	want := []checks.Pair{nodePair("a", "c"), nodePair("b", "c")}
	equalPairs(t, pairs, want)
}

// One-sided lists (only Sources given) fall back to nodes for the missing
// side without needing the other side's list too.
func TestPlanOneSidedFallsBackToNodes(t *testing.T) {
	pairs, err := checks.Plan(checks.Spec{Sources: []string{"a"}, Type: "tcp"}, []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	want := []checks.Pair{nodePair("a", "b"), nodePair("a", "c")}
	equalPairs(t, pairs, want)
}

func TestPlanDropsSelfPairs(t *testing.T) {
	pairs, err := checks.Plan(checks.Spec{Sources: []string{"a"}, Destinations: []string{"a", "b"}, Type: "tcp"}, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	equalPairs(t, pairs, []checks.Pair{nodePair("a", "b")})
}

func TestPlanCollapsesDuplicates(t *testing.T) {
	pairs, err := checks.Plan(checks.Spec{
		Sources: []string{"a", "a"}, Destinations: []string{"b", "b"}, Type: "tcp",
	}, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	equalPairs(t, pairs, []checks.Pair{nodePair("a", "b")})
}

func TestPlanExactly400PairsSucceeds(t *testing.T) {
	dests := make([]string, 400)
	for i := range dests {
		dests[i] = fmt.Sprintf("dst-%d", i)
	}
	pairs, err := checks.Plan(checks.Spec{Sources: []string{"src"}, Destinations: dests, Type: "tcp"}, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(pairs) != 400 {
		t.Fatalf("len(pairs) = %d, want 400", len(pairs))
	}
}

func TestPlan401PairsIsTooMany(t *testing.T) {
	dests := make([]string, 401)
	for i := range dests {
		dests[i] = fmt.Sprintf("dst-%d", i)
	}
	_, err := checks.Plan(checks.Spec{Sources: []string{"src"}, Destinations: dests, Type: "tcp"}, nil)
	if !errors.Is(err, checks.ErrTooManyPairs) {
		t.Fatalf("err = %v, want ErrTooManyPairs", err)
	}
}

func TestPlanUnknownCheckTypeIsRejected(t *testing.T) {
	_, err := checks.Plan(checks.Spec{Sources: []string{"a"}, Destinations: []string{"b"}, Type: "bogus"}, nil)
	if !errors.Is(err, checks.ErrUnknownType) {
		t.Fatalf("err = %v, want ErrUnknownType", err)
	}
}

func TestPlanEmptyNodeListIsAClearError(t *testing.T) {
	_, err := checks.Plan(checks.Spec{Type: "tcp"}, nil)
	if !errors.Is(err, checks.ErrNoNodes) {
		t.Fatalf("err = %v, want ErrNoNodes (not a silent zero-pair run)", err)
	}
	if _, err := checks.Plan(checks.Spec{Type: "tcp"}, []string{}); !errors.Is(err, checks.ErrNoNodes) {
		t.Fatalf("err = %v, want ErrNoNodes for an explicitly empty node slice too", err)
	}
}

// TestPlanErrorsCarryExactlyOnePackagePrefix pins the RENDERED message; the package's own
// convention (memory.go, runner.go, reconciler.go) is exactly one "checks: <op>: " per
// error-producing site.
func TestPlanErrorsCarryExactlyOnePackagePrefix(t *testing.T) {
	cases := []struct {
		name     string
		spec     checks.Spec
		nodes    []string
		sentinel error
		want     string
	}{
		{
			name:     "no nodes",
			spec:     checks.Spec{Type: "tcp"},
			sentinel: checks.ErrNoNodes,
			want:     "checks: plan: no nodes available to plan against",
		},
		{
			name:     "unknown type",
			spec:     checks.Spec{Sources: []string{"a"}, Destinations: []string{"b"}, Type: "bogus"},
			sentinel: checks.ErrUnknownType,
			want:     `checks: plan: unknown check type: "bogus"`,
		},
		{
			name:     "no pairs",
			spec:     checks.Spec{Sources: []string{"a"}, Destinations: []string{"a"}, Type: "tcp"},
			sentinel: checks.ErrNoPairs,
			want:     "checks: plan: no pairs to check (every combination is a self-pair or duplicate)",
		},
		{
			name: "invalid destination",
			spec: checks.Spec{
				Sources:           []string{"a"},
				TypedDestinations: []checks.Destination{{Kind: "target"}},
				Type:              "tcp",
			},
			sentinel: checks.ErrInvalidDestination,
			want:     `checks: plan: destination 0: invalid destination: kind "target" needs an address`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := checks.Plan(tc.spec, tc.nodes)
			if !errors.Is(err, tc.sentinel) {
				t.Fatalf("err = %v, want errors.Is(%v)", err, tc.sentinel)
			}
			if got := err.Error(); got != tc.want {
				t.Errorf("err.Error() =\n  %q\nwant\n  %q", got, tc.want)
			}
		})
	}
}

// TestPlanTooManyPairsMessage is the same pin for the one error whose detail is
// built from a count, kept apart so the table above stays a plain spec->message
// mapping.
func TestPlanTooManyPairsMessage(t *testing.T) {
	dests := make([]string, 401)
	for i := range dests {
		dests[i] = fmt.Sprintf("dst-%d", i)
	}
	_, err := checks.Plan(checks.Spec{Sources: []string{"src"}, Destinations: dests, Type: "tcp"}, nil)
	if !errors.Is(err, checks.ErrTooManyPairs) {
		t.Fatalf("err = %v, want ErrTooManyPairs", err)
	}
	const want = "checks: plan: too many pairs: computed 401 pairs, limit 400"
	if got := err.Error(); got != want {
		t.Errorf("err.Error() =\n  %q\nwant\n  %q", got, want)
	}
}

// TestAssignErrorsCarryExactlyOnePackagePrefix is the assign.go half of the
// same rule: AssignAgents wraps with "checks: assign: ", so ErrNoAgents and
// ErrUnknownSelection must not repeat the package name either.
func TestAssignErrorsCarryExactlyOnePackagePrefix(t *testing.T) {
	defs := []checks.Definition{{
		ID: "d1", Name: "d1", Selection: checks.SelectAll,
		CheckType: "tcp", DestinationAddress: "example.test:443", Enabled: true,
	}}

	_, _, err := checks.AssignAgents(defs, nil)
	if !errors.Is(err, checks.ErrNoAgents) {
		t.Fatalf("err = %v, want ErrNoAgents", err)
	}
	const wantNoAgents = "checks: assign: no agents available to assign against"
	if got := err.Error(); got != wantNoAgents {
		t.Errorf("err.Error() =\n  %q\nwant\n  %q", got, wantNoAgents)
	}

	bogus := []checks.Definition{{
		ID: "d1", Name: "d1", Selection: checks.Selection("bogus"),
		CheckType: "tcp", DestinationAddress: "example.test:443", Enabled: true,
	}}
	_, _, err = checks.AssignAgents(bogus, []checks.AgentRef{{NodeName: "a"}})
	if !errors.Is(err, checks.ErrUnknownSelection) {
		t.Fatalf("err = %v, want ErrUnknownSelection", err)
	}
	const wantUnknown = `checks: assign: unknown selection: "bogus" (definition "d1")`
	if got := err.Error(); got != wantUnknown {
		t.Errorf("err.Error() =\n  %q\nwant\n  %q", got, wantUnknown)
	}
}

// When both Sources and Destinations are explicit, an empty topology node
// list must not matter -- Plan never needed it.
func TestPlanExplicitBothSidesIgnoresEmptyNodes(t *testing.T) {
	pairs, err := checks.Plan(checks.Spec{Sources: []string{"a"}, Destinations: []string{"b"}, Type: "tcp"}, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	equalPairs(t, pairs, []checks.Pair{nodePair("a", "b")})
}

// A spec whose Sources/Destinations are both explicit and non-empty, but whose only combination is
// a self-pair.
func TestPlanSelfPairOnlyIsNoPairsError(t *testing.T) {
	_, err := checks.Plan(checks.Spec{Sources: []string{"a"}, Destinations: []string{"a"}, Type: "tcp"}, nil)
	if !errors.Is(err, checks.ErrNoPairs) {
		t.Fatalf("err = %v, want ErrNoPairs", err)
	}
}

// A raw Sources x Destinations product that vastly exceeds maxPairs must be rejected before Plan
// allocates anything sized off that product.
func TestPlanHugeCartesianProductRejectedBeforeAllocation(t *testing.T) {
	sources := make([]string, 10_000)
	for i := range sources {
		sources[i] = fmt.Sprintf("s-%d", i)
	}
	destinations := make([]string, 10_000)
	for i := range destinations {
		destinations[i] = fmt.Sprintf("d-%d", i)
	}

	start := time.Now()
	_, err := checks.Plan(checks.Spec{Sources: sources, Destinations: destinations, Type: "tcp"}, nil)
	elapsed := time.Since(start)

	if !errors.Is(err, checks.ErrTooManyPairs) {
		t.Fatalf("err = %v, want ErrTooManyPairs", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("Plan took %v, want a near-instant rejection before any large allocation", elapsed)
	}
}

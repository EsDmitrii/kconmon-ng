package checks_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/checks"
)

// nodePair is a node->node pair -- M3's only shape, and still the shape every
// test in this file asserts. Pair.Destination is a typed checks.Destination
// since M4 (Decision 14); for a node destination it carries Kind "node" and
// the node name, which is exactly what checks.NodeDestination builds.
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

// When both Sources and Destinations are explicit, an empty topology node
// list must not matter -- Plan never needed it.
func TestPlanExplicitBothSidesIgnoresEmptyNodes(t *testing.T) {
	pairs, err := checks.Plan(checks.Spec{Sources: []string{"a"}, Destinations: []string{"b"}, Type: "tcp"}, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	equalPairs(t, pairs, []checks.Pair{nodePair("a", "b")})
}

// A spec whose Sources/Destinations are both explicit and non-empty, but
// whose only combination is a self-pair, must be a clear error -- not a
// silently persisted, immediately "succeeded" zero-pair run (task-22-brief.md
// I-3/minor-b).
func TestPlanSelfPairOnlyIsNoPairsError(t *testing.T) {
	_, err := checks.Plan(checks.Spec{Sources: []string{"a"}, Destinations: []string{"a"}, Type: "tcp"}, nil)
	if !errors.Is(err, checks.ErrNoPairs) {
		t.Fatalf("err = %v, want ErrNoPairs", err)
	}
}

// A raw Sources x Destinations product that vastly exceeds maxPairs must be
// rejected before Plan allocates anything sized off that product -- a naive
// implementation sizing the seen-set/pairs slice off len(sources)*len(destinations)
// would otherwise attempt a huge allocation for a spec this test intentionally
// keeps large (10k x 10k = 100M). Plan must reject it near-instantly instead
// (task-22-brief.md I-3).
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

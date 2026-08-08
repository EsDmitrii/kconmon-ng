package checks_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/EsDmitrii/kconmon-ng/internal/console/checks"
)

// Plan's node-only path is M3 verbatim: every destination it produces must be
// a Kind "node" Destination whose Node is the node name, and whose Label --
// the value that becomes a metric label, a progress frame's "destination",
// and check_results.destination_node -- is that same name.
func TestNodeDestinationLabelIsTheNodeName(t *testing.T) {
	d := checks.NodeDestination("node-a")
	if d.Kind != checks.DestKindNode {
		t.Errorf("Kind = %q, want %q", d.Kind, checks.DestKindNode)
	}
	if d.Node != "node-a" {
		t.Errorf("Node = %q, want node-a", d.Node)
	}
	if got := d.Label(); got != "node-a" {
		t.Errorf("Label() = %q, want node-a", got)
	}
}

// A target/adhoc destination's Label is its NAME, never its address: the name
// is the only external field allowed to become an identifier downstream
// (internal/controller/diagnostics.go's destName comment). An unnamed adhoc
// destination falls back to the address, since something must identify it.
func TestExternalDestinationLabelPrefersName(t *testing.T) {
	named := checks.Destination{Kind: checks.DestKindTarget, Name: "api-prod", Address: "api.example.com:443"}
	if got := named.Label(); got != "api-prod" {
		t.Errorf("Label() = %q, want api-prod", got)
	}
	unnamed := checks.Destination{Kind: checks.DestKindAdhoc, Address: "10.0.0.7"}
	if got := unnamed.Label(); got != "10.0.0.7" {
		t.Errorf("Label() = %q, want the address as the fallback label", got)
	}
}

// A typed target destination expands into one pair per source, alongside no
// node expansion at all -- the node list is never consulted when the spec's
// destinations are already fully explicit.
func TestPlanTypedTargetDestinationOnly(t *testing.T) {
	pairs, err := checks.Plan(checks.Spec{
		Sources: []string{"n1", "n2"},
		TypedDestinations: []checks.Destination{
			{Kind: checks.DestKindTarget, Name: "api-prod", Address: "api.example.com:443"},
		},
		Type: "tcp",
	}, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	want := []checks.Pair{
		{Source: "n1", Destination: checks.Destination{Kind: checks.DestKindTarget, Name: "api-prod", Address: "api.example.com:443"}},
		{Source: "n2", Destination: checks.Destination{Kind: checks.DestKindTarget, Name: "api-prod", Address: "api.example.com:443"}},
	}
	equalPairs(t, pairs, want)
}

// Node destinations and typed destinations coexist in one spec: the node half
// keeps its M3 expansion (and self-pair exclusion), the typed half is appended
// after it, in order.
func TestPlanMixesNodeAndTypedDestinations(t *testing.T) {
	pairs, err := checks.Plan(checks.Spec{
		Sources:      []string{"n1"},
		Destinations: []string{"n1", "n2"},
		TypedDestinations: []checks.Destination{
			{Kind: checks.DestKindAdhoc, Address: "10.0.0.7"},
		},
		Type: "tcp",
	}, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	want := []checks.Pair{
		nodePair("n1", "n2"), // n1->n1 dropped as a self-pair
		{Source: "n1", Destination: checks.Destination{Kind: checks.DestKindAdhoc, Address: "10.0.0.7"}},
	}
	equalPairs(t, pairs, want)
}

// The full-mesh fallback ("no destinations named -> every node") must NOT fire
// when the spec names typed destinations only: an operator who asked for one
// external target does not want a whole-cluster mesh silently added, and an
// empty node list is not an error in that case either.
func TestPlanTypedDestinationsSuppressNodeFallback(t *testing.T) {
	pairs, err := checks.Plan(checks.Spec{
		Sources:           []string{"n1"},
		TypedDestinations: []checks.Destination{{Kind: checks.DestKindTarget, Name: "t", Address: "t.example.com"}},
		Type:              "tcp",
	}, []string{"n1", "n2", "n3"})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(pairs) != 1 {
		t.Fatalf("len(pairs) = %d, want 1 (the node list must not be expanded)", len(pairs))
	}
}

// Self-pair exclusion applies to node<->node pairs ONLY: an external
// destination is never a self-pair, even when its name or address happens to
// equal the source node's name.
func TestPlanExternalDestinationIsNeverASelfPair(t *testing.T) {
	pairs, err := checks.Plan(checks.Spec{
		Sources:           []string{"n1"},
		TypedDestinations: []checks.Destination{{Kind: checks.DestKindAdhoc, Name: "n1", Address: "n1"}},
		Type:              "tcp",
	}, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(pairs) != 1 {
		t.Fatalf("len(pairs) = %d, want 1 (an external destination named like the source is still a real pair)", len(pairs))
	}
}

// maxPairs (400) bounds the WIDENED product, not just the node half.
func TestPlanTooManyPairsCountsTypedDestinations(t *testing.T) {
	typed := make([]checks.Destination, 401)
	for i := range typed {
		typed[i] = checks.Destination{Kind: checks.DestKindAdhoc, Address: fmt.Sprintf("10.0.0.%d", i)}
	}
	_, err := checks.Plan(checks.Spec{Sources: []string{"n1"}, TypedDestinations: typed, Type: "tcp"}, nil)
	if !errors.Is(err, checks.ErrTooManyPairs) {
		t.Fatalf("err = %v, want ErrTooManyPairs", err)
	}
}

// An unrecognized Kind, and a target/adhoc destination with no address, are
// rejected up front -- the same "reject the spec, never guess" posture
// ErrUnknownType takes for Spec.Type.
func TestPlanRejectsInvalidTypedDestinations(t *testing.T) {
	cases := []struct {
		name string
		dest checks.Destination
	}{
		{"unknown kind", checks.Destination{Kind: "sputnik", Address: "x"}},
		{"target without address", checks.Destination{Kind: checks.DestKindTarget, Name: "t"}},
		{"adhoc without address", checks.Destination{Kind: checks.DestKindAdhoc}},
		{"node without node name", checks.Destination{Kind: checks.DestKindNode}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := checks.Plan(checks.Spec{
				Sources: []string{"n1"}, TypedDestinations: []checks.Destination{tc.dest}, Type: "tcp",
			}, nil)
			if !errors.Is(err, checks.ErrInvalidDestination) {
				t.Fatalf("err = %v, want ErrInvalidDestination", err)
			}
		})
	}
}

// A typed destination may also carry Kind "node" -- it is the same thing
// Spec.Destinations' strings expand into, so it dedupes against them and is
// excluded as a self-pair identically.
func TestPlanTypedNodeDestinationDedupesAgainstStringDestinations(t *testing.T) {
	pairs, err := checks.Plan(checks.Spec{
		Sources:           []string{"n1"},
		Destinations:      []string{"n2"},
		TypedDestinations: []checks.Destination{checks.NodeDestination("n2"), checks.NodeDestination("n1")},
		Type:              "tcp",
	}, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	equalPairs(t, pairs, []checks.Pair{nodePair("n1", "n2")})
}

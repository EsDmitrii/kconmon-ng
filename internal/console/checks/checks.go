// Package checks is the Console's on-demand diagnostics fan-out.
package checks

import (
	"errors"
	"fmt"
	"time"
)

const (
	// maxPairs bounds one run's fan-out: a spec that would expand past this is rejected up front,
	// before any dispatch.
	maxPairs = 400

	// maxConcurrency bounds in-flight dispatches for one run (Decision 13).
	maxConcurrency = 8

	// minPerPairTimeout / maxPerPairTimeout clamp Spec.Timeout.
	minPerPairTimeout = 1 * time.Second
	maxPerPairTimeout = 120 * time.Second
)

// The interval-run bounds.
const (
	// MinRunDuration / MaxRunDuration bound Spec.Duration; the ceiling is a day: long enough for
	// "leave it running overnight and show me what happened".
	MinRunDuration = 10 * time.Second
	MaxRunDuration = 24 * time.Hour

	// MaxSamplesPerPair caps how many probes ONE pair contributes to a run; the sample interval is
	// derived from it (sampleInterval).
	MaxSamplesPerPair = 500

	// MinSampleInterval is the fastest a pair is re-probed, whatever the
	// duration. Without it a 10s run would try to sample every 20ms and
	// measure the console's own dispatch loop rather than the network.
	MinSampleInterval = 5 * time.Second
)

// SampleInterval is the cadence at which an interval run re-probes each pair.
func SampleInterval(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	iv := d / MaxSamplesPerPair
	if iv < MinSampleInterval {
		iv = MinSampleInterval
	}
	return iv
}

// None of these sentinels carries the "checks: " package prefix, and none of them ever should.
var (
	// ErrTooManyPairs is returned when a Spec expands past maxPairs (400)
	// pairs.
	ErrTooManyPairs = errors.New("too many pairs")
	// ErrUnknownType is returned when Spec.Type is not one of the
	// controller's validCheckTypes.
	ErrUnknownType = errors.New("unknown check type")
	// ErrNoNodes is returned when Plan needs the current topology's node
	// list (Spec.Sources or Spec.Destinations is empty) and that list is
	// itself empty -- a clear error rather than a silent zero-pair run.
	ErrNoNodes = errors.New("no nodes available to plan against")
	// ErrNoPairs is returned when a Spec's Sources/Destinations are both individually non-empty (so
	// ErrNoNodes does not apply) but every combination is a self-pair or a duplicate; without this,
	// Start would persist and immediately finish a run with PairTotal 0.
	ErrNoPairs = errors.New("no pairs to check (every combination is a self-pair or duplicate)")
	// ErrInvalidDestination is returned when a Spec.TypedDestinations entry names a Kind that is not
	// node|target|adhoc; rejected up front for the same reason ErrUnknownType.
	ErrInvalidDestination = errors.New("invalid destination")
	// ErrDurationOutOfRange is returned when Spec.Duration is non-zero but
	// outside [MinRunDuration, MaxRunDuration]. Refused rather than clamped --
	// see Spec.Duration's own comment for why a duration is not a timeout.
	ErrDurationOutOfRange = errors.New("run duration out of range")
)

// The destination kinds a Spec may name; only DestKindNode is ever resolved against the
// controller's agent registry.
const (
	// DestKindNode is the behaviour verbatim: a node name, resolved against the registry; the
	// zero-value Destination.Kind ("") is read.
	DestKindNode = "node"
	// DestKindTarget is a targets row resolved Console-side to an address.
	DestKindTarget = "target"
	// DestKindAdhoc is an operator-typed address that no targets row backs.
	DestKindAdhoc = "adhoc"
)

// Destination is one resolved destination for a run; address is the thing actually probed and is
// deliberately NOT an identifier.
type Destination struct {
	Kind    string `json:"kind"`
	Node    string `json:"node,omitempty"`    // Kind=node
	Name    string `json:"name,omitempty"`    // Kind=target|adhoc -- the metric-safe label value
	Address string `json:"address,omitempty"` // Kind=target|adhoc
}

// NodeDestination builds the Kind "node" Destination a plain node name expands.
func NodeDestination(node string) Destination {
	return Destination{Kind: DestKindNode, Node: node}
}

// IsNode reports whether d is resolved against the controller's agent registry.
func (d *Destination) IsNode() bool {
	return d.Kind == "" || d.Kind == DestKindNode
}

// Label is the destination's identity everywhere downstream: the progress frame's "destination"; a
// node destination labels as its node name.
func (d *Destination) Label() string {
	if d.IsNode() {
		return d.Node
	}
	if d.Name != "" {
		return d.Name
	}
	return d.Address
}

// validate rejects a destination this package cannot dispatch. Node
// destinations need a node name; external ones need an address (the name is
// optional -- Label falls back).
func (d *Destination) validate() error {
	switch {
	case d.IsNode():
		if d.Node == "" {
			return fmt.Errorf("%w: kind %q needs a node name", ErrInvalidDestination, DestKindNode)
		}
	case d.Kind == DestKindTarget, d.Kind == DestKindAdhoc:
		if d.Address == "" {
			return fmt.Errorf("%w: kind %q needs an address", ErrInvalidDestination, d.Kind)
		}
	default:
		return fmt.Errorf("%w: unknown kind %q (want %s|%s|%s)",
			ErrInvalidDestination, d.Kind, DestKindNode, DestKindTarget, DestKindAdhoc)
	}
	return nil
}

// validCheckTypes mirrors the controller's own validCheckTypes
// (internal/controller/diagnostics.go); it is a deliberate copy, not an import: this package must
// never import internal/controller.
var validCheckTypes = map[string]struct{}{
	"tcp":  {},
	"udp":  {},
	"icmp": {},
	"dns":  {},
	"http": {},
	"mtr":  {},
}

// Pair is one (source, destination) dispatch within a run.
type Pair struct {
	Source      string
	Destination Destination
}

// Spec describes one run request.
type Spec struct {
	Sources      []string // node names; empty = every node in the current topology
	Destinations []string // node names; empty (AND no TypedDestinations) = every node in the current topology
	Type         string   // tcp|udp|icmp|dns|http|mtr -- the controller's validCheckTypes
	Plane        string   // "pod" (the only plane that exists)
	// TypedDestinations are destinations that a node name cannot express: a targets row resolved to an
	// address.
	TypedDestinations []Destination `json:",omitempty"`
	Timeout           time.Duration // per pair; clamped to [1s, 120s]
	// `omitempty` is load-bearing.
	Duration time.Duration `json:",omitempty"`
}

// ValidateDuration accepts an instant run (zero) or a duration inside [MinRunDuration,
// MaxRunDuration].
func ValidateDuration(d time.Duration) error {
	if d == 0 {
		return nil
	}
	if d < MinRunDuration || d > MaxRunDuration {
		return fmt.Errorf("checks: plan: %w: %s, allowed range is %s..%s (or omit it for an instant run)",
			ErrDurationOutOfRange, d, MinRunDuration, MaxRunDuration)
	}
	return nil
}

// Plan expands spec into an ordered, de-duplicated list of pairs; pure and I/O-free -- nodes is
// supplied by the caller (the current topology's node names).
func Plan(spec Spec, nodes []string) ([]Pair, error) { //nolint:gocritic // hugeParam: Spec mirrors the store package's own value-type write-payload structs (store/checks.go)
	if _, ok := validCheckTypes[spec.Type]; !ok {
		return nil, fmt.Errorf("checks: plan: %w: %q", ErrUnknownType, spec.Type)
	}

	sources := spec.Sources
	if len(sources) == 0 {
		if len(nodes) == 0 {
			return nil, fmt.Errorf("checks: plan: %w", ErrNoNodes)
		}
		sources = nodes
	}

	destinations, err := planDestinations(spec.Destinations, spec.TypedDestinations, nodes)
	if err != nil {
		return nil, err
	}

	// Sizing the seen-set/pairs allocations off that raw product, as this package used.
	rawProduct := len(sources) * len(destinations)
	if rawProduct > maxPairs {
		return nil, fmt.Errorf("checks: plan: %w: computed %d pairs, limit %d", ErrTooManyPairs, rawProduct, maxPairs)
	}

	// Capped defensively at maxPairs+1 (the exact bound the in-loop check below rejects at) rather
	// than trusted to already be small.
	hint := rawProduct
	if hint > maxPairs+1 {
		hint = maxPairs + 1
	}
	seen := make(map[Pair]struct{}, hint)
	pairs := make([]Pair, 0, hint)
	for _, src := range sources {
		for _, dst := range destinations {
			// Self-pair exclusion is node<->node only -- see Plan's doc comment.
			if dst.IsNode() && src == dst.Node {
				continue
			}
			p := Pair{Source: src, Destination: dst}
			if _, dup := seen[p]; dup {
				continue
			}
			seen[p] = struct{}{}
			pairs = append(pairs, p)
			if len(pairs) > maxPairs {
				return nil, fmt.Errorf("checks: plan: %w: computed %d pairs, limit %d", ErrTooManyPairs, len(pairs), maxPairs)
			}
		}
	}
	if len(pairs) == 0 {
		return nil, fmt.Errorf("checks: plan: %w", ErrNoPairs)
	}
	return pairs, nil
}

// planDestinations resolves a Spec's two destination halves into the single ordered list Plan
// expands against; only when BOTH halves are empty does the current topology's node list stand.
func planDestinations(nodeNames []string, typed []Destination, nodes []string) ([]Destination, error) {
	if len(nodeNames) == 0 && len(typed) == 0 {
		if len(nodes) == 0 {
			return nil, fmt.Errorf("checks: plan: %w", ErrNoNodes)
		}
		out := make([]Destination, len(nodes))
		for i, n := range nodes {
			out[i] = NodeDestination(n)
		}
		return out, nil
	}

	out := make([]Destination, 0, len(nodeNames)+len(typed))
	for _, n := range nodeNames {
		out = append(out, NodeDestination(n))
	}
	for i := range typed {
		if err := typed[i].validate(); err != nil {
			return nil, fmt.Errorf("checks: plan: destination %d: %w", i, err)
		}
		out = append(out, typed[i])
	}
	return out, nil
}

// clampTimeout bounds d to [minPerPairTimeout, maxPerPairTimeout].
func clampTimeout(d time.Duration) time.Duration {
	switch {
	case d < minPerPairTimeout:
		return minPerPairTimeout
	case d > maxPerPairTimeout:
		return maxPerPairTimeout
	default:
		return d
	}
}

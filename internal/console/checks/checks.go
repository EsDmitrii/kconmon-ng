// Package checks is the Console's on-demand diagnostics fan-out (M3, Plan
// Decision 13): a run is N bounded-concurrency calls to the controller's
// existing POST /api/v1/diagnostics -- the controller itself is not touched.
package checks

import (
	"errors"
	"fmt"
	"time"
)

const (
	// maxPairs bounds one run's fan-out (Decision 13): a spec that would
	// expand past this is rejected up front, before any dispatch, rather
	// than throttled mid-flight or silently truncated.
	maxPairs = 400

	// maxConcurrency bounds in-flight dispatches for one run (Decision 13).
	maxConcurrency = 8

	// minPerPairTimeout / maxPerPairTimeout clamp Spec.Timeout. maxPerPairTimeout
	// mirrors the controller's own maxDiagnosticsTimeout
	// (internal/controller/diagnostics.go) -- controllerclient.Diagnose clamps
	// again independently, so the two can never disagree even if this one is
	// bypassed.
	minPerPairTimeout = 1 * time.Second
	maxPerPairTimeout = 120 * time.Second
)

// Plan's errors.
//
// None of these sentinels carries the "checks: " package prefix, and none of
// them ever should: every site that returns one wraps it with this package's
// own "checks: <op>: " context (Plan's "checks: plan: ", planDestinations'
// "checks: plan: destination %d: "), exactly the way memory.go, runner.go and
// reconciler.go do. A prefix on the sentinel TOO renders twice --
// httpapi.handleRunError puts err.Error() straight into the problem+json
// detail, so the console once showed operators
// "checks: plan: checks: no nodes available to plan against".
// TestPlanErrorsCarryExactlyOnePackagePrefix pins the rendered strings.
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
	// ErrNoPairs is returned when a Spec's Sources/Destinations are both
	// individually non-empty (so ErrNoNodes does not apply) but every
	// combination is a self-pair or a duplicate -- e.g. Sources ==
	// Destinations == ["a"]. Without this, Start would persist and
	// immediately finish a run with PairTotal 0, which finalStatus reports as
	// "succeeded": a vacuous, misleading result rather than the clear error a
	// spec that can never dispatch anything deserves.
	ErrNoPairs = errors.New("no pairs to check (every combination is a self-pair or duplicate)")
	// ErrInvalidDestination is returned when a Spec.TypedDestinations entry
	// names a Kind that is not node|target|adhoc, or omits the field that
	// Kind requires (Node for a node destination, Address for a target or
	// adhoc one). Rejected up front for the same reason ErrUnknownType is: a
	// destination this package cannot describe is not one it may guess at,
	// and a run that dispatched a half-built destination would report a
	// failure that says nothing about the network.
	ErrInvalidDestination = errors.New("invalid destination")
)

// The destination kinds a Spec may name (Plan Decision 14). Only DestKindNode
// is ever resolved against the controller's agent registry; the other two
// travel as destinationKind=external on POST /api/v1/diagnostics, which means
// the CONTROLLER never looks them up either -- it forwards the address to the
// source agent as an ExternalTarget.
const (
	// DestKindNode is M3's behaviour verbatim: a node name, resolved against
	// the registry. The zero-value Destination.Kind ("") is read as this, so
	// a caller that predates typed destinations cannot accidentally build an
	// external one.
	DestKindNode = "node"
	// DestKindTarget is a targets row resolved Console-side to an address.
	DestKindTarget = "target"
	// DestKindAdhoc is an operator-typed address that no targets row backs.
	DestKindAdhoc = "adhoc"
)

// Destination is one resolved destination for a run (Plan Decision 14).
//
// Name, for a target or adhoc destination, is the METRIC-SAFE LABEL VALUE --
// it is what becomes check_results.destination_node, a progress frame's
// "destination", and eventually a Prometheus label. Address is the thing
// actually probed and is deliberately NOT an identifier: it never reaches a
// label, a stored row, or an event. That split is the whole reason this is a
// struct rather than a second string list -- with one string per destination
// there is nowhere to put an address that is not also, silently, an
// identifier.
type Destination struct {
	Kind    string `json:"kind"`
	Node    string `json:"node,omitempty"`    // Kind=node
	Name    string `json:"name,omitempty"`    // Kind=target|adhoc -- the metric-safe label value
	Address string `json:"address,omitempty"` // Kind=target|adhoc
}

// NodeDestination builds the Kind "node" Destination a plain node name
// expands into -- the only shape M3 ever produced, and still what
// Spec.Destinations' strings become.
func NodeDestination(node string) Destination {
	return Destination{Kind: DestKindNode, Node: node}
}

// IsNode reports whether d is resolved against the controller's agent
// registry. The zero value ("" Kind) counts as a node, mirroring the
// controller's own default (internal/controller/diagnostics.go: an absent
// destinationKind takes exactly the old path).
func (d *Destination) IsNode() bool {
	return d.Kind == "" || d.Kind == DestKindNode
}

// Label is the destination's identity everywhere downstream: the progress
// frame's "destination", check_results.destination_node, and the "destination"
// field of the dispatch itself. A node destination labels as its node name; an
// external one as its Name, falling back to Address only when no name was
// given (an adhoc destination an operator typed straight in), which mirrors
// the controller's own destName fallback exactly.
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
// (internal/controller/diagnostics.go). It is a deliberate copy, not an
// import: this package must never import internal/controller (task-22-brief.md
// -- no controller edits, and no controller-package dependency either).
var validCheckTypes = map[string]struct{}{
	"tcp":  {},
	"udp":  {},
	"icmp": {},
	"dns":  {},
	"http": {},
	"mtr":  {},
}

// Pair is one (source, destination) dispatch within a run. Source stays a
// plain node name -- a check is always dispatched FROM an agent, so there is
// nothing to type on that side (Decision 14). Pair stays comparable, which is
// what lets Plan dedupe with a map keyed on it.
type Pair struct {
	Source      string
	Destination Destination
}

// Spec describes one run request.
//
// Destinations and TypedDestinations are additive halves of one list, not
// alternatives: the strings expand into Kind "node" Destinations and the typed
// entries are appended after them, in order. Keeping the M3 string field
// exactly as it was is deliberate -- httpapi builds a Spec from POST
// /api/v1/runs' "destinations" array and is untouched by this change, and a
// node-only Spec still serializes to the same JSON snapshot it did in M3
// (TypedDestinations is `omitempty`).
type Spec struct {
	Sources      []string // node names; empty = every node in the current topology
	Destinations []string // node names; empty (AND no TypedDestinations) = every node in the current topology
	Type         string   // tcp|udp|icmp|dns|http|mtr -- the controller's validCheckTypes
	Plane        string   // "pod" (the only plane that exists)
	// TypedDestinations are destinations that a node name cannot express: a
	// targets row resolved to an address, or an operator-typed one. Naming
	// any of these SUPPRESSES the full-mesh fallback -- an operator who asked
	// for one external target has not asked for the whole cluster mesh too.
	TypedDestinations []Destination `json:",omitempty"`
	Timeout           time.Duration // per pair; clamped to [1s, 120s]
}

// Plan expands spec into an ordered, de-duplicated list of pairs, dropping
// node<->node self-pairs. Pure and I/O-free -- nodes is supplied by the caller
// (the current topology's node names), never fetched here -- which is what
// makes it testable without any controller/store dependency and is where the
// pair-count bound lives.
//
// An empty Spec.Sources falls back to nodes, and so does an empty destination
// side -- but only when Spec.TypedDestinations is empty too: naming an
// external destination is an explicit choice that must not silently drag the
// whole cluster mesh in alongside it. If a fallback is actually needed and
// nodes is itself empty, Plan returns ErrNoNodes rather than silently
// producing a zero-pair run. An unrecognized Spec.Type is rejected with
// ErrUnknownType, and a TypedDestinations entry this package cannot dispatch
// with ErrInvalidDestination.
//
// maxPairs (400) bounds the WIDENED product -- node destinations and typed
// ones together -- exactly as it bounded the node-only one in M3: a spec whose
// raw sources x destinations product exceeds it, or whose in-loop count does
// after self-pair exclusion and dedup, is rejected with ErrTooManyPairs, the
// raw-product guard running first, before any allocation. A spec where both
// sides are individually non-empty but every combination collapses to a
// self-pair or a duplicate is rejected with ErrNoPairs, for the same "no
// silent zero-pair run" reason as ErrNoNodes.
//
// Self-pair exclusion applies to node<->node pairs ONLY. An external
// destination is never a self-pair even when its name or address happens to
// equal the source node's name: "probe api-prod from the node called
// api-prod" is a perfectly ordinary request, and dropping it would be a
// silent, baffling omission rather than the dedup it looks like.
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

	// Reject a spec whose RAW (pre-dedupe, pre-self-pair-exclusion) cartesian
	// product already exceeds maxPairs before allocating anything below.
	// Sizing the seen-set/pairs allocations off that raw product, as this
	// package used to, means a naively large Sources/Destinations pair (e.g.
	// 10k x 10k = 100M) drives a huge allocation long before the in-loop
	// maxPairs check a few lines down -- still the exact arbiter, since dedup
	// and self-pair exclusion can only shrink the eventual count -- ever gets
	// a chance to run. This guard runs before any dedup work, so a spec whose
	// raw product is large purely from duplicate entries that would dedupe
	// under maxPairs is rejected here too; that is intentional, not an
	// oversight -- the point is to bound the allocation size, and this check
	// cannot itself depend on the dedup it precedes.
	rawProduct := len(sources) * len(destinations)
	if rawProduct > maxPairs {
		return nil, fmt.Errorf("checks: plan: %w: computed %d pairs, limit %d", ErrTooManyPairs, rawProduct, maxPairs)
	}

	// Capped defensively at maxPairs+1 (the exact bound the in-loop check
	// below rejects at) rather than trusted to already be small: rawProduct
	// passed the guard above, so in practice it never exceeds maxPairs here,
	// but the hint is bounded independently of that guard's own correctness.
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

// planDestinations resolves a Spec's two destination halves into the single
// ordered list Plan expands against: the node names first (each becoming a
// Kind "node" Destination, M3's shape verbatim), then the typed entries, each
// validated. Only when BOTH halves are empty does the current topology's node
// list stand in -- see Plan's doc comment for why a typed destination
// suppresses that fallback rather than adding to it.
//
// Every typed entry is validated before anything is dispatched, including
// ones that a later maxPairs rejection would have discarded anyway: a spec
// carrying a malformed destination is malformed regardless of how many pairs
// it would have produced, and reporting whichever error the ordering happened
// to reach first would make the failure depend on the spec's size.
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

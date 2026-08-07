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
var (
	// ErrTooManyPairs is returned when a Spec expands past maxPairs (400)
	// pairs.
	ErrTooManyPairs = errors.New("checks: too many pairs")
	// ErrUnknownType is returned when Spec.Type is not one of the
	// controller's validCheckTypes.
	ErrUnknownType = errors.New("checks: unknown check type")
	// ErrNoNodes is returned when Plan needs the current topology's node
	// list (Spec.Sources or Spec.Destinations is empty) and that list is
	// itself empty -- a clear error rather than a silent zero-pair run.
	ErrNoNodes = errors.New("checks: no nodes available to plan against")
	// ErrNoPairs is returned when a Spec's Sources/Destinations are both
	// individually non-empty (so ErrNoNodes does not apply) but every
	// combination is a self-pair or a duplicate -- e.g. Sources ==
	// Destinations == ["a"]. Without this, Start would persist and
	// immediately finish a run with PairTotal 0, which finalStatus reports as
	// "succeeded": a vacuous, misleading result rather than the clear error a
	// spec that can never dispatch anything deserves.
	ErrNoPairs = errors.New("checks: no pairs to check (every combination is a self-pair or duplicate)")
)

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

// Pair is one (source, destination) dispatch within a run.
type Pair struct {
	Source      string
	Destination string
}

// Spec describes one run request.
type Spec struct {
	Sources      []string      // node names; empty = every node in the current topology
	Destinations []string      // node names; empty = every node in the current topology
	Type         string        // tcp|udp|icmp|dns|http|mtr -- the controller's validCheckTypes
	Plane        string        // "pod" (the only plane that exists)
	Timeout      time.Duration // per pair; clamped to [1s, 120s]
}

// Plan expands spec into an ordered, de-duplicated list of pairs, dropping
// self-pairs (source == destination). Pure and I/O-free -- nodes is supplied
// by the caller (the current topology's node names), never fetched here --
// which is what makes it testable without any controller/store dependency
// and is where the pair-count bound lives.
//
// An empty Spec.Sources or Spec.Destinations falls back to nodes (full mesh,
// or a one-sided fan-out/fan-in). If that fallback is actually needed and
// nodes is itself empty, Plan returns ErrNoNodes rather than silently
// producing a zero-pair run. An unrecognized Spec.Type is rejected with
// ErrUnknownType. A spec whose raw Sources x Destinations product exceeds
// maxPairs (400), or whose in-loop count does after self-pair exclusion and
// dedup, is rejected with ErrTooManyPairs -- the raw-product guard runs
// first, before any allocation. A spec where both Sources and Destinations
// are individually non-empty but every combination collapses to a self-pair
// or a duplicate -- so the loop above still produces zero pairs -- is
// rejected with ErrNoPairs, for the same "no silent zero-pair run" reason as
// ErrNoNodes.
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
	destinations := spec.Destinations
	if len(destinations) == 0 {
		if len(nodes) == 0 {
			return nil, fmt.Errorf("checks: plan: %w", ErrNoNodes)
		}
		destinations = nodes
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
			if src == dst {
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

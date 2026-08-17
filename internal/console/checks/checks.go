// Package checks is the Console's on-demand diagnostics fan-out.
package checks

import (
	"errors"
	"fmt"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

const (
	// maxPairs bounds one run's fan-out: a spec that would expand past this is rejected up front,
	// before any dispatch.
	maxPairs = 400

	// maxConcurrency bounds in-flight dispatches for one run (Decision 13).
	maxConcurrency = 8

	// maxPerSourceConcurrency bounds in-flight dispatches against ONE source agent. It sits below
	// the agent's own on-demand task limit so a run never wins the race against the agent's
	// semaphore, which refuses the overflow outright instead of queueing it.
	maxPerSourceConcurrency = 2

	// mtrMinPerPairTimeout is the floor a traceroute pair gets: a trace walks up to 30 hops and the
	// agent may hold it behind its task semaphore first, so a shorter deadline gives up on work that
	// is still running.
	mtrMinPerPairTimeout = 90 * time.Second

	/* udpMinPerPairTimeout is the floor a UDP pair gets, for the same reason MTR has one: the probe
	   waits out a full read deadline PER LOST PACKET, so its worst case is packets x timeout — with
	   the chart's own defaults (packets: 5, timeout: 250ms) that is 1.25s, already past the 1s floor
	   an unspecified diagnostic timeout is clamped to. A pair losing everything is exactly the pair
	   an operator started the run to look at, and it came back as "dispatch timed out" instead of
	   "100% loss": the measurement replaced by a report that the machinery failed.

	   5s covers the shipped defaults with room for a slow agent and its task semaphore; an operator
	   who configures a wider probe can still ask for more. */
	udpMinPerPairTimeout = 5 * time.Second

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

	// MinSampleInterval is the fastest a pair is re-probed by a DERIVED cadence, whatever the
	// duration. Without it a 10s run would try to sample every 20ms and
	// measure the console's own dispatch loop rather than the network.
	//
	// It does not bind Spec.RequestedSampleInterval: this floor exists to stop an arithmetic
	// accident nobody chose, and an operator who typed 1s has chosen. MinRequestedSampleInterval is
	// that path's own floor.
	MinSampleInterval = 5 * time.Second

	// MinRequestedSampleInterval is the fastest cadence an operator may ASK for. One second is the
	// resolution below which the console would be reporting its own dispatch overhead as a network
	// measurement whatever the duration, so it is a range bound rather than a plan adjustment --
	// refused at creation, the way ValidateDuration refuses a duration.
	MinRequestedSampleInterval = 1 * time.Second
)

// The cadence-adjustment reasons. They name why the cadence a run KEEPS is not the cadence its
// duration (or its operator) asked for, and they are snapshotted onto the spec so the permalink can
// say which of the two numbers it is showing. "" is the honest majority: nothing moved.
const (
	// IntervalCapped: the cadence would have produced more than MaxSamplesPerPair samples for one
	// pair, so it was widened to duration/MaxSamplesPerPair. Reported rather than truncated -- an
	// operator who asked for 1s over 24h gets told the cap bound them, not handed 500 samples and
	// left to work out where the other 85 900 went.
	IntervalCapped = "cap"
	// IntervalStretched: one round over this fan-out cannot finish inside that cadence, so the plan
	// is one round's floor. This is the mtr case, and it is the one the whole three-numbers bug was
	// about.
	IntervalStretched = "round"
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
	// ErrSampleIntervalOutOfRange is returned when Spec.RequestedSampleInterval is non-zero but
	// outside [MinRequestedSampleInterval, Spec.Duration], or names a cadence on an instant run.
	//
	// This is the RANGE half of the interval contract; the FEASIBILITY half is never an error. A
	// cadence one round of traces cannot keep is re-planned and reported (IntervalStretched), the
	// same family of automatic adjustment as the MaxSamplesPerPair cap -- see PlanCadence.
	ErrSampleIntervalOutOfRange = errors.New("sample interval out of range")
	/* ErrCancelUnreachable is returned when a cancel targets a run this process does not own and
	   there is no cross-replica bus to hand it to.

	   It is an ERROR and not a quiet nil because the alternative was the worst answer available: the
	   handler returned 204 and the log claimed the cancel had been forwarded, while the run kept
	   dispatching to the whole fleet on the other replica. An operator who is told "cancelled" stops
	   looking. This says the console could not reach the owner, which is true and actionable — the
	   console is running more than one replica without a shared bus. */
	ErrCancelUnreachable = errors.New("cannot reach the replica that owns this run: this console has no cross-replica bus")
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

	// RequestedSampleInterval is the cadence the OPERATOR asked for, verbatim -- the one field on
	// this struct the planner reads rather than writes. Zero is "derive it", which is what every run
	// before this field existed did and what the UI's "Auto" still does.
	//
	// It is kept apart from PlannedSampleIntervalNs on purpose: those two are the same number only
	// when nothing bound the request, and the whole bug this field closes was three surfaces
	// printing one quantity as though it were another. A request that cannot be kept is never
	// silently replaced by the plan -- both travel.
	RequestedSampleInterval time.Duration `json:",omitempty"`

	// PlannedSampleIntervalNs and PlannedSamplesPerPair are DERIVED, snapshotted by Start and
	// overwritten if a client sends them. They are the cadence the run will actually keep, which the
	// base cadence alone no longer predicts once a slow check type stretches it, and they live in the
	// spec column for the reason Duration does: check_runs has no column of their own.
	PlannedSampleIntervalNs int64 `json:",omitempty"`
	PlannedSamplesPerPair   int   `json:",omitempty"`
	// SampleIntervalAdjusted names WHY the two differ -- IntervalCapped or IntervalStretched -- and
	// is empty when they do not. Derived and snapshotted alongside them.
	SampleIntervalAdjusted string `json:",omitempty"`
}

// ValidateSampleInterval accepts an absent request (zero) or one inside [MinRequestedSampleInterval,
// duration]. An instant run has no cadence to dial at all, so a request on one is refused rather
// than ignored -- a value that would have no effect is a misunderstanding, and dropping it silently
// is how an operator ends up believing a run did something it never did.
//
// A cadence LONGER than the run is refused for the same reason a duration outside [10s, 24h] is:
// there is no plan to adjust it to. It would collapse to a single sample, which is an instant run
// the caller did not ask for.
func ValidateSampleInterval(requested, duration time.Duration) error {
	if requested == 0 {
		return nil
	}
	if duration <= 0 {
		return fmt.Errorf("checks: plan: %w: %s was requested for a run with no duration; "+
			"a cadence needs durationNs (or omit it for an instant run)", ErrSampleIntervalOutOfRange, requested)
	}
	if requested < MinRequestedSampleInterval || requested > duration {
		return fmt.Errorf("checks: plan: %w: %s, allowed range is %s..%s (this run's own duration is the ceiling; "+
			"omit it to let the console derive the cadence)",
			ErrSampleIntervalOutOfRange, requested, MinRequestedSampleInterval, duration)
	}
	return nil
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

// roundFloor is the shortest one round over pairs can take: both the run-wide window and the
// per-source gate have to drain, and the slower of the two governs.
func roundFloor(pairs []Pair, perPairTimeout time.Duration) time.Duration {
	perSource := make(map[string]int, len(pairs))
	for i := range pairs {
		perSource[pairs[i].Source]++
	}
	busiest := 0
	for _, n := range perSource {
		if n > busiest {
			busiest = n
		}
	}

	batches := ceilDiv(len(pairs), maxConcurrency)
	if b := ceilDiv(busiest, maxPerSourceConcurrency); b > batches {
		batches = b
	}
	return time.Duration(batches) * perPairTimeout
}

func ceilDiv(a, b int) int {
	if b <= 0 {
		return a
	}
	n := (a + b - 1) / b
	if n < 1 {
		return 1
	}
	return n
}

// BaseSampleInterval is the cadence the dispatch loop is PACED by: the operator's own
// RequestedSampleInterval when there is one, else the derived duration/MaxSamplesPerPair floored at
// MinSampleInterval. Either way the MaxSamplesPerPair cap binds -- it is the hard ceiling on what
// one pair may contribute, and a request cannot buy past it.
//
// The derived branch is byte-for-byte SampleInterval, so a run with no request plans exactly as it
// planned before this function existed.
func BaseSampleInterval(spec *Spec) time.Duration {
	if spec.Duration <= 0 {
		return 0
	}
	if spec.RequestedSampleInterval <= 0 {
		return SampleInterval(spec.Duration)
	}
	// The cap, and only the cap. MinSampleInterval is deliberately absent here: see its doc.
	if ceiling := spec.Duration / MaxSamplesPerPair; spec.RequestedSampleInterval < ceiling {
		return ceiling
	}
	return spec.RequestedSampleInterval
}

// Cadence is the WHOLE cadence decision for one run, so that no caller has to reassemble it from
// two functions and guess which of the numbers it holds is the measured one.
//
// The bug it closes: the same quantity was reported three ways -- the create form said the base
// cadence, the permalink said the worst-case round floor, and the run did neither. Requested,
// Interval and Adjusted are three different facts and now travel as three fields.
type Cadence struct {
	// Requested is what the operator asked for; zero when they asked for nothing.
	Requested time.Duration
	// Base is what paces the dispatch loop: the request or the derived cadence, capped.
	Base time.Duration
	// Interval is the cadence the run is PLANNED to keep -- Base, stretched to one round's floor
	// when a round cannot finish faster. A worst case, not an observation: rounds run back to back,
	// so a healthy run beats it.
	Interval time.Duration
	// SamplesPerPair is PlannedSamplesPerPair over Interval: a floor, capped at MaxSamplesPerPair.
	SamplesPerPair int
	// Adjusted is "" | IntervalCapped | IntervalStretched.
	Adjusted string
}

// PlanCadence resolves a spec's duration and requested interval into the plan the run will execute.
//
// The below-floor rule, stated once: a cadence this fan-out cannot keep is ADJUSTED and reported,
// never refused. An MTR trace walks up to thirty hops in sequence, so "every 1s" over ten pairs is
// not a mistake in the request, it is a fact about traceroute -- and the product's established
// voice is auto-adjustment with honest reporting (the same call the density-422 removal made). What
// IS refused is a value out of range, which ValidateSampleInterval handles before this runs.
func PlanCadence(spec *Spec, pairs []Pair, perPairTimeout time.Duration) Cadence {
	base := BaseSampleInterval(spec)
	cad := Cadence{Requested: spec.RequestedSampleInterval, Base: base, Interval: base}
	if base <= 0 {
		// An instant run: no cadence, one honest pass.
		cad.SamplesPerPair = PlannedSamplesPerPair(spec.Duration, 0)
		return cad
	}

	switch floor := roundFloor(pairs, perPairBudget(spec.Type, perPairTimeout)); {
	case floor > base:
		// One round cannot finish inside the cadence, so the cadence is one round.
		cad.Interval = floor
		cad.Adjusted = IntervalStretched
	case spec.RequestedSampleInterval > 0 && base > spec.RequestedSampleInterval:
		// Nothing stretched it, so the only thing that could have moved it is the cap.
		cad.Adjusted = IntervalCapped
	}
	cad.SamplesPerPair = PlannedSamplesPerPair(spec.Duration, cad.Interval)
	return cad
}

// EffectiveSampleInterval is the cadence a run can actually keep: the base cadence, stretched to
// one round's floor when the check type is slower than it. A run whose cadence cannot be kept is
// re-planned rather than refused -- the same family of automatic adjustment as the
// MaxSamplesPerPair cap. Rounds still run back-to-back when a round overruns, so traces that finish
// early densify on their own; this is the plan, not a floor on the work.
//
// PlanCadence is the fuller answer (it also names WHY the plan moved); this stays as the one-number
// spelling its callers and tests already read.
func EffectiveSampleInterval(spec *Spec, pairs []Pair, perPairTimeout time.Duration) time.Duration {
	return PlanCadence(spec, pairs, perPairTimeout).Interval
}

// perPairBudget is how long one probe is EXPECTED to take, which is not its timeout. A tcp, udp,
// icmp, dns or http probe answers in milliseconds and its timeout is only the ceiling on a probe
// that has already failed, so planning a cadence around it would slow down every healthy run.
// A traceroute is different in kind: it walks up to 30 hops in sequence and routinely spends tens
// of seconds doing it, so for mtr the budget is the real thing to plan around. A trace that
// finishes early still densifies on its own, because rounds run back to back.
func perPairBudget(checkType string, perPairTimeout time.Duration) time.Duration {
	if checkType == string(model.CheckMTR) {
		return perPairTimeout
	}
	return 0
}

// PlannedSamplesPerPair is a FLOOR, not a target: how many probes one pair contributes if every
// round takes its worst case. Rounds repeat until the requested duration elapses and a round that
// finishes early starts the next one, so a healthy run produces MORE than this. The true upper
// bound is MaxSamplesPerPair. It is at least one, so a duration shorter than a single round is one
// honest pass rather than a refusal.
func PlannedSamplesPerPair(duration, interval time.Duration) int {
	if duration <= 0 || interval <= 0 {
		return 1
	}
	n := int(duration / interval)
	if n < 1 {
		return 1
	}
	if n > MaxSamplesPerPair {
		return MaxSamplesPerPair
	}
	return n
}

// clampTimeoutFor bounds d for one check type. mtr gets a higher floor than the operator can ask
// for: a deadline under the trace budget turns work that is still running into a dispatch timeout.
func clampTimeoutFor(checkType string, d time.Duration) time.Duration {
	out := clampTimeout(d)
	if checkType == string(model.CheckMTR) && out < mtrMinPerPairTimeout {
		return mtrMinPerPairTimeout
	}
	if checkType == string(model.CheckUDP) && out < udpMinPerPairTimeout {
		return udpMinPerPairTimeout
	}
	return out
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

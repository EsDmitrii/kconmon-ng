package checks

import (
	"errors"
	"fmt"
	"sort"
)

// protocolsPerDefinitionMirror is the "protocols" half of Decision 12's
// `agents x protocols`, kept LOCAL on purpose: it mirrors httpapi's
// protocolsPerDefinition (internal/console/httpapi/definitions.go) rather
// than importing it, for the same reason validCheckTypes above is a
// deliberate copy -- this package stays free of store/controller/httpapi
// dependencies so the planner can be tested with nothing but slices. A
// definition names exactly ONE check type, so it contributes one protocol's
// worth of series per assigned agent. The day a definition can probe several
// protocols from one spec, this becomes a function of the definition and the
// projection arithmetic below does not change shape.
const protocolsPerDefinitionMirror = 1

// AssignAgents's errors. Prefix-free for the same reason Plan's are (see the
// var block in checks.go): AssignAgents wraps every one of them with
// "checks: assign: " itself, so a "checks: " on the sentinel would render the
// package name twice in the message an operator actually reads.
var (
	// ErrNoAgents is returned when at least one ENABLED definition needs
	// agents to run on and the topology snapshot has none -- a clear error
	// rather than a silent zero-assignment push that would look like a
	// successful reconcile while nothing is being probed. It mirrors
	// ErrNoNodes's posture for Plan.
	ErrNoAgents = errors.New("no agents available to assign against")
	// ErrUnknownSelection is returned when a Definition.Selection is not one
	// of SelectAll, SelectPerZone or SelectOnePerZone. It mirrors
	// ErrUnknownType: an unrecognized mode is rejected up front, never
	// silently treated as the default, because guessing here would quietly
	// change how many series a definition exports.
	ErrUnknownSelection = errors.New("unknown selection")
)

// Selection is a definition's agent-selection strategy (TARGETS.md 7.2).
// one-per-zone is the DEFAULT because it is the only mode whose series count
// scales with ZONE count rather than NODE count, and node count is the number
// that grows without an operator noticing (Plan Decision 11).
type Selection string

// The selection strategies a definition may name. The wire values match
// store.DefinitionInput.SourceSelection's validated set (all | per-zone |
// one-per-zone), which is what lets a stored row be handed to AssignAgents
// unchanged.
const (
	SelectAll        Selection = "all"
	SelectPerZone    Selection = "per-zone"
	SelectOnePerZone Selection = "one-per-zone"
)

// AgentRef is one agent in the topology snapshot the caller supplies. AgentID
// == NodeName in this codebase (exactly one agent per node), so the node name
// is both the identity a push is addressed to and the tiebreak that makes
// one-per-zone deterministic. Zone is "" for a node the cluster never labeled;
// that is a real bucket, not a missing value (see assignedAgents).
type AgentRef struct {
	NodeName string
	Zone     string
}

// Definition is the checks-local view of a saved check definition: the fields
// the planner actually needs and no more. It is deliberately NOT
// store.Definition -- no Params, no timestamps -- because this package must
// never depend on store (same posture as validCheckTypes' deliberate copy).
// The caller projects its stored rows onto this type.
type Definition struct {
	ID                 string
	Name               string
	Selection          Selection
	CheckType          string
	DestinationAddress string
	Enabled            bool
}

// AssignedSpec is one thing an agent must continuously probe. It carries the
// definition's ID so the agent (and any operator reading a metric) can trace a
// series back to the row that produced it.
type AssignedSpec struct {
	DefinitionID       string
	Name               string
	CheckType          string
	DestinationAddress string
}

// Assignment is one agent's complete external-check workload. The set is
// absolute, never a delta: the controller replaces an agent's whole
// assignment on each push, exactly as PeerUpdate.FULL_SYNC does
// (api/proto/kconmon.proto:73), so a dropped message can never leave an agent
// probing a target the operator deleted.
type Assignment struct {
	AgentID string
	Specs   []AssignedSpec
}

// AssignAgents resolves defs against the agents of a topology snapshot into
// per-agent assignment sets, and returns the number of Prometheus series
// those assignments project to. Pure and I/O-free -- agents is supplied by
// the caller, never fetched here -- which is what makes it testable without a
// store or controller dependency, the same property that makes Plan testable.
//
// Selection semantics (Plan Decision 11, mirrored by httpapi's
// projectedAgents): "all" and "per-zone" both resolve to EVERY agent --
// per-zone groups the same set by zone for downstream metric labeling rather
// than shrinking it -- while "one-per-zone" resolves to exactly one agent per
// zone, the first by sorted node name. That sort is a correctness property,
// not tidiness: a non-deterministic pick would move the probe on every
// reconcile tick and churn half-populated series (plan line 424). Zoneless
// agents (Zone == "") collectively form ONE bucket under one-per-zone, the
// same rule the "" zone key gives any other zone and the same convention
// httpapi counts with, so planner and projection guard can never disagree.
// A zone with no agents simply does not appear -- zones are derived from the
// agent list, so there is nothing to skip and nothing to error about.
//
// A DISABLED definition produces no specs and no series, but its selection is
// still validated: a malformed stored row is a configuration error worth
// surfacing now, not a runtime surprise waiting for an operator to flip
// Enabled. An unrecognized selection is rejected with ErrUnknownSelection.
// Zero usable agents with at least one enabled definition is rejected with
// ErrNoAgents rather than reported as a successful empty reconcile; zero
// agents with only disabled definitions is not an error, because nothing
// wanted an agent.
//
// The result is deterministic: only agents carrying at least one spec appear,
// keyed by AgentID, and each Assignment's Specs are sorted by DefinitionID.
// Map iteration order is Go-random, but the CONTENT of two runs over the same
// input is deep-equal.
// The results are named for readability only -- every return below is
// explicit, never naked.
func AssignAgents(defs []Definition, agents []AgentRef) (assignments map[string]Assignment, projectedSeries int, err error) {
	for i := range defs {
		switch defs[i].Selection {
		case SelectAll, SelectPerZone, SelectOnePerZone:
		default:
			return nil, 0, fmt.Errorf("checks: assign: %w: %q (definition %q)",
				ErrUnknownSelection, defs[i].Selection, defs[i].ID)
		}
	}

	// Normalize before the emptiness check, not after: an agent with no node
	// name has no identity to address a push to, and a node name repeated in
	// the snapshot is still one agent. Both would otherwise skew the series
	// count -- the number the projection guard is supposed to bound.
	sorted := normalizeAgents(agents)

	enabled := 0
	for i := range defs {
		if defs[i].Enabled {
			enabled++
		}
	}
	if enabled == 0 {
		return map[string]Assignment{}, 0, nil
	}
	if len(sorted) == 0 {
		return nil, 0, fmt.Errorf("checks: assign: %w", ErrNoAgents)
	}

	specsByAgent := make(map[string][]AssignedSpec, len(sorted))
	series := 0
	for i := range defs {
		if !defs[i].Enabled {
			continue
		}
		targets := assignedAgents(defs[i].Selection, sorted)
		spec := AssignedSpec{
			DefinitionID:       defs[i].ID,
			Name:               defs[i].Name,
			CheckType:          defs[i].CheckType,
			DestinationAddress: defs[i].DestinationAddress,
		}
		for _, id := range targets {
			specsByAgent[id] = append(specsByAgent[id], spec)
		}
		series += len(targets) * protocolsPerDefinitionMirror
	}

	out := make(map[string]Assignment, len(specsByAgent))
	for id, specs := range specsByAgent {
		sort.Slice(specs, func(a, b int) bool { return specs[a].DefinitionID < specs[b].DefinitionID })
		out[id] = Assignment{AgentID: id, Specs: specs}
	}
	return out, series, nil
}

// normalizeAgents returns agents sorted by node name, with unnamed and
// duplicate entries dropped. Sorting once here is what makes every downstream
// pick deterministic without re-sorting per definition.
func normalizeAgents(agents []AgentRef) []AgentRef {
	out := make([]AgentRef, 0, len(agents))
	for _, a := range agents {
		if a.NodeName == "" {
			continue
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeName < out[j].NodeName })
	deduped := out[:0]
	for i := range out {
		if i > 0 && out[i].NodeName == out[i-1].NodeName {
			continue
		}
		deduped = append(deduped, out[i])
	}
	return deduped
}

// assignedAgents resolves one selection against an already-normalized (sorted,
// deduplicated, non-empty) agent list and returns the node names to assign,
// in sorted order. For one-per-zone the sorted input means the FIRST agent
// seen in a zone is that zone's sorted-first, so the representative never
// changes between two runs over the same snapshot.
func assignedAgents(selection Selection, sorted []AgentRef) []string {
	if selection != SelectOnePerZone {
		ids := make([]string, 0, len(sorted))
		for i := range sorted {
			ids = append(ids, sorted[i].NodeName)
		}
		return ids
	}
	seen := make(map[string]struct{}, len(sorted))
	ids := make([]string, 0, len(sorted))
	for i := range sorted {
		if _, dup := seen[sorted[i].Zone]; dup {
			continue
		}
		seen[sorted[i].Zone] = struct{}{}
		ids = append(ids, sorted[i].NodeName)
	}
	return ids
}

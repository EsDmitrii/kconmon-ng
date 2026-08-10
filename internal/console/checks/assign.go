package checks

import (
	"errors"
	"fmt"
	"sort"
)

// protocolsPerDefinitionMirror is the "protocols" half.
const protocolsPerDefinitionMirror = 1

// AssignAgents's errors; prefix-free for the same reason Plan's are (see the var block in
// checks.go).
var (
	// ErrNoAgents is returned when at least one ENABLED definition needs agents to run on and the
	// topology snapshot has none.
	ErrNoAgents = errors.New("no agents available to assign against")
	// ErrUnknownSelection is returned when a Definition.Selection is not one of SelectAll; it mirrors
	// ErrUnknownType: an unrecognized mode is rejected up front.
	ErrUnknownSelection = errors.New("unknown selection")
)

// Selection is a definition's agent-selection strategy (TARGETS.md 7.2).
type Selection string

// The selection strategies a definition may name.
const (
	SelectAll        Selection = "all"
	SelectPerZone    Selection = "per-zone"
	SelectOnePerZone Selection = "one-per-zone"
)

// AgentRef is one agent in the topology snapshot the caller supplies; zone is "" for a node the
// cluster never labeled.
type AgentRef struct {
	NodeName string
	Zone     string
}

// Definition is the checks-local view of a saved check definition; it is deliberately NOT
// store.Definition -- no Params.
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

// Assignment is one agent's complete external-check workload; the set is absolute, never a delta:
// the controller replaces an agent's whole assignment on each push.
type Assignment struct {
	AgentID string
	Specs   []AssignedSpec
}

// AssignAgents resolves defs against the agents of a topology snapshot into per-agent assignment
// sets; pure and I/O-free -- agents is supplied by the caller, never fetched here.
func AssignAgents(defs []Definition, agents []AgentRef) (assignments map[string]Assignment, projectedSeries int, err error) {
	for i := range defs {
		switch defs[i].Selection {
		case SelectAll, SelectPerZone, SelectOnePerZone:
		default:
			return nil, 0, fmt.Errorf("checks: assign: %w: %q (definition %q)",
				ErrUnknownSelection, defs[i].Selection, defs[i].ID)
		}
	}

	// Both would otherwise skew the series count -- the number the projection guard is supposed to
	// bound.
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

// assignedAgents resolves one selection against an already-normalized (sorted, deduplicated,
// non-empty) agent list and returns the node names to assign; for one-per-zone the sorted input
// means the FIRST agent seen in a zone is that zone's sorted-first.
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

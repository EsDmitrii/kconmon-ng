package checks

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
)

/*
 * The zone-pair investigation preset (roadmap M10-3).
 *
 * A zone pair is the scope zone ALERTS name, and under a sparse topology it is exactly the scope
 * whose per-pair data no longer exists — so the one-click answer has to come from on-demand runs.
 * The preset expands both zones to their agent node lists and starts ordinary Runner.Start runs,
 * chunked so no single run exceeds the maxPairs bound Plan enforces.
 *
 * Chunking splits the SOURCE side only, on purpose. Chunks with disjoint sources can run in
 * parallel without ganging up on any one agent: each run holds itself to maxPerSourceConcurrency
 * in-flight dispatches per source, and a source that appears in exactly one run keeps that the
 * fleet-wide bound too. Splitting destinations instead would put the same source in several
 * concurrent runs and race the agent's own task semaphore, which refuses overflow rather than
 * queueing it — manufacturing failed pairs in the very investigation meant to explain failures.
 */

// PresetMaxRuns bounds how many runs one preset invocation may start. Eight runs of maxPairs pairs
// is a 3200-pair investigation fanned out at up to 8*maxConcurrency concurrent dispatches — already
// a deliberate burst; a zone pair wider than that needs a narrower scope, not a bigger hammer.
const PresetMaxRuns = 8

// MaxPairsPerRun re-exports the per-run pair bound (maxPairs) that the chunker packs against, so
// callers and tests can state the contract without a magic 400.
const MaxPairsPerRun = maxPairs

var (
	// ErrUnknownZone is returned when a ZonePairSpec names a zone no registered agent carries.
	ErrUnknownZone = errors.New("unknown zone")
	// ErrZonePairTooLarge is returned when a zone pair cannot be covered within the preset's
	// bounds: the destination zone alone exceeds one run's pair budget, or the chunk count would
	// exceed PresetMaxRuns.
	ErrZonePairTooLarge = errors.New("zone pair too large for the investigation preset")
)

// ZonePairSpec is one preset invocation: probe every source-zone node against every
// destination-zone node with one check type.
type ZonePairSpec struct {
	SourceZone      string
	DestinationZone string
	Type            string // the controller's validCheckTypes, same as Spec.Type
	Plane           string // "" defaults to "pod", same as Spec.Plane
	Timeout         time.Duration
}

// ZonePairRun is one started chunk: the run id plus the pair count its plan produced.
type ZonePairRun struct {
	ID        string
	PairTotal int
}

// ChunkZonePairSources splits sources into ordered, disjoint, contiguous groups whose raw product
// with destinations stays within MaxPairsPerRun. Pure; both inputs are used as given.
//
// The raw product is the bound — not the post-dedup pair count — because Plan itself refuses on the
// raw product before expanding (checks.go), and a chunk this function emits must never be a chunk
// Plan turns away.
func ChunkZonePairSources(sources, destinations []string) ([][]string, error) {
	if len(sources) == 0 || len(destinations) == 0 {
		return nil, fmt.Errorf("checks: zone-pair preset: %w", ErrNoNodes)
	}
	if len(destinations) > maxPairs {
		return nil, fmt.Errorf("checks: zone-pair preset: %w: the destination zone has %d nodes, so even a "+
			"single source exceeds the %d-pair run bound; narrow the scope (a node pair, or a smaller zone)",
			ErrZonePairTooLarge, len(destinations), maxPairs)
	}
	perChunk := maxPairs / len(destinations) // >= 1: len(destinations) <= maxPairs above
	n := ceilDiv(len(sources), perChunk)
	if n > PresetMaxRuns {
		return nil, fmt.Errorf("checks: zone-pair preset: %w: %d sources x %d destinations needs %d runs of "+
			"at most %d pairs, limit %d; narrow the scope",
			ErrZonePairTooLarge, len(sources), len(destinations), n, maxPairs, PresetMaxRuns)
	}
	chunks := make([][]string, 0, n)
	for start := 0; start < len(sources); start += perChunk {
		end := min(start+perChunk, len(sources))
		chunks = append(chunks, sources[start:end])
	}
	return chunks, nil
}

/*
 * StartZonePair expands spec's two zones against the live topology and starts one ordinary run per
 * source chunk. Every chunk is planned BEFORE the first run starts, so a spec that is invalid in
 * any part (unknown type, a zone that only self-pairs) is refused whole rather than half-started.
 *
 * All chunks are started immediately — capped-parallel, where the cap is PresetMaxRuns itself:
 * chunk sources are disjoint, so per-agent concurrency stays at each run's own
 * maxPerSourceConcurrency no matter how many chunks run at once.
 *
 * A non-empty slice returned WITH an error means a mid-loop Start failed: the named runs are real
 * and running, the rest never started. The caller owes the operator that distinction.
 */
func (r *Runner) StartZonePair(ctx context.Context, spec ZonePairSpec, initiator authz.Subject) ([]ZonePairRun, error) { //nolint:gocritic // hugeParam: Subject is a value type by design, same as Start
	zones, err := r.zoneNodes(ctx)
	if err != nil {
		return nil, err
	}
	sources := zones[spec.SourceZone]
	if len(sources) == 0 {
		return nil, fmt.Errorf("checks: zone-pair preset: %w: no agent reports zone %q", ErrUnknownZone, spec.SourceZone)
	}
	destinations := zones[spec.DestinationZone]
	if len(destinations) == 0 {
		return nil, fmt.Errorf("checks: zone-pair preset: %w: no agent reports zone %q", ErrUnknownZone, spec.DestinationZone)
	}

	chunks, err := ChunkZonePairSources(sources, destinations)
	if err != nil {
		return nil, err
	}

	// Plan every chunk first: the same expansion Start will repeat, run here so nothing starts
	// unless everything can.
	specs := make([]Spec, 0, len(chunks))
	totals := make([]int, 0, len(chunks))
	for _, group := range chunks {
		chunkSpec := Spec{
			Sources:      group,
			Destinations: destinations,
			Type:         spec.Type,
			Plane:        spec.Plane,
			Timeout:      spec.Timeout,
		}
		pairs, planErr := Plan(chunkSpec, nil)
		if planErr != nil {
			return nil, planErr
		}
		specs = append(specs, chunkSpec)
		totals = append(totals, len(pairs))
	}

	started := make([]ZonePairRun, 0, len(specs))
	for i := range specs {
		id, startErr := r.Start(ctx, specs[i], initiator)
		if startErr != nil {
			return started, fmt.Errorf("checks: zone-pair preset: started %d of %d runs, then: %w",
				len(started), len(specs), startErr)
		}
		started = append(started, ZonePairRun{ID: id, PairTotal: totals[i]})
	}
	return started, nil
}

// zoneNodes reads the current topology and groups agent node names by zone, each list sorted and
// de-duplicated — the same node source resolveNodes uses, because only nodes with agents can probe.
func (r *Runner) zoneNodes(ctx context.Context) (map[string][]string, error) {
	topo, err := r.ctrl.Topology(ctx)
	if err != nil {
		return nil, fmt.Errorf("checks: zone-pair preset: resolve topology: %w", err)
	}
	seen := make(map[string]struct{}, len(topo.Agents))
	zones := make(map[string][]string)
	for i := range topo.Agents {
		a := &topo.Agents[i]
		if a.NodeName == "" {
			continue
		}
		if _, dup := seen[a.NodeName]; dup {
			continue
		}
		seen[a.NodeName] = struct{}{}
		zones[a.Zone] = append(zones[a.Zone], a.NodeName)
	}
	for _, nodes := range zones {
		sort.Strings(nodes)
	}
	return zones, nil
}

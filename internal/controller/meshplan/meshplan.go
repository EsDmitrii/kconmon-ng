/*
Package meshplan plans the sparse probe mesh (M10): which peers each agent probes when the fleet is
too large for every agent to probe every other.

The plan is three deterministic layers over the same agent set:

 1. A ring over the agents sorted by node name: each agent probes its ringDegree successors. The
    ring alone makes the directed graph strongly connected and gives every agent at least one
    prober.
 2. Cross-zone chords: zoneChords extra targets per agent, the highest-random-weight (HRW) scored
    candidates among agents of OTHER zones. HRW is a pure function of the two agent IDs, so every
    controller replica computes the same chords with no shared state.
 3. Zone-pair repair: HRW chords make cross-zone probes likely, not guaranteed, so any ordered zone
    pair still unobserved gets one deterministic probe (HRW-picked source and target). This is what
    lets zone-level alerting keep working on a sparse mesh.

Everything is a pure function of the agent set and the config: no randomness, no map-iteration
dependence, so N controller replicas agree on the plan and a re-run after a restart is identical.
*/
package meshplan

import (
	"hash/fnv"
	"sort"
	"strings"

	"github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

// Plan maps an agent ID to the sorted IDs of the peers it probes. A nil Plan means full mesh —
// consumers skip filtering entirely, which pins pre-M10 behavior byte for byte. A Plan is never
// mutated after Build returns, so it is safe to share across goroutines read-only.
type Plan map[string][]string

// Build computes the probe plan for the fleet. It returns nil — full mesh — unless the mode is
// sparse AND the fleet is not below the autoThreshold floor (fleets SMALLER than the threshold
// keep full mesh: they lose nothing by probing everyone).
func Build(agents []model.AgentInfo, cfg config.TopologyConfig) Plan {
	if !strings.EqualFold(cfg.Mode, config.TopologyModeSparse) {
		return nil
	}
	n := len(agents)
	if cfg.Sparse.AutoThreshold > 0 && n < cfg.Sparse.AutoThreshold {
		return nil
	}

	// Node-name order defines the ring; the ID tiebreak keeps the order total even when two agents
	// share a node during a rolling-restart overlap.
	ring := make([]model.AgentInfo, n)
	copy(ring, agents)
	sort.Slice(ring, func(i, j int) bool {
		if ring[i].NodeName != ring[j].NodeName {
			return ring[i].NodeName < ring[j].NodeName
		}
		return ring[i].ID < ring[j].ID
	})

	ringDeg := cfg.Sparse.RingDegree
	if ringDeg > n-1 {
		ringDeg = n - 1 // more successors than peers just means "all of them"
	}

	adj := make(map[string]map[string]struct{}, n)
	for i := range ring {
		adj[ring[i].ID] = make(map[string]struct{}, ringDeg+cfg.Sparse.ZoneChords)
	}

	for i := range ring {
		for d := 1; d <= ringDeg; d++ {
			adj[ring[i].ID][ring[(i+d)%n].ID] = struct{}{}
		}
	}

	// One FNV hash per ID, mixed per pair below: hashing every (agent, candidate) string pair from
	// scratch made Build O(N^2) full hash computations, which is the dominant cost at N=1000.
	idHash := make([]uint64, n)
	for i := range ring {
		idHash[i] = fnvHashParts(ring[i].ID)
	}

	addChords(ring, idHash, adj, ringDeg, cfg.Sparse.ZoneChords)
	repairZonePairs(ring, idHash, adj)

	plan := make(Plan, n)
	for id, set := range adj {
		peers := make([]string, 0, len(set))
		for p := range set {
			peers = append(peers, p)
		}
		// Sorted output is part of the determinism contract: the map sets above are
		// iteration-order-free only because this is the last word on ordering.
		sort.Strings(peers)
		plan[id] = peers
	}
	return plan
}

// scored is one chord candidate: its ring index and its HRW weight for the agent being planned.
type scored struct {
	idx   int
	score uint64
}

// outranks reports whether a beats b: higher weight, ties broken by the (deterministic) ring
// order so equal scores cannot make two replicas disagree.
func (a scored) outranks(b scored) bool {
	if a.score != b.score {
		return a.score > b.score
	}
	return a.idx < b.idx
}

// addChords gives every agent up to zoneChords extra targets in OTHER zones, picked by HRW.
// Candidates already probed via the ring are skipped — a chord duplicating a ring edge buys no
// coverage — which is why the selection keeps ringDeg spare entries.
func addChords(ring []model.AgentInfo, idHash []uint64, adj map[string]map[string]struct{}, ringDeg, zoneChords int) {
	if zoneChords == 0 {
		return
	}
	keep := zoneChords + ringDeg
	best := make([]scored, 0, keep)
	for i := range ring {
		src := ring[i]
		best = best[:0]
		for j := range ring {
			if ring[j].Zone == src.Zone {
				continue
			}
			best = pushBest(best, scored{idx: j, score: pairScore(idHash[i], idHash[j])}, keep)
		}
		added := 0
		for _, c := range best {
			if added == zoneChords {
				break
			}
			dst := ring[c.idx].ID
			if _, ok := adj[src.ID][dst]; ok {
				continue
			}
			adj[src.ID][dst] = struct{}{}
			added++
		}
	}
}

// pushBest keeps the k top-ranked candidates in best, ordered best-first. Insertion into a k-sized
// window is O(k) only when the candidate actually places, so scanning N candidates stays ~O(N).
func pushBest(best []scored, c scored, k int) []scored {
	switch {
	case len(best) < k:
		best = append(best, c)
	case c.outranks(best[len(best)-1]):
		best[len(best)-1] = c
	default:
		return best
	}
	for i := len(best) - 1; i > 0 && best[i].outranks(best[i-1]); i-- {
		best[i-1], best[i] = best[i], best[i-1]
	}
	return best
}

/*
repairZonePairs adds one probe for every ordered zone pair the ring and chords left unobserved.

HRW chords are per-agent, so nothing guarantees SOME agent in zone A picked a target in zone B —
a one-agent zone with few chords provably cannot reach every other zone. Zone-level alerting
(PairWentSilent and the zonal aggregates) reasons in ordered zone pairs, so an uncovered pair is a
monitoring blind spot, not a rounding error. The source and target of the repair probe are picked
by HRW keyed on the zone pair, which spreads repair load across agents and keeps replicas agreed.
*/
func repairZonePairs(ring []model.AgentInfo, idHash []uint64, adj map[string]map[string]struct{}) {
	zoneIdx := make(map[string][]int)
	for i := range ring {
		zoneIdx[ring[i].Zone] = append(zoneIdx[ring[i].Zone], i)
	}
	if len(zoneIdx) < 2 {
		return
	}
	zones := make([]string, 0, len(zoneIdx))
	for z := range zoneIdx {
		zones = append(zones, z)
	}
	sort.Strings(zones)

	// Coverage is a set, so building it from map iteration is order-independent.
	zoneOf := make(map[string]string, len(ring))
	for i := range ring {
		zoneOf[ring[i].ID] = ring[i].Zone
	}
	covered := make(map[[2]string]struct{})
	for src, peers := range adj {
		for dst := range peers {
			covered[[2]string{zoneOf[src], zoneOf[dst]}] = struct{}{}
		}
	}

	for _, z1 := range zones {
		for _, z2 := range zones {
			if z1 == z2 {
				continue
			}
			if _, ok := covered[[2]string{z1, z2}]; ok {
				continue
			}
			salt := fnvHashParts("zone-pair-repair", z1, z2)
			src := ring[argmaxByScore(zoneIdx[z1], idHash, salt)].ID
			dst := ring[argmaxByScore(zoneIdx[z2], idHash, salt)].ID
			adj[src][dst] = struct{}{}
		}
	}
}

// argmaxByScore picks the HRW winner among the given ring indices for the salt.
func argmaxByScore(indices []int, idHash []uint64, salt uint64) int {
	winner := scored{idx: -1}
	for _, i := range indices {
		if c := (scored{idx: i, score: pairScore(salt, idHash[i])}); winner.idx == -1 || c.outranks(winner) {
			winner = c
		}
	}
	return winner.idx
}

// fnvHashParts hashes the parts as one FNV-1a stream with NUL separators, so ("ab","c") and
// ("a","bc") cannot collide by concatenation.
func fnvHashParts(parts ...string) uint64 {
	h := fnv.New64a()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return h.Sum64()
}

// pairScore is the HRW weight of the ordered pair: a splitmix64-style avalanche over an
// asymmetric mix of the two per-ID hashes. Asymmetric so a->b and b->a rank independently; a pure
// function of the inputs so every replica ranks candidates identically.
func pairScore(src, dst uint64) uint64 {
	x := src ^ (dst * 0x9E3779B97F4A7C15)
	x ^= x >> 30
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 27
	x *= 0x94D049BB133111EB
	x ^= x >> 31
	return x
}

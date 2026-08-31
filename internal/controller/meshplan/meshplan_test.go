package meshplan

import (
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"testing"

	"github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

func sparseCfg(ringDegree, zoneChords, autoThreshold int) config.TopologyConfig {
	return config.TopologyConfig{
		Mode: config.TopologyModeSparse,
		Sparse: config.SparseTopologyConfig{
			RingDegree:    ringDegree,
			ZoneChords:    zoneChords,
			AutoThreshold: autoThreshold,
		},
	}
}

func agent(id, node, zone string) model.AgentInfo {
	return model.AgentInfo{ID: id, NodeName: node, Zone: zone, PodIP: "10.0.0.1"}
}

// randomFleet builds n agents spread over zoneCount zones. IDs are decoupled from the node-name
// order, and one zone is sometimes the empty string, because agents without a resolvable zone are
// a real fleet shape, not an error.
func randomFleet(rng *rand.Rand, n, zoneCount int) []model.AgentInfo {
	zones := make([]string, zoneCount)
	for z := range zones {
		zones[z] = fmt.Sprintf("zone-%d", z)
	}
	if zoneCount > 1 && rng.Intn(4) == 0 {
		zones[0] = ""
	}
	agents := make([]model.AgentInfo, 0, n)
	for i := 0; i < n; i++ {
		agents = append(agents, model.AgentInfo{
			// Random infix decorrelates the ID order from the node order.
			ID:       fmt.Sprintf("id-%08x-%d", rng.Uint32(), i),
			NodeName: fmt.Sprintf("node-%08x-%d", rng.Uint32(), i),
			Zone:     zones[rng.Intn(zoneCount)],
			PodIP:    "10.0.0.1",
		})
	}
	return agents
}

func TestBuildFullModeReturnsNil(t *testing.T) {
	agents := []model.AgentInfo{agent("a", "n1", "z1"), agent("b", "n2", "z2")}
	for _, mode := range []string{"", "full", "Full"} {
		cfg := config.TopologyConfig{Mode: mode}
		if p := Build(agents, cfg); p != nil {
			t.Errorf("mode %q: Build returned a plan, want nil (full mesh)", mode)
		}
	}
}

func TestBuildBelowAutoThresholdReturnsNil(t *testing.T) {
	agents := []model.AgentInfo{agent("a", "n1", "z1"), agent("b", "n2", "z1"), agent("c", "n3", "z2")}
	// Threshold ABOVE the fleet size: full mesh wins.
	if p := Build(agents, sparseCfg(2, 2, len(agents)+1)); p != nil {
		t.Fatalf("N < autoThreshold must degrade to full mesh (nil), got %v", p)
	}
	// "Smaller than" is strict: N == autoThreshold plans sparse.
	if p := Build(agents, sparseCfg(2, 2, len(agents))); p == nil {
		t.Fatal("N == autoThreshold must stay sparse, got nil")
	}
}

// TestBuildRingSuccessors pins the ring shape on a hand-checked fleet: node-name order decides the
// ring, IDs do not, and each agent probes exactly its ringDegree successors when chords are off.
func TestBuildRingSuccessors(t *testing.T) {
	agents := []model.AgentInfo{
		agent("id-d", "node-3", "z1"),
		agent("id-a", "node-1", "z1"),
		agent("id-c", "node-4", "z1"),
		agent("id-b", "node-2", "z1"),
	}
	// Ring by node name: node-1(id-a) -> node-2(id-b) -> node-3(id-d) -> node-4(id-c) -> wrap.
	want := Plan{
		"id-a": {"id-b", "id-d"},
		"id-b": {"id-c", "id-d"},
		"id-d": {"id-a", "id-c"},
		"id-c": {"id-a", "id-b"},
	}
	got := Build(agents, sparseCfg(2, 0, 0))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ring plan mismatch:\n got %v\nwant %v", got, want)
	}
}

func TestBuildSingleAgentHasEmptyEntry(t *testing.T) {
	got := Build([]model.AgentInfo{agent("only", "n1", "z1")}, sparseCfg(2, 2, 0))
	if got == nil {
		t.Fatal("sparse plan for one agent must not be nil (nil means full mesh)")
	}
	peers, ok := got["only"]
	if !ok || len(peers) != 0 {
		t.Fatalf("single agent must have an entry with no peers, got %v", got)
	}
}

func TestBuildRingDegreeLargerThanFleetProbesEveryone(t *testing.T) {
	agents := []model.AgentInfo{agent("a", "n1", "z1"), agent("b", "n2", "z1"), agent("c", "n3", "z1")}
	got := Build(agents, sparseCfg(10, 0, 0))
	for _, a := range agents {
		if len(got[a.ID]) != len(agents)-1 {
			t.Fatalf("ringDegree >= N-1 must probe all others; %s probes %v", a.ID, got[a.ID])
		}
	}
}

// TestBuildNonRingEdgesAreCrossZone pins the chord restriction: everything the plan adds beyond
// the ring successors targets OTHER zones only (both HRW chords and the coverage repair edges).
func TestBuildNonRingEdgesAreCrossZone(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	agents := randomFleet(rng, 60, 4)
	ringOnly := Build(agents, sparseCfg(1, 0, 0))
	full := Build(agents, sparseCfg(1, 2, 0))

	zoneOf := map[string]string{}
	for _, a := range agents {
		zoneOf[a.ID] = a.Zone
	}
	for src, peers := range full {
		ring := map[string]bool{}
		for _, p := range ringOnly[src] {
			ring[p] = true
		}
		for _, dst := range peers {
			if ring[dst] {
				continue
			}
			if zoneOf[src] == zoneOf[dst] {
				t.Fatalf("non-ring edge %s -> %s stays inside zone %q", src, dst, zoneOf[src])
			}
		}
	}
}

/*
TestPlanProperties is the roadmap's property suite over random fleets (sizes 3..500, 1..8 zones,
seeded): (a) the directed probe graph is strongly connected; (b) every agent is probed by at least
one other; (c) every ordered zone pair with agents on both sides has at least one planned probe;
(d) the plan is a pure function of the agent SET — permuting the input changes nothing; (e) a
threshold above the fleet size degrades the plan to full mesh (nil).
*/
func TestPlanProperties(t *testing.T) {
	for seed := int64(0); seed < 40; seed++ {
		rng := rand.New(rand.NewSource(seed))
		n := 3 + rng.Intn(498)
		zoneCount := 1 + rng.Intn(8)
		ringDegree := 1 + rng.Intn(3)
		zoneChords := rng.Intn(3)
		agents := randomFleet(rng, n, zoneCount)
		cfg := sparseCfg(ringDegree, zoneChords, 0)

		plan := Build(agents, cfg)
		if plan == nil {
			t.Fatalf("seed %d: sparse plan is nil", seed)
		}
		name := fmt.Sprintf("seed=%d n=%d zones=%d ring=%d chords=%d", seed, n, zoneCount, ringDegree, zoneChords)

		assertPlanIsWellFormed(t, name, agents, plan)
		assertStronglyConnected(t, name, agents, plan)
		assertEveryoneIsProbed(t, name, agents, plan)
		assertZonePairsCovered(t, name, agents, plan)

		// (d) determinism: same agent set in another order is the same plan, entry for entry.
		shuffled := make([]model.AgentInfo, len(agents))
		copy(shuffled, agents)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		if again := Build(shuffled, cfg); !reflect.DeepEqual(plan, again) {
			t.Fatalf("%s: plan differs after permuting the input", name)
		}

		// (e) the auto-threshold floor.
		if p := Build(agents, sparseCfg(ringDegree, zoneChords, n+1)); p != nil {
			t.Fatalf("%s: N < autoThreshold must return nil", name)
		}
	}
}

// assertPlanIsWellFormed: every agent has an entry, every target exists, no self-probes, no
// duplicates, and each list is sorted (the sort is part of the determinism contract).
func assertPlanIsWellFormed(t *testing.T, name string, agents []model.AgentInfo, plan Plan) {
	t.Helper()
	known := map[string]bool{}
	for _, a := range agents {
		known[a.ID] = true
	}
	if len(plan) != len(agents) {
		t.Fatalf("%s: plan has %d entries for %d agents", name, len(plan), len(agents))
	}
	for src, peers := range plan {
		if !known[src] {
			t.Fatalf("%s: plan entry for unknown agent %s", name, src)
		}
		if !sort.StringsAreSorted(peers) {
			t.Fatalf("%s: peers of %s are not sorted: %v", name, src, peers)
		}
		seen := map[string]bool{}
		for _, dst := range peers {
			if dst == src {
				t.Fatalf("%s: %s probes itself", name, src)
			}
			if !known[dst] {
				t.Fatalf("%s: %s probes unknown agent %s", name, src, dst)
			}
			if seen[dst] {
				t.Fatalf("%s: %s probes %s twice", name, src, dst)
			}
			seen[dst] = true
		}
	}
}

// assertStronglyConnected checks reachability in both edge directions from one agent, which for a
// directed graph is equivalent to strong connectivity.
func assertStronglyConnected(t *testing.T, name string, agents []model.AgentInfo, plan Plan) {
	t.Helper()
	if len(agents) == 0 {
		return
	}
	reverse := map[string][]string{}
	for src, peers := range plan {
		for _, dst := range peers {
			reverse[dst] = append(reverse[dst], src)
		}
	}
	forwardFrom := func(start string, adj func(string) []string) int {
		visited := map[string]bool{start: true}
		queue := []string{start}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			for _, next := range adj(cur) {
				if !visited[next] {
					visited[next] = true
					queue = append(queue, next)
				}
			}
		}
		return len(visited)
	}
	start := agents[0].ID
	if got := forwardFrom(start, func(id string) []string { return plan[id] }); got != len(agents) {
		t.Fatalf("%s: only %d of %d agents reachable from %s", name, got, len(agents), start)
	}
	if got := forwardFrom(start, func(id string) []string { return reverse[id] }); got != len(agents) {
		t.Fatalf("%s: only %d of %d agents reach %s", name, got, len(agents), start)
	}
}

func assertEveryoneIsProbed(t *testing.T, name string, agents []model.AgentInfo, plan Plan) {
	t.Helper()
	if len(agents) < 2 {
		return
	}
	probed := map[string]bool{}
	for _, peers := range plan {
		for _, dst := range peers {
			probed[dst] = true
		}
	}
	for _, a := range agents {
		if !probed[a.ID] {
			t.Fatalf("%s: nobody probes %s", name, a.ID)
		}
	}
}

func assertZonePairsCovered(t *testing.T, name string, agents []model.AgentInfo, plan Plan) {
	t.Helper()
	zoneOf := map[string]string{}
	zones := map[string]bool{}
	for _, a := range agents {
		zoneOf[a.ID] = a.Zone
		zones[a.Zone] = true
	}
	covered := map[[2]string]bool{}
	for src, peers := range plan {
		for _, dst := range peers {
			covered[[2]string{zoneOf[src], zoneOf[dst]}] = true
		}
	}
	for z1 := range zones {
		for z2 := range zones {
			if z1 == z2 {
				continue
			}
			if !covered[[2]string{z1, z2}] {
				t.Fatalf("%s: ordered zone pair (%q -> %q) has no planned probe", name, z1, z2)
			}
		}
	}
}

// BenchmarkBuild1000 is the runtime the roadmap notes ask for: one plan over a 1000-node fleet.
func BenchmarkBuild1000(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	agents := randomFleet(rng, 1000, 6)
	cfg := sparseCfg(2, 2, 0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if Build(agents, cfg) == nil {
			b.Fatal("nil plan")
		}
	}
}

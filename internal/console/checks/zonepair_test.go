package checks_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/checks"
	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/ws"
)

// names returns n distinct node names with a stable sort order.
func names(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s-%03d", prefix, i)
	}
	return out
}

/*
 * The chunker's whole contract as properties, over random shapes. What matters is not any one
 * example but that NO shape can produce a chunk a run would refuse (raw product over the 400-pair
 * bound), lose or duplicate a source (the disjointness is what keeps parallel preset runs inside
 * the per-agent dispatch budget), or exceed the preset's own run cap.
 */
func TestChunkZonePairSourcesProperties(t *testing.T) {
	rng := rand.New(rand.NewSource(1)) //nolint:gosec // deterministic property-test shapes, not crypto
	for i := 0; i < 500; i++ {
		nSrc := 1 + rng.Intn(60)
		nDst := 1 + rng.Intn(500)
		sources := names("s", nSrc)
		destinations := names("d", nDst)

		chunks, err := checks.ChunkZonePairSources(sources, destinations)
		if err != nil {
			if !errors.Is(err, checks.ErrZonePairTooLarge) {
				t.Fatalf("S=%d D=%d: unexpected error class: %v", nSrc, nDst, err)
			}
			continue
		}

		if len(chunks) > checks.PresetMaxRuns {
			t.Fatalf("S=%d D=%d: %d chunks, cap %d", nSrc, nDst, len(chunks), checks.PresetMaxRuns)
		}
		var rebuilt []string
		for _, c := range chunks {
			if len(c) == 0 {
				t.Fatalf("S=%d D=%d: empty chunk", nSrc, nDst)
			}
			if len(c)*nDst > checks.MaxPairsPerRun {
				t.Fatalf("S=%d D=%d: chunk of %d sources gives raw product %d > %d",
					nSrc, nDst, len(c), len(c)*nDst, checks.MaxPairsPerRun)
			}
			rebuilt = append(rebuilt, c...)
		}
		// Concatenation equality covers order, coverage, disjointness and duplicates all at once.
		if len(rebuilt) != nSrc {
			t.Fatalf("S=%d D=%d: chunks rebuild %d sources", nSrc, nDst, len(rebuilt))
		}
		for j := range rebuilt {
			if rebuilt[j] != sources[j] {
				t.Fatalf("S=%d D=%d: source %d is %q, want %q", nSrc, nDst, j, rebuilt[j], sources[j])
			}
		}
	}
}

func TestChunkZonePairSourcesBounds(t *testing.T) {
	// A destination side wider than one run's pair bound cannot be chunked without stacking
	// parallel runs on one source agent, so it is refused, not split.
	if _, err := checks.ChunkZonePairSources(names("s", 1), names("d", checks.MaxPairsPerRun+1)); !errors.Is(err, checks.ErrZonePairTooLarge) {
		t.Fatalf("D=%d: err = %v, want ErrZonePairTooLarge", checks.MaxPairsPerRun+1, err)
	}
	// One source per chunk at D=400: the 9th source needs a 9th run, over the preset cap of 8.
	if _, err := checks.ChunkZonePairSources(names("s", checks.PresetMaxRuns+1), names("d", checks.MaxPairsPerRun)); !errors.Is(err, checks.ErrZonePairTooLarge) {
		t.Fatalf("S=%d D=%d: err = %v, want ErrZonePairTooLarge", checks.PresetMaxRuns+1, checks.MaxPairsPerRun, err)
	}
	// The degenerate empty sides are ErrNoNodes: "the zone has no agents", not "too large".
	if _, err := checks.ChunkZonePairSources(nil, names("d", 1)); !errors.Is(err, checks.ErrNoNodes) {
		t.Fatalf("empty sources: err = %v, want ErrNoNodes", err)
	}
	if _, err := checks.ChunkZonePairSources(names("s", 1), nil); !errors.Is(err, checks.ErrNoNodes) {
		t.Fatalf("empty destinations: err = %v, want ErrNoNodes", err)
	}

	// A whole small mesh fits one run: one chunk, all sources.
	chunks, err := checks.ChunkZonePairSources(names("s", 20), names("d", 20))
	if err != nil {
		t.Fatalf("S=20 D=20: %v", err)
	}
	if len(chunks) != 1 || len(chunks[0]) != 20 {
		t.Fatalf("S=20 D=20: chunks = %d (first %d sources), want 1 chunk of 20", len(chunks), len(chunks[0]))
	}
}

// zonedAgents registers one agent per node with the given zone assignment on the fake controller.
func zonedAgents(fake *fakeDiagnosticsServer, zones map[string][]string) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.agents = nil
	for zone, nodes := range zones {
		for _, n := range nodes {
			fake.agents = append(fake.agents, controllerclient.Agent{ID: n + "-agent", NodeName: n, PodIP: "10.0.0.1", Zone: zone})
		}
	}
}

func newZonePairRunner(t *testing.T, ctrl *controllerclient.Client) (*checks.Runner, *checks.MemoryStore) {
	t.Helper()
	bus := cache.NewInProcessBus()
	hub := ws.NewHub(bus, testMetrics(t))
	mem := checks.NewMemoryStore()
	runner := checks.NewRunner(ctrl, hub, bus, mem, testMetrics(t))
	t.Cleanup(func() {
		runner.CancelAll()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		runner.Wait(ctx)
	})
	return runner, mem
}

func TestStartZonePairExpandsZonesAndStartsOneRunWhenItFits(t *testing.T) {
	fake, ctrl := startFakeDiagnosticsServer(t)
	zonedAgents(fake, map[string][]string{
		"zone-a": {"a1", "a2", "a3"},
		"zone-b": {"b1", "b2"},
	})
	runner, mem := newZonePairRunner(t, ctrl)

	runs, err := runner.StartZonePair(context.Background(), checks.ZonePairSpec{
		SourceZone: "zone-a", DestinationZone: "zone-b", Type: "tcp",
	}, authz.Subject{Kind: authz.SubjectUser, ID: "op"})
	if err != nil {
		t.Fatalf("StartZonePair: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	if runs[0].PairTotal != 6 {
		t.Errorf("pairTotal = %d, want 6 (3 sources x 2 destinations)", runs[0].PairTotal)
	}
	run, err := mem.GetRun(context.Background(), runs[0].ID)
	if err != nil {
		t.Fatalf("run %s not persisted: %v", runs[0].ID, err)
	}
	if run.PairTotal != 6 {
		t.Errorf("persisted pairTotal = %d, want 6", run.PairTotal)
	}
}

func TestStartZonePairChunksALargePairIntoDisjointSourceRuns(t *testing.T) {
	// 3 sources x 200 destinations = 600 raw pairs: over one run's bound, so the preset must split
	// the SOURCES (2 per run at this width) and never a destination list.
	fake, ctrl := startFakeDiagnosticsServer(t)
	zonedAgents(fake, map[string][]string{
		"zone-a": names("src", 3),
		"zone-b": names("dst", 200),
	})
	runner, mem := newZonePairRunner(t, ctrl)

	runs, err := runner.StartZonePair(context.Background(), checks.ZonePairSpec{
		SourceZone: "zone-a", DestinationZone: "zone-b", Type: "tcp",
	}, authz.Subject{Kind: authz.SubjectUser, ID: "op"})
	if err != nil {
		t.Fatalf("StartZonePair: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(runs))
	}
	total := 0
	for _, zr := range runs {
		total += zr.PairTotal
		if zr.PairTotal > checks.MaxPairsPerRun {
			t.Errorf("run %s pairTotal %d > %d", zr.ID, zr.PairTotal, checks.MaxPairsPerRun)
		}
		if _, gerr := mem.GetRun(context.Background(), zr.ID); gerr != nil {
			t.Errorf("run %s not persisted: %v", zr.ID, gerr)
		}
	}
	if total != 600 {
		t.Errorf("summed pairTotal = %d, want 600", total)
	}
}

func TestStartZonePairRefusesAZoneWithoutAgents(t *testing.T) {
	fake, ctrl := startFakeDiagnosticsServer(t)
	zonedAgents(fake, map[string][]string{"zone-a": {"a1"}})
	runner, _ := newZonePairRunner(t, ctrl)

	_, err := runner.StartZonePair(context.Background(), checks.ZonePairSpec{
		SourceZone: "zone-a", DestinationZone: "zone-ghost", Type: "tcp",
	}, authz.Subject{Kind: authz.SubjectUser, ID: "op"})
	if !errors.Is(err, checks.ErrUnknownZone) {
		t.Fatalf("err = %v, want ErrUnknownZone", err)
	}
}

func TestStartZonePairRefusesAnUnknownCheckTypeBeforeStartingAnything(t *testing.T) {
	fake, ctrl := startFakeDiagnosticsServer(t)
	zonedAgents(fake, map[string][]string{"zone-a": {"a1"}, "zone-b": {"b1"}})
	runner, mem := newZonePairRunner(t, ctrl)

	runs, err := runner.StartZonePair(context.Background(), checks.ZonePairSpec{
		SourceZone: "zone-a", DestinationZone: "zone-b", Type: "carrier-pigeon",
	}, authz.Subject{Kind: authz.SubjectUser, ID: "op"})
	if !errors.Is(err, checks.ErrUnknownType) {
		t.Fatalf("err = %v, want ErrUnknownType", err)
	}
	if len(runs) != 0 {
		t.Errorf("%d runs started despite the refused type", len(runs))
	}
	page, err := mem.ListRuns(context.Background(), checks.ListFilter{})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(page.Runs) != 0 {
		t.Errorf("%d runs persisted despite the refused type", len(page.Runs))
	}
}

// The same zone on both sides is a legitimate scope (intra-zone weather); Plan drops the
// self-pairs, so 3 nodes give 6 directed pairs, not 9.
func TestStartZonePairSameZoneDropsSelfPairs(t *testing.T) {
	fake, ctrl := startFakeDiagnosticsServer(t)
	zonedAgents(fake, map[string][]string{"zone-a": {"a1", "a2", "a3"}})
	runner, _ := newZonePairRunner(t, ctrl)

	runs, err := runner.StartZonePair(context.Background(), checks.ZonePairSpec{
		SourceZone: "zone-a", DestinationZone: "zone-a", Type: "tcp",
	}, authz.Subject{Kind: authz.SubjectUser, ID: "op"})
	if err != nil {
		t.Fatalf("StartZonePair: %v", err)
	}
	if len(runs) != 1 || runs[0].PairTotal != 6 {
		t.Fatalf("runs = %+v, want one run of 6 pairs", runs)
	}
}

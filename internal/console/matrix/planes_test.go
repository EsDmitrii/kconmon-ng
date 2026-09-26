package matrix_test

import (
	"slices"
	"testing"

	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/matrix"
)

func agent(node string, caps ...string) controllerclient.Agent {
	return controllerclient.Agent{ID: node, NodeName: node, Capabilities: caps}
}

func failCell(src, dst string) matrix.Cell {
	f := 0.0
	return matrix.Cell{Source: src, Destination: dst, FailRatio: &f}
}

func TestPlaneRunnersFailsOpen(t *testing.T) {
	runs := matrix.PlaneRunners([]controllerclient.Agent{
		agent("a", "plane:tcp", "plane:icmp"),
		agent("b", "plane:tcp"),
		agent("c"), // lists no planes: predates plane capabilities
		agent("d", "plane:tcp"),
		agent("d", "plane:icmp"), // a second agent on d runs it
	}, "icmp")
	want := map[string]bool{"a": true, "b": false, "c": true, "d": true}
	for node, v := range want {
		if runs[node] != v {
			t.Errorf("runs[%s] = %v, want %v", node, runs[node], v)
		}
	}
}

// A source that stopped advertising the plane keeps its counters for the whole rate window; those
// cells are stale green, not a measurement, so they go, and the row stays for the "not run" reading.
func TestRestrictToPlaneDropsCellsOfSourcesThatNoLongerRunIt(t *testing.T) {
	m := &matrix.Matrix{Protocol: "icmp", Nodes: []string{"a", "b"}, Cells: []matrix.Cell{failCell("a", "b"), failCell("b", "a")}}
	got := matrix.RestrictToPlane(m, map[string]bool{"a": true, "b": false})
	if !slices.Equal(got.Nodes, []string{"a", "b"}) {
		t.Errorf("nodes = %v, want both rows kept", got.Nodes)
	}
	if len(got.Cells) != 1 || got.Cells[0].Source != "a" {
		t.Errorf("cells = %+v, want only a->b", got.Cells)
	}
	if len(m.Cells) != 2 {
		t.Error("RestrictToPlane modified its input, which the matrix cache shares")
	}
}

// A plane switched off fleet-wide leaves no series at all: the nodes come from the topology, so the
// matrix shows every row as not run instead of "no probe data yet".
func TestRestrictToPlaneNamesTheFleetWhenNobodyRunsIt(t *testing.T) {
	m := &matrix.Matrix{Protocol: "icmp", Nodes: []string{}, Cells: []matrix.Cell{}}
	got := matrix.RestrictToPlane(m, map[string]bool{"b": false, "a": false})
	if !slices.Equal(got.Nodes, []string{"a", "b"}) {
		t.Errorf("nodes = %v, want the fleet", got.Nodes)
	}
	// Some node running it means the empty matrix is genuinely "no data yet".
	got = matrix.RestrictToPlane(m, map[string]bool{"a": true, "b": false})
	if len(got.Nodes) != 0 {
		t.Errorf("nodes = %v, want none while a runs the plane", got.Nodes)
	}
	if got = matrix.RestrictToPlane(m, nil); len(got.Nodes) != 0 {
		t.Errorf("nodes = %v, want none without a topology", got.Nodes)
	}
}

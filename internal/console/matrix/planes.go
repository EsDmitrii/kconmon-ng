package matrix

import (
	"slices"
	"strings"

	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

// PlaneRunners reads the agents' advertised planes for protocol, by node: true where some agent on
// the node runs it or lists no planes at all (an agent older than plane capabilities, read as all
// planes), false where every agent on it lists its planes and leaves protocol out.
func PlaneRunners(agents []controllerclient.Agent, protocol string) map[string]bool {
	out := make(map[string]bool, len(agents))
	for i := range agents {
		node := agents[i].NodeName
		if node == "" {
			continue
		}
		out[node] = out[node] || runsPlane(agents[i].Capabilities, protocol)
	}
	return out
}

func runsPlane(caps []string, protocol string) bool {
	listed := false
	for _, c := range caps {
		if c == model.CapabilityPlanePrefix+protocol {
			return true
		}
		if strings.HasPrefix(c, model.CapabilityPlanePrefix) {
			listed = true
		}
	}
	return !listed
}

// RestrictToPlane returns a copy of m without the cells of sources that no longer run the protocol:
// their counters outlive the switch-off by the whole rate window and read as healthy. When no series
// is left and no node runs the protocol, the nodes are the fleet's, so the grid reads "not run"
// rather than "no data yet". A nil runs (no topology) returns m's content unchanged.
func RestrictToPlane(m *Matrix, runs map[string]bool) *Matrix {
	out := *m
	out.Cells = make([]Cell, 0, len(m.Cells))
	for _, c := range m.Cells {
		if running, known := runs[c.Source]; known && !running {
			continue
		}
		out.Cells = append(out.Cells, c)
	}
	out.Nodes = slices.Clone(m.Nodes)
	if len(out.Nodes) == 0 && len(runs) > 0 {
		fleet := make([]string, 0, len(runs))
		for node, running := range runs {
			if running {
				return &out
			}
			fleet = append(fleet, node)
		}
		slices.Sort(fleet)
		out.Nodes = fleet
	}
	return &out
}

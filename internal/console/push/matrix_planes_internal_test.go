package push

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/prometheus/client_golang/prometheus"
)

type staleICMP struct{}

func (staleICMP) Query(_ context.Context, query string, _ time.Time) (json.RawMessage, error) {
	if strings.Contains(query, "_results_total") {
		return json.RawMessage(`{"status":"success","data":{"resultType":"vector","result":[` +
			`{"metric":{"source_node":"a","destination_node":"b"},"value":[1767225600,"0"]}]}}`), nil
	}
	return json.RawMessage(`{"status":"success","data":{"resultType":"vector","result":[]}}`), nil
}

type planeTopology struct {
	agents []controllerclient.Agent
	err    error
}

func (p planeTopology) Topology(context.Context) (*controllerclient.Topology, error) {
	if p.err != nil {
		return nil, p.err
	}
	return &controllerclient.Topology{Agents: p.agents}, nil
}

// The pushed snapshot must agree with GET /api/v1/matrix: a source that stopped advertising the
// plane pushes no stale cells either.
func TestMatrixPusherFollowsTheAdvertisedPlanes(t *testing.T) {
	m := metrics.New("kconmon_ng", prometheus.NewRegistry())
	p := NewMatrixPusher(staleICMP{}, nil, "kconmon_ng", time.Hour, m)
	p.SetTopology(planeTopology{agents: []controllerclient.Agent{
		{ID: "a", NodeName: "a", Capabilities: []string{"plane:tcp"}},
		{ID: "b", NodeName: "b", Capabilities: []string{"plane:tcp"}},
	}})

	agents := p.advertisedAgents(context.Background())
	mx, err := p.compute(context.Background(), "icmp", agents)
	if err != nil {
		t.Fatal(err)
	}
	if len(mx.Cells) != 0 || len(mx.Nodes) != 2 {
		t.Errorf("icmp snapshot = nodes %v, cells %+v; want both nodes and no stale cell", mx.Nodes, mx.Cells)
	}

	// An unreadable topology leaves the snapshot as Prometheus has it.
	p.SetTopology(planeTopology{err: errors.New("controller unavailable")})
	mx, err = p.compute(context.Background(), "icmp", p.advertisedAgents(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	if len(mx.Cells) != 1 {
		t.Errorf("icmp snapshot without a topology has %d cells, want the measured one", len(mx.Cells))
	}
}

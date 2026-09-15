package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
)

// baselineInterval is how often a connected ingester records the controller's whole topology again.
// Retention prunes rows by age, so a baseline written only when the stream came up would age out
// under a console that stays connected longer than database.retentionDays.
const baselineInterval = time.Hour

// topologyBaselineDetails is the details JSON of a topology_baseline row. The store's fold mirrors
// it by hand (store.topologyBaselineDetails); PodIP and capabilities stay out, as they do from every
// topology_changed row.
type topologyBaselineDetails struct {
	Nodes  []baselineNode  `json:"nodes"`
	Agents []baselineAgent `json:"agents"`
}

type baselineNode struct {
	Name  string `json:"name"`
	Zone  string `json:"zone"`
	Ready bool   `json:"ready"`
}

type baselineAgent struct {
	ID       string            `json:"id"`
	NodeName string            `json:"nodeName"`
	Zone     string            `json:"zone"`
	Labels   map[string]string `json:"labels,omitempty"`
}

// topologyBaselineEvent turns a controller topology snapshot into the row the sink stores. It is
// stamped with the controller's own snapshot time, the clock every event row already carries; now
// stands in only for a controller that sent none.
func topologyBaselineEvent(topo *controllerclient.Topology, now time.Time) (LiveEvent, error) {
	details := topologyBaselineDetails{
		Nodes:  make([]baselineNode, 0, len(topo.Nodes)),
		Agents: make([]baselineAgent, 0, len(topo.Agents)),
	}
	for _, n := range topo.Nodes {
		details.Nodes = append(details.Nodes, baselineNode{Name: n.Name, Zone: n.Zone, Ready: n.Ready})
	}
	for _, a := range topo.Agents {
		details.Agents = append(details.Agents, baselineAgent{ID: a.ID, NodeName: a.NodeName, Zone: a.Zone, Labels: a.Labels})
	}
	raw, err := json.Marshal(details)
	if err != nil {
		return LiveEvent{}, fmt.Errorf("marshal topology baseline: %w", err)
	}

	ts := topo.Timestamp
	if ts.IsZero() {
		ts = now
	}
	return LiveEvent{
		ID:        fmt.Sprintf("0-%d", ts.UnixNano()),
		Type:      TypeTopologyBaseline,
		Severity:  SeverityInfo,
		Scope:     scopeCluster,
		Timestamp: ts,
		Summary:   fmt.Sprintf("topology baseline: %d nodes, %d agents", len(details.Nodes), len(details.Agents)),
		Details:   raw,
	}, nil
}

// recordBaselines writes a baseline once the attempt's stream has proven itself, then every
// baselineInterval until ctx ends. Waiting for the stream matters: a snapshot taken before the
// controller accepted the subscription would leave the changes in between in neither place.
func (i *Ingester) recordBaselines(ctx context.Context, established <-chan struct{}) {
	select {
	case <-ctx.Done():
		return
	case <-established:
	}
	ticker := time.NewTicker(i.baselineInterval)
	defer ticker.Stop()
	for {
		i.recordBaseline(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// recordBaseline fetches and persists one baseline. A failure costs the Time Machine this one
// snapshot and is logged; the stream is never touched.
func (i *Ingester) recordBaseline(ctx context.Context) {
	topo, err := i.ctrl.Topology(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("could not record a topology baseline for the Time Machine", "error", err)
		}
		return
	}
	live, err := topologyBaselineEvent(topo, time.Now())
	if err != nil {
		slog.Warn("could not record a topology baseline for the Time Machine", "error", err)
		return
	}
	i.persist(ctx, live)
}

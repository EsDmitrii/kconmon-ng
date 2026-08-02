package push

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/ws"
)

// TopologyPusher refetches the controller's authoritative topology snapshot and
// broadcasts it on the "topology" topic.
type TopologyPusher struct {
	ctrl     *controllerclient.Client
	hub      *ws.Hub
	interval time.Duration
	metrics  *metrics.Metrics
	nudgeCh  chan struct{}
}

// NewTopologyPusher returns a pusher over ctrl. A non-positive interval becomes
// defaultInterval.
func NewTopologyPusher(ctrl *controllerclient.Client, hub *ws.Hub, interval time.Duration, m *metrics.Metrics) *TopologyPusher {
	if interval <= 0 {
		interval = defaultInterval
	}
	return &TopologyPusher{
		ctrl:     ctrl,
		hub:      hub,
		interval: interval,
		metrics:  m,
		nudgeCh:  make(chan struct{}, 1),
	}
}

// Nudge asks for an out-of-band refetch. Non-blocking and coalescing.
func (p *TopologyPusher) Nudge() { signalNudge(p.nudgeCh) }

// Run pushes once immediately, then on every tick or nudge, until ctx is
// cancelled. A controller that is unreachable or has no leader is recorded and
// retried on the next tick; it never terminates the loop.
//
// Run must be called exactly once per pusher: a second concurrent Run would
// share nudgeCh and broadcast the "topology" topic concurrently, violating
// ws.Hub's one-goroutine-per-topic serialization assumption (see Hub.Broadcast).
func (p *TopologyPusher) Run(ctx context.Context) {
	p.push(ctx)

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.push(ctx)
		case <-p.nudgeCh:
			p.push(ctx)
		}
	}
}

// push always broadcasts the controller's own snapshot rather than anything
// derived from the event that triggered it: TopologyChanged is a refetch
// signal, not a payload, so the browser can never end up holding a topology the
// controller does not agree with.
func (p *TopologyPusher) push(ctx context.Context) {
	topo, err := p.ctrl.Topology(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down, not a push failure
		}
		p.metrics.PushSnapshots.WithLabelValues(ws.TopicTopology, "error").Inc()
		slog.Warn("topology snapshot push failed", "topic", ws.TopicTopology, "error", err)
		return
	}

	data, err := json.Marshal(topo)
	if err != nil {
		p.metrics.PushSnapshots.WithLabelValues(ws.TopicTopology, "error").Inc()
		slog.Warn("topology snapshot marshal failed", "topic", ws.TopicTopology, "error", err)
		return
	}

	p.hub.Broadcast(ws.TopicTopology, ws.TypeSnapshot, data)
	p.metrics.PushSnapshots.WithLabelValues(ws.TopicTopology, "ok").Inc()
}

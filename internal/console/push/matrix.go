package push

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/matrix"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/ws"
)

// defaultInterval is the fallback push period when the caller passes a
// non-positive interval. It matches the frontend's MATRIX_POLL_MS/
// TOPOLOGY_POLL_MS so the WebSocket path is never staler than polling.
const defaultInterval = 15 * time.Second

// matrixProtocols is the fixed protocol set the console pushes. It mirrors the
// protocols matrix.Compute accepts; there is no "all" mode.
var matrixProtocols = []string{"tcp", "udp", "icmp", "pmtu"}

// MatrixPusher recomputes the connectivity matrix for every protocol and
// broadcasts it as a full snapshot on the matrix:<protocol>:pod topics.
type MatrixPusher struct {
	prom          matrix.Querier
	hub           *ws.Hub
	metricsPrefix string
	interval      time.Duration
	metrics       *metrics.Metrics
	nudgeCh       chan struct{}
	// topology, when set, is read once per round for the planes the agents advertise.
	topology PlaneTopology
}

// PlaneTopology is where the pusher reads the advertised planes; *controllerclient.Client satisfies it.
type PlaneTopology interface {
	Topology(ctx context.Context) (*controllerclient.Topology, error)
}

// planeTopologyTimeout bounds that read; past it a round is pushed as Prometheus has it.
const planeTopologyTimeout = 2 * time.Second

// SetTopology makes every snapshot follow the planes the agents advertise, as GET /api/v1/matrix
// does. Call it before Run.
func (p *MatrixPusher) SetTopology(t PlaneTopology) { p.topology = t }

// NewMatrixPusher returns a pusher over prom. A non-positive interval becomes
// defaultInterval.
func NewMatrixPusher(prom matrix.Querier, hub *ws.Hub, metricsPrefix string, interval time.Duration, m *metrics.Metrics) *MatrixPusher {
	if interval <= 0 {
		interval = defaultInterval
	}
	return &MatrixPusher{
		prom:          prom,
		hub:           hub,
		metricsPrefix: metricsPrefix,
		interval:      interval,
		metrics:       m,
		nudgeCh:       make(chan struct{}, 1),
	}
}

// Nudge asks for an out-of-band recompute. Non-blocking and coalescing.
func (p *MatrixPusher) Nudge() { signalNudge(p.nudgeCh) }

// Run pushes once immediately (so a freshly connected browser is not waiting a whole interval for
// its first snapshot); a failing Prometheus is recorded and retried on the next tick.
func (p *MatrixPusher) Run(ctx context.Context) {
	p.pushAll(ctx)

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.pushAll(ctx)
		case <-p.nudgeCh:
			p.pushAll(ctx)
		}
	}
}

func (p *MatrixPusher) pushAll(ctx context.Context) {
	agents := p.advertisedAgents(ctx)
	for _, protocol := range matrixProtocols {
		p.pushOne(ctx, protocol, agents)
	}
}

// advertisedAgents is the topology's agents, or nil when there is no topology to read.
func (p *MatrixPusher) advertisedAgents(ctx context.Context) []controllerclient.Agent {
	if p.topology == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, planeTopologyTimeout)
	defer cancel()
	topo, err := p.topology.Topology(ctx)
	if err != nil {
		slog.Debug("matrix push: topology unavailable, pushing without the plane check", "error", err)
		return nil
	}
	return topo.Agents
}

// compute is matrix.Compute checked against agents' planes when there are any.
func (p *MatrixPusher) compute(ctx context.Context, protocol string, agents []controllerclient.Agent) (*matrix.Matrix, error) {
	mx, err := matrix.Compute(ctx, p.prom, p.metricsPrefix, protocol)
	if err != nil || agents == nil {
		return mx, err
	}
	return matrix.RestrictToPlane(mx, matrix.PlaneRunners(agents, protocol)), nil
}

// pushOne reuses matrix.Compute verbatim — the exact call the REST handler in
// internal/console/httpapi/data.go already makes.
func (p *MatrixPusher) pushOne(ctx context.Context, protocol string, agents []controllerclient.Agent) {
	topic := ws.MatrixTopic(protocol)

	mx, err := p.compute(ctx, protocol, agents)
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down, not a push failure
		}
		p.metrics.PushSnapshots.WithLabelValues(topic, "error").Inc()
		slog.Warn("matrix snapshot push failed", "topic", topic, "error", err)
		return
	}

	data, err := json.Marshal(mx)
	if err != nil {
		p.metrics.PushSnapshots.WithLabelValues(topic, "error").Inc()
		slog.Warn("matrix snapshot marshal failed", "topic", topic, "error", err)
		return
	}

	p.hub.Broadcast(topic, ws.TypeSnapshot, data)
	p.metrics.PushSnapshots.WithLabelValues(topic, "ok").Inc()
}

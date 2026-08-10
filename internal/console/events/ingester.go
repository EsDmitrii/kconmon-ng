package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

// busTopicLive is the cache.Bus topic the live feed travels on; it MUST equal ws.TopicLive.
const busTopicLive = "live"

// capabilityEvents is the flag the controller must advertise on
// GET /api/v1/version before this console will dial its gRPC event stream.
const capabilityEvents = "events"

// Reconnect reason labels for metrics.IngesterReconnects. Bounded set.
const (
	reasonCapability = "capability"
	reasonDial       = "dial"
	reasonStream     = "stream"
)

// Backoff bounds, identical to the agent's WatchTasks loop
// (internal/agent/agent.go): 1s doubling to a 15s ceiling.
const (
	initialBackoff = 1 * time.Second
	maxBackoff     = 15 * time.Second
)

// connectGrace is how long a stream must survive without the server hanging up before the ingester
// reports healthy on the strength of the connection alone (an event arriving sooner promotes it
// immediately).
const connectGrace = 2 * time.Second

// gRPC keepalive, identical to internal/agent/grpc_client.go's dial options —
// the console is a second long-lived streaming client against the same
// controller Service and has no reason to behave differently.
const (
	keepaliveTime    = 10 * time.Second
	keepaliveTimeout = 5 * time.Second
)

// sinkTimeout bounds one EventSink.InsertEvent call; it is independent of the stream context (which
// lives as long as the whole attempt, and dies on reconnect) and of context.Background (which would
// outlive shutdown).
const sinkTimeout = 5 * time.Second

var (
	// errNoCapability is the "the controller is not offering events" case. It is
	// deliberately handled exactly like a failed dial: back off, retry.
	errNoCapability = errors.New(`controller does not advertise the "events" capability`)
	// errNoControllerClient guards the misconfiguration where a gRPC address is
	// set but controller.url is not, so there is nothing to precheck against.
	errNoControllerClient = errors.New("controller HTTP client is not configured, cannot run the capability precheck")
)

// EventSink durably records a live event.
type EventSink interface {
	InsertEvent(ctx context.Context, ev LiveEvent) (inserted bool, err error)
}

// Option configures an Ingester. Variadic, so the eleven existing call sites
// (one in cmd/console, ten in ingester_test.go) need no edit.
type Option func(*Ingester)

// WithEventSink turns on durable persistence of every published live event.
func WithEventSink(sink EventSink) Option {
	return func(i *Ingester) { i.sink = sink }
}

// Ingester is one console replica's client for the controller's EventStream.WatchEvents stream.
type Ingester struct {
	ctrl     *controllerclient.Client
	grpcAddr string
	bus      cache.Bus
	metrics  *metrics.Metrics

	// sink is nil unless WithEventSink was passed to NewIngester.
	sink EventSink

	// connectGrace defaults to the connectGrace const. Overridable via
	// export_test.go so a test can stretch it far enough that a promotion can
	// only have come from a received event. Never changed after Run starts.
	connectGrace time.Duration

	connected atomic.Bool
}

// NewIngester returns an ingester for the controller at grpcAddr.
func NewIngester(ctrl *controllerclient.Client, grpcAddr string, bus cache.Bus, m *metrics.Metrics, opts ...Option) *Ingester {
	i := &Ingester{ctrl: ctrl, grpcAddr: grpcAddr, bus: bus, metrics: m, connectGrace: connectGrace}
	for _, opt := range opts {
		opt(i)
	}
	return i
}

// Healthy reports whether this replica currently holds a WatchEvents stream that has proven itself;
// it is the single input to the console's "events" capability flag.
func (i *Ingester) Healthy() bool { return i.connected.Load() }

// Run blocks until ctx is cancelled, reconnecting with the repo's standard 1s→15s doubling backoff
// (the shape in internal/agent/agent.go's WatchTasks loop).
func (i *Ingester) Run(ctx context.Context) {
	if i.grpcAddr == "" {
		slog.Info("realtime event ingestion disabled: no controller gRPC address configured")
		return
	}

	backoff := initialBackoff
	for {
		reason, err := i.attempt(ctx)
		if ctx.Err() != nil {
			return
		}

		i.metrics.IngesterReconnects.WithLabelValues(reason).Inc()
		switch {
		case errors.Is(err, errNoCapability):
			// Only THIS case is benign.
			slog.Info("controller is not offering realtime events yet, retrying",
				"error", err, "backoff", backoff)
		case reason == reasonCapability:
			slog.Warn("controller capability precheck failed, retrying",
				"reason", reason, "error", err, "backoff", backoff)
		default:
			slog.Warn("controller event stream disconnected, reconnecting",
				"reason", reason, "error", err, "backoff", backoff)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// attempt performs one whole cycle — capability precheck, dial, consume — and
// returns the metrics reason label to count plus the error that ended the
// attempt. It never returns a nil error: the loop above only stops on ctx.
func (i *Ingester) attempt(ctx context.Context) (string, error) {
	if err := i.precheck(ctx); err != nil {
		return reasonCapability, err
	}

	// Same dial options as internal/agent/grpc_client.go. grpc.NewClient is
	// lazy, so a wrong or unreachable address surfaces on the WatchEvents call
	// below rather than here.
	conn, err := grpc.NewClient(i.grpcAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                keepaliveTime,
			Timeout:             keepaliveTimeout,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return reasonDial, fmt.Errorf("dial controller %s: %w", i.grpcAddr, err)
	}
	defer func() { _ = conn.Close() }()

	// A Go server-streaming client returns from WatchEvents BEFORE the server has accepted the stream.
	stream, err := pb.NewEventStreamClient(conn).WatchEvents(ctx, &pb.WatchEventsRequest{})
	if err != nil {
		return reasonDial, fmt.Errorf("open WatchEvents stream on %s: %w", i.grpcAddr, err)
	}

	return reasonStream, i.consume(ctx, stream)
}

// precheck is a: feature-detect before every dial attempt, including every reconnect.
func (i *Ingester) precheck(ctx context.Context) error {
	if i.ctrl == nil {
		return errNoControllerClient
	}
	v, err := i.ctrl.Version(ctx)
	if err != nil {
		return fmt.Errorf("controller capability probe: %w", err)
	}
	if !v.HasCapability(capabilityEvents) {
		return fmt.Errorf("%w (advertised: %v)", errNoCapability, v.Capabilities)
	}
	return nil
}

// consume runs the stream to its end; it does NOT report the ingester healthy just because the
// stream object exists.
func (i *Ingester) consume(ctx context.Context, stream pb.EventStream_WatchEventsClient) error {
	gate := &connectGate{ing: i}
	defer gate.finish()

	grace := time.AfterFunc(i.connectGrace, gate.markConnected)
	defer grace.Stop()

	for {
		ev, err := stream.Recv()
		if err != nil {
			return fmt.Errorf("receive event: %w", err)
		}
		// An event in hand is the strongest possible proof of a live stream, so
		// it promotes the ingester ahead of the grace period.
		gate.markConnected()
		i.publish(ctx, ev)
	}
}

// connectGate owns the connected state of ONE attempt.
type connectGate struct {
	ing *Ingester

	mu       sync.Mutex
	finished bool // the attempt is over; no further promotion is permitted
	up       bool // this attempt promoted the ingester and owes a demotion
}

// markConnected promotes the ingester at most once per attempt, and never after finish.
func (g *connectGate) markConnected() {
	if !g.promote() {
		return
	}
	slog.Info("controller event stream established", "address", g.ing.grpcAddr)
}

// promote reports whether this call is the one that flipped the attempt to
// connected.
func (g *connectGate) promote() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.finished || g.up {
		return false
	}
	g.up = true
	g.ing.setConnected(true)
	return true
}

// finish closes the attempt out, demoting the ingester if this attempt had
// promoted it. After it returns, Healthy() is false and no late timer can
// change that.
func (g *connectGate) finish() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.finished = true
	if g.up {
		g.up = false
		g.ing.setConnected(false)
	}
}

// setConnected keeps Healthy and the ingester_connected gauge in step; the gauge is written BEFORE
// the flag on purpose.
func (i *Ingester) setConnected(up bool) {
	value := 0.0
	if up {
		value = 1
	}
	i.metrics.IngesterConnected.WithLabelValues().Set(value)
	i.connected.Store(up)
}

// publish converts one controller event and hands it to the bus; every failure here drops a single
// event and is logged.
func (i *Ingester) publish(ctx context.Context, ev *pb.Event) {
	live, err := ToLiveEvent(ev)
	if err != nil {
		slog.Warn("dropping controller event", "seq", ev.GetSeq(), "error", err)
		return
	}
	data, err := json.Marshal(live)
	if err != nil {
		slog.Warn("dropping controller event, marshal failed", "id", live.ID, "error", err)
		return
	}

	i.metrics.EventsReceived.WithLabelValues(live.Type).Inc()
	if err := i.bus.Publish(ctx, busTopicLive, cache.Message{Type: "event", Data: data}); err != nil {
		// Deliberately no early return: see the doc comment. Realtime is lost
		// for this event, history need not be.
		slog.Warn("publishing live event to the bus failed", "id", live.ID, "error", err)
	}

	if i.sink == nil {
		return
	}
	// Its own short context, derived from the process context: not the stream
	// context (which dies on reconnect), and not context.Background() (which
	// would outlive shutdown).
	sinkCtx, cancel := context.WithTimeout(ctx, sinkTimeout)
	defer cancel()
	switch inserted, err := i.sink.InsertEvent(sinkCtx, live); {
	case err != nil:
		i.metrics.EventsPersisted.WithLabelValues("error").Inc()
		slog.Warn("failed to persist live event", "error", err, "type", live.Type) // WARN, never ERROR: history is degraded, the pipeline is not
	case inserted:
		i.metrics.EventsPersisted.WithLabelValues("ok").Inc()
	default:
		i.metrics.EventsPersisted.WithLabelValues("conflict").Inc() // another replica won the race; expected, not a problem
	}
}

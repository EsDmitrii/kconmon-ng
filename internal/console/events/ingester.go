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

// busTopicLive is the cache.Bus topic the live feed travels on. It MUST equal
// ws.TopicLive; this package cannot import ws (ws imports this one, to dedupe
// by LiveEvent.ID), so the equality is pinned by ingester_test.go, which
// subscribes through ws.TopicLive and would fail if the two ever drifted.
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

// connectGrace is how long a stream must survive without the server hanging up
// before the ingester reports healthy on the strength of the connection alone
// (an event arriving sooner promotes it immediately). It damps the Healthy flap
// against a controller that accepts and then instantly rejects every stream,
// which matters because Task 12 derives the console's browser-facing
// capabilities from Healthy.
const connectGrace = 2 * time.Second

// gRPC keepalive, identical to internal/agent/grpc_client.go's dial options —
// the console is a second long-lived streaming client against the same
// controller Service and has no reason to behave differently.
const (
	keepaliveTime    = 10 * time.Second
	keepaliveTimeout = 5 * time.Second
)

// sinkTimeout bounds one EventSink.InsertEvent call. It is independent of the
// stream context (which lives as long as the whole attempt, and dies on
// reconnect) and of context.Background() (which would outlive shutdown): a
// database hiccup must degrade history within a bounded time, never wedge the
// consume loop for the life of the stream.
const sinkTimeout = 5 * time.Second

var (
	// errNoCapability is the "the controller is not offering events" case. It is
	// deliberately handled exactly like a failed dial: back off, retry.
	errNoCapability = errors.New(`controller does not advertise the "events" capability`)
	// errNoControllerClient guards the misconfiguration where a gRPC address is
	// set but controller.url is not, so there is nothing to precheck against.
	errNoControllerClient = errors.New("controller HTTP client is not configured, cannot run the capability precheck")
)

// EventSink durably records a live event. Satisfied by *store.DB via a thin
// adapter in cmd/console. Nil sink = persistence off (the default, and the
// entire M1/M2 posture).
type EventSink interface {
	InsertEvent(ctx context.Context, ev LiveEvent) (inserted bool, err error)
}

// Option configures an Ingester. Variadic, so the eleven existing call sites
// (one in cmd/console, ten in ingester_test.go) need no edit.
type Option func(*Ingester)

// WithEventSink turns on durable persistence of every published live event.
// Omit it (the default) to leave persistence off, exactly as in M1/M2.
func WithEventSink(sink EventSink) Option {
	return func(i *Ingester) { i.sink = sink }
}

// Ingester is one console replica's client for the controller's
// EventStream.WatchEvents stream. It runs forever: capability precheck, dial,
// consume, publish to the bus, and on any failure back off and start over.
//
// Every replica runs its own Ingester, so with several replicas the same
// pb.Event is ingested (and published) more than once. That redundancy is the
// point — a replica whose own stream is down still serves events another replica
// ingested — and the ws.Hub de-duplicates by LiveEvent.ID on the way out.
type Ingester struct {
	ctrl     *controllerclient.Client
	grpcAddr string
	bus      cache.Bus
	metrics  *metrics.Metrics

	// sink is nil unless WithEventSink was passed to NewIngester, which is the
	// entire M1/M2 posture and every deployment with database.mode=disabled.
	sink EventSink

	// connectGrace defaults to the connectGrace const. Overridable via
	// export_test.go so a test can stretch it far enough that a promotion can
	// only have come from a received event. Never changed after Run starts.
	connectGrace time.Duration

	connected atomic.Bool
}

// NewIngester returns an ingester for the controller at grpcAddr. An empty
// grpcAddr means realtime ingestion is disabled: Run returns immediately and
// Healthy always reports false, which is what makes the console's own
// /api/v1/version correctly advertise no "events" capability.
//
// opts is variadic so the existing call sites (cmd/console and
// ingester_test.go) need no edit; today the only Option is WithEventSink.
func NewIngester(ctrl *controllerclient.Client, grpcAddr string, bus cache.Bus, m *metrics.Metrics, opts ...Option) *Ingester {
	i := &Ingester{ctrl: ctrl, grpcAddr: grpcAddr, bus: bus, metrics: m, connectGrace: connectGrace}
	for _, opt := range opts {
		opt(i)
	}
	return i
}

// Healthy reports whether this replica currently holds a WatchEvents stream
// that has proven itself — see consume for what "proven" means. It is the single
// input to the console's "events" capability flag, so it must be true only while
// events can actually arrive, and must not flap while a controller is refusing
// subscriptions.
//
// One case the grace period cannot detect: a connection that is open but
// blackholed (the peer vanished without a FIN) satisfies the grace and reports
// healthy until the gRPC keepalive above declares it dead — Time + Timeout, so
// ~15s worst case. The keepalive is the backstop for that; the grace only
// separates "accepted then refused" from "actually streaming".
func (i *Ingester) Healthy() bool { return i.connected.Load() }

// Run blocks until ctx is cancelled, reconnecting with the repo's standard
// 1s→15s doubling backoff (the shape in internal/agent/agent.go's WatchTasks
// loop). Like that loop it does not reset the backoff after a successful
// connection: the ceiling is 15s, so the worst case is a 15s hole, and a
// stream that flaps is better served by a calm retry rate than a fast one.
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
			// Not an error condition: a controller with events disabled, or a
			// pre-M2 controller, is a supported deployment. Info, not Warn.
			//
			// Only THIS case is benign. Every other precheck failure — an
			// unreachable or 5xx controller, a DNS failure, a missing HTTP
			// client — shares the reason="capability" metric label but is a real
			// problem, so it must not hide behind the same Info line.
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

	// A Go server-streaming client returns from WatchEvents BEFORE the server has
	// accepted the stream, so this error is only ever a client-side/transport
	// one. Server rejections — notably the leader gate, where a non-leader
	// replica answers codes.Unavailable — arrive at the first Recv in consume
	// and are counted reason="stream", not reason="dial".
	stream, err := pb.NewEventStreamClient(conn).WatchEvents(ctx, &pb.WatchEventsRequest{})
	if err != nil {
		return reasonDial, fmt.Errorf("open WatchEvents stream on %s: %w", i.grpcAddr, err)
	}

	return reasonStream, i.consume(ctx, stream)
}

// precheck is Decision 4a: feature-detect before every dial attempt, including
// every reconnect, and treat "no capability" as just another failed attempt.
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

// consume runs the stream to its end. It does NOT report the ingester healthy
// just because the stream object exists: a Go server-streaming client hands back
// a stream before the server has accepted it, so a controller that rejects every
// subscription (a non-leader answering codes.Unavailable) would otherwise flap
// Healthy — and therefore the console's advertised capabilities and the
// browser's realtime badge — true/false on every retry cycle.
//
// Healthy turns on at the first proof the stream really works: the first event
// received, or connectGrace elapsed without the server hanging up, whichever
// comes first. It turns off the moment this function returns.
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

// connectGate owns the connected state of ONE attempt. It exists to make two
// things impossible: reporting healthy twice, and — the subtle one — a grace
// timer that fires after the attempt already failed silently resurrecting
// connected for the next attempt to inherit.
type connectGate struct {
	ing *Ingester

	mu       sync.Mutex
	finished bool // the attempt is over; no further promotion is permitted
	up       bool // this attempt promoted the ingester and owes a demotion
}

// markConnected promotes the ingester at most once per attempt, and never after
// finish. Called from both the stream goroutine and the grace timer goroutine.
// The log happens after the lock is released: a slog handler is arbitrary
// user-supplied code and has no business running inside this mutex.
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

// setConnected keeps Healthy and the ingester_connected gauge in step. The
// gauge is written BEFORE the flag on purpose: Healthy() is what the console's
// /api/v1/version capability check reads, so anything that observes
// Healthy() == true must already see the gauge at 1 rather than a stale 0.
func (i *Ingester) setConnected(up bool) {
	value := 0.0
	if up {
		value = 1
	}
	i.metrics.IngesterConnected.WithLabelValues().Set(value)
	i.connected.Store(up)
}

// publish converts one controller event and hands it to the bus. Every failure
// here drops a single event and is logged; none of them ends the stream, because
// one unconvertible event must not cost the console its realtime feed.
//
// The bus and the sink are ordered but INDEPENDENT, in both directions.
// Ordered: the bus goes first because realtime delivery is the user-visible
// path with a hard latency budget, while durable history is the recoverable
// one. Independent: neither failure is allowed to suppress the other write.
//   - A sink failure never aborts or delays the publish that already happened
//     -- a database hiccup degrades history, it does not blind the Live page.
//   - A bus failure never skips the sink -- a Valkey outage degrades realtime,
//     it does not stop durable history while the database is perfectly healthy
//     (the events would then be unrecoverable, which is strictly worse than the
//     case this ordering exists to protect against).
//
// So the bus error below is logged and metered and then execution FALLS
// THROUGH to the sink block; do not turn it back into an early return.
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

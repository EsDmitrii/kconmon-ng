package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/redis/rueidis"
)

// valkeyChannelPrefix namespaces every Console pub/sub channel, matching
// DATA.md §5.3's events:* convention.
const valkeyChannelPrefix = "events:"

// ValkeyBus is a Bus backed by a real Valkey/Redis server via rueidis. It
// reuses InProcessBus's local fan-out for the "deliver to this replica's
// ws.Hub subscribers" half of the job; the network half is a single
// dedicated Receive loop subscribing to "events:*" via PSUBSCRIBE, so N local
// topics share ONE server-side subscription connection.
type ValkeyBus struct {
	client rueidis.Client
	local  *InProcessBus
	cancel context.CancelFunc
	closed atomic.Bool
}

// compile-time proof ValkeyBus satisfies the frozen Bus seam.
var _ Bus = (*ValkeyBus)(nil)

// NewValkeyBus dials address (host:port) and starts the background receive
// loop. Transient connection loss is retried by rueidis internally inside
// Receive; the loop's own backoff is a backstop for what rueidis declines to
// retry (and for benign subscription-end returns) — not the primary reconnect
// path. Construction itself only fails on the initial dial (a malformed
// address or completely unreachable host at startup) — rueidis dials eagerly.
//
// Teardown needs Close(): cancelling ctx stops the receive loop but does NOT
// release the underlying rueidis client, so ctx cancellation alone leaks the
// connection pool. The owner must call Close() (Task 14 wires this in
// cmd/console).
func NewValkeyBus(ctx context.Context, address string, dialTimeout time.Duration) (*ValkeyBus, error) {
	client, err := rueidis.NewClient(rueidis.ClientOption{
		InitAddress:       []string{address},
		Dialer:            net.Dialer{Timeout: dialTimeout},
		ForceSingleClient: true, // a single bundled/external Valkey instance, never a cluster, in M2
	})
	if err != nil {
		return nil, fmt.Errorf("valkey connect %s: %w", address, err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	b := &ValkeyBus{client: client, local: NewInProcessBus(), cancel: cancel}
	go b.receiveLoop(runCtx)
	return b, nil
}

// receiveLoop keeps a PSUBSCRIBE on "events:*" alive, exiting only when ctx is
// cancelled. Note it rarely runs its retry body: rueidis's Receive retries
// transient connection loss internally (goto retry) and only returns for
// benign subscription-end frames, cancellation, or errors it declines to
// retry — this loop is the backstop for those. It follows
// internal/agent/agent.go's WatchTasks reconnect loop (exponential backoff
// 1s → 15s) but deliberately deviates from its shape in two ways, both forced
// by rueidis's Receive contract — see below.
func (b *ValkeyBus) receiveLoop(ctx context.Context) {
	const (
		minBackoff = time.Second
		maxBackoff = 15 * time.Second
		// A Receive call that lasted this long is treated as a fresh incident,
		// restarting the backoff from minBackoff. Heuristic, not proof of
		// health: the measured span includes rueidis's internal retries, so a
		// long outage spent retrying inside Receive also qualifies — the cost
		// is a ~1s retry pace instead of 15s, still bounded by rueidis's own
		// internal retry pacing.
		healthySession = 30 * time.Second
	)
	backoff := minBackoff

	for {
		// The command MUST be rebuilt every iteration and never hoisted out of
		// this loop: rueidis recycles the Completed into a shared sync.Pool on
		// every nil return from Receive (singleClient.Receive ->
		// cmds.PutCompleted -> Put(c.cs)). Reusing a hoisted command would
		// hand a zeroed, pool-owned CommandSlice to the next Receive, panicking
		// in Verify() or racing a concurrent Publish that drew the same slice.
		cmd := b.client.B().Psubscribe().Pattern(valkeyChannelPrefix + "*").Build()

		start := time.Now()
		err := b.client.Receive(ctx, cmd, func(m rueidis.PubSubMessage) {
			topic := strings.TrimPrefix(m.Channel, valkeyChannelPrefix)
			var msg Message
			if jsonErr := json.Unmarshal([]byte(m.Message), &msg); jsonErr != nil {
				slog.Warn("valkey: dropping malformed message", "channel", m.Channel, "error", jsonErr)
				return
			}
			_ = b.local.Publish(ctx, topic, msg)
		})
		session := time.Since(start)

		if ctx.Err() != nil {
			return
		}

		if session >= healthySession {
			backoff = minBackoff
		}

		// Second deviation from WatchTasks: that loop only ever observes a real
		// error, so it grows the backoff unconditionally. Receive also returns
		// nil when the subscription merely ends on a still-healthy connection,
		// and treating that benign case as a failure would ratchet a perfectly
		// healthy bus all the way to maxBackoff. So only real errors escalate.
		if err == nil {
			slog.Info("valkey subscription ended, re-subscribing", "session", session, "backoff", backoff)
		} else {
			slog.Warn("valkey receive loop disconnected, retrying", "error", err, "backoff", backoff)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		if err != nil {
			backoff = min(backoff*2, maxBackoff)
		}
	}
}

// Publish sends msg to the Valkey channel "events:"+topic. Every replica
// running a receiveLoop (including this one) receives it back and re-delivers
// it to local subscribers via InProcessBus.Publish — so a single-process
// deployment still works end-to-end even with Valkey in the loop.
func (b *ValkeyBus) Publish(ctx context.Context, topic string, msg Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal message for topic %s: %w", topic, err)
	}
	cmd := b.client.B().Publish().Channel(valkeyChannelPrefix + topic).Message(string(data)).Build()
	return b.client.Do(ctx, cmd).Error()
}

// Subscribe registers a local subscriber, fed by receiveLoop. Delegates
// entirely to the embedded InProcessBus.
func (b *ValkeyBus) Subscribe(topic string) (msgs <-chan Message, unsubscribe func()) {
	return b.local.Subscribe(topic)
}

// Close stops the receive loop and releases the underlying rueidis client.
// Idempotent.
func (b *ValkeyBus) Close() {
	if b.closed.CompareAndSwap(false, true) {
		b.cancel()
		b.client.Close()
	}
}

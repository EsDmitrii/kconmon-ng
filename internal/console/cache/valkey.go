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

// ValkeyBus is a Bus backed by a real Valkey/Redis server via rueidis.
type ValkeyBus struct {
	client rueidis.Client
	local  *InProcessBus
	cancel context.CancelFunc
	closed atomic.Bool
}

// compile-time proof ValkeyBus satisfies the frozen Bus seam.
var _ Bus = (*ValkeyBus)(nil)

// NewValkeyBus dials address (host:port) and starts the background receive loop; construction
// itself only fails on the initial dial (a malformed address or completely unreachable host at
// startup). An empty password means no AUTH.
func NewValkeyBus(ctx context.Context, address string, dialTimeout time.Duration, password string) (*ValkeyBus, error) {
	client, err := rueidis.NewClient(rueidis.ClientOption{
		InitAddress:       []string{address},
		Dialer:            net.Dialer{Timeout: dialTimeout},
		ForceSingleClient: true, // a single bundled/external Valkey instance, never a cluster, in M2
		// Empty password means no AUTH, which is what an unauthenticated bundled Valkey wants.
		Password: password,
	})
	if err != nil {
		return nil, fmt.Errorf("valkey connect %s: %w", address, err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	b := &ValkeyBus{client: client, local: NewInProcessBus(), cancel: cancel}
	go b.receiveLoop(runCtx)
	return b, nil
}

// receiveLoop keeps a PSUBSCRIBE on "events:*" alive, exiting only when ctx is cancelled.
func (b *ValkeyBus) receiveLoop(ctx context.Context) {
	const (
		minBackoff = time.Second
		maxBackoff = 15 * time.Second
		// A Receive call that lasted this long is treated as a fresh incident, restarting the backoff
		// from minBackoff.
		healthySession = 30 * time.Second
	)
	backoff := minBackoff

	for {
		// The command MUST be rebuilt every iteration and never hoisted out of this loop.
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

		// Second deviation from WatchTasks: that loop only ever observes a real error, so it grows the
		// backoff unconditionally.
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

// Publish sends msg to the Valkey channel "events:"+topic.
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

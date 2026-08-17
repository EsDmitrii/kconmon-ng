package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/redis/rueidis"
)

// valkeyChannelPrefix namespaces every Console pub/sub channel, matching
// DATA.md §5.3's events:* convention.
const valkeyChannelPrefix = "events:"

// RedisBus is a Bus backed by a real Redis-compatible server (Valkey, Redis, …) via rueidis.
type RedisBus struct {
	client rueidis.Client
	local  *InProcessBus
	cancel context.CancelFunc
	closed atomic.Bool
}

// compile-time proof RedisBus satisfies the frozen Bus seam.
var _ Bus = (*RedisBus)(nil)

// NewRedisBus dials the DSN and starts the background receive loop; construction
// itself only fails on the initial dial (a malformed address or completely unreachable host at
// startup). An empty password means no AUTH.
func NewRedisBus(ctx context.Context, dsn string, dialTimeout time.Duration) (*RedisBus, error) {
	/* The DSN says everything: host, port, username, password, database number, and TLS through the
	   scheme (rediss:// / valkeys://). rueidis parses the form the servers' own documentation uses,
	   so a managed endpoint's connection string can be pasted in as-is. */
	opt, err := rueidis.ParseURL(dsn)
	if err != nil {
		// The DSN carries a credential and must never reach a log line, so the error is CLASSIFIED.
		return nil, fmt.Errorf("redis: dsn is not a valid redis:// URL: %w", redactDSNError(err))
	}
	opt.Dialer = net.Dialer{Timeout: dialTimeout}
	opt.ForceSingleClient = true // one server, never a cluster
	client, err := rueidis.NewClient(opt)
	if err != nil {
		return nil, fmt.Errorf("redis connect: %w", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	b := &RedisBus{client: client, local: NewInProcessBus(), cancel: cancel}
	go b.receiveLoop(runCtx)
	return b, nil
}

// receiveLoop keeps a PSUBSCRIBE on "events:*" alive, exiting only when ctx is cancelled.
func (b *RedisBus) receiveLoop(ctx context.Context) {
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

// CrossReplica is true: a PUBLISH reaches every console subscribed to the channel, which is what
// makes cross-replica forwarding (a run cancel, for one) meaningful.
func (b *RedisBus) CrossReplica() bool { return true }

// Publish sends msg to the Valkey channel "events:"+topic.
func (b *RedisBus) Publish(ctx context.Context, topic string, msg Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal message for topic %s: %w", topic, err)
	}
	cmd := b.client.B().Publish().Channel(valkeyChannelPrefix + topic).Message(string(data)).Build()
	return b.client.Do(ctx, cmd).Error()
}

// Subscribe registers a local subscriber, fed by receiveLoop. Delegates
// entirely to the embedded InProcessBus.
func (b *RedisBus) Subscribe(topic string) (msgs <-chan Message, unsubscribe func()) {
	return b.local.Subscribe(topic)
}

// Close stops the receive loop and releases the underlying rueidis client.
// Idempotent.
func (b *RedisBus) Close() {
	if b.closed.CompareAndSwap(false, true) {
		b.cancel()
		b.client.Close()
	}
}

/*
 * redactDSNError strips a URL out of a parse error.
 *
 * url.Parse embeds the input in its error text, and this input is a credential: an unparseable DSN
 * would otherwise put the password into the console's own startup log, where it outlives the
 * mistake that produced it.
 */
func redactDSNError(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return fmt.Errorf("%s: %w", uerr.Op, uerr.Err)
	}
	return err
}

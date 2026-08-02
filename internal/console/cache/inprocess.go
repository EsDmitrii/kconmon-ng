package cache

import (
	"context"
	"log/slog"
	"sync"
)

const localSubscriberBuffer = 32

// InProcessBus is a pure in-memory Bus: Publish fans out synchronously to
// every current local subscriber of the topic. It has no cross-replica
// delivery — the documented degradation when console.valkey.mode=disabled
// (ADR-002): only correct for console.replicas=1.
type InProcessBus struct {
	mu   sync.RWMutex
	subs map[string]map[int]chan Message
	next int
}

// compile-time proof InProcessBus is a drop-in for the ValkeyBus (Task 8).
var _ Bus = (*InProcessBus)(nil)

// NewInProcessBus returns a ready-to-use in-memory Bus.
func NewInProcessBus() *InProcessBus {
	return &InProcessBus{subs: make(map[string]map[int]chan Message)}
}

// Publish never blocks: a subscriber whose buffer is full has a message
// dropped for it (logged), rather than stalling the publisher.
func (b *InProcessBus) Publish(_ context.Context, topic string, msg Message) error {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for id, ch := range b.subs[topic] {
		select {
		case ch <- msg:
		default:
			slog.Warn("dropping message, local subscriber channel full", "topic", topic, "subscriber", id)
		}
	}
	return nil
}

// Subscribe registers a new local subscriber for topic. The returned
// unsubscribe func is idempotent and closes the channel.
func (b *InProcessBus) Subscribe(topic string) (msgs <-chan Message, unsubscribe func()) {
	ch := make(chan Message, localSubscriberBuffer)

	b.mu.Lock()
	id := b.next
	b.next++
	if b.subs[topic] == nil {
		b.subs[topic] = make(map[int]chan Message)
	}
	b.subs[topic][id] = ch
	b.mu.Unlock()

	var once sync.Once
	unsubscribe = func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs[topic], id)
			if len(b.subs[topic]) == 0 {
				delete(b.subs, topic)
			}
			b.mu.Unlock()
			close(ch)
		})
	}
	return ch, unsubscribe
}

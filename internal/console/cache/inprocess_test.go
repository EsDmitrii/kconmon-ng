package cache_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
)

// discardLogs silences the drop warnings InProcessBus emits, for the tests
// that overflow subscriber buffers on purpose. Tests in a package run
// sequentially, so swapping the default logger is safe here.
func discardLogs(t *testing.T) {
	t.Helper()
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
}

func TestInProcessBusDeliversToSubscriber(t *testing.T) {
	bus := cache.NewInProcessBus()
	msgs, unsubscribe := bus.Subscribe("live")
	defer unsubscribe()

	if err := bus.Publish(context.Background(), "live", cache.Message{Type: "event", Data: json.RawMessage(`{"a":1}`)}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case got := <-msgs:
		if got.Type != "event" || string(got.Data) != `{"a":1}` {
			t.Errorf("unexpected message: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("message not delivered")
	}
}

func TestInProcessBusTopicIsolation(t *testing.T) {
	bus := cache.NewInProcessBus()
	liveMsgs, unsub1 := bus.Subscribe("live")
	defer unsub1()
	topoMsgs, unsub2 := bus.Subscribe("topology")
	defer unsub2()

	_ = bus.Publish(context.Background(), "topology", cache.Message{Type: "snapshot", Data: json.RawMessage(`{}`)})

	select {
	case <-topoMsgs:
	case <-time.After(time.Second):
		t.Fatal("topology subscriber did not receive its message")
	}
	select {
	case m := <-liveMsgs:
		t.Fatalf("live subscriber must not receive a topology publish, got %+v", m)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestInProcessBusMultipleSubscribersSameTopic(t *testing.T) {
	bus := cache.NewInProcessBus()
	a, unsubA := bus.Subscribe("live")
	defer unsubA()
	b, unsubB := bus.Subscribe("live")
	defer unsubB()

	_ = bus.Publish(context.Background(), "live", cache.Message{Type: "event", Data: json.RawMessage(`{}`)})

	for _, ch := range []<-chan cache.Message{a, b} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatal("every subscriber to the same topic must receive the publish")
		}
	}
}

func TestInProcessBusUnsubscribeClosesChannel(t *testing.T) {
	bus := cache.NewInProcessBus()
	msgs, unsubscribe := bus.Subscribe("live")
	unsubscribe()

	select {
	case _, ok := <-msgs:
		if ok {
			t.Error("expected channel closed after unsubscribe")
		}
	case <-time.After(time.Second):
		t.Fatal("channel was not closed after unsubscribe")
	}
}

// A subscriber that never reads must not stall the publisher: the documented
// policy is to drop the message for that subscriber, not to block.
func TestInProcessBusSlowSubscriberDropsInsteadOfBlocking(t *testing.T) {
	const published = 1000 // far beyond any sane subscriber buffer

	discardLogs(t)

	bus := cache.NewInProcessBus()
	msgs, unsubscribe := bus.Subscribe("live")
	defer unsubscribe()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range published {
			_ = bus.Publish(context.Background(), "live", cache.Message{Type: "event", Data: json.RawMessage(`{}`)})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a subscriber that is not reading")
	}

	// Whatever the buffer size, the subscriber got some messages and strictly
	// fewer than were published — the overflow was dropped.
	buffered := len(msgs)
	if buffered == 0 {
		t.Error("subscriber received nothing at all")
	}
	if buffered >= published {
		t.Errorf("expected messages to be dropped, subscriber holds %d of %d", buffered, published)
	}
}

func TestInProcessBusUnsubscribeIsIdempotent(t *testing.T) {
	bus := cache.NewInProcessBus()
	msgs, unsubscribe := bus.Subscribe("live")

	unsubscribe()
	unsubscribe() // must not panic on a second close

	if _, ok := <-msgs; ok {
		t.Error("expected channel closed after unsubscribe")
	}
	// Publishing to a topic whose only subscriber left must stay safe.
	if err := bus.Publish(context.Background(), "live", cache.Message{Type: "event", Data: json.RawMessage(`{}`)}); err != nil {
		t.Errorf("Publish after unsubscribe: %v", err)
	}
}

// Exercises the lock discipline under -race: publishers running while
// subscribers come and go.
func TestInProcessBusConcurrentPublishAndSubscribeChurn(t *testing.T) {
	const iterations = 200

	discardLogs(t)

	bus := cache.NewInProcessBus()
	var wg sync.WaitGroup

	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				_ = bus.Publish(context.Background(), "live", cache.Message{Type: "event", Data: json.RawMessage(`{}`)})
			}
		}()
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				msgs, unsubscribe := bus.Subscribe("live")
				// Drain whatever happens to be there; delivery timing is not
				// what this test asserts, absence of races is.
				for drained := true; drained; {
					select {
					case <-msgs:
					default:
						drained = false
					}
				}
				unsubscribe()
			}
		}()
	}

	wg.Wait()
}

func TestInProcessBusPublishWithNoSubscribersDoesNotBlock(t *testing.T) {
	bus := cache.NewInProcessBus()
	done := make(chan struct{})
	go func() {
		_ = bus.Publish(context.Background(), "live", cache.Message{Type: "event", Data: json.RawMessage(`{}`)})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish with no subscribers must not block")
	}
}

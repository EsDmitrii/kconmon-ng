//go:build integration

package cache_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
)

// TestValkeyBusPublishSubscribeRoundtrip requires a real Valkey/Redis server.
// Run: docker run --rm -d -p 6379:6379 valkey/valkey:8-alpine
// Then: VALKEY_TEST_ADDR=127.0.0.1:6379 go test -tags=integration ./internal/console/cache/... -run TestValkeyBus -v
func TestValkeyBusPublishSubscribeRoundtrip(t *testing.T) {
	addr := os.Getenv("VALKEY_TEST_ADDR")
	if addr == "" {
		t.Skip("VALKEY_TEST_ADDR not set; see docker command in this test's comment")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bus, err := cache.NewValkeyBus(ctx, addr, 5*time.Second)
	if err != nil {
		t.Fatalf("NewValkeyBus: %v", err)
	}
	defer bus.Close()

	msgs, unsubscribe := bus.Subscribe("live")
	defer unsubscribe()

	// The receive loop's PSUBSCRIBE needs a moment to register with the server
	// before a publish is guaranteed to be delivered.
	time.Sleep(200 * time.Millisecond)

	want := cache.Message{Type: "event", Data: json.RawMessage(`{"hello":"world"}`)}
	if err := bus.Publish(ctx, "live", want); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case got := <-msgs:
		if got.Type != want.Type || string(got.Data) != string(want.Data) {
			t.Errorf("roundtrip mismatch: got %+v, want %+v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("message not delivered within 5s")
	}
}

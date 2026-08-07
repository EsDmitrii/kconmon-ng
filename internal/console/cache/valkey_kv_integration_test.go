//go:build integration

package cache_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
)

// TestValkeyKVSetGetDeleteRoundtrip requires a real Valkey/Redis server.
// Run: docker run --rm -d -p 6379:6379 valkey/valkey:8-alpine
// Then: VALKEY_TEST_ADDR=127.0.0.1:6379 go test -tags=integration ./internal/console/cache/... -run TestValkeyKV -v
func TestValkeyKVSetGetDeleteRoundtrip(t *testing.T) {
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

	kv := cache.NewValkeyKVFromBus(bus)
	key := "sess:integration-test"
	t.Cleanup(func() { _ = kv.Delete(context.Background(), key) })

	// Miss before anything is written.
	if _, ok, err := kv.Get(ctx, key); err != nil || ok {
		t.Fatalf("expected a clean miss before Set, got ok=%v err=%v", ok, err)
	}

	if err := kv.Set(ctx, key, []byte("hello"), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}

	val, ok, err := kv.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("expected a hit after Set")
	}
	if string(val) != "hello" {
		t.Errorf("got %q, want %q", val, "hello")
	}

	if err := kv.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok, err := kv.Get(ctx, key); err != nil || ok {
		t.Fatalf("expected a clean miss after Delete, got ok=%v err=%v", ok, err)
	}
}

// TestValkeyKVTTLExpiry requires a real Valkey/Redis server; see the docker
// command above.
func TestValkeyKVTTLExpiry(t *testing.T) {
	addr := os.Getenv("VALKEY_TEST_ADDR")
	if addr == "" {
		t.Skip("VALKEY_TEST_ADDR not set; see docker command in TestValkeyKVSetGetDeleteRoundtrip")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bus, err := cache.NewValkeyBus(ctx, addr, 5*time.Second)
	if err != nil {
		t.Fatalf("NewValkeyBus: %v", err)
	}
	defer bus.Close()

	kv := cache.NewValkeyKVFromBus(bus)
	key := "sess:integration-ttl-test"
	t.Cleanup(func() { _ = kv.Delete(context.Background(), key) })

	if err := kv.Set(ctx, key, []byte("x"), 200*time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}

	if _, ok, err := kv.Get(ctx, key); err != nil || !ok {
		t.Fatalf("expected a hit immediately after Set, got ok=%v err=%v", ok, err)
	}

	time.Sleep(500 * time.Millisecond)

	if _, ok, err := kv.Get(ctx, key); err != nil || ok {
		t.Fatalf("expected the key to have expired server-side, got ok=%v err=%v", ok, err)
	}
}

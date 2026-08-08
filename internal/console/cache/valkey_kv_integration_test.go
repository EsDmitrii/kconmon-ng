//go:build integration

package cache_test

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
)

// newIntegrationKV dials the VALKEY_TEST_ADDR server and returns a ValkeyKV
// on it, skipping the test when the address is unset. Same setup every test
// in this file did inline before IncrWithTTL added three more of them.
func newIntegrationKV(t *testing.T) *cache.ValkeyKV {
	t.Helper()
	addr := os.Getenv("VALKEY_TEST_ADDR")
	if addr == "" {
		t.Skip("VALKEY_TEST_ADDR not set; see docker command in TestValkeyKVSetGetDeleteRoundtrip")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	bus, err := cache.NewValkeyBus(ctx, addr, 5*time.Second)
	if err != nil {
		t.Fatalf("NewValkeyBus: %v", err)
	}
	t.Cleanup(bus.Close)
	return cache.NewValkeyKVFromBus(bus)
}

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

// --- IncrWithTTL: the fixed-window primitive (M4 Task 8) ------------------

// TestValkeyKVIncrWithTTLCountsOneToNWithinTheWindow requires a real
// Valkey/Redis server; see the docker command in
// TestValkeyKVSetGetDeleteRoundtrip.
func TestValkeyKVIncrWithTTLCountsOneToNWithinTheWindow(t *testing.T) {
	kv := newIntegrationKV(t)
	ctx := context.Background()
	key := "rl:integration-window"
	if err := kv.Delete(ctx, key); err != nil {
		t.Fatalf("pre-clean Delete: %v", err)
	}
	t.Cleanup(func() { _ = kv.Delete(context.Background(), key) })

	for want := int64(1); want <= 5; want++ {
		got, err := kv.IncrWithTTL(ctx, key, time.Minute)
		if err != nil {
			t.Fatalf("IncrWithTTL #%d: %v", want, err)
		}
		if got != want {
			t.Fatalf("IncrWithTTL #%d = %d, want %d", want, got, want)
		}
	}
}

// TestValkeyKVIncrWithTTLDoesNotExtendTheWindow pins the PEXPIRE ... NX
// half of the primitive against a real server: the TTL is set by the first
// hit of a window and NEVER re-armed by the ones that follow it.
func TestValkeyKVIncrWithTTLDoesNotExtendTheWindow(t *testing.T) {
	kv := newIntegrationKV(t)
	ctx := context.Background()
	key := "rl:integration-fixed"
	if err := kv.Delete(ctx, key); err != nil {
		t.Fatalf("pre-clean Delete: %v", err)
	}
	t.Cleanup(func() { _ = kv.Delete(context.Background(), key) })

	const ttl = 600 * time.Millisecond
	start := time.Now()
	for i := range 3 {
		if _, err := kv.IncrWithTTL(ctx, key, ttl); err != nil {
			t.Fatalf("IncrWithTTL #%d: %v", i, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(time.Until(start.Add(750 * time.Millisecond)))

	got, err := kv.IncrWithTTL(ctx, key, ttl)
	if err != nil {
		t.Fatalf("IncrWithTTL after the window: %v", err)
	}
	if got != 1 {
		t.Fatalf("IncrWithTTL after the window = %d, want 1 -- PEXPIRE was re-armed by a later hit "+
			"(sliding window), which is not what a fixed-window rate limit may do", got)
	}
}

// TestValkeyKVIncrWithTTLConcurrentFirstHitsNeverLeaveAKeyTTLLess is the
// race the doc comment reasons about, against a real server: many clients
// racing on a key that does not exist yet must all agree on 1..N and must
// leave a key that still expires.
func TestValkeyKVIncrWithTTLConcurrentFirstHitsNeverLeaveAKeyTTLLess(t *testing.T) {
	kv := newIntegrationKV(t)
	ctx := context.Background()
	key := "rl:integration-race"
	if err := kv.Delete(ctx, key); err != nil {
		t.Fatalf("pre-clean Delete: %v", err)
	}
	t.Cleanup(func() { _ = kv.Delete(context.Background(), key) })

	const goroutines = 32
	const ttl = 1500 * time.Millisecond

	seen := make([]int64, goroutines)
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			n, err := kv.IncrWithTTL(ctx, key, ttl)
			if err != nil {
				t.Errorf("goroutine %d: IncrWithTTL: %v", g, err)
				return
			}
			seen[g] = n
		}(g)
	}
	wg.Wait()

	got := map[int64]bool{}
	for _, n := range seen {
		if n < 1 || n > goroutines {
			t.Fatalf("IncrWithTTL returned %d, outside 1..%d", n, goroutines)
		}
		if got[n] {
			t.Fatalf("IncrWithTTL returned %d twice -- INCR is not atomic", n)
		}
		got[n] = true
	}

	time.Sleep(2 * ttl)
	after, err := kv.IncrWithTTL(ctx, key, ttl)
	if err != nil {
		t.Fatalf("IncrWithTTL after the window: %v", err)
	}
	if after != 1 {
		t.Fatalf("IncrWithTTL after the window = %d, want 1 -- the concurrent first hits left the key TTL-less", after)
	}
}

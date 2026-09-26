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

// newIntegrationKV dials the REDIS_TEST_ADDR server and returns a RedisKV
// on it, skipping the test when the address is unset.
func newIntegrationKV(t *testing.T) *cache.RedisKV {
	t.Helper()
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("REDIS_TEST_ADDR not set; see docker command in TestRedisKVSetGetDeleteRoundtrip")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	bus, err := cache.NewRedisBus(ctx, "redis://"+addr, 5*time.Second)
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	t.Cleanup(bus.Close)
	return cache.NewRedisKVFromBus(bus)
}

// TestRedisKVSetGetDeleteRoundtrip requires a real Valkey/Redis server.
// Run: docker run --rm -d -p 6379:6379 valkey/valkey:9-alpine
// Then: REDIS_TEST_ADDR=127.0.0.1:6379 go test -tags=integration ./internal/console/cache/... -run TestRedisKV -v
func TestRedisKVSetGetDeleteRoundtrip(t *testing.T) {
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("REDIS_TEST_ADDR not set; see docker command in this test's comment")
	}

	ctx := t.Context()

	bus, err := cache.NewRedisBus(ctx, "redis://"+addr, 5*time.Second)
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	defer bus.Close()

	kv := cache.NewRedisKVFromBus(bus)
	key := "sess:integration-test"
	t.Cleanup(func() { _ = kv.Delete(context.Background(), key) })

	// Miss before anything is written.
	if _, ok, getErr := kv.Get(ctx, key); getErr != nil || ok {
		t.Fatalf("expected a clean miss before Set, got ok=%v err=%v", ok, getErr)
	}

	if err = kv.Set(ctx, key, []byte("hello"), time.Minute); err != nil {
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

// TestRedisKVTTLExpiry requires a real Valkey/Redis server; see the docker
// command above.
func TestRedisKVTTLExpiry(t *testing.T) {
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("REDIS_TEST_ADDR not set; see docker command in TestRedisKVSetGetDeleteRoundtrip")
	}

	ctx := t.Context()

	bus, err := cache.NewRedisBus(ctx, "redis://"+addr, 5*time.Second)
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	defer bus.Close()

	kv := cache.NewRedisKVFromBus(bus)
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

// TestRedisKVIncrWithTTLCountsOneToNWithinTheWindow requires a real
// Valkey/Redis server; see the docker command in
// TestRedisKVSetGetDeleteRoundtrip.
func TestRedisKVIncrWithTTLCountsOneToNWithinTheWindow(t *testing.T) {
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

// TestRedisKVIncrWithTTLDoesNotExtendTheWindow pins the PEXPIRE ... NX
// half of the primitive against a real server: the TTL is set by the first
// hit of a window and NEVER re-armed by the ones that follow it.
func TestRedisKVIncrWithTTLDoesNotExtendTheWindow(t *testing.T) {
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

// TestRedisKVIncrWithTTLConcurrentFirstHitsNeverLeaveAKeyTTLLess is the race the doc comment
// reasons about.
func TestRedisKVIncrWithTTLConcurrentFirstHitsNeverLeaveAKeyTTLLess(t *testing.T) {
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

// TestRedisKVSetNXIsALockAndSetXXNeverRecreates pins the two conditional writes the session write
// lock and the session rewrite rely on, against a real server.
func TestRedisKVSetNXIsALockAndSetXXNeverRecreates(t *testing.T) {
	kv := newIntegrationKV(t)
	ctx := context.Background()
	lock := "it:sesslock:" + time.Now().Format("150405.000000000")
	rec := "it:sess:" + time.Now().Format("150405.000000000")
	t.Cleanup(func() {
		_ = kv.Delete(context.Background(), lock)
		_ = kv.Delete(context.Background(), rec)
	})

	const goroutines = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := 0
	for range goroutines {
		wg.Go(func() {
			ok, err := kv.SetNX(ctx, lock, []byte("holder"), 300*time.Millisecond)
			if err != nil {
				t.Errorf("SetNX: %v", err)
				return
			}
			if ok {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if winners != 1 {
		t.Fatalf("%d of %d concurrent SetNX calls took the lock, want exactly 1", winners, goroutines)
	}
	time.Sleep(600 * time.Millisecond)
	if ok, err := kv.SetNX(ctx, lock, []byte("next"), time.Second); err != nil || !ok {
		t.Fatalf("SetNX after the lock's TTL = %v, %v; want true, nil", ok, err)
	}

	if ok, err := kv.SetXX(ctx, rec, []byte("x"), time.Minute); err != nil || ok {
		t.Fatalf("SetXX on an absent key = %v, %v; want false, nil", ok, err)
	}
	if _, found, _ := kv.Get(ctx, rec); found {
		t.Fatal("a refused SetXX created the key")
	}
	if err := kv.Set(ctx, rec, []byte("old"), time.Minute); err != nil {
		t.Fatal(err)
	}
	if ok, err := kv.SetXX(ctx, rec, []byte("new"), time.Minute); err != nil || !ok {
		t.Fatalf("SetXX on a live key = %v, %v; want true, nil", ok, err)
	}
	if val, found, err := kv.Get(ctx, rec); err != nil || !found || string(val) != "new" {
		t.Errorf("Get after SetXX = %q, %v, %v; want %q", val, found, err, "new")
	}
}

// DeleteIfEqual releases the session write lock against a real server: a stale holder's token
// leaves the current holder's lock in place, the holder's own token removes it.
func TestRedisKVDeleteIfEqualIsACompareAndDelete(t *testing.T) {
	kv := newIntegrationKV(t)
	ctx := context.Background()
	lock := "it:sesslock:cad:" + time.Now().Format("150405.000000000")
	t.Cleanup(func() { _ = kv.Delete(context.Background(), lock) })

	if ok, err := kv.DeleteIfEqual(ctx, lock, []byte("a")); err != nil || ok {
		t.Fatalf("DeleteIfEqual on an absent key = %v, %v; want false, nil", ok, err)
	}
	if ok, err := kv.SetNX(ctx, lock, []byte("b"), time.Minute); err != nil || !ok {
		t.Fatalf("SetNX = %v, %v", ok, err)
	}
	if ok, err := kv.DeleteIfEqual(ctx, lock, []byte("a")); err != nil || ok {
		t.Fatalf("DeleteIfEqual with a stale token = %v, %v; want false, nil", ok, err)
	}
	if val, found, err := kv.Get(ctx, lock); err != nil || !found || string(val) != "b" {
		t.Fatalf("after a refused DeleteIfEqual: %q, %v, %v; want the holder's %q", val, found, err, "b")
	}
	if ok, err := kv.DeleteIfEqual(ctx, lock, []byte("b")); err != nil || !ok {
		t.Fatalf("DeleteIfEqual with the holder's token = %v, %v; want true, nil", ok, err)
	}
	if _, found, _ := kv.Get(ctx, lock); found {
		t.Error("DeleteIfEqual reported a delete but the key is still there")
	}
}

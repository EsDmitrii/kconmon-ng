package cache_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
)

func TestInProcessKVSetGetRoundtrip(t *testing.T) {
	t.Parallel()
	kv := cache.NewInProcessKV()
	t.Cleanup(kv.Close)

	if err := kv.Set(context.Background(), "sess:a", []byte("hello"), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}

	val, ok, err := kv.Get(context.Background(), "sess:a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("expected a hit")
	}
	if string(val) != "hello" {
		t.Errorf("got %q, want %q", val, "hello")
	}
}

func TestInProcessKVMissIsNeverAnError(t *testing.T) {
	t.Parallel()
	kv := cache.NewInProcessKV()
	t.Cleanup(kv.Close)

	val, ok, err := kv.Get(context.Background(), "sess:does-not-exist")
	if err != nil {
		t.Fatalf("a miss must never return an error, got: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for a missing key")
	}
	if val != nil {
		t.Errorf("expected nil val for a miss, got %v", val)
	}
}

func TestInProcessKVTTLExpiryMakesKeyDisappear(t *testing.T) {
	t.Parallel()
	kv := cache.NewInProcessKV()
	t.Cleanup(kv.Close)

	if err := kv.Set(context.Background(), "sess:short", []byte("x"), 5*time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}

	time.Sleep(30 * time.Millisecond)

	_, ok, err := kv.Get(context.Background(), "sess:short")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Fatal("expected the key to have expired")
	}
}

func TestInProcessKVDeleteIsIdempotent(t *testing.T) {
	t.Parallel()
	kv := cache.NewInProcessKV()
	t.Cleanup(kv.Close)

	if err := kv.Set(context.Background(), "sess:d", []byte("x"), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}

	if err := kv.Delete(context.Background(), "sess:d"); err != nil {
		t.Fatalf("first Delete: %v", err)
	}
	if err := kv.Delete(context.Background(), "sess:d"); err != nil {
		t.Fatalf("second Delete on an already-absent key must not error: %v", err)
	}
	if err := kv.Delete(context.Background(), "sess:never-existed"); err != nil {
		t.Fatalf("Delete on a key that never existed must not error: %v", err)
	}

	_, ok, err := kv.Get(context.Background(), "sess:d")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Fatal("expected the key to be gone after Delete")
	}
}

// TestInProcessKVSweeperReclaimsMemory guards against a long-running console
// leaking one map entry per expired login: entries that are set and never
// read again must still be reclaimed by the background sweeper, not just
// evicted lazily on Get.
func TestInProcessKVSweeperReclaimsMemory(t *testing.T) {
	t.Parallel()
	kv := cache.NewInProcessKV()
	t.Cleanup(kv.Close)

	const keys = 1000
	for i := range keys {
		key := fmt.Sprintf("sess:sweep-%d", i)
		if err := kv.Set(context.Background(), key, []byte("x"), 10*time.Millisecond); err != nil {
			t.Fatalf("Set %s: %v", key, err)
		}
	}

	if got := kv.Len(); got != keys {
		t.Fatalf("expected all %d keys present before expiry, got %d", keys, got)
	}

	// Advance real time past both the TTL and several sweeper ticks, without
	// ever calling Get (which would evict lazily and defeat the point of
	// this test).
	deadline := time.Now().Add(2 * time.Second)
	for kv.Len() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if got := kv.Len(); got != 0 {
		t.Fatalf("expected the background sweeper to have drained all expired entries, %d remain", got)
	}
}

// --- IncrWithTTL: the fixed-window primitive (M4 Task 8) ------------------

func TestInProcessKVIncrWithTTLCountsOneToNWithinTheWindow(t *testing.T) {
	t.Parallel()
	kv := cache.NewInProcessKV()
	t.Cleanup(kv.Close)

	for want := int64(1); want <= 5; want++ {
		got, err := kv.IncrWithTTL(context.Background(), "rl:window", time.Minute)
		if err != nil {
			t.Fatalf("IncrWithTTL #%d: %v", want, err)
		}
		if got != want {
			t.Fatalf("IncrWithTTL #%d = %d, want %d", want, got, want)
		}
	}
}

// TestInProcessKVIncrWithTTLDoesNotExtendTheWindow is the whole point of the
// primitive: a FIXED window starts at its first hit and later hits inside it
// must not push the expiry out, or a client hammering the endpoint would
// keep its own counter alive forever and never get a fresh allowance.
func TestInProcessKVIncrWithTTLDoesNotExtendTheWindow(t *testing.T) {
	t.Parallel()
	kv := cache.NewInProcessKV()
	t.Cleanup(kv.Close)

	const ttl = 600 * time.Millisecond
	start := time.Now()
	for i := range 3 {
		if _, err := kv.IncrWithTTL(context.Background(), "rl:fixed", ttl); err != nil {
			t.Fatalf("IncrWithTTL #%d: %v", i, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	// t is now ~600ms; the third hit landed at ~400ms. A sliding window would
	// keep the key alive until ~1000ms, a fixed one dies at ~600ms.
	time.Sleep(time.Until(start.Add(750 * time.Millisecond)))

	got, err := kv.IncrWithTTL(context.Background(), "rl:fixed", ttl)
	if err != nil {
		t.Fatalf("IncrWithTTL after the window: %v", err)
	}
	if got != 1 {
		t.Fatalf("IncrWithTTL after the window = %d, want 1 -- later hits extended the TTL "+
			"(sliding window), which is not what a fixed-window rate limit may do", got)
	}
}

// TestInProcessKVIncrWithTTLConcurrentFirstHitsNeverLeaveAKeyTTLLess pins the
// race the doc comment calls out: many goroutines racing on a key that does
// not exist yet must produce exactly N as the final count AND a key that
// still expires -- a lost TTL would lock a subject out permanently.
func TestInProcessKVIncrWithTTLConcurrentFirstHitsNeverLeaveAKeyTTLLess(t *testing.T) {
	t.Parallel()
	kv := cache.NewInProcessKV()
	t.Cleanup(kv.Close)

	const goroutines = 32
	const ttl = 250 * time.Millisecond

	seen := make([]int64, goroutines)
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			n, err := kv.IncrWithTTL(context.Background(), "rl:race", ttl)
			if err != nil {
				t.Errorf("goroutine %d: IncrWithTTL: %v", g, err)
				return
			}
			seen[g] = n
		}(g)
	}
	wg.Wait()

	// Every 1..N must have been handed out exactly once.
	got := map[int64]bool{}
	for _, n := range seen {
		if n < 1 || n > goroutines {
			t.Fatalf("IncrWithTTL returned %d, outside 1..%d", n, goroutines)
		}
		if got[n] {
			t.Fatalf("IncrWithTTL returned %d twice -- the increment is not atomic", n)
		}
		got[n] = true
	}

	time.Sleep(2 * ttl)
	after, err := kv.IncrWithTTL(context.Background(), "rl:race", ttl)
	if err != nil {
		t.Fatalf("IncrWithTTL after the window: %v", err)
	}
	if after != 1 {
		t.Fatalf("IncrWithTTL after the window = %d, want 1 -- the concurrent first hits left the key TTL-less", after)
	}
}

// TestInProcessKVIncrWithTTLOnANonNumericValueErrors mirrors Valkey's own
// INCR contract: a key holding something that is not an integer is an error,
// not a silent reset -- otherwise a key collision between a counter and a
// session blob would quietly wipe the rate limit.
func TestInProcessKVIncrWithTTLOnANonNumericValueErrors(t *testing.T) {
	t.Parallel()
	kv := cache.NewInProcessKV()
	t.Cleanup(kv.Close)

	if err := kv.Set(context.Background(), "rl:not-a-number", []byte("hello"), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := kv.IncrWithTTL(context.Background(), "rl:not-a-number", time.Minute); err == nil {
		t.Fatal("IncrWithTTL on a non-numeric value must return an error, got nil")
	}
}

// TestInProcessKVConcurrentSetGetDelete exercises the lock discipline under
// -race: many goroutines hammering Set/Get/Delete on overlapping keys.
func TestInProcessKVConcurrentSetGetDelete(t *testing.T) {
	t.Parallel()
	kv := cache.NewInProcessKV()
	t.Cleanup(kv.Close)

	const goroutines = 8
	const iterations = 200

	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			ctx := context.Background()
			for i := range iterations {
				key := fmt.Sprintf("sess:concurrent-%d", i%16)
				switch (g + i) % 3 {
				case 0:
					_ = kv.Set(ctx, key, []byte("x"), time.Minute)
				case 1:
					_, _, _ = kv.Get(ctx, key)
				case 2:
					_ = kv.Delete(ctx, key)
				}
			}
		}(g)
	}
	wg.Wait()
}

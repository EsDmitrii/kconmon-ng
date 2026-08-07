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

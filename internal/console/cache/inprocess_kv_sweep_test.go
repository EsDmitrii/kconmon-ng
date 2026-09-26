package cache

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"
)

// Get checks expiry itself, so the sweep only reclaims memory: it must release the lock between
// bounded batches, so a Get never waits for the whole map.
func TestInProcessKVSweepReleasesTheLockBetweenBatches(t *testing.T) {
	kv := &InProcessKV{entries: map[string]kvEntry{}, stop: make(chan struct{})}
	ctx := context.Background()
	expired := 2*kvSweepBatch + 7
	for i := range expired {
		_ = kv.Set(ctx, fmt.Sprintf("rl:%d", i), []byte("1"), -time.Second)
	}
	_ = kv.Set(ctx, "sess:live", []byte("x"), time.Minute)
	var left []int
	kv.afterSweepBatch = func() {
		if !kv.mu.TryRLock() {
			t.Fatal("the sweep still holds the lock between two batches")
		}
		left = append(left, len(kv.entries))
		kv.mu.RUnlock()
	}
	kv.sweep()
	if want := []int{expired + 1 - kvSweepBatch, expired + 1 - 2*kvSweepBatch, 1}; !slices.Equal(left, want) {
		t.Fatalf("entries left after each batch = %v, want %v (batches of %d)", left, want, kvSweepBatch)
	}
}

package checker

import (
	"fmt"
	"testing"
	"time"
)

func TestMTRCheckerTryAcquire(t *testing.T) {
	c := NewMTRChecker(30, 1*time.Second, 5*time.Second)

	// First call: should succeed and record the time.
	if !c.TryAcquire("node-1", "node-2") {
		t.Error("expected TryAcquire=true on first call")
	}

	// Immediate second call: cooldown not elapsed.
	if c.TryAcquire("node-1", "node-2") {
		t.Error("expected TryAcquire=false within cooldown")
	}

	// Different destination: independent cooldown, should succeed.
	if !c.TryAcquire("node-1", "node-3") {
		t.Error("expected TryAcquire=true for different pair")
	}
}

func TestMTRCheckerTryAcquireCooldownExpired(t *testing.T) {
	c := NewMTRChecker(30, 1*time.Second, 100*time.Millisecond)

	// Seed an old entry so the cooldown is already expired.
	c.mu.Lock()
	c.lastRun["node-1->node-2"] = time.Now().Add(-200 * time.Millisecond)
	c.mu.Unlock()

	if !c.TryAcquire("node-1", "node-2") {
		t.Error("expected TryAcquire=true after cooldown expired")
	}
}

func TestMTRCheckerTryAcquireAtomicRecord(t *testing.T) {
	c := NewMTRChecker(30, 1*time.Second, 1*time.Second)

	// TryAcquire must record the run so that a subsequent ShouldRun-like
	// check immediately blocks without a separate MarkRun step.
	if !c.TryAcquire("src", "dst") {
		t.Fatal("expected first TryAcquire=true")
	}

	c.mu.Lock()
	_, recorded := c.lastRun["src->dst"]
	c.mu.Unlock()

	if !recorded {
		t.Error("TryAcquire must record the key atomically")
	}
}

func TestMTRCheckerExpiredEntriesPurged(t *testing.T) {
	c := NewMTRChecker(30, 1*time.Second, 100*time.Millisecond)

	// Seed several expired entries directly.
	c.mu.Lock()
	for i := range 5 {
		key := fmt.Sprintf("node-%d->node-x", i)
		c.lastRun[key] = time.Now().Add(-200 * time.Millisecond)
	}
	c.mu.Unlock()

	// TryAcquire for a new pair triggers cleanup of the expired entries.
	if !c.TryAcquire("node-new", "node-x") {
		t.Fatal("expected TryAcquire=true for new pair")
	}

	c.mu.Lock()
	remaining := len(c.lastRun)
	c.mu.Unlock()

	// Only the newly recorded entry should remain.
	if remaining != 1 {
		t.Errorf("expected 1 entry after purge, got %d", remaining)
	}
}

package cache

import (
	"context"
	"sync"
	"time"
)

// kvSweepInterval is how often InProcessKV's background goroutine scans for
// expired entries and reclaims them. It is independent of any individual
// entry's TTL: Get always re-checks expiry itself (belt), so correctness
// never depends on the sweeper's timing — the sweeper only bounds how long a
// key that is set and never read again (e.g. an expired session nobody polls)
// can hold memory (braces), which is what keeps a long-running console from
// leaking one entry per expired login.
const kvSweepInterval = 20 * time.Millisecond

type kvEntry struct {
	val       []byte
	expiresAt time.Time
}

func (e kvEntry) expired(now time.Time) bool {
	return now.After(e.expiresAt)
}

// InProcessKV is a pure in-memory KV: a TTL-swept map, mirroring
// InProcessBus's role as the single-replica fallback for
// console.valkey.mode=disabled. It has no cross-replica visibility, so a
// session written on one replica is invisible to another — the documented
// limitation (ADR-002) that makes multi-replica consoles with
// console.valkey.mode=disabled and session auth an unsupported combination
// (Task 18's chart-render guard).
type InProcessKV struct {
	mu       sync.RWMutex
	entries  map[string]kvEntry
	stop     chan struct{}
	stopOnce sync.Once
}

// compile-time proof InProcessKV is a drop-in for ValkeyKV.
var _ KV = (*InProcessKV)(nil)

// NewInProcessKV returns a ready-to-use in-memory KV and starts its
// background sweeper goroutine. Callers that create short-lived instances
// (tests) should call Close to stop it; the long-lived console singleton
// does not need to.
func NewInProcessKV() *InProcessKV {
	kv := &InProcessKV{
		entries: make(map[string]kvEntry),
		stop:    make(chan struct{}),
	}
	go kv.sweepLoop()
	return kv
}

// Get reports a miss, never an error, for both an absent key and an expired
// one — expiry is checked here regardless of whether the background sweeper
// has caught up yet.
func (kv *InProcessKV) Get(_ context.Context, key string) (val []byte, ok bool, err error) {
	kv.mu.RLock()
	defer kv.mu.RUnlock()

	e, present := kv.entries[key]
	if !present || e.expired(time.Now()) {
		return nil, false, nil
	}
	// Return a copy: the caller must not be able to mutate our stored bytes
	// through the returned slice.
	val = make([]byte, len(e.val))
	copy(val, e.val)
	return val, true, nil
}

// Set stores a copy of val under key with the given ttl, replacing any
// existing value and expiry.
func (kv *InProcessKV) Set(_ context.Context, key string, val []byte, ttl time.Duration) error {
	stored := make([]byte, len(val))
	copy(stored, val)

	kv.mu.Lock()
	defer kv.mu.Unlock()
	kv.entries[key] = kvEntry{val: stored, expiresAt: time.Now().Add(ttl)}
	return nil
}

// Delete removes key. Deleting an absent or already-expired key is a no-op,
// never an error.
func (kv *InProcessKV) Delete(_ context.Context, key string) error {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	delete(kv.entries, key)
	return nil
}

// Len reports the number of entries currently held, including any not yet
// reclaimed by the background sweeper (i.e. it is not simply "live key
// count"). Exposed for tests and diagnostics, not part of the KV interface.
func (kv *InProcessKV) Len() int {
	kv.mu.RLock()
	defer kv.mu.RUnlock()
	return len(kv.entries)
}

// Close stops the background sweeper goroutine. Idempotent. The KV remains
// usable afterward (Get/Set/Delete keep working; entries just stop being
// swept in the background — Get's own expiry check still keeps it correct).
func (kv *InProcessKV) Close() {
	kv.stopOnce.Do(func() { close(kv.stop) })
}

func (kv *InProcessKV) sweepLoop() {
	ticker := time.NewTicker(kvSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			kv.sweep()
		case <-kv.stop:
			return
		}
	}
}

func (kv *InProcessKV) sweep() {
	now := time.Now()
	kv.mu.Lock()
	defer kv.mu.Unlock()
	for key, e := range kv.entries {
		if e.expired(now) {
			delete(kv.entries, key)
		}
	}
}

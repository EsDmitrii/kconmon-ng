package cache

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// kvSweepInterval is how often InProcessKV's background goroutine scans for expired entries and
// reclaims them; it is independent of any individual entry's TTL: Get always re-checks expiry
// itself (belt).
const kvSweepInterval = 20 * time.Millisecond

type kvEntry struct {
	val       []byte
	expiresAt time.Time
}

func (e kvEntry) expired(now time.Time) bool {
	return now.After(e.expiresAt)
}

// InProcessKV is a pure in-memory KV: a TTL-swept map.
type InProcessKV struct {
	mu       sync.RWMutex
	entries  map[string]kvEntry
	stop     chan struct{}
	stopOnce sync.Once
}

// compile-time proof InProcessKV is a drop-in for RedisKV.
var _ KV = (*InProcessKV)(nil)

// NewInProcessKV returns a ready-to-use in-memory KV and starts its background sweeper goroutine.
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

// IncrWithTTL increments key's counter under the write lock and returns the new value.
func (kv *InProcessKV) IncrWithTTL(_ context.Context, key string, ttl time.Duration) (int64, error) {
	now := time.Now()

	kv.mu.Lock()
	defer kv.mu.Unlock()

	e, present := kv.entries[key]
	if !present || e.expired(now) {
		kv.entries[key] = kvEntry{val: []byte("1"), expiresAt: now.Add(ttl)}
		return 1, nil
	}

	n, err := strconv.ParseInt(string(e.val), 10, 64)
	if err != nil {
		// Valkey answers "value is not an integer or out of range" here; the
		// shape a caller can act on is the same either way -- an error, never
		// a silent reset that would erase whatever the limit had counted.
		return 0, fmt.Errorf("inprocess kv incr %s: value is not an integer: %w", key, err)
	}
	n++
	// expiresAt is deliberately carried over untouched: this is what makes the
	// window FIXED rather than sliding.
	kv.entries[key] = kvEntry{val: []byte(strconv.FormatInt(n, 10)), expiresAt: e.expiresAt}
	return n, nil
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

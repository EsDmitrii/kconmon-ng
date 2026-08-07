package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/rueidis"
)

// ValkeyKV is a KV backed by a real Valkey/Redis server via rueidis, reusing
// the rueidis client an already-open ValkeyBus holds rather than dialling a
// second connection pool — see NewValkeyKVFromBus. TTL is native: every
// value is written with a PX expiry, so Valkey itself does the reclaiming
// (no local sweeper needed, unlike InProcessKV).
type ValkeyKV struct {
	client rueidis.Client
}

// compile-time proof ValkeyKV satisfies KV.
var _ KV = (*ValkeyKV)(nil)

// NewValkeyKVFromBus builds a ValkeyKV around vb's already-open rueidis
// client. It does not dial and does not own the client: closing it is
// ValkeyBus.Close's job, so cmd/console's existing closeBus stays the single
// teardown point for both the Bus and the KV built on top of it.
func NewValkeyKVFromBus(vb *ValkeyBus) *ValkeyKV {
	return &ValkeyKV{client: vb.client}
}

// Get reports (nil, false, nil) on a miss, mapping rueidis's nil reply
// (key absent or expired — Valkey enforces the PX ttl itself) to that same
// miss shape KV promises, never surfacing it as an error.
func (kv *ValkeyKV) Get(ctx context.Context, key string) (val []byte, ok bool, err error) {
	cmd := kv.client.B().Get().Key(key).Build()
	resp := kv.client.Do(ctx, cmd)
	if respErr := resp.Error(); respErr != nil {
		if rueidis.IsRedisNil(respErr) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("valkey kv get %s: %w", key, respErr)
	}
	val, err = resp.AsBytes()
	if err != nil {
		return nil, false, fmt.Errorf("valkey kv get %s: decode reply: %w", key, err)
	}
	return val, true, nil
}

// Set writes val under key with a PX expiry of ttl.
func (kv *ValkeyKV) Set(ctx context.Context, key string, val []byte, ttl time.Duration) error {
	cmd := kv.client.B().Set().Key(key).Value(rueidis.BinaryString(val)).Px(ttl).Build()
	if err := kv.client.Do(ctx, cmd).Error(); err != nil {
		return fmt.Errorf("valkey kv set %s: %w", key, err)
	}
	return nil
}

// Delete removes key. DEL on an absent key returns a count of 0 rather than
// an error in Valkey, so this is idempotent without any extra handling.
func (kv *ValkeyKV) Delete(ctx context.Context, key string) error {
	cmd := kv.client.B().Del().Key(key).Build()
	if err := kv.client.Do(ctx, cmd).Error(); err != nil {
		return fmt.Errorf("valkey kv delete %s: %w", key, err)
	}
	return nil
}

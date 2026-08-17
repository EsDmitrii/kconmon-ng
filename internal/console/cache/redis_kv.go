package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/rueidis"
)

// RedisKV is a KV backed by a real Redis-compatible server via rueidis.
type RedisKV struct {
	client rueidis.Client
}

// compile-time proof RedisKV satisfies KV.
var _ KV = (*RedisKV)(nil)

// NewRedisKVFromBus builds a RedisKV around vb's already-open rueidis client.
func NewRedisKVFromBus(vb *RedisBus) *RedisKV {
	return &RedisKV{client: vb.client}
}

// Get reports (nil, false, nil) on a miss, mapping rueidis's nil reply
// (key absent or expired — Valkey enforces the PX ttl itself) to that same
// miss shape KV promises, never surfacing it as an error.
func (kv *RedisKV) Get(ctx context.Context, key string) (val []byte, ok bool, err error) {
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
func (kv *RedisKV) Set(ctx context.Context, key string, val []byte, ttl time.Duration) error {
	cmd := kv.client.B().Set().Key(key).Value(rueidis.BinaryString(val)).Px(ttl).Build()
	if err := kv.client.Do(ctx, cmd).Error(); err != nil {
		return fmt.Errorf("valkey kv set %s: %w", key, err)
	}
	return nil
}

// IncrWithTTL is INCR followed by PEXPIRE; letting EVERY call issue PEXPIRE NX moves the condition
// into the server.
func (kv *RedisKV) IncrWithTTL(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	resps := kv.client.DoMulti(ctx,
		kv.client.B().Incr().Key(key).Build(),
		kv.client.B().Pexpire().Key(key).Milliseconds(ttl.Milliseconds()).Nx().Build(),
	)

	n, err := resps[0].AsInt64()
	if err != nil {
		return 0, fmt.Errorf("valkey kv incr %s: %w", key, err)
	}
	// The PEXPIRE reply itself (1 = armed, 0 = a TTL was already there) carries no decision.
	if expErr := resps[1].Error(); expErr != nil {
		return 0, fmt.Errorf("valkey kv incr %s: set window ttl: %w", key, expErr)
	}
	return n, nil
}

// Delete removes key. DEL on an absent key returns a count of 0 rather than
// an error in Valkey, so this is idempotent without any extra handling.
func (kv *RedisKV) Delete(ctx context.Context, key string) error {
	cmd := kv.client.B().Del().Key(key).Build()
	if err := kv.client.Do(ctx, cmd).Error(); err != nil {
		return fmt.Errorf("valkey kv delete %s: %w", key, err)
	}
	return nil
}

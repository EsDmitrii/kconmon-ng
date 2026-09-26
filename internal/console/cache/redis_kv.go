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
		return nil, false, fmt.Errorf("valkey kv get %s: %w", logKey(key), respErr)
	}
	val, err = resp.AsBytes()
	if err != nil {
		return nil, false, fmt.Errorf("valkey kv get %s: decode reply: %w", logKey(key), err)
	}
	return val, true, nil
}

// Set writes val under key with a PX expiry of ttl.
func (kv *RedisKV) Set(ctx context.Context, key string, val []byte, ttl time.Duration) error {
	cmd := kv.client.B().Set().Key(key).Value(rueidis.BinaryString(val)).Px(ttl).Build()
	if err := kv.client.Do(ctx, cmd).Error(); err != nil {
		return fmt.Errorf("valkey kv set %s: %w", logKey(key), err)
	}
	return nil
}

// SetNX is SET NX PX; Valkey's nil reply is the refusal.
func (kv *RedisKV) SetNX(ctx context.Context, key string, val []byte, ttl time.Duration) (bool, error) {
	cmd := kv.client.B().Set().Key(key).Value(rueidis.BinaryString(val)).Nx().Px(ttl).Build()
	return kv.setIf(ctx, "setnx", key, cmd)
}

// SetXX is SET XX PX; Valkey's nil reply is the refusal.
func (kv *RedisKV) SetXX(ctx context.Context, key string, val []byte, ttl time.Duration) (bool, error) {
	cmd := kv.client.B().Set().Key(key).Value(rueidis.BinaryString(val)).Xx().Px(ttl).Build()
	return kv.setIf(ctx, "setxx", key, cmd)
}

func (kv *RedisKV) setIf(ctx context.Context, op, key string, cmd rueidis.Completed) (bool, error) {
	if err := kv.client.Do(ctx, cmd).Error(); err != nil {
		if rueidis.IsRedisNil(err) {
			return false, nil
		}
		return false, fmt.Errorf("valkey kv %s %s: %w", op, logKey(key), err)
	}
	return true, nil
}

// IncrWithTTL is INCR plus a PTTL read, and a plain PEXPIRE only when the key has no TTL yet. PEXPIRE
// NX would do this in one command but needs Redis/Valkey 7.0; the PTTL check works on any version
// and also re-arms a counter an earlier interrupted call left without a TTL.
func (kv *RedisKV) IncrWithTTL(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	resps := kv.client.DoMulti(ctx,
		kv.client.B().Incr().Key(key).Build(),
		kv.client.B().Pttl().Key(key).Build(),
	)

	n, err := resps[0].AsInt64()
	if err != nil {
		return 0, fmt.Errorf("valkey kv incr %s: %w", logKey(key), err)
	}
	pttl, err := resps[1].AsInt64()
	if err != nil {
		return 0, fmt.Errorf("valkey kv incr %s: read window ttl: %w", logKey(key), err)
	}
	// -1 is "exists without a TTL"; -2 means the window expired after the INCR, so the next call
	// starts a fresh one.
	if pttl == -1 {
		cmd := kv.client.B().Pexpire().Key(key).Milliseconds(ttl.Milliseconds()).Build()
		if expErr := kv.client.Do(ctx, cmd).Error(); expErr != nil {
			return 0, fmt.Errorf("valkey kv incr %s: set window ttl: %w", logKey(key), expErr)
		}
	}
	return n, nil
}

// deleteIfEqualScript is a compare-and-delete in one server-side step. EVAL runs on every Redis and
// Valkey version the chart accepts; Valkey 9's DELIFEQ would do the same on that server only.
const deleteIfEqualScript = `if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) end return 0`

// DeleteIfEqual removes key only if it holds val, atomically on the server.
func (kv *RedisKV) DeleteIfEqual(ctx context.Context, key string, val []byte) (bool, error) {
	cmd := kv.client.B().Eval().Script(deleteIfEqualScript).Numkeys(1).Key(key).Arg(rueidis.BinaryString(val)).Build()
	n, err := kv.client.Do(ctx, cmd).AsInt64()
	if err != nil {
		return false, fmt.Errorf("valkey kv deleteifequal %s: %w", logKey(key), err)
	}
	return n == 1, nil
}

// Delete removes key. DEL on an absent key returns a count of 0 rather than
// an error in Valkey, so this is idempotent without any extra handling.
func (kv *RedisKV) Delete(ctx context.Context, key string) error {
	cmd := kv.client.B().Del().Key(key).Build()
	if err := kv.client.Do(ctx, cmd).Error(); err != nil {
		return fmt.Errorf("valkey kv delete %s: %w", logKey(key), err)
	}
	return nil
}

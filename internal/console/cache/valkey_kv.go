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

// IncrWithTTL is INCR followed by PEXPIRE ... NX, pipelined into a single
// round trip (rueidis DoMulti sends both on the same connection, in order,
// with one write and one read).
//
// Why NX rather than "EXPIRE only when INCR returned 1": INCR returning 1
// does identify the caller that created the window, but making the EXPIRE
// conditional on that in the CLIENT means a client that dies between its INCR
// and its EXPIRE leaves the key with no TTL at all -- and a rate-limit counter
// that never expires is a permanent lockout for that subject. Letting EVERY
// call issue PEXPIRE NX moves the condition into the server: it arms the TTL
// iff the key currently has none, so
//   - the normal case is unchanged (the second and later hits of a window find
//     a TTL already set and NX makes their PEXPIRE a no-op -- the window stays
//     FIXED, never extended);
//   - a concurrent first-hit race is harmless by construction rather than by
//     luck (whichever call wins arms the TTL, the losers no-op);
//   - a TTL-less key left behind by a crash mid-pipeline self-heals on the very
//     next hit, at the cost of that one window starting late, which is the
//     benign direction to fail in.
//
// PEXPIRE ... NX needs Valkey 9 / Redis 7+ (the chart pins valkey:9). The
// milliseconds unit matches Set's Px, so sub-second TTLs survive the trip --
// integration tests rely on that.
func (kv *ValkeyKV) IncrWithTTL(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	resps := kv.client.DoMulti(ctx,
		kv.client.B().Incr().Key(key).Build(),
		kv.client.B().Pexpire().Key(key).Milliseconds(ttl.Milliseconds()).Nx().Build(),
	)

	n, err := resps[0].AsInt64()
	if err != nil {
		return 0, fmt.Errorf("valkey kv incr %s: %w", key, err)
	}
	// The PEXPIRE reply itself (1 = armed, 0 = a TTL was already there) carries
	// no decision -- both are the expected outcomes above -- but a transport or
	// server error on it must not be swallowed: it would mean a key counting up
	// with no expiry, i.e. a limit that never releases.
	if expErr := resps[1].Error(); expErr != nil {
		return 0, fmt.Errorf("valkey kv incr %s: set window ttl: %w", key, expErr)
	}
	return n, nil
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

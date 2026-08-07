package cache

import (
	"context"
	"time"
)

// KV is the Console's short-TTL key/value seam (DATA.md §5.3 "Sessions:
// sess:{id} — instant revocation. M3, with auth"). It is a SIBLING of Bus,
// not an extension of it: BACKEND.md §4.3 calls the Bus seam "deliberately
// frozen", and widening it would break the reasoning the WebSocket
// capability flag rests on (internal/console/httpapi/server.go:150-152).
type KV interface {
	// Get reports (nil, false, nil) on a miss — including an expired key —
	// never an error: a missing entry is not a failure condition for the
	// KV layer, and callers (SessionStore, and eventually rate limits/locks)
	// must be free to treat it as "not present" without inspecting err.
	Get(ctx context.Context, key string) ([]byte, bool, error)
	// Set stores val under key with the given ttl, replacing any existing
	// value and its expiry.
	Set(ctx context.Context, key string, val []byte, ttl time.Duration) error
	// Delete removes key. It is idempotent: deleting an absent key is not
	// an error.
	Delete(ctx context.Context, key string) error
}

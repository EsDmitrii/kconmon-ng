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
	// IncrWithTTL atomically increments key and returns the new value, setting
	// ttl only when the key was created by THIS call (Valkey: INCR + a
	// conditional EXPIRE, so a fixed window starts at its first hit and is not
	// extended by later ones). It is the fixed-window primitive behind both the
	// per-user diagnostics limit (TARGETS.md §7.3) and the login limit
	// (follow-up ticket 3); Bus stays frozen, KV does not (DATA.md §5.3).
	//
	// Unlike Get, a backend failure IS reported here: a rate limiter cannot
	// tell "you are under the limit" apart from "I could not count" on its
	// own, so the decision of what to do about an unreadable backend belongs
	// to the caller (httpapi's limiter fails OPEN and counts it -- a Valkey
	// outage must not become a login outage).
	//
	// A key holding a non-integer value is an error, mirroring Valkey's own
	// INCR contract, rather than a silent reset to 1.
	IncrWithTTL(ctx context.Context, key string, ttl time.Duration) (int64, error)
}

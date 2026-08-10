package cache

import (
	"context"
	"time"
)

// KV is the Console's short-TTL key/value seam; it is a SIBLING of Bus, not an extension.
type KV interface {
	// Get reports (nil, false, nil) on a miss — including an expired key — never an error.
	Get(ctx context.Context, key string) ([]byte, bool, error)
	// Set stores val under key with the given ttl, replacing any existing
	// value and its expiry.
	Set(ctx context.Context, key string, val []byte, ttl time.Duration) error
	// Delete removes key. It is idempotent: deleting an absent key is not
	// an error.
	Delete(ctx context.Context, key string) error
	// IncrWithTTL atomically increments key and returns the new value.
	IncrWithTTL(ctx context.Context, key string, ttl time.Duration) (int64, error)
}

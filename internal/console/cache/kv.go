package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// KV is the Console's short-TTL key/value seam; it is a SIBLING of Bus, not an extension.
type KV interface {
	// Get reports (nil, false, nil) on a miss — including an expired key — never an error.
	Get(ctx context.Context, key string) ([]byte, bool, error)
	// Set stores val under key with the given ttl, replacing any existing
	// value and its expiry.
	Set(ctx context.Context, key string, val []byte, ttl time.Duration) error
	// SetNX stores val under key with ttl only if key is absent or expired, and reports whether it
	// did. It is a lock's acquire: of two concurrent callers, exactly one gets true.
	SetNX(ctx context.Context, key string, val []byte, ttl time.Duration) (bool, error)
	// SetXX replaces key's value and expiry only if key is present, and reports whether it did, so a
	// rewrite can never bring back a key that was deleted in the meantime.
	SetXX(ctx context.Context, key string, val []byte, ttl time.Duration) (bool, error)
	// Delete removes key. It is idempotent: deleting an absent key is not
	// an error.
	Delete(ctx context.Context, key string) error
	// DeleteIfEqual removes key only if it holds val, in one atomic step, and reports whether it did.
	// It is a lock's release: a holder whose lease lapsed cannot delete the lock someone else took since.
	DeleteIfEqual(ctx context.Context, key string, val []byte) (bool, error)
	// IncrWithTTL atomically increments key and returns the new value.
	IncrWithTTL(ctx context.Context, key string, ttl time.Duration) (int64, error)
}

// logKey is key as an error may show it: the prefix up to the first ':' and a short hash of the rest.
// Session keys end in the bearer session id and rate-limit keys in a username or address, and callers
// log these errors.
func logKey(key string) string {
	i := strings.IndexByte(key, ':') + 1
	sum := sha256.Sum256([]byte(key[i:]))
	return key[:i] + "#" + hex.EncodeToString(sum[:4])
}

// Package authn resolves a request into an authz.Subject. This first slice
// is storage only: SessionStore persists Sessions in a cache.KV under
// "sess:{id}" (DATA.md §5.3), giving Delete instant revocation regardless of
// backend. Authenticators that populate a Session from a login flow (local
// password, OIDC) land in Tasks 14-15.
package authn

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
)

// sessionIDBytes is 256 bits of crypto/rand, base64url-encoded (43 chars,
// no padding) into the session id.
const sessionIDBytes = 32

// sessionKeyPrefix namespaces every session in the KV store, exactly as
// DATA.md §5.3 specifies: "sess:{id}".
const sessionKeyPrefix = "sess:"

// ErrSessionNotFound is returned by Refresh for an id that does not resolve
// to a live session (absent, corrupted, or already past ExpiresAt). Get
// reports the same situations as a clean (Session{}, false, nil) instead,
// since a lookup miss is not a failure — but Refresh has no "not found"
// boolean to report through, so it needs a sentinel callers can match with
// errors.Is.
var ErrSessionNotFound = errors.New("authn: session not found")

// Session is what a request resolves to after authentication succeeds. It is
// the server-side record: RefreshToken and AccessExpiry are OIDC-only state
// that is stored here but never sent to the browser.
type Session struct {
	ID          string    `json:"id"`
	Username    string    `json:"username"`
	DisplayName string    `json:"displayName"`
	Groups      []string  `json:"groups"`
	IssuedAt    time.Time `json:"issuedAt"`
	ExpiresAt   time.Time `json:"expiresAt"`

	// OIDC only; never leaves the server.
	RefreshToken string    `json:"refreshToken,omitempty"` //nolint:gosec // not a hardcoded credential; gosec's G117 name heuristic flags any field named *Token
	AccessExpiry time.Time `json:"accessExpiry,omitempty"`
}

// SessionStore persists Sessions in a cache.KV. ttl is the KV entry's TTL
// and Create's default Session.ExpiresAt when the caller leaves it zero;
// Get and Refresh always verify ExpiresAt independently of the KV's own TTL
// (see Get).
type SessionStore struct {
	kv  cache.KV
	ttl time.Duration
}

// NewSessionStore returns a SessionStore backed by kv, issuing sessions with
// lifetime ttl.
func NewSessionStore(kv cache.KV, ttl time.Duration) *SessionStore {
	return &SessionStore{kv: kv, ttl: ttl}
}

func sessionKey(id string) string {
	return sessionKeyPrefix + id
}

// Create issues a fresh 256-bit random id (collision probability is
// cryptographically negligible, so this does not check for reuse) and
// stores sess under "sess:{id}", with the KV entry's expiry derived from the
// session's own ExpiresAt so the two mechanisms can never disagree in the
// dangerous direction (a KV entry evicted while the session claims to be
// live). sess.ID and sess.IssuedAt are always stamped by the store (the
// caller does not choose them); sess.ExpiresAt is defaulted to IssuedAt+ttl
// only when the caller leaves it zero — an explicit caller-supplied
// ExpiresAt (shorter OR longer than the store ttl, or an already-past one in
// a test) is honored as-is: shorter is additionally enforced by Get's
// belt-and-braces check, longer extends the KV TTL to match.
//
//nolint:gocritic // Create(ctx, sess Session) is the task-13-brief.md interface shape verbatim; Session by value, not *Session.
func (s *SessionStore) Create(ctx context.Context, sess Session) (id string, err error) {
	id, err = newSessionID()
	if err != nil {
		return "", fmt.Errorf("authn: create session: %w", err)
	}

	sess.ID = id
	sess.IssuedAt = time.Now()
	if sess.ExpiresAt.IsZero() {
		sess.ExpiresAt = sess.IssuedAt.Add(s.ttl)
	}

	data, err := json.Marshal(sess) //nolint:gosec // G117: server-side session record persisted to the KV store; RefreshToken lives there by design and is never sent to clients
	if err != nil {
		return "", fmt.Errorf("authn: create session: marshal: %w", err)
	}

	// The KV TTL tracks the session's real lifetime; the floor keeps Set
	// well-defined for an already-past ExpiresAt (Get still misses on it via
	// the belt-and-braces check).
	kvTTL := time.Until(sess.ExpiresAt)
	if kvTTL < time.Second {
		kvTTL = time.Second
	}
	if err := s.kv.Set(ctx, sessionKey(id), data, kvTTL); err != nil {
		return "", fmt.Errorf("authn: create session: %w", err)
	}
	return id, nil
}

func newSessionID() (string, error) {
	buf := make([]byte, sessionIDBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// Get looks up id. A miss, a corrupted stored value, and an expired session
// are all reported identically as (Session{}, false, nil) — never an error:
// a failed auth lookup is a 401, not a 500. Expiry is checked against
// Session.ExpiresAt independently of whatever the KV backend's own TTL says
// (belt and braces): an auth decision must not rest on a single expiry
// mechanism. A corrupted value additionally logs a warning, since store
// corruption is operationally interesting even though it must never panic
// or surface a partially-populated Session.
func (s *SessionStore) Get(ctx context.Context, id string) (Session, bool, error) {
	sess, err := s.loadFresh(ctx, id)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return Session{}, false, nil
		}
		return Session{}, false, fmt.Errorf("authn: get session: %w", err)
	}
	return sess, true, nil
}

// Delete removes id from the store. Get on id misses immediately afterward
// regardless of backend — the "instant revocation" DATA.md promises. Like
// cache.KV.Delete, it is idempotent: deleting an absent or already-expired
// id is not an error.
func (s *SessionStore) Delete(ctx context.Context, id string) error {
	if err := s.kv.Delete(ctx, sessionKey(id)); err != nil {
		return fmt.Errorf("authn: delete session: %w", err)
	}
	return nil
}

// Refresh slides id's expiry forward by ttl from now, rewriting both the KV
// entry's TTL and the stored Session.ExpiresAt so the two stay in lockstep
// (Get trusts ExpiresAt independently of the KV TTL, so bumping only the KV
// side would leave Get treating a freshly-refreshed session as expired).
// Refresh applies the same freshness check Get does — an id that is absent,
// corrupted, or already past its old ExpiresAt returns ErrSessionNotFound
// rather than resurrecting it.
func (s *SessionStore) Refresh(ctx context.Context, id string) error {
	sess, err := s.loadFresh(ctx, id)
	if err != nil {
		return fmt.Errorf("authn: refresh session: %w", err)
	}

	sess.ExpiresAt = time.Now().Add(s.ttl)
	data, err := json.Marshal(sess) //nolint:gosec // G117: same server-side session record as Create; never leaves the KV store
	if err != nil {
		return fmt.Errorf("authn: refresh session: marshal: %w", err)
	}

	if err := s.kv.Set(ctx, sessionKey(id), data, s.ttl); err != nil {
		return fmt.Errorf("authn: refresh session: %w", err)
	}
	return nil
}

// loadFresh loads id from the KV and returns it only if it parses and has
// not passed ExpiresAt; every other case (absent, corrupted, expired) comes
// back as ErrSessionNotFound so Get and Refresh can share one freshness
// check.
func (s *SessionStore) loadFresh(ctx context.Context, id string) (Session, error) {
	data, ok, err := s.kv.Get(ctx, sessionKey(id))
	if err != nil {
		return Session{}, err
	}
	if !ok {
		return Session{}, ErrSessionNotFound
	}

	var sess Session
	if err := json.Unmarshal(data, &sess); err != nil {
		slog.Warn("authn: corrupted session value, treating as not found", "id", id, "error", err)
		return Session{}, ErrSessionNotFound
	}

	if !sess.ExpiresAt.IsZero() && time.Now().After(sess.ExpiresAt) {
		return Session{}, ErrSessionNotFound
	}

	return sess, nil
}

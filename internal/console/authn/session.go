// Package authn resolves a request into an authz.Subject; this first slice is storage only:
// SessionStore persists Sessions in a cache.KV under "sess:{id}" (DATA.md §5.3).
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

// ErrSessionNotFound is returned by Refresh for an id that does not resolve to a live session
// (absent, corrupted, or already past ExpiresAt).
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
	// LastSeenAt is when a request last used this session; it is what the idle timeout measures
	// from. Zero on a session issued before the field existed, which reads as IssuedAt.
	LastSeenAt time.Time `json:"lastSeenAt"`
	// PasswordStamp ties a local session to the password it was opened with and to the user's
	// session epoch (SessionStamp): a password change or a disable leaves every older session stale
	// without an index of sessions by user. Empty on sessions issued before 2.5.0, which stay valid
	// until they expire rather than logging everyone out on upgrade.
	PasswordStamp string `json:"passwordStamp,omitempty"`

	// OIDC only; never leaves the server.
	RefreshToken string    `json:"refreshToken,omitempty"` //nolint:gosec // not a hardcoded credential; gosec's G117 name heuristic flags any field named *Token
	AccessExpiry time.Time `json:"accessExpiry"`
}

// SessionStore persists Sessions in a cache.KV.
type SessionStore struct {
	kv   cache.KV
	ttl  time.Duration
	idle time.Duration
}

// NewSessionStore returns a SessionStore backed by kv. ttl is the ABSOLUTE lifetime, counted from
// login and never extended; idle is how long a session may go unused before it is refused, sliding
// forward on every request but never past the absolute deadline. A non-positive idle disables the
// idle check and leaves ttl the only bound.
func NewSessionStore(kv cache.KV, ttl, idle time.Duration) *SessionStore {
	return &SessionStore{kv: kv, ttl: ttl, idle: idle}
}

// touchInterval is how stale LastSeenAt may get before a request writes it back. Sliding on every
// request would put a KV write on every authenticated call for no extra safety; a tenth of the idle
// window keeps the deadline accurate to within that tenth.
func (s *SessionStore) touchInterval() time.Duration {
	return s.idle / 10
}

// errSessionExpired is loadFresh's reason for refusing a session that WAS there, which is what tells
// Get to purge the key rather than leave a dead record behind.
// It wraps ErrSessionNotFound so Refresh keeps its documented contract for callers that only ask
// "is this session gone".
var errSessionExpired = fmt.Errorf("authn: session expired: %w", ErrSessionNotFound)

// idleDeadline is when this session goes stale, or the zero time when the idle check is off.
func (s *SessionStore) idleDeadline(sess *Session) time.Time {
	if s.idle <= 0 {
		return time.Time{}
	}
	from := sess.LastSeenAt
	if from.IsZero() {
		from = sess.IssuedAt
	}
	return from.Add(s.idle)
}

func sessionKey(id string) string {
	return sessionKeyPrefix + id
}

// sessionLockKeyPrefix names the per-session write lock every read-modify-write of a session record
// takes (the idle touch and the OIDC refresh). Replicas share the records through the KV, so an
// in-process mutex cannot stop one from writing back a record older than another's.
const sessionLockKeyPrefix = "sesslock:"

// touchLockTTL bounds how long a replica that dies mid-touch can keep a session's write lock.
const touchLockTTL = 2 * time.Second

// unlockTimeout bounds the release, which runs detached from a request that may already be gone.
const unlockTimeout = 3 * time.Second

// lockSession takes id's write lock for ttl and reports whether it got it; release it with
// unlockSession and the returned token.
func (s *SessionStore) lockSession(ctx context.Context, id string, ttl time.Duration) (token string, ok bool, err error) {
	token, err = newSessionID()
	if err != nil {
		return "", false, fmt.Errorf("lock session: %w", err)
	}
	ok, err = s.kv.SetNX(ctx, sessionLockKeyPrefix+id, []byte(token), ttl)
	if err != nil {
		return "", false, fmt.Errorf("lock session: %w", err)
	}
	return token, ok, nil
}

// unlockSession releases id's write lock if token still holds it; a lock that lapsed and was taken
// by another replica is left alone.
func (s *SessionStore) unlockSession(ctx context.Context, id, token string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unlockTimeout)
	defer cancel()
	if _, err := s.kv.DeleteIfEqual(ctx, sessionLockKeyPrefix+id, []byte(token)); err != nil {
		slog.Warn("authn: could not release a session's write lock", "error", err)
	}
}

// rewrite stores sess over its existing record and reports false when the record is gone (a logout
// or an expiry in the meantime), which it never recreates. Callers hold the session's write lock.
//
//nolint:gocritic // Session is passed by value throughout this package
func (s *SessionStore) rewrite(ctx context.Context, sess Session) (bool, error) {
	data, err := json.Marshal(sess) //nolint:gosec // G117: server-side session record persisted to the KV store; RefreshToken lives there by design and is never sent to clients
	if err != nil {
		return false, fmt.Errorf("marshal: %w", err)
	}
	// The key never outlives the absolute deadline, so an abandoned session disappears on its own.
	kvTTL := max(time.Until(sess.ExpiresAt), time.Second)
	return s.kv.SetXX(ctx, sessionKey(sess.ID), data, kvTTL)
}

// Create issues a fresh 256-bit random id (collision probability is cryptographically negligible,
// so this does not check for reuse) and stores sess under "sess:{id}".
//
//nolint:gocritic // Create(ctx, sess Session) is the interface shape verbatim; Session by value
func (s *SessionStore) Create(ctx context.Context, sess Session) (id string, err error) {
	id, err = newSessionID()
	if err != nil {
		return "", fmt.Errorf("authn: create session: %w", err)
	}

	sess.ID = id
	sess.IssuedAt = time.Now()
	sess.LastSeenAt = sess.IssuedAt
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
	kvTTL := max(time.Until(sess.ExpiresAt), time.Second)
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

// A miss, a corrupted stored value, and an expired session are all reported identically as
// (Session{}, false, nil).
func (s *SessionStore) Get(ctx context.Context, id string) (Session, bool, error) {
	sess, err := s.loadFresh(ctx, id)
	if err != nil {
		if errors.Is(err, errSessionExpired) {
			// Purge rather than wait for the KV TTL: an idle-expired session still has absolute
			// lifetime left on its key, and leaving it there keeps a usable-looking record around.
			if delErr := s.Delete(ctx, id); delErr != nil {
				slog.Warn("authn: could not purge an expired session", "error", delErr)
			}
			return Session{}, false, nil
		}
		if errors.Is(err, ErrSessionNotFound) {
			return Session{}, false, nil
		}
		return Session{}, false, fmt.Errorf("authn: get session: %w", err)
	}

	// This request IS the activity the idle timeout measures, so the deadline slides here.
	if s.idle > 0 && time.Since(sess.LastSeenAt) >= s.touchInterval() {
		if err := s.Refresh(ctx, id); err != nil && !errors.Is(err, ErrSessionNotFound) {
			slog.Warn("authn: could not slide a session's idle deadline", "error", err)
		}
	}
	return sess, true, nil
}

// Delete removes id from the store.
func (s *SessionStore) Delete(ctx context.Context, id string) error {
	if err := s.kv.Delete(ctx, sessionKey(id)); err != nil {
		return fmt.Errorf("authn: delete session: %w", err)
	}
	return nil
}

// Refresh slides id's IDLE deadline to now. The absolute ExpiresAt is never moved: a session that
// keeps being used still ends at the absolute lifetime it was issued with, which is the whole point
// of having two bounds instead of one. While another writer holds the session's write lock (an OIDC
// refresh, or a touch on another replica) it does nothing: the next request slides the deadline.
func (s *SessionStore) Refresh(ctx context.Context, id string) error {
	token, locked, err := s.lockSession(ctx, id, touchLockTTL)
	if err != nil {
		return fmt.Errorf("authn: refresh session: %w", err)
	}
	if !locked {
		return nil
	}
	defer s.unlockSession(ctx, id, token)

	sess, err := s.loadFresh(ctx, id)
	if err != nil {
		return fmt.Errorf("authn: refresh session: %w", err)
	}

	sess.LastSeenAt = time.Now()
	ok, err := s.rewrite(ctx, sess)
	if err != nil {
		return fmt.Errorf("authn: refresh session: %w", err)
	}
	if !ok {
		return fmt.Errorf("authn: refresh session: %w", ErrSessionNotFound)
	}
	return nil
}

// loadFresh loads id from the KV and returns it only if it parses and has not passed ExpiresAt.
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
		slog.Warn("authn: corrupted session value, treating as not found", "error", err)
		return Session{}, ErrSessionNotFound
	}

	now := time.Now()
	if !sess.ExpiresAt.IsZero() && now.After(sess.ExpiresAt) {
		return Session{}, errSessionExpired
	}
	if deadline := s.idleDeadline(&sess); !deadline.IsZero() && now.After(deadline) {
		return Session{}, errSessionExpired
	}

	return sess, nil
}

package authn_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
)

// captureLogs swaps the default slog logger for one writing into the returned buffer; NOTE: the
// tests in this file use t.Parallel.
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return buf
}

// syncBuffer is a mutex-guarded bytes.Buffer.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes())
}

func newStore(t *testing.T, ttl time.Duration) (*authn.SessionStore, *cache.InProcessKV) {
	t.Helper()
	kv := cache.NewInProcessKV()
	t.Cleanup(kv.Close)
	return authn.NewSessionStore(kv, ttl), kv
}

func TestSessionStoreCreateIDShapeAndUniqueness(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t, time.Minute)

	id1, err := store.Create(context.Background(), authn.Session{Username: "alice"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id2, err := store.Create(context.Background(), authn.Session{Username: "bob"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if id1 == id2 {
		t.Fatalf("two Create calls must not collide, both returned %q", id1)
	}

	for _, id := range []string{id1, id2} {
		if len(id) != 43 {
			t.Errorf("expected a 43-char base64url id, got %d chars: %q", len(id), id)
		}
		if _, err := base64.RawURLEncoding.DecodeString(id); err != nil {
			t.Errorf("id %q is not valid base64url: %v", id, err)
		}
	}
}

func TestSessionStoreGetUnknownIDIsCleanMiss(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t, time.Minute)

	sess, ok, err := store.Get(context.Background(), "does-not-exist")
	if err != nil {
		t.Fatalf("a miss must never return an error, got: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for an unknown id")
	}
	if !reflect.DeepEqual(sess, authn.Session{}) {
		t.Errorf("expected a zero-value Session on a miss, got %+v", sess)
	}
}

func TestSessionStoreCreateGetRoundtrip(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t, time.Minute)

	want := authn.Session{
		Username:    "alice",
		DisplayName: "Alice Example",
		Groups:      []string{"admins", "sre"},
	}
	id, err := store.Create(context.Background(), want)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, ok, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("expected a hit right after Create")
	}
	if got.ID != id || got.Username != want.Username || got.DisplayName != want.DisplayName {
		t.Errorf("roundtrip mismatch: got %+v", got)
	}
	if got.IssuedAt.IsZero() || got.ExpiresAt.IsZero() {
		t.Errorf("expected Create to stamp IssuedAt/ExpiresAt, got %+v", got)
	}
}

// TestSessionStoreExpiredSessionIsMissEvenIfKVStillHoldsBytes is the belt-and-braces check.
func TestSessionStoreExpiredSessionIsMissEvenIfKVStillHoldsBytes(t *testing.T) {
	t.Parallel()
	// A long KV ttl: the KV entry itself is nowhere near expiring.
	store, kv := newStore(t, time.Hour)

	id, err := store.Create(context.Background(), authn.Session{
		Username:  "alice",
		ExpiresAt: time.Now().Add(-time.Minute), // already expired, business-level
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Prove the KV itself still has the bytes: cache.KV.Get must be a hit.
	if _, ok, kvErr := kv.Get(context.Background(), "sess:"+id); kvErr != nil || !ok {
		t.Fatalf("expected the KV entry to still be present, got ok=%v err=%v", ok, kvErr)
	}

	sess, ok, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Fatalf("expected a miss for an already-expired session, got %+v", sess)
	}
}

func TestSessionStoreDeleteIsInstantRevocation(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t, time.Minute)

	id, err := store.Create(context.Background(), authn.Session{Username: "alice"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if delErr := store.Delete(context.Background(), id); delErr != nil {
		t.Fatalf("Delete: %v", delErr)
	}

	_, ok, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Fatal("expected Get to miss immediately after Delete")
	}

	// Idempotent, like cache.KV.Delete.
	if delErr := store.Delete(context.Background(), id); delErr != nil {
		t.Fatalf("second Delete must not error: %v", delErr)
	}
}

// TestSessionStoreCorruptedValueIsMissNotPanic proves a corrupted KV entry (e.g. a value written by
// some future incompatible version, or bit rot) degrades to a clean miss plus a logged warning.
func TestSessionStoreCorruptedValueIsMissNotPanic(t *testing.T) {
	t.Parallel()
	logs := captureLogs(t)

	kv := cache.NewInProcessKV()
	t.Cleanup(kv.Close)
	store := authn.NewSessionStore(kv, time.Minute)

	const id = "corrupted-id"
	if err := kv.Set(context.Background(), "sess:"+id, []byte("not json"), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Get panicked on a corrupted value: %v", r)
			}
		}()
		sess, ok, err := store.Get(context.Background(), id)
		if err != nil {
			t.Fatalf("a corrupted value must be reported as a miss, not an error, got: %v", err)
		}
		if ok {
			t.Fatal("expected ok=false for a corrupted value")
		}
		if !reflect.DeepEqual(sess, authn.Session{}) {
			t.Errorf("expected a zero-value Session for a corrupted value, got %+v", sess)
		}
	}()

	if !bytes.Contains(logs.Bytes(), []byte("WARN")) {
		t.Errorf("expected a warning to be logged for the corrupted value, got logs: %s", logs.String())
	}
}

func TestSessionStoreKeyFormatIsSessColonID(t *testing.T) {
	t.Parallel()
	store, kv := newStore(t, time.Minute)

	id, err := store.Create(context.Background(), authn.Session{Username: "alice"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, ok, err := kv.Get(context.Background(), "sess:"+id); err != nil || !ok {
		t.Fatalf("expected the session to be stored under key %q, got ok=%v err=%v", "sess:"+id, ok, err)
	}
}

func TestSessionStoreRefreshUnknownIDIsNotFound(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t, time.Minute)

	err := store.Refresh(context.Background(), "does-not-exist")
	if !errors.Is(err, authn.ErrSessionNotFound) {
		t.Fatalf("expected ErrSessionNotFound, got: %v", err)
	}
}

func TestSessionStoreRefreshSlidesExpiry(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t, 50*time.Millisecond)

	id, err := store.Create(context.Background(), authn.Session{Username: "alice"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	before, ok, err := store.Get(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("Get before refresh: ok=%v err=%v", ok, err)
	}

	time.Sleep(30 * time.Millisecond)
	if refreshErr := store.Refresh(context.Background(), id); refreshErr != nil {
		t.Fatalf("Refresh: %v", refreshErr)
	}

	// Without the refresh, the original 50ms ttl would have expired by now.
	time.Sleep(30 * time.Millisecond)
	after, ok, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get after refresh: %v", err)
	}
	if !ok {
		t.Fatal("expected the session to still be alive after Refresh slid its TTL forward")
	}
	if !after.ExpiresAt.After(before.ExpiresAt) {
		t.Errorf("expected ExpiresAt to move forward: before=%v after=%v", before.ExpiresAt, after.ExpiresAt)
	}
}

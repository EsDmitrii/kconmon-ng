//go:build integration

package store_test

// TestUser*, TestBindings*, TestToken*, TestAudit* require a real PostgreSQL.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// testPasswordHash is a well-formed argon2id PHC string for users the tests create.
const testPasswordHash = "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA"

// disableHoldingTheLock starts a guarded disable of userID whose guard holds the guarded writes' lock
// until another guarded write is parked on it, and returns once that guard runs. The channel yields
// the disable's result.
func disableHoldingTheLock(t *testing.T, db *store.DB, dsn, userID string) <-chan error {
	t.Helper()
	disabled := true
	inGuard := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- db.UpdateUserGuarded(context.Background(), userID, store.UserChange{Disabled: &disabled}, func(ctx context.Context) error {
			close(inGuard)
			return waitForAdvisoryWaiter(ctx, dsn)
		})
	}()
	<-inGuard
	return done
}

// waitForAdvisoryWaiter returns once some session waits on an advisory lock, polling pg_locks on a
// connection of its own.
func waitForAdvisoryWaiter(ctx context.Context, dsn string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("waitForAdvisoryWaiter: connect: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	for {
		var waiting int
		err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`).Scan(&waiting)
		if err != nil {
			return fmt.Errorf("waitForAdvisoryWaiter: no second guarded write reached the lock: %w", err)
		}
		if waiting > 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waitForAdvisoryWaiter: no second guarded write reached the lock: %w", ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// newAuthDB opens a *store.DB with migrations applied, dropping and re-creating the schema first.
func newAuthDB(t *testing.T) (*store.DB, string) {
	t.Helper()
	dsn := testDSN(t)
	dropSchema(t, dsn)
	t.Cleanup(func() { dropSchema(t, dsn) })

	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()

	db, err := store.Open(ctx, dsn, 5, connectTimeout, true)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(db.Close)
	return db, dsn
}

// backdateAuditEntry directly UPDATEs audit_log.at for id.
func backdateAuditEntry(t *testing.T, dsn string, id int64, ageDays int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("backdateAuditEntry: connect: %v", err)
	}
	defer pool.Close()

	at := time.Now().Add(-time.Duration(ageDays) * 24 * time.Hour)
	if _, err := pool.Exec(ctx, `UPDATE audit_log SET at = $1 WHERE id = $2`, at, id); err != nil {
		t.Fatalf("backdateAuditEntry: update: %v", err)
	}
}

// tokenHash returns a stand-in for the real SHA-256(256 random bits) hash.
func tokenHash(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

// ---------------------------------------------------------------------------
// Users
// ---------------------------------------------------------------------------

// TestUserCreateFetchUniqueness covers create -> fetch round trip and the
// UNIQUE(username) constraint surfacing as store.ErrAlreadyExists.
func TestUserCreateFetchUniqueness(t *testing.T) {
	db, _ := newAuthDB(t)
	ctx := context.Background()

	created, err := db.CreateUser(ctx, "alice", "argon2id$fake-hash", "Alice Anderson")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if created.ID == "" {
		t.Fatal("CreateUser: ID is empty, want a generated UUID")
	}
	if created.Username != "alice" || created.DisplayName != "Alice Anderson" || created.Disabled {
		t.Errorf("CreateUser: got %+v", created)
	}

	got, err := db.GetUserByUsername(ctx, "alice")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	if got != created {
		t.Errorf("GetUserByUsername: got %+v, want %+v", got, created)
	}

	if _, err := db.GetUserByUsername(ctx, "nobody"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetUserByUsername(unknown): err = %v, want ErrNotFound", err)
	}

	if _, err := db.CreateUser(ctx, "alice", "argon2id$another-hash", "Alice Duplicate"); !errors.Is(err, store.ErrAlreadyExists) {
		t.Errorf("CreateUser(duplicate username): err = %v, want ErrAlreadyExists", err)
	}
}

// TestUserPasswordAndDisabledLifecycle covers UpdateUserPassword, a disable through
// UpdateUserGuarded, ListUsers and CountUsers together, since they all mutate or read the same row.
func TestUserPasswordAndDisabledLifecycle(t *testing.T) {
	db, _ := newAuthDB(t)
	ctx := context.Background()

	if n, err := db.CountUsers(ctx); err != nil || n != 0 {
		t.Fatalf("CountUsers(empty): n=%d err=%v, want 0, nil", n, err)
	}

	u, err := db.CreateUser(ctx, "bob", "argon2id$initial", "Bob")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if n, countErr := db.CountUsers(ctx); countErr != nil || n != 1 {
		t.Fatalf("CountUsers: n=%d err=%v, want 1, nil", n, countErr)
	}

	if err = db.UpdateUserPassword(ctx, u.ID, "argon2id$rotated"); err != nil {
		t.Fatalf("UpdateUserPassword: %v", err)
	}
	got, err := db.GetUserByUsername(ctx, "bob")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	if got.PasswordHash != "argon2id$rotated" {
		t.Errorf("PasswordHash = %q, want argon2id$rotated", got.PasswordHash)
	}
	if !got.UpdatedAt.After(u.UpdatedAt) && !got.UpdatedAt.Equal(u.UpdatedAt) {
		t.Errorf("UpdatedAt did not advance: before=%v after=%v", u.UpdatedAt, got.UpdatedAt)
	}

	disabled, enabled := true, false
	if err = db.UpdateUserGuarded(ctx, u.ID, store.UserChange{Disabled: &disabled}, nil); err != nil {
		t.Fatalf("UpdateUserGuarded(disable): %v", err)
	}
	got, err = db.GetUserByUsername(ctx, "bob")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	if !got.Disabled {
		t.Error("the disable did not persist")
	}

	users, err := db.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 1 || users[0].Username != "bob" {
		t.Errorf("ListUsers: got %+v, want one user 'bob'", users)
	}

	randomID := "00000000-0000-0000-0000-000000000000"
	if err := db.UpdateUserPassword(ctx, randomID, "x"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UpdateUserPassword(unknown id): err = %v, want ErrNotFound", err)
	}
	if err := db.UpdateUserGuarded(ctx, randomID, store.UserChange{Disabled: &enabled}, nil); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UpdateUserGuarded(unknown id): err = %v, want ErrNotFound", err)
	}
}

// RehashUserPassword is the login's transparent hash upgrade: it must never overwrite a password an
// admin reset after the login verified the old one.
func TestRehashUserPasswordIsACompareAndSwap(t *testing.T) {
	db, _ := newAuthDB(t)
	ctx := context.Background()

	u, err := db.CreateUser(ctx, "carol", "argon2id$legacy", "Carol")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err = db.UpdateUserPassword(ctx, u.ID, "argon2id$admin-reset"); err != nil {
		t.Fatalf("UpdateUserPassword: %v", err)
	}
	swapped, err := db.RehashUserPassword(ctx, u.ID, "argon2id$legacy", "argon2id$legacy-rehashed")
	if err != nil || swapped {
		t.Fatalf("RehashUserPassword over a reset hash = %v, %v; want false, nil", swapped, err)
	}
	if got, _ := db.GetUserByUsername(ctx, "carol"); got.PasswordHash != "argon2id$admin-reset" {
		t.Fatalf("PasswordHash = %q, want the admin reset kept", got.PasswordHash)
	}

	swapped, err = db.RehashUserPassword(ctx, u.ID, "argon2id$admin-reset", "argon2id$rehashed")
	if err != nil || !swapped {
		t.Fatalf("RehashUserPassword over the current hash = %v, %v; want true, nil", swapped, err)
	}
	if got, _ := db.GetUserByUsername(ctx, "carol"); got.PasswordHash != "argon2id$rehashed" {
		t.Errorf("PasswordHash = %q, want argon2id$rehashed", got.PasswordHash)
	}

	randomID := "00000000-0000-0000-0000-000000000000"
	if swapped, err := db.RehashUserPassword(ctx, randomID, "x", "y"); err != nil || swapped {
		t.Errorf("RehashUserPassword(unknown id) = %v, %v; want false, nil", swapped, err)
	}
}

// TestGetUserByIDFoundNotFoundAndDisabledRoundTrip covers the new authn.WithOwnerDisabledCheck
// lookup path directly.
func TestGetUserByIDFoundNotFoundAndDisabledRoundTrip(t *testing.T) {
	db, _ := newAuthDB(t)
	ctx := context.Background()

	created, err := db.CreateUser(ctx, "dave", "argon2id$fake-hash", "Dave Duncan")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	got, err := db.GetUserByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if got.ID != created.ID || got.Username != "dave" || got.DisplayName != "Dave Duncan" || got.Disabled {
		t.Errorf("GetUserByID: got %+v", got)
	}
	if got.PasswordHash != "" {
		t.Errorf("GetUserByID: PasswordHash = %q, want \"\" (never exposed via GetUserByID)", got.PasswordHash)
	}

	randomID := "00000000-0000-0000-0000-000000000000"
	if _, err = db.GetUserByID(ctx, randomID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetUserByID(unknown uuid): err = %v, want ErrNotFound", err)
	}
	if _, err = db.GetUserByID(ctx, "not-a-uuid"); err == nil {
		t.Error("GetUserByID(malformed id): err = nil, want a non-nil error")
	}

	disabled := true
	if err = db.UpdateUserGuarded(ctx, created.ID, store.UserChange{Disabled: &disabled}, nil); err != nil {
		t.Fatalf("UpdateUserGuarded(disable): %v", err)
	}
	got, err = db.GetUserByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetUserByID (after disable): %v", err)
	}
	if !got.Disabled {
		t.Error("GetUserByID: the disable did not round-trip")
	}
}

// TestListUsersNeverExposesPasswordHash mirrors TestListTokensNeverExposesHash (below, for
// api_tokens.token_hash).
func TestListUsersNeverExposesPasswordHash(t *testing.T) {
	db, _ := newAuthDB(t)
	ctx := context.Background()

	if _, err := db.CreateUser(ctx, "carol", "argon2id$real-hash", "Carol"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	users, err := db.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 1 || users[0].Username != "carol" {
		t.Fatalf("ListUsers: got %+v, want one user 'carol'", users)
	}
	if users[0].PasswordHash != "" {
		t.Errorf("ListUsers: PasswordHash = %q, want \"\" (never exposed via ListUsers)", users[0].PasswordHash)
	}

	got, err := db.GetUserByUsername(ctx, "carol")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	if got.PasswordHash != "argon2id$real-hash" {
		t.Errorf("GetUserByUsername: PasswordHash = %q, want argon2id$real-hash", got.PasswordHash)
	}
}

// ---------------------------------------------------------------------------
// Roles and bindings
// ---------------------------------------------------------------------------

// TestRoleUpsertListDelete covers the custom-role CRUD path.
func TestRoleUpsertListDelete(t *testing.T) {
	db, _ := newAuthDB(t)
	ctx := context.Background()

	r, err := db.UpsertRole(ctx, "on-call", []string{"events:read", "runs:read"})
	if err != nil {
		t.Fatalf("UpsertRole: %v", err)
	}
	if r.Name != "on-call" || len(r.Permissions) != 2 {
		t.Errorf("UpsertRole: got %+v", r)
	}

	// Upsert again with different permissions: same name, new permission set.
	r2, err := db.UpsertRole(ctx, "on-call", []string{"events:read"})
	if err != nil {
		t.Fatalf("UpsertRole (update): %v", err)
	}
	if len(r2.Permissions) != 1 || r2.Permissions[0] != "events:read" {
		t.Errorf("UpsertRole (update): got %+v, want permissions=[events:read]", r2)
	}

	roles, err := db.ListRoles(ctx)
	if err != nil {
		t.Fatalf("ListRoles: %v", err)
	}
	if len(roles) != 1 {
		t.Fatalf("ListRoles: got %d roles, want 1", len(roles))
	}

	if err := db.DeleteRole(ctx, "on-call"); err != nil {
		t.Fatalf("DeleteRole: %v", err)
	}
	if err := db.DeleteRole(ctx, "on-call"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DeleteRole (already deleted): err = %v, want ErrNotFound", err)
	}
}

// TestListBindingsForSubjectResolvesUserAndGroupInOneCall is the core binding assertion.
func TestListBindingsForSubjectResolvesUserAndGroupInOneCall(t *testing.T) {
	db, _ := newAuthDB(t)
	ctx := context.Background()

	userBinding, err := db.CreateBinding(ctx, "operator", "user", "alice")
	if err != nil {
		t.Fatalf("CreateBinding(user): %v", err)
	}
	groupBinding, err := db.CreateBinding(ctx, "viewer", "group", "sre-team")
	if err != nil {
		t.Fatalf("CreateBinding(group): %v", err)
	}
	if _, err = db.CreateBinding(ctx, "admin", "user", "mallory"); err != nil {
		t.Fatalf("CreateBinding(unrelated user): %v", err)
	}
	if _, err = db.CreateBinding(ctx, "admin", "group", "other-team"); err != nil {
		t.Fatalf("CreateBinding(unrelated group): %v", err)
	}

	got, err := db.ListBindingsForSubject(ctx, "user", "alice", []string{"sre-team", "another-group"})
	if err != nil {
		t.Fatalf("ListBindingsForSubject: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListBindingsForSubject: got %d bindings, want 2: %+v", len(got), got)
	}
	byRole := make(map[string]store.RoleBinding, len(got))
	for _, b := range got {
		byRole[b.RoleName] = b
	}
	if b, ok := byRole["operator"]; !ok || b.ID != userBinding.ID {
		t.Errorf("missing/mismatched user binding: %+v", byRole["operator"])
	}
	if b, ok := byRole["viewer"]; !ok || b.ID != groupBinding.ID {
		t.Errorf("missing/mismatched group binding: %+v", byRole["viewer"])
	}

	// A subject with no group memberships gets only its own bindings.
	onlyUser, err := db.ListBindingsForSubject(ctx, "user", "alice", nil)
	if err != nil {
		t.Fatalf("ListBindingsForSubject(no groups): %v", err)
	}
	if len(onlyUser) != 1 || onlyUser[0].RoleName != "operator" {
		t.Errorf("ListBindingsForSubject(no groups): got %+v, want just the operator binding", onlyUser)
	}

	/* And the caller's KIND is part of the match, not decoration. A token whose id happens to equal a
	   user's binding subject used to resolve that user's role through the 'user' branch — the RBAC
	   API refuses to CREATE a 'token' binding, so this was the way in. */
	asToken, err := db.ListBindingsForSubject(ctx, "token", "alice", nil)
	if err != nil {
		t.Fatalf("ListBindingsForSubject(token kind): %v", err)
	}
	if len(asToken) != 0 {
		t.Errorf("ListBindingsForSubject(token kind): got %+v, want none — a token must not inherit a user binding", asToken)
	}

	if err := db.DeleteBinding(ctx, userBinding.ID); err != nil {
		t.Fatalf("DeleteBinding: %v", err)
	}
	if err := db.DeleteBinding(ctx, userBinding.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DeleteBinding (already deleted): err = %v, want ErrNotFound", err)
	}

	if _, err := db.CreateBinding(ctx, "viewer", "group", "sre-team"); !errors.Is(err, store.ErrAlreadyExists) {
		t.Errorf("CreateBinding (duplicate role/subject): err = %v, want ErrAlreadyExists", err)
	}
}

// TestListBindingsReturnsEveryBindingUnscoped is the addition to RoleStore.
func TestListBindingsReturnsEveryBindingUnscoped(t *testing.T) {
	db, _ := newAuthDB(t)
	ctx := context.Background()

	a, err := db.CreateBinding(ctx, "operator", "user", "alice")
	if err != nil {
		t.Fatalf("CreateBinding(user): %v", err)
	}
	b, err := db.CreateBinding(ctx, "viewer", "group", "sre-team")
	if err != nil {
		t.Fatalf("CreateBinding(group): %v", err)
	}

	got, err := db.ListBindings(ctx)
	if err != nil {
		t.Fatalf("ListBindings: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListBindings: got %d bindings, want 2: %+v", len(got), got)
	}
	byID := make(map[int64]store.RoleBinding, len(got))
	for _, binding := range got {
		byID[binding.ID] = binding
	}
	if bound, ok := byID[a.ID]; !ok || bound.RoleName != "operator" || bound.SubjectKind != "user" || bound.SubjectID != "alice" {
		t.Errorf("missing/mismatched user binding: %+v", byID[a.ID])
	}
	if bound, ok := byID[b.ID]; !ok || bound.RoleName != "viewer" || bound.SubjectKind != "group" || bound.SubjectID != "sre-team" {
		t.Errorf("missing/mismatched group binding: %+v", byID[b.ID])
	}

	if delErr := db.DeleteBinding(ctx, a.ID); delErr != nil {
		t.Fatalf("DeleteBinding: %v", delErr)
	}
	got, err = db.ListBindings(ctx)
	if err != nil {
		t.Fatalf("ListBindings after delete: %v", err)
	}
	if len(got) != 1 || got[0].ID != b.ID {
		t.Fatalf("ListBindings after delete: got %+v, want just the group binding", got)
	}
}

// ---------------------------------------------------------------------------
// Tokens
// ---------------------------------------------------------------------------

// TestGetTokenByHashUnknownHashReturnsNotFound hits the unique index on a
// hash nothing has ever written and asserts the lookup returns nothing.
func TestGetTokenByHashUnknownHashReturnsNotFound(t *testing.T) {
	db, _ := newAuthDB(t)
	ctx := context.Background()

	if _, err := db.GetTokenByHash(ctx, tokenHash("never-issued")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetTokenByHash(unknown): err = %v, want ErrNotFound", err)
	}
}

// TestTokenLifecycleRevokedExpiredAndUnknownAreDistinguishable creates a live token, a revoked
// token and an expired token.
func TestTokenLifecycleRevokedExpiredAndUnknownAreDistinguishable(t *testing.T) {
	db, _ := newAuthDB(t)
	ctx := context.Background()

	live, err := db.CreateToken(ctx, "ci-runner", tokenHash("live-token"), "system", nil)
	if err != nil {
		t.Fatalf("CreateToken(live): %v", err)
	}
	if live.RevokedAt != nil || live.ExpiresAt != nil {
		t.Errorf("live token: got RevokedAt=%v ExpiresAt=%v, want both nil", live.RevokedAt, live.ExpiresAt)
	}

	toRevoke, err := db.CreateToken(ctx, "old-integration", tokenHash("revoked-token"), "alice", nil)
	if err != nil {
		t.Fatalf("CreateToken(to-revoke): %v", err)
	}
	if err = db.RevokeToken(ctx, toRevoke.ID); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}

	past := time.Now().Add(-time.Hour)
	_, err = db.CreateToken(ctx, "expired", tokenHash("expired-token"), "bob", &past)
	if err != nil {
		t.Fatalf("CreateToken(expired): %v", err)
	}

	gotLive, err := db.GetTokenByHash(ctx, tokenHash("live-token"))
	if err != nil {
		t.Fatalf("GetTokenByHash(live): %v", err)
	}
	if gotLive.RevokedAt != nil || gotLive.ExpiresAt != nil {
		t.Errorf("live token round-trip: got RevokedAt=%v ExpiresAt=%v, want both nil", gotLive.RevokedAt, gotLive.ExpiresAt)
	}

	gotRevoked, err := db.GetTokenByHash(ctx, tokenHash("revoked-token"))
	if err != nil {
		t.Fatalf("GetTokenByHash(revoked): %v", err)
	}
	if gotRevoked.RevokedAt == nil {
		t.Error("revoked token: RevokedAt is nil, want set")
	}
	if gotRevoked.ExpiresAt != nil {
		t.Error("revoked token: ExpiresAt is set, want nil")
	}

	gotExpired, err := db.GetTokenByHash(ctx, tokenHash("expired-token"))
	if err != nil {
		t.Fatalf("GetTokenByHash(expired): %v", err)
	}
	if gotExpired.ExpiresAt == nil || !gotExpired.ExpiresAt.Before(time.Now()) {
		t.Errorf("expired token: ExpiresAt = %v, want a past timestamp", gotExpired.ExpiresAt)
	}
	if gotExpired.RevokedAt != nil {
		t.Error("expired token: RevokedAt is set, want nil")
	}

	if _, err = db.GetTokenByHash(ctx, tokenHash("never-issued")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetTokenByHash(unknown): err = %v, want ErrNotFound", err)
	}

	// RevokeToken on an already-revoked token: the WHERE revoked_at IS NULL
	// guard means 0 rows, surfaced as ErrNotFound, not silently ok.
	if err = db.RevokeToken(ctx, toRevoke.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("RevokeToken(already revoked): err = %v, want ErrNotFound", err)
	}

	if err = db.TouchTokenLastUsed(ctx, live.ID); err != nil {
		t.Fatalf("TouchTokenLastUsed: %v", err)
	}
	touched, err := db.GetTokenByHash(ctx, tokenHash("live-token"))
	if err != nil {
		t.Fatalf("GetTokenByHash(after touch): %v", err)
	}
	if touched.LastUsedAt == nil {
		t.Error("TouchTokenLastUsed did not set LastUsedAt")
	}

	// A duplicate token_hash (astronomically unlikely for real random tokens,
	// but the unique index and this package's handling of it are still
	// exercised here) surfaces as ErrAlreadyExists.
	if _, err := db.CreateToken(ctx, "dup", tokenHash("live-token"), "system", nil); !errors.Is(err, store.ErrAlreadyExists) {
		t.Errorf("CreateToken(duplicate hash): err = %v, want ErrAlreadyExists", err)
	}
}

// TestListTokensNeverExposesHash asserts ListTokens's result set has no way
// to leak a token_hash: store.Token simply has no such field, so this test
// pins that the returned tokens still carry every other field correctly.
func TestListTokensNeverExposesHash(t *testing.T) {
	db, _ := newAuthDB(t)
	ctx := context.Background()

	if _, err := db.CreateToken(ctx, "tok-a", tokenHash("a"), "alice", nil); err != nil {
		t.Fatalf("CreateToken(a): %v", err)
	}
	if _, err := db.CreateToken(ctx, "tok-b", tokenHash("b"), "bob", nil); err != nil {
		t.Fatalf("CreateToken(b): %v", err)
	}

	tokens, err := db.ListTokens(ctx)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(tokens) != 2 {
		t.Fatalf("ListTokens: got %d tokens, want 2", len(tokens))
	}
	byName := make(map[string]store.Token, len(tokens))
	for _, tok := range tokens {
		byName[tok.Name] = tok
	}
	if byName["tok-a"].Owner != "alice" || byName["tok-b"].Owner != "bob" {
		t.Errorf("ListTokens: got %+v", tokens)
	}
}

// TestGetTokenByIDRoundTrip is the primitive ListTokens' full scan used to stand in for on the mint
// path; it also pins the three boundaries a single-row lookup has to get right.
func TestGetTokenByIDRoundTrip(t *testing.T) {
	db, _ := newAuthDB(t)
	ctx := context.Background()

	expires := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Millisecond)
	created, err := db.CreateToken(ctx, "ci-runner", tokenHash("by-id"), "alice", &expires)
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	got, err := db.GetTokenByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetTokenByID(%s): %v", created.ID, err)
	}
	if got.ID != created.ID || got.Name != "ci-runner" || got.Owner != "alice" {
		t.Errorf("GetTokenByID = %+v, want id/name/owner to match the created token %+v", got, created)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(expires) {
		t.Errorf("GetTokenByID: ExpiresAt = %v, want %v", got.ExpiresAt, expires)
	}
	if got.RevokedAt != nil || got.LastUsedAt != nil {
		t.Errorf("GetTokenByID on a fresh token: RevokedAt = %v, LastUsedAt = %v, want both nil", got.RevokedAt, got.LastUsedAt)
	}

	// A well-formed id that names no row: ErrNotFound.
	const absent = "0e2a6b3c-1f4d-4a7b-9c8e-3d5f7a9b1c2e"
	if _, err = db.GetTokenByID(ctx, absent); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetTokenByID(unknown uuid): err = %v, want ErrNotFound", err)
	}

	// A malformed id is NOT ErrNotFound (see the unit test in auth_test.go for
	// the full table; this pins the distinction against a real database too,
	// so the answer cannot differ between the pre-check and the query).
	if _, err = db.GetTokenByID(ctx, "not-a-uuid"); err == nil || errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetTokenByID(malformed): err = %v, want a parse error that is not ErrNotFound", err)
	}

	// Revoked still resolves, with RevokedAt now set.
	if err = db.RevokeToken(ctx, created.ID); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	revoked, err := db.GetTokenByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetTokenByID after revoke: %v", err)
	}
	if revoked.RevokedAt == nil {
		t.Error("GetTokenByID after revoke: RevokedAt is nil, want it set")
	}
	if revoked.Owner != "alice" {
		t.Errorf("GetTokenByID after revoke: Owner = %q, want it unchanged", revoked.Owner)
	}
}

// ---------------------------------------------------------------------------
// Audit log
// ---------------------------------------------------------------------------

// TestAuditInsertAndKeysetPagedList covers InsertAuditEntry followed by a
// keyset-paged ListAuditEntries, the same shape TestListEventsPagesWithoutDuplicatesOrGaps
// exercises for topology_events.
func TestAuditInsertAndKeysetPagedList(t *testing.T) {
	db, _ := newAuthDB(t)
	ctx := context.Background()

	const total = 25
	for i := range total {
		_, err := db.InsertAuditEntry(ctx, "user", "alice", "POST /api/v1/runs", "runs", "allowed", "127.0.0.1", nil)
		if err != nil {
			t.Fatalf("InsertAuditEntry(%d): %v", i, err)
		}
	}

	var (
		seen      = make(map[int64]bool, total)
		pageSizes []int
		cursor    string
	)
	for {
		page, err := db.ListAuditEntries(ctx, store.AuditFilter{Limit: 10, Cursor: cursor})
		if err != nil {
			t.Fatalf("ListAuditEntries: %v", err)
		}
		pageSizes = append(pageSizes, len(page.Entries))
		for _, e := range page.Entries {
			if seen[e.ID] {
				t.Fatalf("ListAuditEntries: duplicate id %d across pages", e.ID)
			}
			seen[e.ID] = true
			if string(e.Detail) != "{}" {
				t.Errorf("entry %d: Detail = %s, want the column default {}", e.ID, e.Detail)
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if len(pageSizes) > total {
			t.Fatal("ListAuditEntries: paging did not terminate")
		}
	}

	if want := []int{10, 10, 5}; len(pageSizes) != len(want) {
		t.Fatalf("page sizes = %v, want %v", pageSizes, want)
	} else {
		for i := range want {
			if pageSizes[i] != want[i] {
				t.Errorf("page %d size = %d, want %d", i, pageSizes[i], want[i])
			}
		}
	}
	if len(seen) != total {
		t.Errorf("saw %d distinct entries across all pages, want %d", len(seen), total)
	}
}

// TestAuditListFiltersBySubject asserts subject_kind/subject_id filtering.
func TestAuditListFiltersBySubject(t *testing.T) {
	db, _ := newAuthDB(t)
	ctx := context.Background()

	mustInsertAudit(ctx, t, db, "user", "alice")
	mustInsertAudit(ctx, t, db, "user", "bob")
	mustInsertAudit(ctx, t, db, "token", "ci-runner")

	page, err := db.ListAuditEntries(ctx, store.AuditFilter{SubjectKind: "user", SubjectID: "alice"})
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}
	if len(page.Entries) != 1 || page.Entries[0].SubjectID != "alice" {
		t.Errorf("ListAuditEntries(user,alice): got %+v", page.Entries)
	}
}

// An audit row is written whatever the recorded fields carry. PostgreSQL refuses a JSONB value with
// a lone UTF-16 surrogate escape, a \u0000 escape, invalid UTF-8 or a number numeric cannot hold, and
// a TEXT value with NUL or invalid UTF-8; before, such a detail made the insert fail and the action
// went unrecorded.
func TestAuditInsertStoresARowWhateverTheFieldsCarry(t *testing.T) {
	db, _ := newAuthDB(t)
	ctx := context.Background()

	cases := []struct {
		name   string
		detail string
		want   string // substring of the stored detail
	}{
		{"lone high surrogate", `{"name":"ci-\ud800-token"}`, "ci-\ufffd-token"},
		{"lone low surrogate", `{"name":"x\udc00y"}`, "x\ufffdy"},
		{"NUL escape", `{"name":"a\u0000b"}`, "a\ufffdb"},
		{"surrogate in a key", `{"\ud800":"v"}`, "\ufffd"},
		{"invalid UTF-8", "{\"name\":\"bad-\xff-name\"}", "bad-\ufffd-name"},
		{"not JSON at all", `{"name":`, "unstorable"},
		{"number beyond numeric's range", `{"username":1e200000}`, `"1e200000"`},
		{"number beyond numeric's scale", `{"username":1e-20000}`, `"1e-20000"`},
	}
	newest := func(name string, id int64) store.AuditEntry {
		t.Helper()
		page, err := db.ListAuditEntries(ctx, store.AuditFilter{Limit: 1})
		if err != nil {
			t.Fatalf("ListAuditEntries: %v", err)
		}
		if len(page.Entries) != 1 || page.Entries[0].ID != id {
			t.Fatalf("%s: the newest row is not the one just inserted: %+v", name, page.Entries)
		}
		return page.Entries[0]
	}
	for _, tc := range cases {
		e, err := db.InsertAuditEntry(ctx, "user", "mallory", "POST /api/v1/tokens", "tokens",
			"allowed", "127.0.0.1", json.RawMessage(tc.detail))
		if err != nil {
			t.Errorf("%s: InsertAuditEntry: %v", tc.name, err)
			continue
		}
		if got := newest(tc.name, e.ID); !strings.Contains(string(got.Detail), tc.want) {
			t.Errorf("%s: stored detail %s, want it to contain %q", tc.name, got.Detail, tc.want)
		}
	}

	e, err := db.InsertAuditEntry(ctx, "user", "mallory\x00\xff", "POST /api/v1/tokens", "tokens\xfe",
		"allowed", "127.0.0.1", nil)
	if err != nil {
		t.Fatalf("NUL and invalid UTF-8 in the text columns: InsertAuditEntry: %v", err)
	}
	if got := newest("text columns", e.ID); got.SubjectID != "mallory\ufffd\ufffd" || got.Resource != "tokens\ufffd" {
		t.Errorf("stored subject %q resource %q", got.SubjectID, got.Resource)
	}
}

func mustInsertAudit(ctx context.Context, t *testing.T, db *store.DB, subjectKind, subjectID string) {
	t.Helper()
	if _, err := db.InsertAuditEntry(ctx, subjectKind, subjectID, "GET /api/v1/topology", "topology", "allowed", "127.0.0.1", nil); err != nil {
		t.Fatalf("InsertAuditEntry(%s,%s): %v", subjectKind, subjectID, err)
	}
}

// TestPrunerDeletesOldAuditLogRows is the retention-side counterpart of
// TestPruneOnceDeletesRowsPastRetention (prune_integration_test.go).
func TestPrunerDeletesOldAuditLogRows(t *testing.T) {
	db, dsn := newAuthDB(t)
	ctx := context.Background()

	seedAuditEntryAtAge(ctx, t, db, dsn, 1)
	seedAuditEntryAtAge(ctx, t, db, dsn, 2)
	seedAuditEntryAtAge(ctx, t, db, dsn, 200)
	seedAuditEntryAtAge(ctx, t, db, dsn, 201)

	p := store.NewPruner(db, retention90d, newTestMetrics())
	deleted, err := p.PruneOnce(ctx)
	if err != nil {
		t.Fatalf("PruneOnce: %v", err)
	}
	if got := deleted["audit_log"]; got != 2 {
		t.Fatalf("PruneOnce: deleted[audit_log] = %d, want 2", got)
	}

	page, err := db.ListAuditEntries(ctx, store.AuditFilter{})
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}
	if len(page.Entries) != 2 {
		t.Fatalf("rows remaining after PruneOnce = %d, want 2", len(page.Entries))
	}
}

// seedAuditEntryAtAge inserts one audit_log row via the store's own InsertAuditEntry (which always
// stamps `at` as now) and then backdates it with a direct UPDATE.
func seedAuditEntryAtAge(ctx context.Context, t *testing.T, db *store.DB, dsn string, ageDays int) {
	t.Helper()
	e, err := db.InsertAuditEntry(ctx, "user", "seed", "GET /api/v1/topology", "topology", "allowed", "127.0.0.1", nil)
	if err != nil {
		t.Fatalf("seedAuditEntryAtAge: InsertAuditEntry: %v", err)
	}
	backdateAuditEntry(t, dsn, e.ID, ageDays)
}

func TestCreateUserWithRoleBindsInOneTransaction(t *testing.T) {
	db, _ := newAuthDB(t)
	ctx := context.Background()

	u, err := db.CreateUserWithRole(ctx, "bob", "argon2id$fake-hash", "Bob", "operator")
	if err != nil {
		t.Fatalf("CreateUserWithRole: %v", err)
	}
	bindings, err := db.ListBindings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range bindings {
		if b.SubjectKind == "user" && b.SubjectID == u.ID && b.RoleName == "operator" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no operator binding for %s in %+v", u.ID, bindings)
	}

	before := len(bindings)
	if _, err = db.CreateUserWithRole(ctx, "bob", "argon2id$other", "Bob Again", "admin"); !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("duplicate username = %v, want ErrAlreadyExists", err)
	}
	after, err := db.ListBindings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != before {
		t.Fatalf("a failed create left %d bindings behind", len(after)-before)
	}
}

// Both user creates word and meter a failed binding insert the same way, and leave no user behind.
func TestCreateUserBindingFailureReadsTheSameOnBothPaths(t *testing.T) {
	db, _ := newAuthDB(t)
	m := newTestMetrics()
	db.SetMetrics(m)
	ctx := context.Background()
	const badRole = "view\x00er" // TEXT refuses NUL, so the binding insert fails after the user insert
	_, plainErr := db.CreateUserWithRole(ctx, "plain", testPasswordHash, "P", badRole)
	_, guardedErr := db.CreateUserWithRoleGuarded(ctx, "guarded", testPasswordHash, "G", badRole, nil)
	for name, err := range map[string]error{"CreateUserWithRole": plainErr, "CreateUserWithRoleGuarded": guardedErr} {
		if err == nil || !strings.HasPrefix(err.Error(), "store: create user: binding: ") {
			t.Errorf("%s with an unstorable role = %v, want a \"store: create user: binding: \" error", name, err)
		}
	}
	if got := testutil.ToFloat64(m.StoreQueries.WithLabelValues("CreateUser", "error")); got != 2 {
		t.Errorf(`store_queries_total{query="CreateUser",result="error"} = %v, want 2`, got)
	}
	for _, username := range []string{"plain", "guarded"} {
		if _, err := db.GetUserByUsername(ctx, username); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("user %s survived its failed binding: %v", username, err)
		}
	}
}

// A role change replaces the user's direct bindings and leaves group bindings alone.
func TestUpdateUserGuardedRoleReplacesOnlyTheUsersDirectBindings(t *testing.T) {
	db, _ := newAuthDB(t)
	ctx := context.Background()
	u, err := db.CreateUserWithRole(ctx, "carol", "argon2id$fake", "Carol", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.CreateBinding(ctx, "operator", "group", "sre"); err != nil {
		t.Fatal(err)
	}
	role := "admin"
	if err = db.UpdateUserGuarded(ctx, u.ID, store.UserChange{Role: &role}, nil); err != nil {
		t.Fatalf("UpdateUserGuarded(role): %v", err)
	}
	bindings, err := db.ListBindings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var direct []string
	groupKept := false
	for _, b := range bindings {
		if b.SubjectKind == "user" && b.SubjectID == u.ID {
			direct = append(direct, b.RoleName)
		}
		if b.SubjectKind == "group" && b.SubjectID == "sre" {
			groupKept = true
		}
	}
	if len(direct) != 1 || direct[0] != "admin" {
		t.Fatalf("direct bindings = %v, want exactly [admin]", direct)
	}
	if !groupKept {
		t.Fatal("a group binding was removed by a user role change")
	}
}

// Two role changes for one user at the same moment serialise, so the user ends with one direct role
// rather than both.
func TestUpdateUserGuardedConcurrentRoleChangesLeaveOneDirectRole(t *testing.T) {
	db, _ := newAuthDB(t)
	ctx := context.Background()

	for round := range 30 {
		u, err := db.CreateUserWithRole(ctx, fmt.Sprintf("race-%d", round), "argon2id$fake", "", "viewer")
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		errs := make(chan error, 2)
		for _, role := range []string{"admin", "operator"} {
			go func() {
				<-start
				errs <- db.UpdateUserGuarded(ctx, u.ID, store.UserChange{Role: &role}, nil)
			}()
		}
		close(start)
		for range 2 {
			if setErr := <-errs; setErr != nil {
				t.Fatalf("round %d: UpdateUserGuarded(role): %v", round, setErr)
			}
		}

		bindings, err := db.ListBindings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var direct []string
		for _, b := range bindings {
			if b.SubjectKind == "user" && b.SubjectID == u.ID {
				direct = append(direct, b.RoleName)
			}
		}
		if len(direct) != 1 {
			t.Fatalf("round %d: direct bindings = %v, want exactly one", round, direct)
		}
	}
}

func TestUpdateUserGuardedRoleOfAnUnknownUserIsNotFound(t *testing.T) {
	db, _ := newAuthDB(t)
	role := "admin"
	err := db.UpdateUserGuarded(context.Background(), "3f1d1a2f-6f8e-4a3a-9a0e-7f3f9d0f1c22", store.UserChange{Role: &role}, nil)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("UpdateUserGuarded(role, unknown id) = %v, want ErrNotFound", err)
	}
}

// Every disable bumps the session epoch the local session stamp carries; enabling does not.
func TestUserDisableBumpsTheSessionEpoch(t *testing.T) {
	db, _ := newAuthDB(t)
	ctx := context.Background()
	u, err := db.CreateUserWithRole(ctx, "epoch", testPasswordHash, "Epoch", "viewer")
	if err != nil {
		t.Fatalf("CreateUserWithRole: %v", err)
	}
	epoch := func() int64 {
		t.Helper()
		got, err := db.GetUserByUsername(ctx, "epoch")
		if err != nil {
			t.Fatalf("GetUserByUsername: %v", err)
		}
		return got.SessionEpoch
	}
	if got := epoch(); got != 0 {
		t.Fatalf("new user epoch = %d, want 0", got)
	}
	disabled, enabled := true, false
	for i, change := range []*bool{&disabled, &enabled, &disabled} {
		if err := db.UpdateUserGuarded(ctx, u.ID, store.UserChange{Disabled: change}, nil); err != nil {
			t.Fatalf("change %d: %v", i, err)
		}
	}
	if got := epoch(); got != 2 {
		t.Errorf("epoch after disable, enable, disable = %d, want 2", got)
	}
}

// The check and the write of one guarded change run under a lock every guarded change takes, so a
// second change's check sees the first change's write.
func TestUpdateUserGuardedSerializesTheCheckWithTheWrite(t *testing.T) {
	db, dsn := newAuthDB(t)
	ctx := context.Background()
	a, err := db.CreateUserWithRole(ctx, "a", testPasswordHash, "A", "admin")
	if err != nil {
		t.Fatal(err)
	}
	b, err := db.CreateUserWithRole(ctx, "b", testPasswordHash, "B", "admin")
	if err != nil {
		t.Fatal(err)
	}
	disabled := true
	firstDone := disableHoldingTheLock(t, db, dsn, a.ID)
	var sawFirstWrite bool
	err = db.UpdateUserGuarded(ctx, b.ID, store.UserChange{Disabled: &disabled}, func(ctx context.Context) error {
		first, gerr := db.GetUserByID(ctx, a.ID)
		if gerr != nil {
			return gerr
		}
		sawFirstWrite = first.Disabled
		if first.Disabled {
			return errors.New("last admin")
		}
		return nil
	})
	if ferr := <-firstDone; ferr != nil {
		t.Fatalf("first change: %v", ferr)
	}
	if !sawFirstWrite || err == nil || err.Error() != "last admin" {
		t.Fatalf("second guard saw the first write = %v, second change err = %v; want true and the guard's own error", sawFirstWrite, err)
	}
	if got, _ := db.GetUserByID(ctx, b.ID); got.Disabled {
		t.Error("a change whose guard refused was written anyway")
	}
}

// A guarded change carrying both fields lands whole or not at all, and names ErrNotFound for an
// unknown user.
func TestUpdateUserGuardedIsAllOrNothing(t *testing.T) {
	db, _ := newAuthDB(t)
	ctx := context.Background()
	u, err := db.CreateUserWithRole(ctx, "whole", testPasswordHash, "W", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	disabled, role := true, "operator"
	refused := errors.New("refused")
	if err := db.UpdateUserGuarded(ctx, u.ID, store.UserChange{Disabled: &disabled, Role: &role},
		func(context.Context) error { return refused }); !errors.Is(err, refused) {
		t.Fatalf("refused change = %v, want the guard's error", err)
	}
	got, _ := db.GetUserByID(ctx, u.ID)
	bindings, _ := db.ListBindingsForSubject(ctx, "user", u.ID, nil)
	if got.Disabled || len(bindings) != 1 || bindings[0].RoleName != "viewer" {
		t.Fatalf("a refused change wrote something: disabled %v, bindings %+v", got.Disabled, bindings)
	}

	if err := db.UpdateUserGuarded(ctx, u.ID, store.UserChange{Disabled: &disabled, Role: &role}, nil); err != nil {
		t.Fatalf("UpdateUserGuarded: %v", err)
	}
	got, _ = db.GetUserByID(ctx, u.ID)
	bindings, _ = db.ListBindingsForSubject(ctx, "user", u.ID, nil)
	if !got.Disabled || len(bindings) != 1 || bindings[0].RoleName != "operator" {
		t.Fatalf("change = disabled %v, bindings %+v; want disabled and operator", got.Disabled, bindings)
	}

	if err := db.UpdateUserGuarded(ctx, "00000000-0000-4000-8000-00000000abcd", store.UserChange{Disabled: &disabled}, nil); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown user = %v, want ErrNotFound", err)
	}
}

// UpdateUserGuarded is metered as UpdateUser whichever fields it changes: a role-only change is
// not a disable.
func TestUpdateUserGuardedIsMeteredAsUpdateUser(t *testing.T) {
	db, _ := newAuthDB(t)
	m := newTestMetrics()
	db.SetMetrics(m)
	ctx := context.Background()
	u, err := db.CreateUserWithRole(ctx, "metered", testPasswordHash, "M", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	role := "operator"
	if err := db.UpdateUserGuarded(ctx, u.ID, store.UserChange{Role: &role}, nil); err != nil {
		t.Fatalf("UpdateUserGuarded: %v", err)
	}
	if got := testutil.ToFloat64(m.StoreQueries.WithLabelValues("UpdateUser", "ok")); got != 1 {
		t.Errorf(`store_queries_total{query="UpdateUser",result="ok"} = %v, want 1`, got)
	}
	if got := testutil.ToFloat64(m.StoreQueries.WithLabelValues("SetUserDisabled", "ok")); got != 0 {
		t.Errorf(`store_queries_total{query="SetUserDisabled",result="ok"} = %v, want 0 for a role-only change`, got)
	}
}

// A binding delete and a user disable share one lock, so the second guard sees the first write:
// deleting the last admin's binding while the other admin is being disabled cannot both pass.
func TestDeleteBindingGuardedSharesTheLockWithUpdateUserGuarded(t *testing.T) {
	db, dsn := newAuthDB(t)
	ctx := context.Background()
	a, err := db.CreateUserWithRole(ctx, "a", testPasswordHash, "A", "admin")
	if err != nil {
		t.Fatal(err)
	}
	b, err := db.CreateUserWithRole(ctx, "b", testPasswordHash, "B", "admin")
	if err != nil {
		t.Fatal(err)
	}
	bBindings, err := db.ListBindingsForSubject(ctx, "user", b.ID, nil)
	if err != nil || len(bBindings) != 1 {
		t.Fatalf("b's bindings = %+v, %v; want one", bBindings, err)
	}

	firstDone := disableHoldingTheLock(t, db, dsn, a.ID)
	var sawFirstWrite bool
	err = db.DeleteBindingGuarded(ctx, bBindings[0].ID, func(ctx context.Context) error {
		first, gerr := db.GetUserByID(ctx, a.ID)
		if gerr != nil {
			return gerr
		}
		sawFirstWrite = first.Disabled
		if first.Disabled {
			return errors.New("last admin")
		}
		return nil
	})
	if ferr := <-firstDone; ferr != nil {
		t.Fatalf("disable: %v", ferr)
	}
	if !sawFirstWrite || err == nil || err.Error() != "last admin" {
		t.Fatalf("binding guard saw the disable = %v, delete err = %v; want true and the guard's own error", sawFirstWrite, err)
	}
	if left, _ := db.ListBindingsForSubject(ctx, "user", b.ID, nil); len(left) != 1 {
		t.Error("a binding delete whose guard refused was written anyway")
	}
}

// The guarded RBAC writes land only when the guard passes, and keep the plain methods' answers.
func TestRBACGuardedWritesRespectTheGuard(t *testing.T) {
	db, _ := newAuthDB(t)
	ctx := context.Background()
	refused := errors.New("refused")
	refuse := func(context.Context) error { return refused }

	if _, err := db.UpsertRole(ctx, "ops", []string{"users:manage"}); err != nil {
		t.Fatal(err)
	}
	binding, err := db.CreateBinding(ctx, "ops", "user", "u-1")
	if err != nil {
		t.Fatal(err)
	}

	if _, uerr := db.UpsertRoleGuarded(ctx, "ops", []string{"targets:read"}, refuse); !errors.Is(uerr, refused) {
		t.Fatalf("refused upsert = %v, want the guard's error", uerr)
	}
	if derr := db.DeleteBindingGuarded(ctx, binding.ID, refuse); !errors.Is(derr, refused) {
		t.Fatalf("refused binding delete = %v, want the guard's error", derr)
	}
	if derr := db.DeleteRoleGuarded(ctx, "ops", refuse); !errors.Is(derr, refused) {
		t.Fatalf("refused role delete = %v, want the guard's error", derr)
	}
	roles, _ := db.ListRoles(ctx)
	bindings, _ := db.ListBindings(ctx)
	if len(roles) != 1 || len(roles[0].Permissions) != 1 || roles[0].Permissions[0] != "users:manage" || len(bindings) != 1 {
		t.Fatalf("a refused write changed something: roles %+v, bindings %+v", roles, bindings)
	}

	role, err := db.UpsertRoleGuarded(ctx, "ops", []string{"targets:read"}, nil)
	if err != nil || role.Name != "ops" || len(role.Permissions) != 1 || role.Permissions[0] != "targets:read" {
		t.Fatalf("UpsertRoleGuarded = %+v, %v; want ops with targets:read", role, err)
	}
	if err := db.DeleteBindingGuarded(ctx, binding.ID, nil); err != nil {
		t.Fatalf("DeleteBindingGuarded: %v", err)
	}
	if err := db.DeleteRoleGuarded(ctx, "ops", nil); err != nil {
		t.Fatalf("DeleteRoleGuarded: %v", err)
	}
	if err := db.DeleteBindingGuarded(ctx, binding.ID, nil); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DeleteBindingGuarded(gone) = %v, want ErrNotFound", err)
	}
	if err := db.DeleteRoleGuarded(ctx, "ops", nil); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DeleteRoleGuarded(gone) = %v, want ErrNotFound", err)
	}
}

// More concurrent guarded writes than the pool has connections all finish: each writer holds its
// transaction's connection while it waits for the lock, so the guard of the one holding the lock
// must read through that transaction, not wait for a second connection none of the waiters returns.
func TestGuardedWritesBeyondThePoolSizeDoNotDeadlock(t *testing.T) {
	db, _ := newAuthDB(t) // a 5-connection pool
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	const writers = 2*5 + 1
	users := make([]store.User, writers)
	for i := range users {
		u, err := db.CreateUserWithRole(ctx, fmt.Sprintf("u%02d", i), testPasswordHash, "U", "admin")
		if err != nil {
			t.Fatal(err)
		}
		users[i] = u
	}
	if _, err := db.UpsertRole(ctx, "ops", []string{"users:manage"}); err != nil {
		t.Fatal(err)
	}
	// The reads the last-admin guards make: roles, the target, every user and their bindings.
	guard := func(id string) func(context.Context) error {
		return func(ctx context.Context) error {
			time.Sleep(20 * time.Millisecond) // let the other writers park on the lock
			if _, err := db.ListRoles(ctx); err != nil {
				return err
			}
			if _, err := db.GetUserByID(ctx, id); err != nil {
				return err
			}
			all, err := db.ListUsers(ctx)
			if err != nil {
				return err
			}
			for i := range all {
				if _, berr := db.ListBindingsForSubject(ctx, "user", all[i].ID, nil); berr != nil {
					return berr
				}
			}
			_, err = db.ListBindings(ctx)
			return err
		}
	}

	start := time.Now()
	errs := make(chan error, writers)
	for i := range users {
		go func() {
			var err error
			switch i % 3 {
			case 0:
				role := "operator"
				err = db.UpdateUserGuarded(ctx, users[i].ID, store.UserChange{Role: &role}, guard(users[i].ID))
			case 1:
				_, err = db.UpsertRoleGuarded(ctx, "ops", []string{"users:manage", "targets:read"}, guard(users[i].ID))
			default:
				disabled := false
				err = db.UpdateUserGuarded(ctx, users[i].ID, store.UserChange{Disabled: &disabled}, guard(users[i].ID))
			}
			errs <- err
		}()
	}
	var failed []error
	for range writers {
		if err := <-errs; err != nil {
			failed = append(failed, err)
		}
	}
	if len(failed) > 0 {
		t.Fatalf("%d of %d guarded writes on a 5-connection pool failed after %v; first: %v", len(failed), writers, time.Since(start), failed[0])
	}
	if _, err := db.ListUsers(ctx); err != nil {
		t.Fatalf("the pool is still usable after the guarded writes: %v", err)
	}
}

// A binding create takes the same lock as the other guarded writes: its guard sees a user disabled
// meanwhile, a refusal writes nothing, and a duplicate still answers ErrAlreadyExists.
func TestCreateBindingGuardedSharesTheLockAndRespectsTheGuard(t *testing.T) {
	db, dsn := newAuthDB(t)
	ctx := context.Background()
	a, err := db.CreateUserWithRole(ctx, "a", testPasswordHash, "A", "admin")
	if err != nil {
		t.Fatal(err)
	}

	firstDone := disableHoldingTheLock(t, db, dsn, a.ID)
	var sawFirstWrite bool
	_, err = db.CreateBindingGuarded(ctx, "viewer", "user", "u-2", func(ctx context.Context) error {
		first, gerr := db.GetUserByID(ctx, a.ID)
		if gerr != nil {
			return gerr
		}
		sawFirstWrite = first.Disabled
		return errors.New("last admin")
	})
	if ferr := <-firstDone; ferr != nil {
		t.Fatalf("disable: %v", ferr)
	}
	if !sawFirstWrite || err == nil || err.Error() != "last admin" {
		t.Fatalf("binding guard saw the disable = %v, create err = %v; want true and the guard's own error", sawFirstWrite, err)
	}
	if got, _ := db.ListBindingsForSubject(ctx, "user", "u-2", nil); len(got) != 0 {
		t.Fatalf("a binding create whose guard refused was written anyway: %+v", got)
	}

	binding, err := db.CreateBindingGuarded(ctx, "viewer", "user", "u-2", nil)
	if err != nil || binding.ID == 0 || binding.RoleName != "viewer" || binding.SubjectKind != "user" || binding.SubjectID != "u-2" {
		t.Fatalf("CreateBindingGuarded = %+v, %v; want viewer -> user u-2", binding, err)
	}
	if _, err := db.CreateBindingGuarded(ctx, "viewer", "user", "u-2", nil); !errors.Is(err, store.ErrAlreadyExists) {
		t.Errorf("duplicate CreateBindingGuarded = %v, want ErrAlreadyExists", err)
	}
}

// A user create whose guard refuses leaves neither the user nor a binding, and the guard runs under
// the lock the other guarded user and role writes take.
func TestCreateUserWithRoleGuardedSharesTheLockAndRespectsTheGuard(t *testing.T) {
	db, dsn := newAuthDB(t)
	ctx := context.Background()
	a, err := db.CreateUserWithRole(ctx, "a", testPasswordHash, "A", "admin")
	if err != nil {
		t.Fatal(err)
	}

	firstDone := disableHoldingTheLock(t, db, dsn, a.ID)
	var sawFirstWrite bool
	_, err = db.CreateUserWithRoleGuarded(ctx, "b", testPasswordHash, "B", "viewer", func(ctx context.Context) error {
		first, gerr := db.GetUserByID(ctx, a.ID)
		if gerr != nil {
			return gerr
		}
		sawFirstWrite = first.Disabled
		return errors.New("unknown role")
	})
	if ferr := <-firstDone; ferr != nil {
		t.Fatalf("disable: %v", ferr)
	}
	if !sawFirstWrite || err == nil || err.Error() != "unknown role" {
		t.Fatalf("create guard saw the disable = %v, create err = %v; want true and the guard's own error", sawFirstWrite, err)
	}
	if _, gerr := db.GetUserByUsername(ctx, "b"); !errors.Is(gerr, store.ErrNotFound) {
		t.Fatalf("a user create whose guard refused was written anyway: %v", gerr)
	}

	b, err := db.CreateUserWithRoleGuarded(ctx, "b", testPasswordHash, "B", "viewer", nil)
	if err != nil || b.Username != "b" {
		t.Fatalf("CreateUserWithRoleGuarded = %+v, %v; want user b", b, err)
	}
	if got, _ := db.ListBindingsForSubject(ctx, "user", b.ID, nil); len(got) != 1 || got[0].RoleName != "viewer" {
		t.Fatalf("bindings of b = %+v, want one viewer binding", got)
	}
	if _, err = db.CreateUserWithRoleGuarded(ctx, "b", testPasswordHash, "B", "viewer", nil); !errors.Is(err, store.ErrAlreadyExists) {
		t.Errorf("duplicate CreateUserWithRoleGuarded = %v, want ErrAlreadyExists", err)
	}
}

func TestDeleteUserGuardedRemovesTheUserAndTheirBindings(t *testing.T) {
	db, _ := newAuthDB(t)
	ctx := context.Background()
	u, err := db.CreateUserWithRole(ctx, "gone", testPasswordHash, "G", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateUserWithRole(ctx, "stays", testPasswordHash, "S", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	refused := errors.New("refused")
	if err := db.DeleteUserGuarded(ctx, u.ID, func(context.Context) error { return refused }); !errors.Is(err, refused) {
		t.Fatalf("refused delete = %v, want the guard's error", err)
	}
	if _, err := db.GetUserByID(ctx, u.ID); err != nil {
		t.Fatalf("a refused delete removed the user: %v", err)
	}

	if err := db.DeleteUserGuarded(ctx, u.ID, nil); err != nil {
		t.Fatalf("DeleteUserGuarded: %v", err)
	}
	if _, err := db.GetUserByID(ctx, u.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted user = %v, want ErrNotFound", err)
	}
	if bindings, _ := db.ListBindingsForSubject(ctx, "user", u.ID, nil); len(bindings) != 0 {
		t.Fatalf("bindings outlived the user: %+v", bindings)
	}
	if bindings, _ := db.ListBindingsForSubject(ctx, "user", other.ID, nil); len(bindings) != 1 {
		t.Fatalf("another user's bindings = %+v, want untouched", bindings)
	}
	if _, err := db.CreateUserWithRole(ctx, "gone", testPasswordHash, "G2", "viewer"); err != nil {
		t.Fatalf("the username is not free again: %v", err)
	}
	if err := db.DeleteUserGuarded(ctx, u.ID, nil); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleting again = %v, want ErrNotFound", err)
	}
	if err := db.DeleteUserGuarded(ctx, "not-a-uuid", nil); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("malformed id = %v, want ErrNotFound", err)
	}
}

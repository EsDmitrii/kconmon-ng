//go:build integration

package store_test

// TestUser*, TestBindings*, TestToken*, TestAudit* require a real PostgreSQL.
// Run: docker run --rm -d -p 5432:5432 -e POSTGRES_PASSWORD=test -e POSTGRES_DB=kconmon postgres:17-alpine
// Then: KCONMON_TEST_DATABASE_DSN='postgres://postgres:test@127.0.0.1:5432/kconmon?sslmode=disable' \
//       go test -tags=integration ./internal/console/store/... -v

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// newAuthDB opens a *store.DB with migrations applied, dropping and
// re-creating the schema first -- same convention as newEventStoreDB
// (events_integration_test.go) and newPrunerDB (prune_integration_test.go);
// this file shares one database with every other file in package
// store_test, so each test must leave it clean. dsn is also returned for
// tests (seedAuditEntryAtAge) that need to reach past *store.DB's public API.
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

// backdateAuditEntry directly UPDATEs audit_log.at for id: the query layer
// intentionally has no way to write an arbitrary `at` (audit timestamps must
// always be server time, DEFAULT now()), so ageing a row for a retention test
// has to go around it, the same way seedTopologyEventsAtAges
// (prune_integration_test.go) bulk-inserts around EventStore.InsertEvent.
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

// tokenHash returns a stand-in for the real SHA-256(256 random bits) hash
// Decision 11 specifies: deterministic per input string, which is all these
// tests need to tell tokens apart.
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

// TestUserPasswordAndDisabledLifecycle covers UpdateUserPassword,
// SetUserDisabled, ListUsers and CountUsers together, since they all mutate
// or read the same row.
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

	if n, err := db.CountUsers(ctx); err != nil || n != 1 {
		t.Fatalf("CountUsers: n=%d err=%v, want 1, nil", n, err)
	}

	if err := db.UpdateUserPassword(ctx, u.ID, "argon2id$rotated"); err != nil {
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

	if err := db.SetUserDisabled(ctx, u.ID, true); err != nil {
		t.Fatalf("SetUserDisabled: %v", err)
	}
	got, err = db.GetUserByUsername(ctx, "bob")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	if !got.Disabled {
		t.Error("SetUserDisabled(true) did not persist")
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
	if err := db.SetUserDisabled(ctx, randomID, false); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SetUserDisabled(unknown id): err = %v, want ErrNotFound", err)
	}
}

// TestGetUserByIDFoundNotFoundAndDisabledRoundTrip covers the new
// authn.WithOwnerDisabledCheck lookup path directly: a found row (with
// PasswordHash always "", like ListUsers), ErrNotFound for both an unknown
// UUID and a malformed one, and that a SetUserDisabled flip is visible on the
// very next GetUserByID call -- the same re-query guarantee local.go's
// GetUserByUsername already gives.
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
	if _, err := db.GetUserByID(ctx, randomID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetUserByID(unknown uuid): err = %v, want ErrNotFound", err)
	}
	if _, err := db.GetUserByID(ctx, "not-a-uuid"); err == nil {
		t.Error("GetUserByID(malformed id): err = nil, want a non-nil error")
	}

	if err := db.SetUserDisabled(ctx, created.ID, true); err != nil {
		t.Fatalf("SetUserDisabled: %v", err)
	}
	got, err = db.GetUserByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetUserByID (after disable): %v", err)
	}
	if !got.Disabled {
		t.Error("GetUserByID: Disabled flip via SetUserDisabled did not round-trip")
	}
}

// TestListUsersNeverExposesPasswordHash mirrors TestListTokensNeverExposesHash
// (below, for api_tokens.token_hash): ListUsers's result set must never carry
// a usable password_hash, while GetUserByUsername -- the one path that exists
// specifically to verify a password -- still returns the real one.
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

// TestListBindingsForSubjectResolvesUserAndGroupInOneCall is the brief's core
// binding assertion: a user binding and a group binding the caller belongs to
// both come back from a single ListBindingsForSubject call, and a binding for
// an unrelated user or group does not.
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
	if _, err := db.CreateBinding(ctx, "admin", "user", "mallory"); err != nil {
		t.Fatalf("CreateBinding(unrelated user): %v", err)
	}
	if _, err := db.CreateBinding(ctx, "admin", "group", "other-team"); err != nil {
		t.Fatalf("CreateBinding(unrelated group): %v", err)
	}

	got, err := db.ListBindingsForSubject(ctx, "alice", []string{"sre-team", "another-group"})
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
	onlyUser, err := db.ListBindingsForSubject(ctx, "alice", nil)
	if err != nil {
		t.Fatalf("ListBindingsForSubject(no groups): %v", err)
	}
	if len(onlyUser) != 1 || onlyUser[0].RoleName != "operator" {
		t.Errorf("ListBindingsForSubject(no groups): got %+v, want just the operator binding", onlyUser)
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

// TestListBindingsReturnsEveryBindingUnscoped is Task 17's addition to
// RoleStore: unlike ListBindingsForSubject (one subject's own resolution),
// ListBindings must return every binding regardless of who it names --
// the RBAC admin API's GET /api/v1/rbac/bindings and its delete-role
// guard rail both depend on this.
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

// TestTokenLifecycleRevokedExpiredAndUnknownAreDistinguishable creates a
// live token, a revoked token and an expired token, and asserts
// GetTokenByHash lets the caller tell all three apart from each other and
// from an unknown hash -- store.ErrNotFound only ever means "no such row";
// RevokedAt/ExpiresAt being set or unset is how the caller (the authn layer)
// distinguishes the rest.
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
	if err := db.RevokeToken(ctx, toRevoke.ID); err != nil {
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

	if _, err := db.GetTokenByHash(ctx, tokenHash("never-issued")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetTokenByHash(unknown): err = %v, want ErrNotFound", err)
	}

	// RevokeToken on an already-revoked token: the WHERE revoked_at IS NULL
	// guard means 0 rows, surfaced as ErrNotFound, not silently ok.
	if err := db.RevokeToken(ctx, toRevoke.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("RevokeToken(already revoked): err = %v, want ErrNotFound", err)
	}

	if err := db.TouchTokenLastUsed(ctx, live.ID); err != nil {
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
	for i := 0; i < total; i++ {
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

	mustInsertAudit(t, ctx, db, "user", "alice")
	mustInsertAudit(t, ctx, db, "user", "bob")
	mustInsertAudit(t, ctx, db, "token", "ci-runner")

	page, err := db.ListAuditEntries(ctx, store.AuditFilter{SubjectKind: "user", SubjectID: "alice"})
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}
	if len(page.Entries) != 1 || page.Entries[0].SubjectID != "alice" {
		t.Errorf("ListAuditEntries(user,alice): got %+v", page.Entries)
	}
}

func mustInsertAudit(t *testing.T, ctx context.Context, db *store.DB, subjectKind, subjectID string) {
	t.Helper()
	if _, err := db.InsertAuditEntry(ctx, subjectKind, subjectID, "GET /api/v1/topology", "topology", "allowed", "127.0.0.1", nil); err != nil {
		t.Fatalf("InsertAuditEntry(%s,%s): %v", subjectKind, subjectID, err)
	}
}

// TestPrunerDeletesOldAuditLogRows is the retention-side counterpart of
// TestPruneOnceDeletesRowsPastRetention (prune_integration_test.go): seeds
// audit_log rows both inside and outside a 90d retention window and asserts
// PruneOnce removes exactly the old ones, crediting the "audit_log" key in
// its returned map.
func TestPrunerDeletesOldAuditLogRows(t *testing.T) {
	db, dsn := newAuthDB(t)
	ctx := context.Background()

	seedAuditEntryAtAge(t, ctx, db, dsn, 1)
	seedAuditEntryAtAge(t, ctx, db, dsn, 2)
	seedAuditEntryAtAge(t, ctx, db, dsn, 200)
	seedAuditEntryAtAge(t, ctx, db, dsn, 201)

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

// seedAuditEntryAtAge inserts one audit_log row via the store's own
// InsertAuditEntry (which always stamps `at` as now()) and then backdates it
// with a direct UPDATE, since the query layer intentionally has no way to
// write an arbitrary `at` -- audit timestamps must always be server time.
func seedAuditEntryAtAge(t *testing.T, ctx context.Context, db *store.DB, dsn string, ageDays int) {
	t.Helper()
	e, err := db.InsertAuditEntry(ctx, "user", "seed", "GET /api/v1/topology", "topology", "allowed", "127.0.0.1", nil)
	if err != nil {
		t.Fatalf("seedAuditEntryAtAge: InsertAuditEntry: %v", err)
	}
	backdateAuditEntry(t, dsn, e.ID, ageDays)
}

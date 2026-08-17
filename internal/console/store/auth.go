package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store/gen"
)

// Sentinel errors the four stores below return; every consumer compares against these with
// errors.Is rather than importing pgx to recognize pgx.ErrNoRows or a Postgres error code directly.
var (
	// ErrNotFound is returned by every single-row lookup or targeted update/delete (GetUserByUsername,
	// GetTokenByHash, UpdateUserPassword, SetUserDisabled, DeleteRole, DeleteBinding, RevokeToken,
	// TouchTokenLastUsed) when no row matches.
	ErrNotFound = errors.New("store: not found")
	// ErrAlreadyExists is returned when a unique constraint blocks a write: CreateUser on a taken
	// username.
	ErrAlreadyExists = errors.New("store: already exists")
)

// uniqueViolationCode is PostgreSQL's SQLSTATE for a unique_violation.
const uniqueViolationCode = "23505"

// wrapNoRows turns pgx.ErrNoRows into this package's own ErrNotFound.
func wrapNoRows(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// wrapUniqueViolation turns a unique-constraint PgError into this package's
// own ErrAlreadyExists, leaving every other error (including a nil one)
// unchanged.
func wrapUniqueViolation(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolationCode {
		return ErrAlreadyExists
	}
	return err
}

// parseUUID parses s (a canonical UUID string) into the pgtype.UUID every id column in migration
// 00002 needs; every public type in this file exposes ids as plain strings.
func parseUUID(s string) (pgtype.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("parse id %q: %w", s, err)
	}
	return pgtype.UUID{Bytes: id, Valid: true}, nil
}

// formatUUID is parseUUID's inverse.
func formatUUID(id pgtype.UUID) string {
	return uuid.UUID(id.Bytes).String()
}

// nullTime converts a nullable pgtype.Timestamptz (expires_at, last_used_at,
// revoked_at) into *time.Time: nil when unset, a pointer to the value when
// set.
func nullTime(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time
	return &t
}

// timestamptzFromPtr is nullTime's inverse, for write params.
func timestamptzFromPtr(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

// ---------------------------------------------------------------------------
// Users
// ---------------------------------------------------------------------------

// User is a database-authenticated Console user (database.mode=enabled).
// PasswordHash is always an argon2id PHC string -- never a raw password, and
// this type has no field a raw password could ever land in.
type User struct {
	ID       string
	Username string
	// PasswordHash is populated only by GetUserByUsername (for verification) and by CreateUser
	// (echoing the hash the caller itself supplied).
	PasswordHash string `json:"-"`
	DisplayName  string
	Disabled     bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// UserStore is the seam the authn layer and an admin user-management API take.
type UserStore interface {
	// GetUserByID returns ErrNotFound when no user has that id; a malformed (non-UUID) id is its own.
	GetUserByID(ctx context.Context, id string) (User, error)
	// GetUserByUsername returns ErrNotFound when no user has that username.
	GetUserByUsername(ctx context.Context, username string) (User, error)
	// CreateUser returns ErrAlreadyExists when username is already taken.
	CreateUser(ctx context.Context, username, passwordHash, displayName string) (User, error)
	// UpdateUserPassword returns ErrNotFound when id does not name a user.
	UpdateUserPassword(ctx context.Context, id, passwordHash string) error
	// ListUsers never returns a user's password hash: every returned User has PasswordHash == "".
	ListUsers(ctx context.Context) ([]User, error)
	// CountUsers drives the bootstrap-admin decision: a fresh
	// database.mode=enabled deployment with zero rows here needs one made,
	// an existing one must not get a second one made for it.
	CountUsers(ctx context.Context) (int64, error)
	// SetUserDisabled returns ErrNotFound when id does not name a user.
	SetUserDisabled(ctx context.Context, id string, disabled bool) error
}

var _ UserStore = (*DB)(nil)

func userFromRow(u *gen.User) User {
	return User{
		ID:           formatUUID(u.ID),
		Username:     u.Username,
		PasswordHash: u.PasswordHash,
		DisplayName:  u.DisplayName,
		Disabled:     u.Disabled,
		CreatedAt:    u.CreatedAt,
		UpdatedAt:    u.UpdatedAt,
	}
}

// userFromListRow maps a ListUsers row. gen.ListUsersRow is a distinct type
// from gen.User -- its SELECT list omits password_hash -- so the returned
// User's PasswordHash is always "", never populated from this path.
func userFromListRow(u *gen.ListUsersRow) User {
	return User{
		ID:          formatUUID(u.ID),
		Username:    u.Username,
		DisplayName: u.DisplayName,
		Disabled:    u.Disabled,
		CreatedAt:   u.CreatedAt,
		UpdatedAt:   u.UpdatedAt,
	}
}

// userFromGetByIDRow maps a GetUserByID row.
func userFromGetByIDRow(u *gen.GetUserByIDRow) User {
	return User{
		ID:          formatUUID(u.ID),
		Username:    u.Username,
		DisplayName: u.DisplayName,
		Disabled:    u.Disabled,
		CreatedAt:   u.CreatedAt,
		UpdatedAt:   u.UpdatedAt,
	}
}

func (db *DB) GetUserByID(ctx context.Context, id string) (User, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return User{}, fmt.Errorf("store: get user by id: %w", err)
	}
	start := time.Now()
	u, err := gen.New(db.pool).GetUserByID(ctx, uid)
	db.observe(queryGetUserByID, start, queryResult(wrapNoRows(err)))
	if err != nil {
		return User{}, fmt.Errorf("store: get user by id: %w", wrapNoRows(err))
	}
	return userFromGetByIDRow(&u), nil
}

func (db *DB) GetUserByUsername(ctx context.Context, username string) (User, error) {
	start := time.Now()
	u, err := gen.New(db.pool).GetUserByUsername(ctx, username)
	db.observe(queryGetUserByUsername, start, queryResult(wrapNoRows(err)))
	if err != nil {
		return User{}, fmt.Errorf("store: get user by username: %w", wrapNoRows(err))
	}
	return userFromRow(&u), nil
}

func (db *DB) CreateUser(ctx context.Context, username, passwordHash, displayName string) (User, error) {
	start := time.Now()
	u, err := gen.New(db.pool).CreateUser(ctx, gen.CreateUserParams{
		Username:     username,
		PasswordHash: passwordHash,
		DisplayName:  displayName,
	})
	db.observe(queryCreateUser, start, queryResult(wrapUniqueViolation(err)))
	if err != nil {
		return User{}, fmt.Errorf("store: create user: %w", wrapUniqueViolation(err))
	}
	return userFromRow(&u), nil
}

// CreateBootstrapAdmin creates the local bootstrap user AND its admin binding in ONE transaction.
//
// The two used to be separate statements with a repair loop behind them: every boot looked for the
// binding and re-created it when it was missing, because a crash between the two would otherwise
// leave an account nobody could use. That repair could not tell a half-finished bootstrap from a
// DELIBERATE revocation, so demoting the shared bootstrap account survived exactly until the next
// pod restart — a rollout, an OOM kill, a node drain — and the re-grant went straight to the store,
// bypassing the audit middleware entirely. The audit log said the binding was deleted and nothing
// after it; the binding list said admin.
//
// A transaction removes the question. There is no partial state to repair, so nothing has to guess
// what a missing binding means, and a revocation stays revoked.
func (db *DB) CreateBootstrapAdmin(ctx context.Context, username, passwordHash, displayName, role string) (User, error) {
	start := time.Now()
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		db.observe(queryCreateUser, start, queryResult(err))
		return User{}, fmt.Errorf("store: create bootstrap admin: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once Commit has run

	q := gen.New(tx)
	u, err := q.CreateUser(ctx, gen.CreateUserParams{
		Username: username, PasswordHash: passwordHash, DisplayName: displayName,
	})
	if err != nil {
		db.observe(queryCreateUser, start, queryResult(wrapUniqueViolation(err)))
		return User{}, fmt.Errorf("store: create bootstrap admin: %w", wrapUniqueViolation(err))
	}
	if _, err := q.CreateBinding(ctx, gen.CreateBindingParams{
		RoleName: role, SubjectKind: "user", SubjectID: u.ID.String(),
	}); err != nil {
		db.observe(queryCreateBinding, start, queryResult(wrapUniqueViolation(err)))
		return User{}, fmt.Errorf("store: create bootstrap admin binding: %w", wrapUniqueViolation(err))
	}
	if err := tx.Commit(ctx); err != nil {
		db.observe(queryCreateUser, start, queryResult(err))
		return User{}, fmt.Errorf("store: create bootstrap admin: commit: %w", err)
	}
	db.observe(queryCreateUser, start, queryResult(nil))
	return userFromRow(&u), nil
}

func (db *DB) UpdateUserPassword(ctx context.Context, id, passwordHash string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return fmt.Errorf("store: update user password: %w", err)
	}
	start := time.Now()
	rows, err := gen.New(db.pool).UpdateUserPassword(ctx, gen.UpdateUserPasswordParams{
		ID:           uid,
		PasswordHash: passwordHash,
	})
	db.observe(queryUpdateUserPassword, start, queryResult(err))
	if err != nil {
		return fmt.Errorf("store: update user password: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("store: update user password: %w", ErrNotFound)
	}
	return nil
}

func (db *DB) ListUsers(ctx context.Context) ([]User, error) {
	start := time.Now()
	rows, err := gen.New(db.pool).ListUsers(ctx)
	db.observe(queryListUsers, start, queryResult(err))
	if err != nil {
		return nil, fmt.Errorf("store: list users: %w", err)
	}
	users := make([]User, len(rows))
	for i := range rows {
		users[i] = userFromListRow(&rows[i])
	}
	return users, nil
}

func (db *DB) CountUsers(ctx context.Context) (int64, error) {
	start := time.Now()
	n, err := gen.New(db.pool).CountUsers(ctx)
	db.observe(queryCountUsers, start, queryResult(err))
	if err != nil {
		return 0, fmt.Errorf("store: count users: %w", err)
	}
	return n, nil
}

func (db *DB) SetUserDisabled(ctx context.Context, id string, disabled bool) error {
	uid, err := parseUUID(id)
	if err != nil {
		return fmt.Errorf("store: set user disabled: %w", err)
	}
	start := time.Now()
	rows, err := gen.New(db.pool).SetUserDisabled(ctx, gen.SetUserDisabledParams{ID: uid, Disabled: disabled})
	db.observe(querySetUserDisabled, start, queryResult(err))
	if err != nil {
		return fmt.Errorf("store: set user disabled: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("store: set user disabled: %w", ErrNotFound)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Roles and role bindings
// ---------------------------------------------------------------------------

// Role is a custom (database-defined) role. The four built-ins are compiled-in
// constants in internal/console/authz and never have a row here (see
// migration 00002's comment on the roles table).
type Role struct {
	Name        string
	Permissions []string
	CreatedAt   time.Time
}

// RoleBinding attaches a role (built-in or custom) to a subject.
type RoleBinding struct {
	ID          int64
	RoleName    string
	SubjectKind string // user | group | token
	SubjectID   string
	CreatedAt   time.Time
}

// RoleStore is the seam an RBAC admin API and authz's custom-role/binding
// resolution take.
type RoleStore interface {
	ListRoles(ctx context.Context) ([]Role, error)
	UpsertRole(ctx context.Context, name string, permissions []string) (Role, error)
	// DeleteRole returns ErrNotFound when name does not name an existing
	// custom role.
	DeleteRole(ctx context.Context, name string) error

	// ListBindingsForSubject resolves every binding for userID and for every group in groups in ONE
	// round trip.
	ListBindingsForSubject(ctx context.Context, callerKind, subjectID string, groups []string) ([]RoleBinding, error)
	// ListBindings returns every role binding; added after landed: neither need can be answered by
	// ListBindingsForSubject.
	ListBindings(ctx context.Context) ([]RoleBinding, error)
	// CreateBinding returns ErrAlreadyExists for a duplicate
	// (roleName, subjectKind, subjectID).
	CreateBinding(ctx context.Context, roleName, subjectKind, subjectID string) (RoleBinding, error)
	// DeleteBinding returns ErrNotFound when id does not name a binding.
	DeleteBinding(ctx context.Context, id int64) error
}

var _ RoleStore = (*DB)(nil)

func roleFromRow(r gen.Role) Role {
	return Role{Name: r.Name, Permissions: r.Permissions, CreatedAt: r.CreatedAt}
}

func bindingFromRow(b *gen.RoleBinding) RoleBinding {
	return RoleBinding{
		ID:          b.ID,
		RoleName:    b.RoleName,
		SubjectKind: b.SubjectKind,
		SubjectID:   b.SubjectID,
		CreatedAt:   b.CreatedAt,
	}
}

func (db *DB) ListRoles(ctx context.Context) ([]Role, error) {
	start := time.Now()
	rows, err := gen.New(db.pool).ListRoles(ctx)
	db.observe(queryListRoles, start, queryResult(err))
	if err != nil {
		return nil, fmt.Errorf("store: list roles: %w", err)
	}
	roles := make([]Role, len(rows))
	for i, r := range rows {
		roles[i] = roleFromRow(r)
	}
	return roles, nil
}

func (db *DB) UpsertRole(ctx context.Context, name string, permissions []string) (Role, error) {
	start := time.Now()
	r, err := gen.New(db.pool).UpsertRole(ctx, gen.UpsertRoleParams{Name: name, Permissions: permissions})
	db.observe(queryUpsertRole, start, queryResult(err))
	if err != nil {
		return Role{}, fmt.Errorf("store: upsert role: %w", err)
	}
	return roleFromRow(r), nil
}

func (db *DB) DeleteRole(ctx context.Context, name string) error {
	start := time.Now()
	rows, err := gen.New(db.pool).DeleteRole(ctx, name)
	db.observe(queryDeleteRole, start, queryResult(err))
	if err != nil {
		return fmt.Errorf("store: delete role: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("store: delete role: %w", ErrNotFound)
	}
	return nil
}

// ListBindingsForSubject resolves one subject's roles. callerKind is the subject's OWN kind: a
// binding only ever applies to a subject of the kind it was written for, or an API token whose UUID
// happened to appear in a 'user' binding would inherit that role.
func (db *DB) ListBindingsForSubject(ctx context.Context, callerKind, subjectID string, groups []string) ([]RoleBinding, error) {
	start := time.Now()
	rows, err := gen.New(db.pool).ListBindingsForSubject(ctx, gen.ListBindingsForSubjectParams{
		CallerKind: callerKind,
		UserID:     subjectID,
		Groups:     groups,
	})
	db.observe(queryListBindingsForSubject, start, queryResult(err))
	if err != nil {
		return nil, fmt.Errorf("store: list bindings for subject: %w", err)
	}
	bindings := make([]RoleBinding, len(rows))
	for i := range rows {
		bindings[i] = bindingFromRow(&rows[i])
	}
	return bindings, nil
}

func (db *DB) ListBindings(ctx context.Context) ([]RoleBinding, error) {
	start := time.Now()
	rows, err := gen.New(db.pool).ListBindings(ctx)
	db.observe(queryListBindings, start, queryResult(err))
	if err != nil {
		return nil, fmt.Errorf("store: list bindings: %w", err)
	}
	bindings := make([]RoleBinding, len(rows))
	for i := range rows {
		bindings[i] = bindingFromRow(&rows[i])
	}
	return bindings, nil
}

func (db *DB) CreateBinding(ctx context.Context, roleName, subjectKind, subjectID string) (RoleBinding, error) {
	start := time.Now()
	b, err := gen.New(db.pool).CreateBinding(ctx, gen.CreateBindingParams{
		RoleName:    roleName,
		SubjectKind: subjectKind,
		SubjectID:   subjectID,
	})
	db.observe(queryCreateBinding, start, queryResult(wrapUniqueViolation(err)))
	if err != nil {
		return RoleBinding{}, fmt.Errorf("store: create binding: %w", wrapUniqueViolation(err))
	}
	return bindingFromRow(&b), nil
}

func (db *DB) DeleteBinding(ctx context.Context, id int64) error {
	start := time.Now()
	rows, err := gen.New(db.pool).DeleteBinding(ctx, id)
	db.observe(queryDeleteBinding, start, queryResult(err))
	if err != nil {
		return fmt.Errorf("store: delete binding: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("store: delete binding: %w", ErrNotFound)
	}
	return nil
}

// ---------------------------------------------------------------------------
// API tokens
// ---------------------------------------------------------------------------

// Token is one API token's metadata; TokenHash is deliberately absent: it is SHA-256 of 256 random
// bits.
type Token struct {
	ID         string
	Name       string
	Owner      string
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
	CreatedAt  time.Time
}

// TokenStore is the seam the authn layer and an admin token-management API take; TouchTokenLastUsed
// must NEVER be called synchronously on the request path.
type TokenStore interface {
	// GetTokenByHash returns ErrNotFound for an unknown hash.
	GetTokenByHash(ctx context.Context, hash []byte) (Token, error)
	// CreateToken returns ErrAlreadyExists on the (practically impossible)
	// event of a token_hash collision. expiresAt == nil means the token never
	// expires.
	CreateToken(ctx context.Context, name string, hash []byte, owner string, expiresAt *time.Time) (Token, error)
	// GetTokenByID returns ErrNotFound when id does not name a token; a malformed (non-UUID) id is its
	// own.
	GetTokenByID(ctx context.Context, id string) (Token, error)
	// ListTokens never returns a token's hash -- there is no field in Token
	// to hold one.
	ListTokens(ctx context.Context) ([]Token, error)
	// RevokeToken returns ErrNotFound when id does not name a currently active token (never existed,
	// or already revoked -- both leave the WHERE revoked_at IS NULL guard matching 0 rows; a caller
	// that must tell these apart calls GetTokenByHash first).
	RevokeToken(ctx context.Context, id string) error
	// PurgeToken hard-deletes a token row, but only one that can no longer authenticate anything
	// (already revoked, or past its expiry); it returns ErrNotFound for an unknown id AND for a
	// still-active one, so an active credential cannot be erased without being revoked first.
	PurgeToken(ctx context.Context, id string) error
	// TouchTokenLastUsed returns ErrNotFound when id does not name a token.
	// See the interface doc comment above: never call this synchronously on
	// every authenticated request.
	TouchTokenLastUsed(ctx context.Context, id string) error
}

var _ TokenStore = (*DB)(nil)

// tokenRow is the field-for-field shape every api_tokens query row shares (id, name, owner,
// expires_at, last_used_at, revoked_at, created_at, in that order).
type tokenRow struct {
	ID         pgtype.UUID
	Name       string
	Owner      string
	ExpiresAt  pgtype.Timestamptz
	LastUsedAt pgtype.Timestamptz
	RevokedAt  pgtype.Timestamptz
	CreatedAt  time.Time
}

func tokenFromRow(r *tokenRow) Token {
	return Token{
		ID:         formatUUID(r.ID),
		Name:       r.Name,
		Owner:      r.Owner,
		ExpiresAt:  nullTime(r.ExpiresAt),
		LastUsedAt: nullTime(r.LastUsedAt),
		RevokedAt:  nullTime(r.RevokedAt),
		CreatedAt:  r.CreatedAt,
	}
}

func (db *DB) GetTokenByHash(ctx context.Context, hash []byte) (Token, error) {
	start := time.Now()
	r, err := gen.New(db.pool).GetTokenByHash(ctx, hash)
	db.observe(queryGetTokenByHash, start, queryResult(wrapNoRows(err)))
	if err != nil {
		return Token{}, fmt.Errorf("store: get token by hash: %w", wrapNoRows(err))
	}
	tr := tokenRow(r)
	return tokenFromRow(&tr), nil
}

func (db *DB) CreateToken(ctx context.Context, name string, hash []byte, owner string, expiresAt *time.Time) (Token, error) {
	start := time.Now()
	r, err := gen.New(db.pool).CreateToken(ctx, gen.CreateTokenParams{
		Name:      name,
		TokenHash: hash,
		Owner:     owner,
		ExpiresAt: timestamptzFromPtr(expiresAt),
	})
	db.observe(queryCreateToken, start, queryResult(wrapUniqueViolation(err)))
	if err != nil {
		return Token{}, fmt.Errorf("store: create token: %w", wrapUniqueViolation(err))
	}
	tr := tokenRow(r)
	return tokenFromRow(&tr), nil
}

func (db *DB) GetTokenByID(ctx context.Context, id string) (Token, error) {
	tid, err := parseUUID(id)
	if err != nil {
		return Token{}, fmt.Errorf("store: get token by id: %w", err)
	}
	start := time.Now()
	r, err := gen.New(db.pool).GetTokenByID(ctx, tid)
	db.observe(queryGetTokenByID, start, queryResult(wrapNoRows(err)))
	if err != nil {
		return Token{}, fmt.Errorf("store: get token by id: %w", wrapNoRows(err))
	}
	tr := tokenRow(r)
	return tokenFromRow(&tr), nil
}

func (db *DB) ListTokens(ctx context.Context) ([]Token, error) {
	start := time.Now()
	rows, err := gen.New(db.pool).ListTokens(ctx)
	db.observe(queryListTokens, start, queryResult(err))
	if err != nil {
		return nil, fmt.Errorf("store: list tokens: %w", err)
	}
	tokens := make([]Token, len(rows))
	for i := range rows {
		// (*tokenRow)(&rows[i]): a pointer conversion between *ListTokensRow and *tokenRow.
		tokens[i] = tokenFromRow((*tokenRow)(&rows[i]))
	}
	return tokens, nil
}

func (db *DB) RevokeToken(ctx context.Context, id string) error {
	tid, err := parseUUID(id)
	if err != nil {
		return fmt.Errorf("store: revoke token: %w", err)
	}
	start := time.Now()
	rows, err := gen.New(db.pool).RevokeToken(ctx, tid)
	db.observe(queryRevokeToken, start, queryResult(err))
	if err != nil {
		return fmt.Errorf("store: revoke token: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("store: revoke token: %w", ErrNotFound)
	}
	return nil
}

func (db *DB) PurgeToken(ctx context.Context, id string) error {
	tid, err := parseUUID(id)
	if err != nil {
		return fmt.Errorf("store: purge token: %w", err)
	}
	start := time.Now()
	rows, err := gen.New(db.pool).PurgeToken(ctx, tid)
	db.observe(queryPurgeToken, start, queryResult(err))
	if err != nil {
		return fmt.Errorf("store: purge token: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("store: purge token: %w", ErrNotFound)
	}
	return nil
}

func (db *DB) TouchTokenLastUsed(ctx context.Context, id string) error {
	tid, err := parseUUID(id)
	if err != nil {
		return fmt.Errorf("store: touch token last used: %w", err)
	}
	start := time.Now()
	rows, err := gen.New(db.pool).TouchTokenLastUsed(ctx, tid)
	db.observe(queryTouchTokenLastUsed, start, queryResult(err))
	if err != nil {
		return fmt.Errorf("store: touch token last used: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("store: touch token last used: %w", ErrNotFound)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Audit log
// ---------------------------------------------------------------------------

// AuditEntry is one persisted audit row.
type AuditEntry struct {
	ID          int64
	At          time.Time
	SubjectKind string
	SubjectID   string
	Action      string // "POST /api/v1/runs" style: method + chi route pattern, never a raw path
	Resource    string
	Outcome     string // allowed | denied | error
	RemoteAddr  string
	Detail      json.RawMessage
}

// AuditFilter selects a page of audit entries. All fields optional; Limit is
// clamped to [1,500] the same way EventFilter.Limit is.
type AuditFilter struct {
	SubjectKind string // exact match; empty = all
	SubjectID   string // exact match; empty = all
	Cursor      string // opaque keyset cursor from a previous page
	Limit       int
}

// AuditPage is one page of ListAuditEntries results, same shape as EventPage.
type AuditPage struct {
	Entries    []AuditEntry
	NextCursor string // "" when the page is the last one
}

// AuditStore is the seam an audit-log read API and the retention Pruner take.
type AuditStore interface {
	// InsertAuditEntry appends one row; id and the assigned timestamp come
	// back on the returned AuditEntry. detail == nil is written as the
	// column's own default, {}.
	InsertAuditEntry(ctx context.Context, subjectKind, subjectID, action, resource, outcome, remoteAddr string, detail json.RawMessage) (AuditEntry, error)
	// ListAuditEntries pages newest-first, same keyset cursor shape as
	// EventStore.ListEvents.
	ListAuditEntries(ctx context.Context, f AuditFilter) (AuditPage, error)
	// DeleteAuditEntriesBefore deletes up to limit rows older than before,
	// oldest first, and reports how many were removed. Used by Pruner's
	// sweep (prune.go); exposed here too so it is independently testable.
	DeleteAuditEntriesBefore(ctx context.Context, before time.Time, limit int32) (int64, error)
}

var _ AuditStore = (*DB)(nil)

func (db *DB) InsertAuditEntry(ctx context.Context, subjectKind, subjectID, action, resource, outcome, remoteAddr string, detail json.RawMessage) (AuditEntry, error) {
	if detail == nil {
		detail = json.RawMessage(`{}`)
	}
	start := time.Now()
	r, err := gen.New(db.pool).InsertAuditEntry(ctx, gen.InsertAuditEntryParams{
		SubjectKind: subjectKind,
		SubjectID:   subjectID,
		Action:      action,
		Resource:    resource,
		Outcome:     outcome,
		RemoteAddr:  remoteAddr,
		Detail:      detail,
	})
	db.observe(queryInsertAuditEntry, start, queryResult(err))
	if err != nil {
		return AuditEntry{}, fmt.Errorf("store: insert audit entry: %w", err)
	}
	return AuditEntry{
		ID:          r.ID,
		At:          r.At,
		SubjectKind: subjectKind,
		SubjectID:   subjectID,
		Action:      action,
		Resource:    resource,
		Outcome:     outcome,
		RemoteAddr:  remoteAddr,
		Detail:      detail,
	}, nil
}

func (db *DB) ListAuditEntries(ctx context.Context, f AuditFilter) (AuditPage, error) { //nolint:gocritic // hugeParam: AuditFilter mirrors EventFilter's value semantics (events.go)
	limit := clampLimit(f.Limit)

	var curTime pgtype.Timestamptz
	var curID pgtype.Int8
	if f.Cursor != "" {
		ts, id, ok, err := DecodeCursor(f.Cursor)
		if err != nil {
			return AuditPage{}, fmt.Errorf("store: list audit entries: %w", err)
		}
		if ok {
			curTime = pgtype.Timestamptz{Time: ts, Valid: true}
			curID = pgtype.Int8{Int64: id, Valid: true}
		}
	}

	var subjectKind, subjectID pgtype.Text
	if f.SubjectKind != "" {
		subjectKind = pgtype.Text{String: f.SubjectKind, Valid: true}
	}
	if f.SubjectID != "" {
		subjectID = pgtype.Text{String: f.SubjectID, Valid: true}
	}

	start := time.Now()
	rows, err := gen.New(db.pool).ListAuditEntries(ctx, gen.ListAuditEntriesParams{
		SubjectKind: subjectKind,
		SubjectID:   subjectID,
		CurTime:     curTime,
		CurID:       curID,
		Lim:         int32(limit), //nolint:gosec // limit is clamped to [1,500] above
	})
	db.observe(queryListAuditEntries, start, queryResult(err))
	if err != nil {
		return AuditPage{}, fmt.Errorf("store: list audit entries: %w", err)
	}

	entries := make([]AuditEntry, len(rows))
	for i := range rows {
		r := &rows[i]
		entries[i] = AuditEntry{
			ID:          r.ID,
			At:          r.At,
			SubjectKind: r.SubjectKind,
			SubjectID:   r.SubjectID,
			Action:      r.Action,
			Resource:    r.Resource,
			Outcome:     r.Outcome,
			RemoteAddr:  r.RemoteAddr,
			Detail:      r.Detail,
		}
	}

	var nextCursor string
	if len(rows) == limit {
		last := rows[len(rows)-1]
		nextCursor = EncodeCursor(last.At, last.ID)
	}

	return AuditPage{Entries: entries, NextCursor: nextCursor}, nil
}

func (db *DB) DeleteAuditEntriesBefore(ctx context.Context, before time.Time, limit int32) (int64, error) {
	start := time.Now()
	n, err := gen.New(db.pool).DeleteAuditEntriesBefore(ctx, gen.DeleteAuditEntriesBeforeParams{
		At:    before,
		Limit: limit,
	})
	db.observe(queryDeleteAuditEntriesBefore, start, queryResult(err))
	if err != nil {
		return 0, fmt.Errorf("store: delete audit entries before: %w", err)
	}
	return n, nil
}

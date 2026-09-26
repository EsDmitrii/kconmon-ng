package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
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
	// GetTokenByHash, UpdateUserPassword, UpdateUserGuarded, DeleteRole, DeleteBinding, RevokeToken,
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
	// SessionEpoch counts the disables; populated by GetUserByUsername and CreateUser only.
	SessionEpoch int64
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
		SessionEpoch: u.SessionEpoch,
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
	u, err := db.readQueries(ctx).GetUserByID(ctx, uid)
	db.observe(queryGetUserByID, start, queryResult(wrapNoRows(err)))
	if err != nil {
		return User{}, fmt.Errorf("store: get user by id: %w", wrapNoRows(err))
	}
	return userFromGetByIDRow(&u), nil
}

func (db *DB) GetUserByUsername(ctx context.Context, username string) (User, error) {
	start := time.Now()
	u, err := db.readQueries(ctx).GetUserByUsername(ctx, username)
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

// CreateBootstrapAdmin creates the local bootstrap user AND its admin binding in ONE transaction, so
// there is no half-made bootstrap for a later boot to repair. A repair could not tell a missing
// binding from a deliberate revocation, and would re-grant a demoted account past the audit log.
func (db *DB) CreateBootstrapAdmin(ctx context.Context, username, passwordHash, displayName, role string) (User, error) {
	return db.createUserWithBinding(ctx, "create bootstrap admin", username, passwordHash, displayName, role, nil)
}

// CreateUserWithRole is the admin API's create; same transaction as the bootstrap admin.
func (db *DB) CreateUserWithRole(ctx context.Context, username, passwordHash, displayName, role string) (User, error) {
	return db.createUserWithBinding(ctx, "create user", username, passwordHash, displayName, role, nil)
}

// CreateUserWithRoleGuarded is CreateUserWithRole with guard run first under the guarded writes'
// lock, so a role deleted meanwhile is not bound. guard's error is returned unwrapped.
func (db *DB) CreateUserWithRoleGuarded(ctx context.Context, username, passwordHash, displayName, role string, guard func(context.Context) error) (User, error) {
	return db.createUserWithBinding(ctx, "create user", username, passwordHash, displayName, role, guard)
}

// createUserWithBinding creates a local user and binds it to role in ONE transaction, under the
// guarded writes' lock: a user row without its binding is an account nobody can use, and a half-made
// one cannot be told from a deliberate revocation afterwards.
func (db *DB) createUserWithBinding(ctx context.Context, op, username, passwordHash, displayName, role string, guard func(context.Context) error) (User, error) {
	start := time.Now()
	var user User
	refused, err := db.guardedTx(ctx, op, guard, func(tx pgx.Tx) error {
		q := gen.New(tx)
		u, cerr := q.CreateUser(ctx, gen.CreateUserParams{Username: username, PasswordHash: passwordHash, DisplayName: displayName})
		if cerr != nil {
			return wrapUniqueViolation(cerr)
		}
		if _, berr := q.CreateBinding(ctx, gen.CreateBindingParams{RoleName: role, SubjectKind: "user", SubjectID: u.ID.String()}); berr != nil {
			return fmt.Errorf("binding: %w", wrapUniqueViolation(berr))
		}
		user = userFromRow(&u)
		return nil
	})
	db.observeGuarded(queryCreateUser, start, refused, err)
	if err != nil {
		return User{}, err
	}
	return user, nil
}

// setUserRoleTx replaces every DIRECT binding of a local user with one binding to role inside tx, so
// no reader sees the user with no role or with both; group bindings stay. The caller holds
// userAdminLockKey, which serialises role changes; the row lock doubles as the existence check.
func setUserRoleTx(ctx context.Context, tx pgx.Tx, uid pgtype.UUID, userID, role string) error {
	var one int
	if lockErr := tx.QueryRow(ctx, "SELECT 1 FROM users WHERE id = $1 FOR UPDATE", uid).Scan(&one); lockErr != nil {
		return fmt.Errorf("lock user: %w", wrapNoRows(lockErr))
	}

	q := gen.New(tx)
	direct, err := q.ListBindingsForSubject(ctx, gen.ListBindingsForSubjectParams{
		CallerKind: "user", UserID: userID, Groups: []string{},
	})
	if err != nil {
		return fmt.Errorf("list bindings: %w", err)
	}
	for _, b := range direct {
		if _, err := q.DeleteBinding(ctx, b.ID); err != nil {
			return fmt.Errorf("delete binding %d: %w", b.ID, err)
		}
	}
	if _, err := q.CreateBinding(ctx, gen.CreateBindingParams{
		RoleName: role, SubjectKind: "user", SubjectID: userID,
	}); err != nil {
		return wrapUniqueViolation(err)
	}
	return nil
}

// userAdminLockKey is the pg_advisory_xact_lock key every guarded users and RBAC write takes.
const userAdminLockKey int64 = 2111970507

// UserChange is one update of a local user; a nil field is left as it is.
type UserChange struct {
	Disabled *bool
	Role     *string
}

// UpdateUserGuarded applies change to user id in one transaction. The transaction first takes an
// advisory lock every call shares, then runs guard (when non-nil), and only then writes: two changes
// whose guards read the same state, such as the last-admin check, cannot both pass on a stale read.
// guard's error aborts the change and is returned unwrapped. ErrNotFound when id names no user.
func (db *DB) UpdateUserGuarded(ctx context.Context, id string, change UserChange, guard func(context.Context) error) error {
	uid, err := parseUUID(id)
	if err != nil {
		return fmt.Errorf("store: update user: %w: %w", ErrNotFound, err)
	}
	start := time.Now()
	refused, err := db.guardedTx(ctx, "update user", guard, func(tx pgx.Tx) error {
		if change.Disabled != nil {
			rows, derr := gen.New(tx).SetUserDisabled(ctx, gen.SetUserDisabledParams{ID: uid, Disabled: *change.Disabled})
			if derr != nil {
				return fmt.Errorf("set disabled: %w", derr)
			}
			if rows == 0 {
				return ErrNotFound
			}
		}
		if change.Role != nil {
			if rerr := setUserRoleTx(ctx, tx, uid, id, *change.Role); rerr != nil {
				return fmt.Errorf("set role: %w", rerr)
			}
		}
		return nil
	})
	db.observeGuarded(queryUpdateUser, start, refused, err)
	return err
}

// DeleteUserGuarded deletes user id and the role bindings naming them directly, in one transaction
// under the same lock and guard contract as UpdateUserGuarded; bindings have no foreign key, so
// nothing else would remove them. ErrNotFound when id names no user. API tokens are not touched: the
// caller revokes them first.
func (db *DB) DeleteUserGuarded(ctx context.Context, id string, guard func(context.Context) error) error {
	uid, err := parseUUID(id)
	if err != nil {
		return fmt.Errorf("store: delete user: %w: %w", ErrNotFound, err)
	}
	start := time.Now()
	refused, err := db.guardedTx(ctx, "delete user", guard, func(tx pgx.Tx) error {
		if _, derr := tx.Exec(ctx, "DELETE FROM role_bindings WHERE subject_kind = 'user' AND subject_id = $1", id); derr != nil {
			return fmt.Errorf("delete bindings: %w", derr)
		}
		tag, derr := tx.Exec(ctx, "DELETE FROM users WHERE id = $1", uid)
		if derr != nil {
			return fmt.Errorf("delete user: %w", derr)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
	db.observeGuarded(queryDeleteUser, start, refused, err)
	return err
}

// guardedTx runs write in one transaction that first takes userAdminLockKey and then runs guard
// (when non-nil). guard's error is returned unwrapped with refused set; the others are wrapped with
// op.
func (db *DB) guardedTx(ctx context.Context, op string, guard func(context.Context) error, write func(pgx.Tx) error) (refused bool, err error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("store: %s: begin: %w", op, err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once Commit has run

	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", userAdminLockKey); err != nil {
		return false, fmt.Errorf("store: %s: lock: %w", op, err)
	}
	// The guard's reads run on this transaction (see readQueries): every guarded writer that committed
	// before the lock was granted is visible under READ COMMITTED, and none can commit until this one
	// ends. Reading through the pool instead would need a second connection while the writers parked
	// on the lock hold the rest, and maxConns concurrent guarded writes would wedge the pool.
	if guard != nil {
		if err := guard(context.WithValue(ctx, guardTxKey{}, guardTx{db: db, tx: tx})); err != nil {
			return true, err
		}
	}
	if err := write(tx); err != nil {
		return false, fmt.Errorf("store: %s: %w", op, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: %s: commit: %w", op, err)
	}
	return false, nil
}

// guardTxKey carries a guardedTx's transaction into its guard's context.
type guardTxKey struct{}

type guardTx struct {
	db *DB
	tx pgx.Tx
}

// readQueries is where a user, role or binding read runs: on the guarded transaction when ctx is the
// one guardedTx handed its guard, on the pool otherwise. The transaction is one connection, so a guard
// must not read concurrently.
func (db *DB) readQueries(ctx context.Context) *gen.Queries {
	if g, ok := ctx.Value(guardTxKey{}).(guardTx); ok && g.db == db {
		return gen.New(g.tx)
	}
	return gen.New(db.pool)
}

// observeGuarded records one guarded write; a guard's refusal is the caller's verdict, not a failed
// query.
func (db *DB) observeGuarded(query string, start time.Time, refused bool, err error) {
	if refused {
		err = nil
	}
	db.observe(query, start, queryResult(err))
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

// RehashUserPassword replaces id's password hash with newHash only while it is still oldHash, and
// reports whether it did. false with a nil error means the hash changed since oldHash was read (or
// id names no user), so a login's hash upgrade never overwrites a concurrent password reset.
func (db *DB) RehashUserPassword(ctx context.Context, id, oldHash, newHash string) (bool, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return false, fmt.Errorf("store: rehash user password: %w", err)
	}
	start := time.Now()
	rows, err := gen.New(db.pool).RehashUserPassword(ctx, gen.RehashUserPasswordParams{
		ID:      uid,
		OldHash: oldHash,
		NewHash: newHash,
	})
	db.observe(queryRehashUserPassword, start, queryResult(err))
	if err != nil {
		return false, fmt.Errorf("store: rehash user password: %w", err)
	}
	return rows == 1, nil
}

func (db *DB) ListUsers(ctx context.Context) ([]User, error) {
	start := time.Now()
	rows, err := db.readQueries(ctx).ListUsers(ctx)
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
	n, err := db.readQueries(ctx).CountUsers(ctx)
	db.observe(queryCountUsers, start, queryResult(err))
	if err != nil {
		return 0, fmt.Errorf("store: count users: %w", err)
	}
	return n, nil
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
	rows, err := db.readQueries(ctx).ListRoles(ctx)
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
	rows, err := db.readQueries(ctx).ListBindingsForSubject(ctx, gen.ListBindingsForSubjectParams{
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
	rows, err := db.readQueries(ctx).ListBindings(ctx)
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

// UpsertRoleGuarded is UpsertRole under the lock UpdateUserGuarded takes, written only once guard
// passes; guard's error is returned unwrapped.
func (db *DB) UpsertRoleGuarded(ctx context.Context, name string, permissions []string, guard func(context.Context) error) (Role, error) {
	start := time.Now()
	var role Role
	refused, err := db.guardedTx(ctx, "upsert role", guard, func(tx pgx.Tx) error {
		r, err := gen.New(tx).UpsertRole(ctx, gen.UpsertRoleParams{Name: name, Permissions: permissions})
		if err != nil {
			return err
		}
		role = roleFromRow(r)
		return nil
	})
	db.observeGuarded(queryUpsertRole, start, refused, err)
	if err != nil {
		return Role{}, err
	}
	return role, nil
}

// DeleteRoleGuarded is DeleteRole under the lock UpdateUserGuarded takes, once guard passes.
func (db *DB) DeleteRoleGuarded(ctx context.Context, name string, guard func(context.Context) error) error {
	start := time.Now()
	refused, err := db.guardedTx(ctx, "delete role", guard, func(tx pgx.Tx) error {
		rows, err := gen.New(tx).DeleteRole(ctx, name)
		if err != nil {
			return err
		}
		if rows == 0 {
			return ErrNotFound
		}
		return nil
	})
	db.observeGuarded(queryDeleteRole, start, refused, err)
	return err
}

// DeleteBindingGuarded is DeleteBinding under the lock UpdateUserGuarded takes, once guard passes.
func (db *DB) DeleteBindingGuarded(ctx context.Context, id int64, guard func(context.Context) error) error {
	start := time.Now()
	refused, err := db.guardedTx(ctx, "delete binding", guard, func(tx pgx.Tx) error {
		rows, err := gen.New(tx).DeleteBinding(ctx, id)
		if err != nil {
			return err
		}
		if rows == 0 {
			return ErrNotFound
		}
		return nil
	})
	db.observeGuarded(queryDeleteBinding, start, refused, err)
	return err
}

// CreateBindingGuarded is CreateBinding under the lock UpdateUserGuarded takes, written only once
// guard passes; guard's error is returned unwrapped.
func (db *DB) CreateBindingGuarded(ctx context.Context, roleName, subjectKind, subjectID string, guard func(context.Context) error) (RoleBinding, error) {
	start := time.Now()
	var binding RoleBinding
	refused, err := db.guardedTx(ctx, "create binding", guard, func(tx pgx.Tx) error {
		b, err := gen.New(tx).CreateBinding(ctx, gen.CreateBindingParams{
			RoleName:    roleName,
			SubjectKind: subjectKind,
			SubjectID:   subjectID,
		})
		if err != nil {
			return wrapUniqueViolation(err)
		}
		binding = bindingFromRow(&b)
		return nil
	})
	db.observeGuarded(queryCreateBinding, start, refused, err)
	if err != nil {
		return RoleBinding{}, err
	}
	return binding, nil
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
	// InsertAuditEntry appends one row whatever the fields carry: NUL, lone surrogates and invalid
	// UTF-8 become U+FFFD and a number numeric cannot hold becomes a string (storableText,
	// storableAuditDetail); an empty detail is written as {}. The returned AuditEntry holds the
	// stored values.
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
	detail = storableAuditDetail(detail)
	subjectKind, subjectID, action = storableText(subjectKind), storableText(subjectID), storableText(action)
	resource, outcome, remoteAddr = storableText(resource), storableText(outcome), storableText(remoteAddr)
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

/*
storableAuditDetail returns detail in a form the JSONB column accepts. PostgreSQL refuses a lone
UTF-16 surrogate escape, a \u0000 escape, invalid UTF-8 and a number numeric cannot hold, and an
audit row must be written whatever a caller put in the fields it records, so the text becomes
U+FFFD and such a number its literal as a string instead of failing the insert. A detail that is
not JSON at all is replaced with {"unstorable":true}.
*/
func storableAuditDetail(detail json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(detail)) == 0 {
		return json.RawMessage(`{}`)
	}
	if json.Valid(detail) && validateJSONBStorable("detail", detail) == nil {
		return detail
	}
	unstorable := json.RawMessage(`{"unstorable":true}`)
	dec := json.NewDecoder(bytes.NewReader(detail))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return unstorable
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return unstorable
	}
	out, err := json.Marshal(storableJSONValue(v))
	if err != nil || validateJSONBStorable("detail", out) != nil {
		return unstorable
	}
	return out
}

// storableJSONValue makes a decoded JSON value storable: NUL in a string or key becomes U+FFFD and a
// number numeric cannot hold becomes its literal as a string. Decoding already turned lone
// surrogates and invalid UTF-8 into U+FFFD.
func storableJSONValue(v any) any {
	switch t := v.(type) {
	case string:
		return strings.ReplaceAll(t, "\x00", "\uFFFD")
	case json.Number:
		if !jsonbNumberStorable([]byte(t)) {
			return string(t)
		}
		return t
	case []any:
		for i := range t {
			t[i] = storableJSONValue(t[i])
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[strings.ReplaceAll(k, "\x00", "\uFFFD")] = storableJSONValue(e)
		}
		return out
	default:
		return v
	}
}

// storableText makes s acceptable to a TEXT column, which refuses NUL and invalid UTF-8.
func storableText(s string) string {
	return strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", "\uFFFD"), "\uFFFD")
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

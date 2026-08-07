-- name: GetUserByUsername :one
SELECT id, username, password_hash, display_name, disabled, created_at, updated_at
FROM users
WHERE username = $1;

-- name: GetUserByID :one
-- password_hash is NEVER selected here: this lookup exists solely to back
-- authn's owner-disabled check (GetUserByID, store/auth.go), which only ever
-- needs Disabled -- same guarantee ListUsers' own comment gives, producing a
-- distinct row type (no password_hash column in its SELECT list at all).
SELECT id, username, display_name, disabled, created_at, updated_at
FROM users
WHERE id = $1;

-- name: CreateUser :one
INSERT INTO users (username, password_hash, display_name)
VALUES ($1, $2, $3)
RETURNING id, username, password_hash, display_name, disabled, created_at, updated_at;

-- name: UpdateUserPassword :execrows
UPDATE users SET password_hash = $2, updated_at = now() WHERE id = $1;

-- name: ListUsers :many
-- password_hash is NEVER selected here: this result set is exposed to admin
-- UI and API responses, and the hash must never leave the database once
-- written (same guarantee ListTokens gives token_hash).
SELECT id, username, display_name, disabled, created_at, updated_at
FROM users
ORDER BY username;

-- name: CountUsers :one
SELECT count(*) FROM users;

-- name: SetUserDisabled :execrows
UPDATE users SET disabled = $2, updated_at = now() WHERE id = $1;

-- name: ListRoles :many
SELECT name, permissions, created_at
FROM roles
ORDER BY name;

-- name: UpsertRole :one
INSERT INTO roles (name, permissions)
VALUES ($1, $2)
ON CONFLICT (name) DO UPDATE SET permissions = EXCLUDED.permissions
RETURNING name, permissions, created_at;

-- name: DeleteRole :execrows
DELETE FROM roles WHERE name = $1;

-- name: ListBindingsForSubject :many
-- One round trip per request: resolves both the user's own bindings and every
-- group binding for the caller's group membership in a single query, rather
-- than one query per group.
SELECT id, role_name, subject_kind, subject_id, created_at
FROM role_bindings
WHERE (subject_kind = 'user' AND subject_id = sqlc.arg('user_id')::text)
   OR (subject_kind = 'group' AND subject_id = ANY(sqlc.arg('groups')::text[]))
ORDER BY role_name;

-- name: ListBindings :many
-- Every binding, unscoped -- unlike ListBindingsForSubject (one subject's own
-- resolution), this backs the RBAC admin API's GET /api/v1/rbac/bindings
-- (Task 17) and its delete-role "still referenced" guard rail, neither of
-- which can be answered by a subject-scoped query.
SELECT id, role_name, subject_kind, subject_id, created_at
FROM role_bindings
ORDER BY role_name, subject_kind, subject_id;

-- name: CreateBinding :one
INSERT INTO role_bindings (role_name, subject_kind, subject_id)
VALUES ($1, $2, $3)
RETURNING id, role_name, subject_kind, subject_id, created_at;

-- name: DeleteBinding :execrows
DELETE FROM role_bindings WHERE id = $1;

-- name: GetTokenByHash :one
-- token_hash is deliberately not in the SELECT list, even here: the caller
-- already knows the hash it looked up by, and there is never a reason to hand
-- a hash value back across this boundary.
SELECT id, name, owner, expires_at, last_used_at, revoked_at, created_at
FROM api_tokens
WHERE token_hash = $1;

-- name: CreateToken :one
INSERT INTO api_tokens (name, token_hash, owner, expires_at)
VALUES ($1, $2, $3, $4)
RETURNING id, name, owner, expires_at, last_used_at, revoked_at, created_at;

-- name: ListTokens :many
-- token_hash is NEVER selected here: this result set is exposed to admin UI
-- and API responses, and the hash must never leave the database once written.
SELECT id, name, owner, expires_at, last_used_at, revoked_at, created_at
FROM api_tokens
ORDER BY created_at DESC;

-- name: RevokeToken :execrows
UPDATE api_tokens SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL;

-- name: TouchTokenLastUsed :execrows
-- Callers (the authn layer, Task 14) must debounce this to at most once per
-- minute per token themselves -- see auth.go's TokenStore doc comment. This
-- query is intentionally a plain, cheap single-row UPDATE with no such logic
-- baked in, so the debounce policy stays entirely in the caller's hands.
UPDATE api_tokens SET last_used_at = now() WHERE id = $1;

-- name: InsertAuditEntry :one
INSERT INTO audit_log (subject_kind, subject_id, action, resource, outcome, remote_addr, detail)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id, at;

-- name: ListAuditEntries :many
-- Same keyset cursor shape as ListTopologyEvents: (at, id) DESC, seeked via
-- the row-tuple comparison below against audit_log_at_idx.
SELECT id, at, subject_kind, subject_id, action, resource, outcome, remote_addr, detail
FROM audit_log
WHERE (sqlc.narg('subject_kind')::text IS NULL OR subject_kind = sqlc.narg('subject_kind')::text)
  AND (sqlc.narg('subject_id')::text   IS NULL OR subject_id = sqlc.narg('subject_id')::text)
  AND (sqlc.narg('cur_time')::timestamptz IS NULL OR
       (at, id) < (sqlc.narg('cur_time')::timestamptz, sqlc.narg('cur_id')::bigint))
ORDER BY at DESC, id DESC
LIMIT sqlc.arg('lim');

-- name: DeleteAuditEntriesBefore :execrows
-- al alias on the subquery's own FROM: see DeleteTopologyEventsBefore's
-- comment in topology_events.sql for why sqlc v1.31.1 needs it even though
-- real PostgreSQL resolves the unaliased form unambiguously.
DELETE FROM audit_log
WHERE id IN (SELECT al.id FROM audit_log al WHERE al.at < $1 ORDER BY al.at LIMIT $2);

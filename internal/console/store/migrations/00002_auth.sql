-- +goose Up
CREATE TABLE users (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    username      TEXT        NOT NULL UNIQUE,
    password_hash TEXT        NOT NULL,             -- argon2id PHC string; NEVER a raw password
    display_name  TEXT        NOT NULL DEFAULT '',
    disabled      BOOLEAN     NOT NULL DEFAULT false,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- A row whose name collides with a built-in is rejected by the application, not by this constraint.
CREATE TABLE roles (
    name        TEXT        PRIMARY KEY,
    permissions TEXT[]      NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE role_bindings (
    id           BIGSERIAL   PRIMARY KEY,
    role_name    TEXT        NOT NULL,
    subject_kind TEXT        NOT NULL,   -- user | group | token
    subject_id   TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT role_bindings_unique UNIQUE (role_name, subject_kind, subject_id)
);
-- Deliberately NOT a foreign key to roles(name): a binding may reference a
-- built-in role, which has no row. The application validates the name against
-- built-ins ∪ roles.
CREATE INDEX role_bindings_subject_idx ON role_bindings (subject_kind, subject_id);

CREATE TABLE api_tokens (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    name         TEXT        NOT NULL,
    token_hash   BYTEA       NOT NULL UNIQUE,   -- SHA-256 of 256 random bits (Decision 11), NOT argon2
    owner        TEXT        NOT NULL,          -- creator subject id (users.id UUID for local users, token id for token-created tokens) or 'system'
    expires_at   TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE audit_log (
    id           BIGSERIAL   PRIMARY KEY,
    at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    subject_kind TEXT        NOT NULL,
    subject_id   TEXT        NOT NULL,
    action       TEXT        NOT NULL,   -- "POST /api/v1/runs" style: method + chi ROUTE PATTERN, never a raw path
    resource     TEXT        NOT NULL,
    outcome      TEXT        NOT NULL,   -- allowed | denied | error
    remote_addr  TEXT        NOT NULL,
    detail       JSONB       NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX audit_log_at_idx      ON audit_log (at DESC, id DESC);
CREATE INDEX audit_log_subject_idx ON audit_log (subject_kind, subject_id, at DESC);

-- +goose Down
DROP TABLE audit_log;
DROP TABLE api_tokens;
DROP TABLE role_bindings;
DROP TABLE roles;
DROP TABLE users;

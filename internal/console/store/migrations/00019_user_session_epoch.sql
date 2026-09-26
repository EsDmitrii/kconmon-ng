-- +goose Up
-- +goose StatementBegin

-- Disabling a user bumps session_epoch, and a local session's stamp carries the epoch it was opened
-- under: a session from before a disable stays dead after a re-enable. Existing rows start at 0,
-- which keeps every session issued so far valid across the upgrade.
ALTER TABLE users ADD COLUMN IF NOT EXISTS session_epoch BIGINT NOT NULL DEFAULT 0;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE users DROP COLUMN IF EXISTS session_epoch;
-- +goose StatementEnd

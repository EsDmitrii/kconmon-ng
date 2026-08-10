-- +goose Up
-- Before them, check_schedules recorded only success.
ALTER TABLE check_schedules
    ADD COLUMN last_error    TEXT        NOT NULL DEFAULT '',
    ADD COLUMN last_error_at TIMESTAMPTZ;

-- +goose Down
ALTER TABLE check_schedules
    DROP COLUMN last_error_at,
    DROP COLUMN last_error;

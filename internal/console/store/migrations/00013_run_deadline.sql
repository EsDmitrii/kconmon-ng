-- +goose Up
-- +goose StatementBegin

-- The deadline the RUNNER actually gave this run, written when the row is created.
--
-- Two things had to guess it before, and both guessed badly:
--
--   * The scheduler's overrun guard asks "does this schedule have a run mid-flight?" and trusted the
--     status column forever. A run whose replica died stays 'running' until something finishes it,
--     so one orphan muted its schedule for as long as that took — a ten-second schedule producing
--     nothing for a quarter of an hour, with no metric distinguishing "still working" from "its
--     replica is gone".
--
--   * ReapStuckRuns rebuilt the deadline from the spec and the worst per-pair shape the build
--     allows, which is far larger than the budget the runner computed from the run's own fan-out.
--
-- The runner knows the answer exactly (checks.runDeadline, the same value its context carries), so
-- it writes it down. NULL means a row from before this migration: both readers keep their old
-- estimate for those.
ALTER TABLE check_runs
    ADD COLUMN IF NOT EXISTS deadline_at TIMESTAMPTZ;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE check_runs DROP COLUMN IF EXISTS deadline_at;
-- +goose StatementEnd

-- +goose Up
-- last_error / last_error_at give a schedule somewhere to record WHY its last
-- fire did not produce a run (QA round 5, finding #5).
--
-- Before them, check_schedules recorded only success: last_fired_at moved on
-- every tick whether the fire produced a run or not (scheduler.fireOne always
-- advances the cadence -- see its own doc comment for why leaving a broken
-- schedule due would be a hot loop), so a schedule whose definition pointed at
-- a deleted target, or whose one-per-zone selection could not be resolved,
-- looked IDENTICAL in the console to one firing perfectly every minute:
-- enabled, a fresh "last", a "next" a minute out. The only evidence was a
-- console log line on whichever replica held the advisory lock.
--
-- WHY ON THE SCHEDULE ROW and not in an events table: the question this
-- answers is "is this cadence working RIGHT NOW", which is a property of the
-- current state of one row, read on every list of the Schedules tab. A history
-- of failures is a different question, it is already answered by check_runs
-- for the fires that DID start, and it would need a retention sweep this table
-- deliberately does not have. So: the LAST error, and only that.
--
-- last_error is TEXT NOT NULL DEFAULT '' rather than nullable, matching
-- check_results.error (migration 00003): the empty string is the honest
-- encoding of "nothing wrong", every reader is spared a null check, and the
-- column has one meaning instead of two indistinguishable ones (NULL = never
-- failed vs NULL = cleared). last_error_at IS nullable, for the same reason
-- last_fired_at is: there is no zero timestamp that does not read as the year
-- 1. The pair is written TOGETHER by MarkScheduleFired, which derives the
-- stamp from the text (queries/targets.sql), so "an error with no time" and "a
-- time with no error" are both unrepresentable.
--
-- NO INDEX. Nothing selects BY this column: the scheduler's due poll keys off
-- next_fire_at (check_schedules_due_idx) and the console reads the error on
-- rows it has already listed. An index here would cost every fire a write and
-- buy no read.
ALTER TABLE check_schedules
    ADD COLUMN last_error    TEXT        NOT NULL DEFAULT '',
    ADD COLUMN last_error_at TIMESTAMPTZ;

-- +goose Down
ALTER TABLE check_schedules
    DROP COLUMN last_error_at,
    DROP COLUMN last_error;

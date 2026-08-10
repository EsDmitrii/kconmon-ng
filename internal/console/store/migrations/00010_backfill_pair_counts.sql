-- +goose Up
-- check_runs.pair_ok / pair_failed are supposed to count PAIRS. Before the
-- pair-semantics fix the runner handed FinishRun its SAMPLE tallies instead, so
-- every interval run recorded until then carries a count of probes in a column
-- the API and the run list read as a count of pairs. The result is a row that
-- says «12/1 успешно» -- twelve of one -- and, at worst on the stand, 425 of 9.
-- New runs are already correct; these rows are a relic, and a relic in a column
-- is still the API lying every time it is read.
--
-- THE RULE, and it is the runtime's own: a pair is OK when its LATEST sample
-- succeeded, and only pairs that produced at least one sample are counted at all
-- (internal/console/checks/runner.go, pairOutcomes.pairs). check_results holds
-- exactly those samples, so the honest counts are recomputable from the rows the
-- run detail page already draws -- and recomputing them is what makes the
-- summary and the detail page agree about the same run, which is the invariant
-- pairOutcomes was written to keep.
--
-- WHY DISTINCT ON (run_id, source_node, destination_node) ORDER BY sample_seq
-- DESC, id DESC: sample_seq IS "which probe" (migration 00009), so the highest
-- one is the latest, and id breaks the tie for every row written BEFORE 00009 --
-- those all sit at the sample_seq DEFAULT of 0, one row per pair, because the
-- unique constraint then still collapsed a pair to a single row. Both eras
-- therefore reduce to the same answer through the same expression.
--
-- WHY EVERY TERMINAL RUN, not only the rows where pair_ok > pair_total: that
-- guard catches the loud cases and misses the quiet ones. An interval run over
-- 20 pairs that sampled one of them 12 times recorded pair_ok = 12, which is
-- under its pair_total and just as wrong. Recomputing every terminal run is both
-- simpler to state and strictly more correct, and it is idempotent on the rows
-- that were already right -- the WHERE below skips them so they are not even
-- rewritten. Runs that are still pending or running are left alone: their
-- counters are mid-flight and FinishRun is about to write them.
--
-- WHAT THIS DOES NOT TOUCH: status. A run's terminal status is judged on
-- SAMPLES on purpose (runner.go computes finalStatus from sampleOK/sampleFailed,
-- with an interval run's expected count being its sample count), so it was never
-- the sample/pair confusion's victim and has no business being rewritten here.
--
-- Runs whose pairs never produced a single result get 0/0 rather than being
-- skipped -- COALESCE over the LEFT JOIN -- because "no pair reported" is a
-- count, not an absence of one. check_results is never pruned on its own
-- (retention deletes check_runs and cascades), so a surviving run always still
-- has the rows this recomputes from.
WITH latest_sample AS (
    SELECT DISTINCT ON (run_id, source_node, destination_node)
           run_id,
           success
    FROM check_results
    ORDER BY run_id, source_node, destination_node, sample_seq DESC, id DESC
),
pair_tally AS (
    SELECT run_id,
           count(*) FILTER (WHERE success)::int     AS pair_ok,
           count(*) FILTER (WHERE NOT success)::int AS pair_failed
    FROM latest_sample
    GROUP BY run_id
)
UPDATE check_runs r
SET pair_ok     = COALESCE(t.pair_ok, 0),
    pair_failed = COALESCE(t.pair_failed, 0)
FROM (SELECT id FROM check_runs WHERE status IN ('succeeded', 'partial', 'failed', 'cancelled')) terminal
    LEFT JOIN pair_tally t ON t.run_id = terminal.id
WHERE r.id = terminal.id
  AND (r.pair_ok <> COALESCE(t.pair_ok, 0) OR r.pair_failed <> COALESCE(t.pair_failed, 0));

-- +goose Down
-- Deliberately empty. Down would have to restore the sample counts this replaced,
-- and they are not recoverable: the numbers it overwrote were a tally the runner
-- kept in memory and never wrote anywhere else. Recomputing them from
-- check_results is impossible for the same reason the Up works -- the table
-- records samples per pair, not the wrong total that was derived from them.
-- Rolling this migration back therefore leaves the honest counts in place, which
-- is the only outcome available and the better one anyway.
SELECT 1;

-- name: CreateRun :one
-- status is always the literal 'pending' -- a caller never gets to create a run in any other
-- status.
INSERT INTO check_runs (id, status, check_type, plane, spec, initiator_kind, initiator_id, pair_total, deadline_at)
VALUES ($1, 'pending', $2, $3, $4, $5, $6, $7, $8)
RETURNING id, created_at, started_at, finished_at, status, check_type, plane, spec, initiator_kind, initiator_id, pair_total, pair_ok, pair_failed, deadline_at;

-- name: MarkRunStarted :execrows
-- AND status = 'pending' guards the pending->running transition: a run that's already running, or
-- already terminal.
UPDATE check_runs SET status = 'running', started_at = now() WHERE id = $1 AND status = 'pending';

-- name: FinishRun :execrows
-- status + finished_at + both pair counters in one UPDATE.
UPDATE check_runs
SET status = $2, finished_at = now(), pair_ok = $3, pair_failed = $4
WHERE id = $1 AND status = 'running';

-- name: AbandonRun :execrows
-- The terminal write for a run that never reached 'running'.
--
-- FinishRun is guarded by `status = 'running'`, which is right for it: a run finishes what it
-- started. But a run whose MarkRunStarted failed (a pool exhaustion, a reset connection, a
-- statement timeout) is still 'pending', so the caller's FinishRun matched zero rows, returned
-- ErrWrongState -- and that error was swallowed as "already terminal". The row then sat at
-- 'pending' forever: the run detail page went on saying the run was about to start, and the
-- stuck-run reaper does not touch it either, because it reaps by deadline against 'running'.
--
-- This one matches BOTH pre-terminal states, so the abandon path can actually say what happened.
UPDATE check_runs
SET status = $2, finished_at = now(), pair_ok = 0, pair_failed = 0
WHERE id = $1 AND status IN ('pending', 'running');

-- name: GetRun :one
SELECT id, created_at, started_at, finished_at, status, check_type, plane, spec, initiator_kind, initiator_id, pair_total, pair_ok, pair_failed, deadline_at
FROM check_runs
WHERE id = $1;

-- name: ListRuns :many
-- Same keyset cursor shape as ListTopologyEvents/ListAuditEntries: (created_at, id) DESC.
SELECT id, created_at, started_at, finished_at, status, check_type, plane, spec, initiator_kind, initiator_id, pair_total, pair_ok, pair_failed, deadline_at
FROM check_runs
WHERE (sqlc.narg('check_type')::text IS NULL OR check_type = sqlc.narg('check_type')::text)
  AND (sqlc.narg('status')::text     IS NULL OR status = sqlc.narg('status')::text)
  AND (sqlc.narg('cur_time')::timestamptz IS NULL OR
       (created_at, id) < (sqlc.narg('cur_time')::timestamptz, sqlc.narg('cur_id')::uuid))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('lim');

-- name: GetRunResults :many
-- The run's results, NEWEST first and BOUNDED; the caller reverses to insertion order.
--
-- It used to be the one listing in this package with no limit at all, and the shape of the table
-- makes that expensive rather than theoretical: an interval run is capped at 400 pairs x 500
-- samples = 200 000 rows, each carrying the agent's verbatim result JSONB (an MTR payload is up to
-- 64 hops) -- and the run detail page re-reads the whole set every five seconds for the entire life
-- of a non-terminal run. One long run could hand a console replica a multi-hundred-megabyte
-- marshal, repeatedly.
--
-- Newest first, because a bounded read of a long run must keep the END of it: that is what the page
-- is watching. The caller asks for one row MORE than it will show, which is how it knows to say so
-- rather than silently presenting a tail as the whole.
SELECT id, run_id, source_node, destination_node, success, duration_ns, error, result, recorded_at, sample_seq
FROM check_results
WHERE run_id = $1
ORDER BY id DESC
LIMIT sqlc.arg('lim');

-- name: UpsertRunResult :one
-- A retried pair overwrites rather than erroring: ON CONFLICT ON CONSTRAINT
-- check_results_pair_unique DO UPDATE.
INSERT INTO check_results (run_id, source_node, destination_node, success, duration_ns, error, result, sample_seq)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT ON CONSTRAINT check_results_pair_unique DO UPDATE
SET success = EXCLUDED.success,
    duration_ns = EXCLUDED.duration_ns,
    error = EXCLUDED.error,
    result = EXCLUDED.result,
    recorded_at = now()
RETURNING id, run_id, source_node, destination_node, success, duration_ns, error, result, recorded_at, sample_seq;

-- name: DeleteResultsForRunsBefore :execrows
-- The CASCADE, taken in bounded batches of its own.
--
-- pruneBatchSize counts check_runs rows, and each one owns up to 200 000 check_results (the cap
-- GetRunResults' comment cites for an interval run). One "bounded" batch of 5 000 runs was therefore
-- up to a billion cascaded row deletions inside a single statement — one transaction, the pruner's
-- advisory lock held for all of it, and the WAL to match, with pruneBatchPause separating nothing.
-- Deleting the children first, LIMIT rows at a time, is what makes the batch size mean rows.
DELETE FROM check_results
WHERE id IN (
    SELECT r.id
    FROM check_results r
        JOIN check_runs cr ON cr.id = r.run_id
    WHERE cr.created_at < $1
    LIMIT $2
);

-- name: DeleteRunsBefore :execrows
-- cr alias on the subquery's own FROM: sqlc v1.31.1's own query analyzer (not real PostgreSQL --
-- verified this exact self-join resolves unambiguously against a live postgres:17-alpine) reports
-- an ambiguous column reference for the unaliased form, same quirk documented on
-- DeleteTopologyEventsBefore (topology_events.sql) and DeleteAuditEntriesBefore (auth.sql).
DELETE FROM check_runs
WHERE id IN (SELECT cr.id FROM check_runs cr WHERE cr.created_at < $1 ORDER BY cr.created_at LIMIT $2);

-- name: CountActiveRunsByInitiator :one
-- Runs this initiator has MID-FLIGHT right now, across every replica.
--
-- The scheduler's overrun guard used to consult a map in the firing replica's memory. The advisory
-- lock rotates tick by tick, so with the chart's default of two console replicas the OTHER one had
-- no memory of the run and fired the same schedule again: two overlapping runs of the same check,
-- doubling the fan-out at exactly the moment the first one was already slow.
-- ABANDONED runs do not count. The guard used to trust the status column forever, so a run whose
-- replica died — 'running' until the reaper's next pass — reported an overrun on every tick, and a
-- ten-second schedule produced nothing for as long as that took. A run past its own deadline is not
-- "still working": either it is finishing right now, or nobody is finishing it, and in both cases
-- skipping this occurrence buys nothing.
SELECT count(*)::bigint
FROM check_runs
WHERE initiator_kind = sqlc.arg('initiator_kind')::text
  AND initiator_id = sqlc.arg('initiator_id')::text
  AND status IN ('pending', 'running')
  AND (deadline_at IS NULL OR deadline_at > now());

-- name: ReapStuckRuns :execrows
-- Force-finish runs left 'running' long past any deadline the runner could have given them.
--
-- The counters are recomputed in the SAME statement, and that is the whole point. pair_ok and
-- pair_failed are otherwise written only by FinishRun, from a tally the runner keeps IN MEMORY --
-- so a replica that died mid-run (rollout, OOM, node drain) left the row 'running' with both
-- counters at their CreateRun default of 0, and this reaper then made it terminal at 0. The run
-- list and the run detail page read that as "0 of N succeeded" while check_results still holds the
-- rows for every pair that actually completed: exactly the lie migration 00010 was written to erase,
-- reintroduced by the one path that finishes a run without the runner.
--
-- The tally is 00010's, verbatim in shape: a pair is OK when its LATEST sample succeeded
-- (sample_seq DESC, id DESC breaking the tie for rows written before 00009), and only pairs that
-- produced a sample are counted at all. One statement, so status and counters can never disagree.
-- The cutoff is PER ROW, not one number for the whole fleet. A single global cutoff has to be the
-- worst run this build can accept — 400 pairs from one source over a 24h duration, which is more
-- than thirty hours — so an orphaned five-minute run sat there reporting "0 of 1 ok" for a day and a
-- half before anything touched it. Each row carries what it needs to be judged on its own: its
-- spec's Duration (nanoseconds, absent on an instant run) and its pair_total, which bounds how many
-- sequential batches the per-source gate can impose.
--
-- 'pending' is in scope as well as 'running'. A replica that died between CreateRun and
-- MarkRunStarted left a row 'pending' forever — the reaper skipped it by status and Cancel answered
-- 204 saying the reaper would deal with it, which was never true. Such a row has no samples, so the
-- tally below correctly yields 0/0.
WITH doomed AS (
    SELECT cr.id FROM check_runs cr
    WHERE cr.status IN ('running', 'pending')
      -- The run's OWN deadline when it has one (00013): the runner computed it from this run's real
      -- fan-out and its context carries exactly that value, so nothing here has to reconstruct it.
      -- The estimate below is the fallback for rows written before that column existed.
      AND (
        (cr.deadline_at IS NOT NULL AND cr.deadline_at < now() - (interval '1 second' * sqlc.arg('slack_seconds')::numeric))
        OR
        (cr.deadline_at IS NULL AND cr.created_at < now()
          - (interval '1 microsecond' * (COALESCE(NULLIF(cr.spec->>'Duration', '')::bigint, 0) / 1000))
          - (interval '1 second' * ceil(GREATEST(cr.pair_total, 1)::numeric / sqlc.arg('per_source_concurrency')::numeric)
                                 * sqlc.arg('per_pair_seconds')::numeric)
          - (interval '1 second' * sqlc.arg('slack_seconds')::numeric))
      )
    ORDER BY cr.created_at
    LIMIT sqlc.arg('lim')
),
latest_sample AS (
    SELECT DISTINCT ON (r.run_id, r.source_node, r.destination_node)
           r.run_id,
           r.success
    FROM check_results r
    JOIN doomed d ON d.id = r.run_id
    ORDER BY r.run_id, r.source_node, r.destination_node, r.sample_seq DESC, r.id DESC
),
pair_tally AS (
    SELECT run_id,
           count(*) FILTER (WHERE success)::int     AS pair_ok,
           count(*) FILTER (WHERE NOT success)::int AS pair_failed
    FROM latest_sample
    GROUP BY run_id
)
UPDATE check_runs
SET status = 'cancelled',
    finished_at = now(),
    pair_ok = COALESCE(t.pair_ok, 0),
    pair_failed = COALESCE(t.pair_failed, 0)
FROM doomed d
    LEFT JOIN pair_tally t ON t.run_id = d.id
-- The status is re-checked HERE, not only inside `doomed`.
--
-- Under READ COMMITTED the row is re-locked and this qual is re-evaluated after the lock is granted,
-- so with `id = d.id` alone a run that FinishRun committed while the reaper waited on that lock was
-- rewritten to 'cancelled': FinishRun had already returned success to the runner, which reported the
-- run as succeeded, while the table and the run-detail page said cancelled. MarkRunStarted and
-- FinishRun both carry their own status predicate for exactly this reason.
WHERE check_runs.id = d.id
  AND check_runs.status IN ('running', 'pending');

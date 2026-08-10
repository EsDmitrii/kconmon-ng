-- name: CreateRun :one
-- status is always the literal 'pending' -- a caller never gets to create a run in any other
-- status.
INSERT INTO check_runs (id, status, check_type, plane, spec, initiator_kind, initiator_id, pair_total)
VALUES ($1, 'pending', $2, $3, $4, $5, $6, $7)
RETURNING id, created_at, started_at, finished_at, status, check_type, plane, spec, initiator_kind, initiator_id, pair_total, pair_ok, pair_failed;

-- name: MarkRunStarted :execrows
-- AND status = 'pending' guards the pending->running transition: a run that's already running, or
-- already terminal.
UPDATE check_runs SET status = 'running', started_at = now() WHERE id = $1 AND status = 'pending';

-- name: FinishRun :execrows
-- status + finished_at + both pair counters in one UPDATE.
UPDATE check_runs
SET status = $2, finished_at = now(), pair_ok = $3, pair_failed = $4
WHERE id = $1 AND status = 'running';

-- name: GetRun :one
SELECT id, created_at, started_at, finished_at, status, check_type, plane, spec, initiator_kind, initiator_id, pair_total, pair_ok, pair_failed
FROM check_runs
WHERE id = $1;

-- name: ListRuns :many
-- Same keyset cursor shape as ListTopologyEvents/ListAuditEntries: (created_at, id) DESC.
SELECT id, created_at, started_at, finished_at, status, check_type, plane, spec, initiator_kind, initiator_id, pair_total, pair_ok, pair_failed
FROM check_runs
WHERE (sqlc.narg('check_type')::text IS NULL OR check_type = sqlc.narg('check_type')::text)
  AND (sqlc.narg('status')::text     IS NULL OR status = sqlc.narg('status')::text)
  AND (sqlc.narg('cur_time')::timestamptz IS NULL OR
       (created_at, id) < (sqlc.narg('cur_time')::timestamptz, sqlc.narg('cur_id')::uuid))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('lim');

-- name: GetRunResults :many
-- ORDER BY id, not (pair, sample_seq): id is insertion order.
SELECT id, run_id, source_node, destination_node, success, duration_ns, error, result, recorded_at, sample_seq
FROM check_results
WHERE run_id = $1
ORDER BY id;

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

-- name: DeleteRunsBefore :execrows
-- cr alias on the subquery's own FROM: sqlc v1.31.1's own query analyzer (not real PostgreSQL --
-- verified this exact self-join resolves unambiguously against a live postgres:17-alpine) reports
-- an ambiguous column reference for the unaliased form, same quirk documented on
-- DeleteTopologyEventsBefore (topology_events.sql) and DeleteAuditEntriesBefore (auth.sql).
DELETE FROM check_runs
WHERE id IN (SELECT cr.id FROM check_runs cr WHERE cr.created_at < $1 ORDER BY cr.created_at LIMIT $2);

-- name: ReapStuckRuns :execrows
-- Force-finish runs left 'running' long past any deadline the runner could have given them.
UPDATE check_runs
SET status = 'cancelled', finished_at = now()
WHERE id IN (
    SELECT cr.id FROM check_runs cr
    WHERE cr.status = 'running' AND cr.created_at < $1
    ORDER BY cr.created_at
    LIMIT $2
);

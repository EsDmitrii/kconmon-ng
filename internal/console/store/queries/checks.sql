-- name: CreateRun :one
-- status is always the literal 'pending' -- a caller never gets to create a
-- run in any other status; the lifecycle only ever advances it forward via
-- MarkRunStarted and FinishRun below. id is caller-supplied (not DB-default,
-- unlike users/api_tokens): the runner mints the UUID before this INSERT so
-- it can also be the ephemeral WS "run:{id}" topic name (Task 20) without a
-- round trip first.
INSERT INTO check_runs (id, status, check_type, plane, spec, initiator_kind, initiator_id, pair_total)
VALUES ($1, 'pending', $2, $3, $4, $5, $6, $7)
RETURNING id, created_at, started_at, finished_at, status, check_type, plane, spec, initiator_kind, initiator_id, pair_total, pair_ok, pair_failed;

-- name: MarkRunStarted :execrows
-- AND status = 'pending' guards the pending->running transition: a run
-- that's already running, or already terminal, is left untouched (0 rows),
-- so a caller can tell "no such run" apart from "wrong state" -- see
-- checks.go's disambiguating GetRun lookup on rows == 0.
UPDATE check_runs SET status = 'running', started_at = now() WHERE id = $1 AND status = 'pending';

-- name: FinishRun :execrows
-- status + finished_at + both pair counters in one UPDATE, so a reader never
-- observes a run whose status says "succeeded" while pair_ok/pair_failed
-- still read their zero-value default. AND status = 'running' guards the
-- running->terminal transition the same way MarkRunStarted's AND status =
-- 'pending' does: a run that never started, or already finished, is left
-- untouched (0 rows) rather than silently overwritten by a retry.
UPDATE check_runs
SET status = $2, finished_at = now(), pair_ok = $3, pair_failed = $4
WHERE id = $1 AND status = 'running';

-- name: GetRun :one
SELECT id, created_at, started_at, finished_at, status, check_type, plane, spec, initiator_kind, initiator_id, pair_total, pair_ok, pair_failed
FROM check_runs
WHERE id = $1;

-- name: ListRuns :many
-- Same keyset cursor shape as ListTopologyEvents/ListAuditEntries: (created_at,
-- id) DESC, seeked via the row-tuple comparison below against
-- check_runs_created_idx. id is UUID rather than bigint here -- it does not
-- need to be a meaningful sort order, only a stable deterministic tie-breaker
-- for rows sharing one created_at.
SELECT id, created_at, started_at, finished_at, status, check_type, plane, spec, initiator_kind, initiator_id, pair_total, pair_ok, pair_failed
FROM check_runs
WHERE (sqlc.narg('check_type')::text IS NULL OR check_type = sqlc.narg('check_type')::text)
  AND (sqlc.narg('status')::text     IS NULL OR status = sqlc.narg('status')::text)
  AND (sqlc.narg('cur_time')::timestamptz IS NULL OR
       (created_at, id) < (sqlc.narg('cur_time')::timestamptz, sqlc.narg('cur_id')::uuid))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('lim');

-- name: GetRunResults :many
SELECT id, run_id, source_node, destination_node, success, duration_ns, error, result, recorded_at
FROM check_results
WHERE run_id = $1
ORDER BY id;

-- name: UpsertRunResult :one
-- A retried pair overwrites rather than erroring: ON CONFLICT ON CONSTRAINT
-- check_results_pair_unique DO UPDATE, not DO NOTHING.
INSERT INTO check_results (run_id, source_node, destination_node, success, duration_ns, error, result)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT ON CONSTRAINT check_results_pair_unique DO UPDATE
SET success = EXCLUDED.success,
    duration_ns = EXCLUDED.duration_ns,
    error = EXCLUDED.error,
    result = EXCLUDED.result,
    recorded_at = now()
RETURNING id, run_id, source_node, destination_node, success, duration_ns, error, result, recorded_at;

-- name: DeleteRunsBefore :execrows
-- cr alias on the subquery's own FROM: sqlc v1.31.1's own query analyzer
-- (not real PostgreSQL -- verified this exact self-join resolves
-- unambiguously against a live postgres:17-alpine) reports an ambiguous
-- column reference for the unaliased form, same quirk documented on
-- DeleteTopologyEventsBefore (topology_events.sql) and
-- DeleteAuditEntriesBefore (auth.sql). ON DELETE CASCADE on check_results.run_id
-- means deleting the run row here is enough to also drop its results -- no
-- separate check_results sweep exists or is needed.
DELETE FROM check_runs
WHERE id IN (SELECT cr.id FROM check_runs cr WHERE cr.created_at < $1 ORDER BY cr.created_at LIMIT $2);

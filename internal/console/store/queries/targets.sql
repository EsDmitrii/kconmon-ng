-- name: CreateTarget :one
-- id is caller-supplied, same as CreateRun (checks.sql): targets.id has no
-- column DEFAULT, so the Go layer mints the UUID. That also means a caller
-- retrying a create with the same id gets ErrAlreadyExists rather than a
-- second row.
INSERT INTO targets (id, name, kind, address, labels)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, name, kind, address, labels, created_at, updated_at;

-- name: UpdateTarget :one
-- A full replace, not a patch: every mutable column is written on every call,
-- so the caller's TargetInput is the whole truth about the row afterwards and
-- there is no "field absent means keep" ambiguity to get wrong. :one (not
-- :execrows) so the refreshed updated_at comes back without a second round
-- trip; an id matching nothing yields pgx.ErrNoRows -> ErrNotFound.
UPDATE targets
SET name = $2, kind = $3, address = $4, labels = $5, updated_at = now()
WHERE id = $1
RETURNING id, name, kind, address, labels, created_at, updated_at;

-- name: DeleteTarget :execrows
-- No cascade of any kind: check_definitions.destination_target_id is
-- ON DELETE RESTRICT, so this statement fails with a foreign_key_violation
-- (mapped to ErrInUse) while any definition still probes the target.
DELETE FROM targets WHERE id = $1;

-- name: GetTarget :one
SELECT id, name, kind, address, labels, created_at, updated_at
FROM targets
WHERE id = $1;

-- name: ListTargets :many
-- Same keyset cursor shape as ListRuns: (created_at, id) DESC seeked via a
-- row-tuple comparison. targets carries no (created_at DESC, id DESC) index
-- -- it is a curated configuration table an operator maintains by hand, sized
-- in the tens, so the ordering is a sort over a handful of rows, not a scan
-- the planner needs help with.
SELECT id, name, kind, address, labels, created_at, updated_at
FROM targets
WHERE (sqlc.narg('kind')::text IS NULL OR kind = sqlc.narg('kind')::text)
  AND (sqlc.narg('cur_time')::timestamptz IS NULL OR
       (created_at, id) < (sqlc.narg('cur_time')::timestamptz, sqlc.narg('cur_id')::uuid))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('lim');

-- name: CreateDefinition :one
-- destination_target_id is NULL for every destination_kind other than
-- 'target'; a non-NULL value naming no targets row fails with a
-- foreign_key_violation, which the Go layer reports as ErrNotFound (the
-- reference is missing) rather than ErrInUse (which is the DELETE direction).
INSERT INTO check_definitions (
    id, name, source_selection, destination_kind, destination_target_id,
    destination_address, check_type, plane, params, enabled
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING id, name, source_selection, destination_kind, destination_target_id,
          destination_address, check_type, plane, params, enabled, created_at, updated_at;

-- name: UpdateDefinition :one
-- Full replace, same contract as UpdateTarget.
UPDATE check_definitions
SET name = $2, source_selection = $3, destination_kind = $4, destination_target_id = $5,
    destination_address = $6, check_type = $7, plane = $8, params = $9, enabled = $10,
    updated_at = now()
WHERE id = $1
RETURNING id, name, source_selection, destination_kind, destination_target_id,
          destination_address, check_type, plane, params, enabled, created_at, updated_at;

-- name: DeleteDefinition :execrows
-- ON DELETE CASCADE on check_schedules.definition_id removes the definition's
-- schedules along with it -- no separate schedule sweep exists or is needed.
DELETE FROM check_definitions WHERE id = $1;

-- name: GetDefinition :one
SELECT id, name, source_selection, destination_kind, destination_target_id,
       destination_address, check_type, plane, params, enabled, created_at, updated_at
FROM check_definitions
WHERE id = $1;

-- name: ListDefinitions :many
-- The target_id filter is what check_definitions_target_idx exists for: the
-- "which definitions would this target's deletion break?" question an admin
-- API asks before it ever attempts the DELETE.
SELECT id, name, source_selection, destination_kind, destination_target_id,
       destination_address, check_type, plane, params, enabled, created_at, updated_at
FROM check_definitions
WHERE (sqlc.narg('target_id')::uuid IS NULL OR destination_target_id = sqlc.narg('target_id')::uuid)
  AND (sqlc.narg('enabled')::boolean IS NULL OR enabled = sqlc.narg('enabled')::boolean)
  AND (sqlc.narg('cur_time')::timestamptz IS NULL OR
       (created_at, id) < (sqlc.narg('cur_time')::timestamptz, sqlc.narg('cur_id')::uuid))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('lim');

-- name: CreateSchedule :one
INSERT INTO check_schedules (id, definition_id, kind, interval_ns, run_at, enabled, next_fire_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id, definition_id, kind, interval_ns, run_at, enabled,
          last_fired_at, next_fire_at, created_at, updated_at;

-- name: UpdateSchedule :one
-- definition_id is deliberately NOT updatable: re-pointing a schedule at a
-- different definition is a different schedule, and letting it move would
-- silently reinterpret last_fired_at/next_fire_at against a cadence they were
-- never computed for.
UPDATE check_schedules
SET kind = $2, interval_ns = $3, run_at = $4, enabled = $5, next_fire_at = $6, updated_at = now()
WHERE id = $1
RETURNING id, definition_id, kind, interval_ns, run_at, enabled,
          last_fired_at, next_fire_at, created_at, updated_at;

-- name: DeleteSchedule :execrows
DELETE FROM check_schedules WHERE id = $1;

-- name: GetSchedule :one
SELECT id, definition_id, kind, interval_ns, run_at, enabled,
       last_fired_at, next_fire_at, created_at, updated_at
FROM check_schedules
WHERE id = $1;

-- name: ListSchedules :many
SELECT id, definition_id, kind, interval_ns, run_at, enabled,
       last_fired_at, next_fire_at, created_at, updated_at
FROM check_schedules
WHERE (sqlc.narg('definition_id')::uuid IS NULL OR definition_id = sqlc.narg('definition_id')::uuid)
  AND (sqlc.narg('cur_time')::timestamptz IS NULL OR
       (created_at, id) < (sqlc.narg('cur_time')::timestamptz, sqlc.narg('cur_id')::uuid))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('lim');

-- name: ListDueSchedules :many
-- The WHERE clause is written to match check_schedules_due_idx exactly --
-- "enabled" bare (the index's own predicate) and a plain range test on
-- next_fire_at (its key), nothing else. next_fire_at IS NOT NULL is left
-- implicit: a NULL never satisfies <=, and spelling it out would add a clause
-- the partial index cannot be matched against. ORDER BY next_fire_at is the
-- index's own order, so the scheduler's due poll is an index range scan with
-- no sort, however large the table grows.
SELECT id, definition_id, kind, interval_ns, run_at, enabled,
       last_fired_at, next_fire_at, created_at, updated_at
FROM check_schedules
WHERE enabled AND next_fire_at <= sqlc.arg('due')::timestamptz
ORDER BY next_fire_at
LIMIT sqlc.arg('lim');

-- name: MarkScheduleFired :execrows
-- The scheduler's post-dispatch bookkeeping: stamp what just fired and when
-- the next fire is due, in one UPDATE, so a reader never sees a schedule whose
-- last_fired_at moved while next_fire_at still points at the fire that already
-- happened (which would make ListDueSchedules hand it out a second time).
-- next_fire_at = NULL retires the schedule from the due index without
-- disabling it -- the terminal state of a kind='once' schedule.
UPDATE check_schedules
SET last_fired_at = $2, next_fire_at = $3, updated_at = now()
WHERE id = $1;

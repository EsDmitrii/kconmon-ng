-- name: CreateAlertRule :one
-- id is caller-supplied, same as CreateTarget (targets.sql): the column has a
-- gen_random_uuid() DEFAULT for anything writing this table by hand, but the
-- Go layer mints its own so a create is retry-identifiable. The sync columns
-- are deliberately absent from the INSERT: a rule that has just been typed has
-- never been applied, and 'unsynced' is exactly the column DEFAULT.
INSERT INTO alert_rules (
    id, name, kind, params, severity, for_ns, labels, annotations, enabled, rendered_expr
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING id, name, kind, params, severity, for_ns, labels, annotations, enabled,
          rendered_expr, sync_status, sync_message, last_synced_at, created_at, updated_at;

-- name: GetAlertRule :one
SELECT id, name, kind, params, severity, for_ns, labels, annotations, enabled,
       rendered_expr, sync_status, sync_message, last_synced_at, created_at, updated_at
FROM alert_rules
WHERE id = $1;

-- name: ListAlertRules :many
-- UNPAGED by design, the same call ListWebhooks makes: the row count is rules
-- an operator typed -- dozens, not thousands -- bounded by how many were
-- configured rather than by how long the system has been running. A keyset
-- cursor over that would be machinery with nothing to do.
--
-- ORDER BY lower(name) is the alert_rules_name_lower_idx order, and it is
-- TOTAL rather than merely stable: that index is UNIQUE, so no two rows can
-- share a sort key and no tiebreaker column is needed.
--
-- enabled_only is a plain boolean rather than a nullable narg: "every rule" and
-- "the enabled ones" are the only two questions this list is ever asked, and
-- a three-state filter would invent a third nobody has.
SELECT id, name, kind, params, severity, for_ns, labels, annotations, enabled,
       rendered_expr, sync_status, sync_message, last_synced_at, created_at, updated_at
FROM alert_rules
WHERE NOT sqlc.arg('enabled_only')::boolean OR enabled
ORDER BY lower(name);

-- name: UpdateAlertRule :one
-- The BUILDER half of the row, and only that half. sync_status is reset to
-- 'unsynced' and sync_message cleared in the same statement rather than left
-- alone: a rule whose expression just changed is BY DEFINITION not the rule
-- that was applied to the cluster, and a stale 'synced' would tell the drift
-- view -- and the operator -- the opposite. sync_message goes with it because
-- a message is the explanation OF a status, and an explanation outliving the
-- status it explained is a lie the UI would render verbatim.
--
-- last_synced_at is deliberately KEPT. It is a historical fact ("the last time
-- our bytes reached the cluster"), not a claim about the current row, and
-- clearing it would erase the only evidence that this rule was ever applied.
UPDATE alert_rules
SET name = $2, kind = $3, params = $4, severity = $5, for_ns = $6,
    labels = $7, annotations = $8, enabled = $9, rendered_expr = $10,
    sync_status = 'unsynced', sync_message = '', updated_at = now()
WHERE id = $1
RETURNING id, name, kind, params, severity, for_ns, labels, annotations, enabled,
          rendered_expr, sync_status, sync_message, last_synced_at, created_at, updated_at;

-- name: UpdateAlertRuleSyncStatus :one
-- The RECONCILER's write-back (M7 Decision 5), touching the three sync columns
-- and nothing else -- not even updated_at. updated_at means "when the operator
-- last changed this rule", and a 60s reconcile loop bumping it would make every
-- rule in the list look freshly edited every minute, which is the one thing the
-- column exists to answer.
UPDATE alert_rules
SET sync_status = $2, sync_message = $3, last_synced_at = $4
WHERE id = $1
RETURNING id, name, kind, params, severity, for_ns, labels, annotations, enabled,
          rendered_expr, sync_status, sync_message, last_synced_at, created_at, updated_at;

-- name: DeleteAlertRule :execrows
-- No cascade and no dependents: a rule references nothing and nothing
-- references it. The CRD object it rendered into is the reconciler's to
-- withdraw, not this statement's.
DELETE FROM alert_rules WHERE id = $1;

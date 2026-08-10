-- name: CreateAlertRule :one
-- id is caller-supplied, same as CreateTarget (targets.sql); the sync columns are deliberately
-- absent from the INSERT.
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
-- UNPAGED by design, the same call ListWebhooks makes: the row count is rules an operator typed --
-- dozens.
SELECT id, name, kind, params, severity, for_ns, labels, annotations, enabled,
       rendered_expr, sync_status, sync_message, last_synced_at, created_at, updated_at
FROM alert_rules
WHERE NOT sqlc.arg('enabled_only')::boolean OR enabled
ORDER BY lower(name);

-- name: UpdateAlertRule :one
-- The BUILDER half of the row, and only that half.
UPDATE alert_rules
SET name = $2, kind = $3, params = $4, severity = $5, for_ns = $6,
    labels = $7, annotations = $8, enabled = $9, rendered_expr = $10,
    sync_status = 'unsynced', sync_message = '', updated_at = now()
WHERE id = $1
RETURNING id, name, kind, params, severity, for_ns, labels, annotations, enabled,
          rendered_expr, sync_status, sync_message, last_synced_at, created_at, updated_at;

-- name: UpdateAlertRuleSyncStatus :one
-- The RECONCILER's write-back, touching the three sync columns and nothing else -- not even
-- updated_at.
UPDATE alert_rules
SET sync_status = $2, sync_message = $3, last_synced_at = $4
WHERE id = $1
RETURNING id, name, kind, params, severity, for_ns, labels, annotations, enabled,
          rendered_expr, sync_status, sync_message, last_synced_at, created_at, updated_at;

-- name: DeleteAlertRule :execrows
-- No cascade and no dependents: a rule references nothing and nothing references it.
DELETE FROM alert_rules WHERE id = $1;

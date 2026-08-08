-- name: CreateWebhook :one
-- secret_enc travels in and out of this layer as OPAQUE BYTES: the store never
-- encrypts, decrypts or inspects it (migration 00006's comment on the column).
INSERT INTO webhooks (id, name, url, events, secret_enc, enabled)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, name, url, events, secret_enc, enabled, last_status, last_attempt, failures, created_at;

-- name: GetWebhook :one
SELECT id, name, url, events, secret_enc, enabled, last_status, last_attempt, failures, created_at
FROM webhooks
WHERE id = $1;

-- name: ListWebhooks :many
-- Unpaged by design, the same call ListMTRDestinations makes: the row count is
-- configured endpoints, bounded by how many an operator typed, not by time. A
-- keyset cursor over a table that will hold single digits of rows would be
-- machinery with nothing to do.
SELECT id, name, url, events, secret_enc, enabled, last_status, last_attempt, failures, created_at
FROM webhooks
ORDER BY created_at DESC, id DESC;

-- name: UpdateWebhook :one
-- A full replace of the CONFIGURED half of the row, and only that half:
-- last_status/last_attempt/failures are delivery OUTCOMES, written by
-- UpdateWebhookDelivery, and an operator editing the URL must not reset the
-- endpoint's failure history along with it.
UPDATE webhooks
SET name = $2, url = $3, events = $4, secret_enc = $5, enabled = $6
WHERE id = $1
RETURNING id, name, url, events, secret_enc, enabled, last_status, last_attempt, failures, created_at;

-- name: UpdateWebhookDelivery :execrows
-- The dispatcher's write-back after a terminal delivery outcome (M6 Decision
-- 5). failures is set, not incremented, because the dispatcher -- not this
-- layer -- knows whether the attempt ended a streak (0) or extended one, and a
-- SET is idempotent under the retry that a += is not.
UPDATE webhooks
SET last_status = $2, last_attempt = $3, failures = $4
WHERE id = $1;

-- name: DeleteWebhook :execrows
DELETE FROM webhooks WHERE id = $1;

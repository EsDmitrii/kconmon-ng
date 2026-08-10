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
-- Unpaged by design, the same call ListMTRDestinations makes: the row count is configured
-- endpoints.
SELECT id, name, url, events, secret_enc, enabled, last_status, last_attempt, failures, created_at
FROM webhooks
ORDER BY created_at DESC, id DESC;

-- name: UpdateWebhook :one
-- A full replace of the CONFIGURED half of the row, and only that half.
UPDATE webhooks
SET name = $2, url = $3, events = $4, secret_enc = $5, enabled = $6
WHERE id = $1
RETURNING id, name, url, events, secret_enc, enabled, last_status, last_attempt, failures, created_at;

-- name: UpdateWebhookDelivery :execrows
-- The dispatcher's write-back after a terminal delivery outcome.
UPDATE webhooks
SET last_status = $2, last_attempt = $3, failures = $4
WHERE id = $1;

-- name: DeleteWebhook :execrows
DELETE FROM webhooks WHERE id = $1;

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
--
-- The counter is derived FROM THE ROW, not from a number the caller computed. It used to be
-- `failures = $4`, with the dispatcher passing its own snapshot + 1 — and that snapshot is taken at
-- ENQUEUE time (fanOut reads every endpoint once and copies the count into each job), while a
-- delivery holds its job through a 0s/30s/5m retry ladder, up to ~5.7 minutes. With the alert poll
-- running every 10-30s, several deliveries for one endpoint overlap constantly, and each wrote the
-- same stale base back: three consecutive failures recorded as one. The consecutive-failure count is
-- what an operator reads to decide an endpoint is dead, so undercounting it is the direction that
-- matters.
--
-- `reset` true zeroes it (a delivery succeeded); false increments whatever the row currently holds.
UPDATE webhooks
SET last_status  = $2,
    last_attempt = $3,
    failures     = CASE WHEN sqlc.arg('reset')::boolean THEN 0 ELSE failures + 1 END
WHERE id = $1;

-- name: DeleteWebhook :execrows
DELETE FROM webhooks WHERE id = $1;

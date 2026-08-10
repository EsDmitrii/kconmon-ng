-- +goose Up
-- alert_rules is the console's ALERTING BUILDER MODEL: one row per rule the console owns;
-- Prometheus evaluates.
CREATE TABLE alert_rules (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    name           TEXT        NOT NULL CHECK (length(name) BETWEEN 1 AND 63),
    kind           TEXT        NOT NULL CHECK (kind IN (
                       'pair-loss', 'zone-latency', 'dns-failures', 'http-ttfb',
                       'cert-expiry', 'agent-missing', 'external-target-down', 'raw')),
    params         JSONB       NOT NULL DEFAULT '{}'::jsonb,
    severity       TEXT        NOT NULL CHECK (severity IN ('info', 'warning', 'critical')),
    for_ns         BIGINT      NOT NULL DEFAULT 0 CHECK (for_ns >= 0),
    labels         JSONB       NOT NULL DEFAULT '{}'::jsonb,
    annotations    JSONB       NOT NULL DEFAULT '{}'::jsonb,
    enabled        BOOLEAN     NOT NULL DEFAULT true,
    rendered_expr  TEXT        NOT NULL DEFAULT '',
    sync_status    TEXT        NOT NULL DEFAULT 'unsynced'
                       CHECK (sync_status IN ('unsynced', 'synced', 'drift', 'error')),
    sync_message   TEXT        NOT NULL DEFAULT '',
    last_synced_at TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The case-insensitive uniqueness above, and simultaneously the listing's own
-- order: ListAlertRules sorts by lower(name), so the whole (unpaged) list
-- comes out of this index rather than out of a sort.
CREATE UNIQUE INDEX alert_rules_name_lower_idx ON alert_rules (lower(name));

-- +goose Down
DROP TABLE alert_rules;

-- +goose Up
-- alert_rules is the console's ALERTING BUILDER MODEL (M7 Task 1): one row per
-- rule the console owns, rendered into a PrometheusRule object and
-- server-side-applied by the reconciler (M7 Decision 5). Prometheus evaluates;
-- the console only manages. Nothing in this table is ever evaluated here.
--
-- This table is CONFIGURATION, NOT OBSERVATION (M7 Decision 1), exactly like
-- webhooks: it is what an operator typed, it does not accumulate with time
-- (one row per rule, dozens at most), and it is therefore NEVER swept by
-- retention. See store/prune.go's table-label comment, and the pin
-- TestAlertRulesAreNotARetentionTable that stops a later reader from
-- "completing" the sweep list with it.
--
-- name is the operator-facing handle AND the seed of the rendered alert's
-- `alertname` label, which is why it carries targets.name's length bound
-- (migration 00004) and why its charset is enforced in Go (store/alertrules.go)
-- -- Postgres cannot express that rule, and a name that is not a legal label
-- value is a rule that renders into something Prometheus rejects.
--
-- Uniqueness is pinned CASE-INSENSITIVELY, via a UNIQUE INDEX on lower(name)
-- rather than a UNIQUE constraint on the column. targets/check_definitions use
-- the plain column constraint, and that is deliberately NOT copied here: two
-- rules named `PairLoss` and `pairloss` render into two alerts an operator
-- reads as one, and the group they land in has no way to tell them apart in a
-- dashboard or a notification. The index is the only place this can be
-- enforced -- Go cannot see the other rows.
--
-- kind, severity and sync_status ARE CHECK-constrained, which is the opposite
-- of the choice made for targets.kind and check_schedules.kind (M4 Decision 9,
-- "widening is code, not a migration"). The reason is that these three
-- vocabularies are not ours to widen freely: kind is the set of templates the
-- renderer (M7 Task 2) knows how to turn into PromQL, severity is the label
-- Alertmanager routing keys off, and sync_status is the reconciler's own state
-- machine. A value outside any of them is a row nothing downstream can act on,
-- so the database refuses it rather than storing a rule that renders to
-- nothing. The same closed sets are checked in Go first, so a caller gets a
-- named error instead of a raw constraint violation; the CHECK is the backstop
-- for anything that ever writes this table without going through the store.
--
-- params is the per-kind builder payload, validated CLOSED by the renderer
-- (M7 Task 2), not here: this layer only guarantees it is a JSON OBJECT, since
-- every reader of it indexes by key. kind='raw' is the one exception the store
-- does look inside for -- params.expr must be a non-empty string, because a
-- raw rule with no expression is not a rule.
--
-- for_ns is NANOSECONDS, the repo-wide duration convention (00004's
-- interval_ns). The store does not convert; the renderer formats it as
-- Prometheus' own duration spelling.
--
-- rendered_expr is the DERIVED half kept alongside the builder half: it is
-- what was last rendered from these fields, so the drift view can diff
-- rendered-vs-live without re-running the renderer, and so a rule whose
-- template changed under it is visible as a row whose stored bytes no longer
-- match. It is written by the same UPDATE the builder fields are.
--
-- sync_status/sync_message/last_synced_at are the reconciler's write-back and
-- are updated INDEPENDENTLY of the builder fields (store/alertrules.go's two
-- narrow updates): a sync result must never disturb what the operator typed,
-- and an edit must never claim a sync that has not happened.
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

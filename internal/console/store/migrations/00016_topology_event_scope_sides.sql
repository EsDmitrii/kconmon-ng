-- +goose Up
-- +goose StatementBegin

-- The pair-aware event filter, given something an index can answer.
--
-- ListTopologyEvents' scope_node branch asks "is this node either side of the scope", and scope is a
-- composite string: a node scope is "node-7", a pair scope is "node-7→node-9". The branch was three
-- ORed predicates, and the third one --
--   scope LIKE '%→' || node
-- -- leads with a wildcard, so no btree could serve it. topology_events_scope_time_idx covers only
-- the equality branch, and one unindexable arm of an OR takes the whole clause with it: every
-- GET /api/v1/events?scopeNode=<name> walked the table in event_time order until it had collected
-- `limit` matches. The LIMIT bounds the response, not the work, and the handler applies no default
-- time window -- so a node that stopped being probed, a typo, or simply deep paging read the entire
-- events table, the largest one the console keeps.
--
-- The two sides become columns of their own. Both are STORED generated columns, so they cost one
-- split at insert and nothing at read, and they are exactly what the query asks about:
--   * a NODE scope has no separator, so both sides are the scope itself
--   * a PAIR scope splits on the arrow
-- which means `scope_left = $1 OR scope_right = $1` also covers the plain `scope = $1` case, and
-- both arms are index lookups.
--
-- split_part is immutable, which is what lets these be generated columns at all.
ALTER TABLE topology_events
    ADD COLUMN IF NOT EXISTS scope_left  text
        GENERATED ALWAYS AS (split_part(scope, '→', 1)) STORED,
    ADD COLUMN IF NOT EXISTS scope_right text
        GENERATED ALWAYS AS (
            CASE WHEN position('→' in scope) > 0 THEN split_part(scope, '→', 2) ELSE scope END
        ) STORED;

CREATE INDEX IF NOT EXISTS topology_events_scope_left_time_idx
    ON topology_events (scope_left, event_time DESC, id DESC);

CREATE INDEX IF NOT EXISTS topology_events_scope_right_time_idx
    ON topology_events (scope_right, event_time DESC, id DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS topology_events_scope_right_time_idx;
DROP INDEX IF EXISTS topology_events_scope_left_time_idx;
ALTER TABLE topology_events
    DROP COLUMN IF EXISTS scope_right,
    DROP COLUMN IF EXISTS scope_left;
-- +goose StatementEnd

-- +goose Up
-- topology_events is the durable projection of the controller event stream that
-- events.Ingester already receives (internal/console/events). The natural key is
-- the SAME pair the WebSocket hub uses for cross-replica dedupe: LiveEvent.ID is
-- fmt.Sprintf("%d-%d", ev.Seq, ts.UnixNano()) (internal/console/events/live_event.go),
-- split here into two typed columns. Every console replica ingests the same
-- stream and writes the same row, so the unique constraint plus ON CONFLICT DO
-- NOTHING is what makes N-replica ingestion idempotent instead of N-fold
-- duplicated. Persistence therefore inherits exactly the hub's dedupe guarantee
-- -- no better, no worse: two events that share (seq, event_time) are treated as
-- one, which across a controller restart that resets seq is astronomically
-- unlikely but is not a proof.
--
-- event_time is TIMESTAMPTZ, i.e. microsecond resolution, so ToLiveEvent
-- truncates the controller timestamp to microseconds BEFORE building the id.
-- That is what keeps this column a lossless carrier of the id half: the value
-- written here is bit-for-bit the value the id was derived from, and
-- httpapi.toLiveEvent rebuilds the identical id string when reading the row
-- back. Do not widen the id's resolution without changing this column's type.
CREATE TABLE topology_events (
    id          BIGSERIAL   PRIMARY KEY,
    event_seq   BIGINT      NOT NULL,
    event_time  TIMESTAMPTZ NOT NULL,
    type        TEXT        NOT NULL,
    severity    TEXT        NOT NULL,
    scope       TEXT        NOT NULL,
    summary     TEXT        NOT NULL,
    details     JSONB       NOT NULL,
    ingested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT topology_events_natural_key UNIQUE (event_seq, event_time)
);

-- Keyset pagination is (event_time DESC, id DESC); every list query and the
-- retention sweep ride one of these three indexes.
CREATE INDEX topology_events_time_idx       ON topology_events (event_time DESC, id DESC);
CREATE INDEX topology_events_type_time_idx  ON topology_events (type, event_time DESC, id DESC);
CREATE INDEX topology_events_scope_time_idx ON topology_events (scope, event_time DESC, id DESC);

-- +goose Down
DROP TABLE topology_events;

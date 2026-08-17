-- +goose Up
-- +goose StatementBegin

-- The annotations retention sweep's own index — the one Delete*Before that never had one.
--
-- DeleteAnnotationsBefore filters and orders by coalesce(end_at, start_at): retention is by the END
-- of the span, because a note opened 100 days ago and still in effect is still current. The only
-- indexes on the table were the primary key and annotations_time_idx (start_at DESC, id DESC), and
-- neither can answer an expression over end_at — so every batch of 5 000 sequentially scanned the
-- whole table and top-N sorted everything older than the cutoff, inside the pruner's advisory lock,
-- once per batch.
--
-- This is the same correction 00011 made for maintenance_windows.end_at and incidents.resolved_at;
-- annotations was the one it missed, and an EXPRESSION index is what the coalesce needs.
CREATE INDEX IF NOT EXISTS annotations_end_idx
    ON annotations ((coalesce(end_at, start_at)));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS annotations_end_idx;
-- +goose StatementEnd

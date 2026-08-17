-- +goose Up
-- +goose StatementBegin

-- The retention sweep's own index.
--
-- DeletePathSnapshotsBefore is
--   DELETE FROM mtr_path_snapshots
--   WHERE id IN (SELECT s.id FROM mtr_path_snapshots s WHERE s.last_seen < $1 ORDER BY s.last_seen LIMIT $2)
-- and until now nothing on the table could answer it. The three indexes that existed lead with
-- source_node (the pair browse, twice) or run_id (the FK), and a btree cannot serve a predicate on
-- its third column without the first two -- so every batch was a sequential scan of the whole
-- snapshot table plus a sort of every row older than the cutoff, to pick out the batch limit's
-- worth. The sweep holds the pruner's advisory lock while it does that, and it repeats per batch:
-- on a table of any size the pruner spends its window scanning the same rows over and over and the
-- oldest snapshots outlive the retention window they were supposed to leave by.
--
-- (00014's comment claimed the pair-browse index still covered this. It never did; it covers
-- ListMTRDestinations, which does group by source_node and destination.)
CREATE INDEX IF NOT EXISTS mtr_path_snapshots_last_seen_idx
    ON mtr_path_snapshots (last_seen);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS mtr_path_snapshots_last_seen_idx;
-- +goose StatementEnd

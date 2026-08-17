-- +goose Up
-- +goose StatementBegin

-- Three indexes for three reads that had none, and one correction.
--
-- 00009's header says a new sample-per-pair listing needs "NO NEW INDEX" because the unique key
-- (run_id, source_node, destination_node, sample_seq) already leads with run_id. That was true of
-- the read it was written for and is no longer true of the table: ListPathTraces (the traces behind
-- one MTR route) filters on (source_node, destination_node) over a recorded_at window and orders by
-- recorded_at DESC. The unique index cannot serve any of that -- it leads with run_id, does not
-- contain recorded_at, and PostgreSQL has no b-tree skip scan -- so every page of that listing was a
-- sequential scan of the largest table in the database plus an external sort, repeated per page.
--
-- A migration is immutable, so 00009's comment stays where it is; this is the correction.
CREATE INDEX IF NOT EXISTS check_results_pair_time_idx
    ON check_results (source_node, destination_node, recorded_at DESC, id DESC);

-- 00006 says maintenance_time_idx is "the listing's keyset order AND the retention sweep's scan
-- order". Only the first half holds: the listing pages by start_at, while the sweep deletes by
-- END_at (DeleteMaintenanceWindowsBefore), which no index covered -- so every prune batch seq-scanned
-- and sorted the table. incidents has the same shape: its sweep reads resolved_at while both indexes
-- lead with status/scope + created_at.
--
-- Both tables are operator-sized, so this is small today. It is also exactly the kind of small that
-- stops being small quietly, and a comment claiming an index that is not used is worse than no
-- comment at all.
CREATE INDEX IF NOT EXISTS maintenance_end_idx ON maintenance_windows (end_at);
CREATE INDEX IF NOT EXISTS incidents_resolved_at_idx ON incidents (resolved_at) WHERE resolved_at IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS incidents_resolved_at_idx;
DROP INDEX IF EXISTS maintenance_end_idx;
DROP INDEX IF EXISTS check_results_pair_time_idx;
-- +goose StatementEnd

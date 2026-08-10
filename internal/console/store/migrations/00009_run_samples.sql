-- +goose Up
-- sample_seq turns check_results from "one row per pair per run" into "one row
-- per PROBE per pair per run", which is what an interval run needs: the owner
-- asked for a check that can be started for N minutes/hours because "right now
-- everything is fine, a second later the problem appears and I miss it", and a
-- single overwritten row answers exactly the question that framing rejects.
--
-- WHY A COLUMN ON check_results and not a new check_run_samples table: a
-- sample IS a check result. It has the same columns, the same NOT NULL result
-- payload, the same retention (ON DELETE CASCADE from check_runs), the same
-- readers (GetRunResults, the MTR path-snapshot projector, the run detail
-- page). A second table would duplicate every one of those and force every
-- reader to ask which of the two it wants -- for rows that differ only in how
-- many of them one pair produced.
--
-- DEFAULT 0 is what makes this invisible to instant runs. They write a single
-- sample per pair, at seq 0, and land byte-identical rows to the ones they
-- wrote before this migration; existing rows are backfilled to 0 by the
-- default. Only a run carrying a duration ever writes seq > 0.
--
-- The UNIQUE constraint is WIDENED rather than dropped: the upsert-on-retry
-- contract (a redispatched pair overwrites instead of erroring -- see
-- queries/checks.sql) has to survive, and it must keep applying WITHIN one
-- sample. Without sample_seq in the key an interval run's second probe would
-- silently overwrite its first; without the constraint at all, a retry would
-- append a duplicate and inflate the pair's own history.
--
-- NO NEW INDEX. Every read of this table is already "all rows for one run"
-- (GetRunResults filters on run_id and orders by id) and the widened unique
-- constraint's own index leads with run_id, so the ordering by id stays a
-- cheap sort over one run's rows. An index on sample_seq alone would be
-- selective for nothing.
ALTER TABLE check_results
    ADD COLUMN sample_seq INT NOT NULL DEFAULT 0;

ALTER TABLE check_results
    DROP CONSTRAINT check_results_pair_unique;

ALTER TABLE check_results
    ADD CONSTRAINT check_results_pair_unique
        UNIQUE (run_id, source_node, destination_node, sample_seq);

-- +goose Down
-- Down collapses each pair back to ONE row before narrowing the constraint,
-- keeping the LAST sample (highest seq) -- the closest thing to the single row
-- the pre-migration schema held. Narrowing the key without this delete would
-- fail outright on any interval run's data.
DELETE FROM check_results cr
WHERE cr.sample_seq < (
    SELECT max(inner_cr.sample_seq)
    FROM check_results inner_cr
    WHERE inner_cr.run_id = cr.run_id
      AND inner_cr.source_node = cr.source_node
      AND inner_cr.destination_node = cr.destination_node
);

ALTER TABLE check_results
    DROP CONSTRAINT check_results_pair_unique;

ALTER TABLE check_results
    ADD CONSTRAINT check_results_pair_unique
        UNIQUE (run_id, source_node, destination_node);

ALTER TABLE check_results
    DROP COLUMN sample_seq;

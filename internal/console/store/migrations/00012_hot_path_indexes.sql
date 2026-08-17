-- +goose Up
-- +goose StatementBegin

-- Four indexes for four reads that had none, and one more correction to 00009.
--
-- 00009 says its sample listing needs "NO NEW INDEX" because the unique key
-- (run_id, source_node, destination_node, sample_seq) leads with run_id. 00011 already corrected
-- that claim once for ListPathTraces; here is the other half. GetRunResults was later bounded with
-- `ORDER BY id DESC LIMIT $2`, and no index serves THAT: the unique key has no id, and the primary
-- key has no run_id. So opening the detail page of any run that is not the newest either walked the
-- primary key backwards, discarding every row written since that run, or bitmap-scanned the run and
-- sorted it -- and the page re-polls every five seconds.
--
-- A migration is immutable, so 00009's comment stays where it is; this is the correction.
CREATE INDEX IF NOT EXISTS check_results_run_id_idx
    ON check_results (run_id, id DESC);

-- The overrun guard (CountActiveRunsByInitiator) filters (initiator_kind, initiator_id, status) and
-- ran on the dispatch path of EVERY schedule fire -- which is the whole reason it was moved out of
-- per-replica memory and into the table. Nothing indexed any of those columns.
--
-- Partial, on the two statuses that are ever asked for: an active run is a transient state and the
-- table is mostly history, so the index stays small no matter how long retention is.
CREATE INDEX IF NOT EXISTS check_runs_active_idx
    ON check_runs (initiator_kind, initiator_id)
    WHERE status IN ('pending', 'running');

-- ListRuns' own default filter. The UI offers ?status= and the console's runs page uses it, and the
-- only index on check_runs was the primary key, so every filtered page was a sequential scan plus a
-- sort of the whole run history.
CREATE INDEX IF NOT EXISTS check_runs_status_created_idx
    ON check_runs (status, created_at DESC, id DESC);

-- mtr_path_snapshots.run_id is `REFERENCES check_runs(id) ON DELETE SET NULL` with no index behind
-- it, so deleting ONE run made PostgreSQL run
--   UPDATE ONLY mtr_path_snapshots SET run_id = NULL WHERE $1 = run_id
-- as a sequential scan. A retention batch is thousands of runs, i.e. thousands of full scans of the
-- snapshot table, all inside the sweep that holds the pruner's advisory lock.
CREATE INDEX IF NOT EXISTS mtr_path_snapshots_run_idx
    ON mtr_path_snapshots (run_id);

-- The same omission on check_schedules.definition_id, whose FK cascades on delete.
CREATE INDEX IF NOT EXISTS check_schedules_definition_idx
    ON check_schedules (definition_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS check_schedules_definition_idx;
DROP INDEX IF EXISTS mtr_path_snapshots_run_idx;
DROP INDEX IF EXISTS check_runs_status_created_idx;
DROP INDEX IF EXISTS check_runs_active_idx;
DROP INDEX IF EXISTS check_results_run_id_idx;
-- +goose StatementEnd

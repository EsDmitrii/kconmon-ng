-- +goose Up
-- +goose StatementBegin

-- The run history's OTHER filter.
--
-- ListRuns pages by (created_at DESC, id DESC) and narrows on two optional columns: status, which
-- 00012 indexed as (status, created_at DESC, id DESC), and check_type, which nothing indexed. So
-- GET /api/v1/runs?type=mtr walked check_runs_created_idx in time order and discarded every row of
-- another type until it had a page — and the run history is one of the tables that only grows, with
-- the rarer check types (mtr, http) the ones an operator filters for most, because they are the ones
-- worth finding. The LIMIT bounds the response, not the work.
--
-- Same shape as its status twin, so the filter and the paging come out of one index with no sort.
CREATE INDEX IF NOT EXISTS check_runs_type_created_idx
    ON check_runs (check_type, created_at DESC, id DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS check_runs_type_created_idx;
-- +goose StatementEnd

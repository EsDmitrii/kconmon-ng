-- +goose Up
-- +goose StatementBegin

-- The pair-browse index, following the listing onto the key it now pages by.
--
-- 00005 indexed (source_node, destination, last_seen DESC, id DESC) because ListPathSnapshots
-- ordered by last_seen. That ordering was wrong for a keyset cursor: last_seen is bumped by every
-- repeat trace, so a route re-traced between two pages jumped over the cursor and vanished from the
-- history the console shows. The listing now orders and pages on first_seen, which never changes
-- after the insert — and an index that no longer matches the scan order is a sequential scan plus a
-- sort of every route the pair has ever taken.
--
-- The old index is KEPT: last_seen is still the destinations pane's own order, and
-- ListMTRDestinations groups by (source_node, destination) aggregating max(last_seen), which is
-- exactly what that index leads with. It does NOT cover the retention sweep -- that one filters on
-- last_seen with no pair at all, and a btree cannot answer a predicate on its third column; 00015
-- adds the index it needs.
CREATE INDEX IF NOT EXISTS mtr_snapshots_pair_first_seen_idx
    ON mtr_path_snapshots (source_node, destination, first_seen DESC, id DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS mtr_snapshots_pair_first_seen_idx;
-- +goose StatementEnd

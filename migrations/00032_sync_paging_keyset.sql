-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Index the three streams a sync pull merges for keyset paging.
--
-- A pull orders each stream by (its own timestamp, a tiebreaking id) and asks
-- for the next page with a keyset predicate on the same pair - `(updated_at,
-- id) > ($cursor_at, $cursor_id)` - because a single `since` timestamp cannot
-- resume mid-burst: a bulk annotation import can write hundreds of rows in one
-- transaction sharing the exact same updated_at, and a timestamp-only cursor
-- has no way to ask for "the rest of this same instant".
--
-- None of the three tables had an index with the right shape for that:
--
--   * annotations had (user_id, created_at DESC) - the wrong column, sync
--     reads updated_at, and DESC when the scan is ascending.
--   * reading_progress had (user_id, last_read_at DESC) - a different
--     timestamp again; sync reads updated_at, which is when this server
--     learned the position, not when the reader was last there.
--   * annotation_tombstones had (user_id, deleted_at DESC), closer, but
--     without annotation_id it cannot serve the keyset comparison - only the
--     leading column.
--
-- Without these, every pull after the first page falls back to a sequential
-- scan of one user's whole history of marks or positions filtered on the
-- primary index alone, every single page.
--
-- reading_progress's (user_id, last_read_at DESC) index stays: it is what the
-- reading-history views elsewhere want, a different question from sync's, and
-- nothing here replaces it.
--
-- annotation_tombstones_since_idx (migration 26) does not stay. Sync's own
-- query - `ORDER BY deleted_at, annotation_id` - was the only reader that ever
-- ordered by deleted_at, and it now wants the three-column index below
-- instead; keeping both would be the exact duplicate migration 31 already
-- removed once on a different table, paid on every deletion instead of every
-- highlight.

-- +goose Up

CREATE INDEX annotations_sync_idx ON annotations (user_id, updated_at, id);

CREATE INDEX reading_progress_sync_idx ON reading_progress (user_id, updated_at, book_file_id);

DROP INDEX annotation_tombstones_since_idx;
CREATE INDEX annotation_tombstones_sync_idx ON annotation_tombstones (user_id, deleted_at, annotation_id);

-- +goose Down

DROP INDEX annotation_tombstones_sync_idx;
CREATE INDEX annotation_tombstones_since_idx ON annotation_tombstones (user_id, deleted_at DESC);

DROP INDEX reading_progress_sync_idx;
DROP INDEX annotations_sync_idx;

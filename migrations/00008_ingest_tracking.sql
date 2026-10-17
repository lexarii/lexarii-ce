-- +goose Up

-- When a scan finishes, any book_file it did not touch has been deleted, moved
-- outside the library, or had its drive unmounted. Without this column the only
-- options are keeping ghosts forever or deleting rows that a temporarily
-- unmounted drive would resurrect - and deleting takes the user's annotations
-- and reading positions with it.
--
-- Files are marked missing, never deleted. Removal stays an explicit user action.
ALTER TABLE book_files
    ADD COLUMN last_seen_at timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN missing_since timestamptz,
    -- Recoverable problems found while parsing: skipped archive entries, an
    -- unreadable TOC, a missing cover. Distinct from ingest_error, which means
    -- the file could not be ingested at all.
    ADD COLUMN ingest_warnings text[] NOT NULL DEFAULT '{}';

CREATE INDEX book_files_missing_idx ON book_files (missing_since)
    WHERE missing_since IS NOT NULL;

CREATE INDEX book_files_last_seen_idx ON book_files (last_seen_at);

-- +goose Down
DROP INDEX book_files_last_seen_idx;
DROP INDEX book_files_missing_idx;
ALTER TABLE book_files
    DROP COLUMN ingest_warnings,
    DROP COLUMN missing_since,
    DROP COLUMN last_seen_at;

-- SPDX-License-Identifier: AGPL-3.0-or-later

-- A cover belongs to a FILE, not to an edition.
--
-- `editions.cover_path` has existed since migration 3 and nothing has ever
-- written to it: all 168 editions hold NULL, which is why every book in the
-- library renders as the same typographic placeholder. The column was not
-- wrong, it was unreachable - extracting a cover needs the file, and an edition
-- can have several.
--
-- A PDF's cover is its first page, and rendering that needs a rasteriser. Pure
-- Go has none, and the ones that exist need cgo, which is what CGO_ENABLED=0
-- rules out and why the model link is HTTP rather than bindings. So the page is
-- rendered where a rasteriser already exists and is already being used to read
-- the book: pdf.js, in the browser. The result is uploaded once and stored here.
--
-- The path is relative to the DATA directory. Never the library directory - a
-- derived artifact must not be written next to somebody's books, and the
-- library directory is read-only.

-- +goose Up

ALTER TABLE book_files ADD COLUMN cover_path text;

-- Which files still need one. Partial, because the interesting set is the
-- shrinking remainder rather than the whole table.
CREATE INDEX book_files_without_cover_idx ON book_files (id)
    WHERE cover_path IS NULL AND missing_since IS NULL;

-- +goose Down

DROP INDEX book_files_without_cover_idx;
ALTER TABLE book_files DROP COLUMN cover_path;

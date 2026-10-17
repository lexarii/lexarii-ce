-- +goose Up

-- Uploaded files are stored content-addressed: <data>/books/ab/abcdef….epub.
-- That eliminates path traversal outright rather than defending against it, and
-- makes storage naturally deduplicated.
--
-- But it destroys the filename, and stem_key - which is what lets a PDF and an
-- EPUB of the same book merge into one shelf item - was derived from the path.
-- Without the original name, every uploaded book would stop merging.
ALTER TABLE book_files
    ADD COLUMN original_filename text NOT NULL DEFAULT '';

-- Regenerate stem_key from the original filename when there is one, falling
-- back to the path for files discovered by scanning a library directory.
-- A generated column cannot be altered in place, so it is dropped and rebuilt.
DROP INDEX book_files_stem_key_idx;
ALTER TABLE book_files DROP COLUMN stem_key;

ALTER TABLE book_files
    ADD COLUMN stem_key text
    GENERATED ALWAYS AS (
        regexp_replace(
            lower(regexp_replace(
                regexp_replace(
                    CASE WHEN original_filename <> '' THEN original_filename ELSE path END,
                    '^.*/', ''),
                '\.[^.]*$', '')),
            '[^a-z0-9]', '', 'g')
    ) STORED;

CREATE INDEX book_files_stem_key_idx ON book_files (stem_key) WHERE length(stem_key) >= 12;

-- Where the file came from. Uploaded files live in Lexarii's own managed
-- directory and may be deleted by it; scanned files live in the user's
-- read-only library directory and must never be touched.
CREATE TYPE file_origin AS ENUM ('scanned', 'uploaded');

ALTER TABLE book_files
    ADD COLUMN origin file_origin NOT NULL DEFAULT 'scanned';

CREATE INDEX book_files_origin_idx ON book_files (origin);

-- +goose Down
DROP INDEX book_files_origin_idx;
ALTER TABLE book_files DROP COLUMN origin;
DROP TYPE file_origin;

DROP INDEX book_files_stem_key_idx;
ALTER TABLE book_files DROP COLUMN stem_key;
ALTER TABLE book_files
    ADD COLUMN stem_key text
    GENERATED ALWAYS AS (
        regexp_replace(
            lower(regexp_replace(regexp_replace(path, '^.*/', ''), '\.[^.]*$', '')),
            '[^a-z0-9]', '', 'g')
    ) STORED;
CREATE INDEX book_files_stem_key_idx ON book_files (stem_key) WHERE length(stem_key) >= 12;

ALTER TABLE book_files DROP COLUMN original_filename;

-- +goose Up

-- Two files of the same book should be one shelf item. ISBN is the only
-- metadata signal trustworthy enough to merge on, and PDFs essentially never
-- carry one: across a real 167-book PDF library, zero had an ISBN. So a user
-- who owns a book as both PDF and EPUB sees it twice.
--
-- The filename is a much better signal than it sounds. Measured on that same
-- library plus its EPUB and MOBI counterparts - 171 files - there were 167
-- distinct stems, and every single collision was a genuine same-book set:
--
--   97thingseveryjavaprogrammershouldknow.{pdf,epub,mobi}
--   algebraessentials.{pdf,epub,mobi}
--
-- stem_key is the basename with its extension and all punctuation stripped, so
-- those match exactly. Merging is still restricted to files of DIFFERENT
-- formats with a sufficiently long stem, because two files with the same name
-- and the same format are two different things, and short stems like "notes"
-- or "book" collide by accident.
ALTER TABLE book_files
    ADD COLUMN stem_key text
    GENERATED ALWAYS AS (
        regexp_replace(
            lower(regexp_replace(regexp_replace(path, '^.*/', ''), '\.[^.]*$', '')),
            '[^a-z0-9]', '', 'g')
    ) STORED;

CREATE INDEX book_files_stem_key_idx ON book_files (stem_key) WHERE length(stem_key) >= 12;

-- +goose Down
DROP INDEX book_files_stem_key_idx;
ALTER TABLE book_files DROP COLUMN stem_key;

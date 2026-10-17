-- +goose Up

-- Books with no embedded title are unfindable by title search, and in a real
-- library that is a large minority: 67 of 167 in the corpus this was measured
-- against. Their filenames usually do carry the words, but run together and
-- without separators - "appliedlinearalgebraandoptimizationusingmatlab.pdf" -
-- so a search for "linear algebra" matches nothing.
--
-- Normalising both sides to bare lowercase alphanumerics makes them match:
-- the query "linear algebra" becomes "linearalgebra", which is a substring of
-- the normalised filename. This is a fallback, not a replacement for real
-- metadata: it finds the book without pretending to know its title.
ALTER TABLE book_files
    ADD COLUMN search_key text
    GENERATED ALWAYS AS (regexp_replace(lower(path), '[^a-z0-9]', '', 'g')) STORED;

-- Trigram GIN, because the useful query is an infix LIKE '%...%' that a btree
-- index cannot serve.
CREATE INDEX book_files_search_key_trgm_idx ON book_files USING gin (search_key gin_trgm_ops);

-- +goose Down
DROP INDEX book_files_search_key_trgm_idx;
ALTER TABLE book_files DROP COLUMN search_key;

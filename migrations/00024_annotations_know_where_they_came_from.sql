-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Say whether a mark was made here or imported from the book file.
--
-- A PDF can carry its own highlights, made in whatever reader somebody used
-- before this one, and importing them is worth doing: 1,051 underlines sit in
-- one book of a real library, invisible to everything.
--
-- They have to be told apart from marks made here, for two reasons. Importing
-- twice must not double them, and the only safe way to re-import is to replace
-- what the last import produced - which requires knowing which those were. And
-- a reader deserves to know which marks are their own doing: an imported
-- underline arrives with no note and no context, and showing it as though they
-- had written it here would be a small lie.
--
-- Defaulting to 'reader' is right for every row that exists, because the
-- reader is the only thing that has ever made one.

-- +goose Up

CREATE TYPE annotation_origin AS ENUM ('reader', 'pdf');

ALTER TABLE annotations
    ADD COLUMN origin annotation_origin NOT NULL DEFAULT 'reader';

-- Re-importing deletes by book, reader and origin, so that is what this serves.
CREATE INDEX annotations_origin_idx ON annotations (book_file_id, user_id, origin)
    WHERE origin <> 'reader';

-- +goose Down

DROP INDEX annotations_origin_idx;
ALTER TABLE annotations DROP COLUMN origin;
DROP TYPE annotation_origin;

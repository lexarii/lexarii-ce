-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Make a reader's own notes findable.
--
-- Annotations have been a write-only store: they paint in the reader and they
-- list on a page, and the assistant that answers questions from this library
-- has never been able to see them. A person who wrote "this is the argument I
-- disagree with" next to a paragraph cannot ask their own library about it,
-- which is most of the reason to write it down.
--
-- Both halves are indexed, and which half matters depends on the question. The
-- note is the reader's own words; the quoted passage is the book's, and it is
-- what somebody half-remembers when they go looking. Weighted A and B so a hit
-- in the note itself ranks above a hit in the text it was attached to.
--
-- Generated rather than maintained: a trigger would be one more thing to get
-- wrong on a table two readers already write to.

-- +goose Up

ALTER TABLE annotations
    ADD COLUMN tsv tsvector GENERATED ALWAYS AS (
        setweight(to_tsvector('english', COALESCE(note, '')), 'A') ||
        setweight(to_tsvector('english', COALESCE(selected_text, '')), 'B')
    ) STORED;

CREATE INDEX annotations_tsv_idx ON annotations USING GIN (tsv);

-- Searching a person's own notes always filters by who they are, so the index
-- that serves it should start there.
CREATE INDEX annotations_user_created_idx ON annotations (user_id, created_at DESC);

-- +goose Down

DROP INDEX annotations_user_created_idx;
DROP INDEX annotations_tsv_idx;
ALTER TABLE annotations DROP COLUMN tsv;

-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Let a citation survive the passage being re-chunked.
--
-- citations.chunk_id cascades, and SaveChunks begins by deleting every chunk
-- of a book. So re-chunking - which BookFilesNeedingChunks queues for every
-- file in the library the moment chunk_strategy_version changes, and which is
-- therefore a routine consequence of improving retrieval - deleted every
-- citation that pointed into that book. The assistant's answers stayed;
-- the evidence under them vanished, and the read path inner-joins chunks, so
-- the answers came back with nothing at all beneath them.
--
-- DOMAIN.md: "an assistant claim without one is a defect." An upgrade that
-- silently turns months of answered questions into unsupported assertions is
-- the worst version of that.
--
-- What a citation needs in order to still mean something is not the chunk row.
-- It is the book, the place in the book, and enough text to recognise: all of
-- which are facts about the passage rather than about how it happened to be
-- divided up that week. Those are copied onto the citation now. chunk_id stays
-- for as long as the chunk does, because it is still the cheapest way back to
-- the full text, but it is nullable and no longer takes the citation with it.

-- +goose Up

ALTER TABLE citations
    ADD COLUMN book_file_id uuid REFERENCES book_files (id) ON DELETE SET NULL,
    ADD COLUMN start_major  integer,
    ADD COLUMN start_path   text,
    ADD COLUMN start_offset integer,
    ADD COLUMN chapter_title text,
    ADD COLUMN excerpt      text;

-- Everything already recorded, taken from the chunks it still points at.
UPDATE citations ct
   SET book_file_id  = c.book_file_id,
       start_major   = c.start_major,
       start_path    = c.start_path,
       start_offset  = c.start_offset,
       chapter_title = c.chapter_title,
       excerpt       = left(c.content, 400)
  FROM chunks c
 WHERE c.id = ct.chunk_id;

-- The chunk may go; the citation stays.
ALTER TABLE citations
    DROP CONSTRAINT citations_chunk_id_fkey,
    ALTER COLUMN chunk_id DROP NOT NULL,
    ADD CONSTRAINT citations_chunk_id_fkey
        FOREIGN KEY (chunk_id) REFERENCES chunks (id) ON DELETE SET NULL;

CREATE INDEX citations_book_file_idx ON citations (book_file_id);

-- +goose Down

DROP INDEX citations_book_file_idx;

-- A citation with no chunk cannot be represented by the old shape, and the
-- restored cascade would delete it on the next re-chunk anyway.
DELETE FROM citations WHERE chunk_id IS NULL;

ALTER TABLE citations
    DROP CONSTRAINT citations_chunk_id_fkey,
    ALTER COLUMN chunk_id SET NOT NULL,
    ADD CONSTRAINT citations_chunk_id_fkey
        FOREIGN KEY (chunk_id) REFERENCES chunks (id) ON DELETE CASCADE,
    DROP COLUMN book_file_id,
    DROP COLUMN start_major,
    DROP COLUMN start_path,
    DROP COLUMN start_offset,
    DROP COLUMN chapter_title,
    DROP COLUMN excerpt;

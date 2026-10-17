-- SPDX-License-Identifier: AGPL-3.0-or-later

-- The book an answer's search was narrowed to, when the question named one.
--
-- A follow-up such as "who are the villains in the book?" is now answered
-- from the book it means rather than from the whole library. That is a
-- decision about where to look, and like the rest of how a turn was produced
-- it is kept so a surprising answer can be explained: "it only read this
-- book" is the first thing anybody would want to know.
--
-- A reference, read back through readable_works like a reading list, and
-- cleared rather than cascaded when the work goes: the answer stays.

-- +goose Up

ALTER TABLE messages
    ADD COLUMN scoped_work_id uuid REFERENCES works (id) ON DELETE SET NULL,
    ADD CONSTRAINT messages_scope_only_on_assistant
        CHECK (scoped_work_id IS NULL OR role = 'assistant');

CREATE INDEX messages_scoped_work_idx ON messages (scoped_work_id)
    WHERE scoped_work_id IS NOT NULL;

-- +goose Down

ALTER TABLE messages DROP COLUMN scoped_work_id;

-- SPDX-License-Identifier: AGPL-3.0-or-later

-- A reading list is an answer, and until now it was not kept.
--
-- A find-book or broad-topic turn answers with books rather than prose, and
-- only the prose was stored. So the thread forgot what it had listed: the
-- follow-up "who are the villains in the book?" was routed against a history
-- that said "You answered:" and nothing after it, and the search that
-- followed had no idea which book was meant. Reopening such a thread showed
-- an empty answer for the same reason.
--
-- A reference to the work rather than a copy of its title. A copy would keep
-- naming a book after the person lost access to it; a reference is read back
-- through readable_works, so the list shrinks with what they may still open.

-- +goose Up

CREATE TABLE message_works (
    message_id uuid NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
    work_id    uuid NOT NULL REFERENCES works (id) ON DELETE CASCADE,
    -- The order the list was shown in.
    position   int  NOT NULL,

    PRIMARY KEY (message_id, work_id),
    CONSTRAINT message_works_position_non_negative CHECK (position >= 0)
);

-- Deleting or merging a work cascades here.
CREATE INDEX message_works_work_idx ON message_works (work_id);

-- +goose Down

DROP TABLE message_works;

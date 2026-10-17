-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Remember that a mark was deleted, so a phone that was offline can find out.
--
-- A client that has been out of signal for a week asks what changed. Every
-- creation and every edit is answerable from the annotations table, because
-- the row is there with a newer updated_at. A DELETION is not: the row is
-- gone, and "gone" is indistinguishable from "never existed" and from "exists
-- but you have not been told about it yet". Without this, a highlight deleted
-- on a laptop comes back the moment the phone next syncs, because the phone
-- still has it and the server has nothing to say otherwise.
--
-- A separate table rather than a deleted_at column on annotations. The live
-- table has a dozen queries against it - the reader's list, the notes page,
-- retrieval, sharing, the searches - and a soft delete would make every one of
-- them wrong unless it grew a predicate. The one that got forgotten would be
-- the one that showed somebody a mark they had deleted, which is the worst
-- failure available here. A tombstone table cannot be forgotten by a query
-- that does not mention it.
--
-- The book is recorded as well as the mark, so a client can discard a deletion
-- for a book it does not have without asking about it.

-- +goose Up

CREATE TABLE annotation_tombstones (
    annotation_id uuid PRIMARY KEY,
    user_id       uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    book_file_id  uuid NOT NULL,
    deleted_at    timestamptz NOT NULL DEFAULT now()
);

-- What a sync asks for: this person's deletions since a moment.
CREATE INDEX annotation_tombstones_since_idx
    ON annotation_tombstones (user_id, deleted_at DESC);

-- No foreign key to annotations: the row this points at is gone, which is the
-- entire point. The book file has none either, because a book removed from the
-- library must not take the record of the deletion with it - a client still
-- has to be told not to bring the mark back.

-- +goose Down

DROP TABLE annotation_tombstones;

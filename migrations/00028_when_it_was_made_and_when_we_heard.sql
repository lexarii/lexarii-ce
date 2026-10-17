-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Separate when a mark was MADE from when this server heard about it.
--
-- Migration 27 made the trigger honour a timestamp the statement set, so that
-- an edit made on a phone last Monday is stored as Monday. That fixed
-- last-write-wins, which must compare authorship: an edit made on Wednesday
-- should beat one made on Monday however late the Monday one arrives.
--
-- And it broke the pull cursor, which is the same column read for a different
-- question. A client asks "what changed since my last sync". A mark pushed on
-- Friday but stamped Monday is already older than every other device's cursor,
-- so it is delivered to none of them - ever, short of a full resync. The
-- comment in migration 27 listed that failure as one it was fixing. It was
-- describing the behaviour it introduced.
--
-- One column cannot answer both questions, so there are two. `updated_at` is
-- when the row changed HERE, assigned by this server, monotonic in the order
-- this server learned things, and it is what a cursor reads. `edited_at` is
-- when the person made the change, supplied by whatever device they made it
-- on, and it is what last-write-wins compares.
--
-- Existing rows get edited_at = updated_at, which is exactly right for every
-- mark made in the reader: the two moments were the same.

-- +goose Up

ALTER TABLE annotations
    ADD COLUMN edited_at timestamptz NOT NULL DEFAULT now();

UPDATE annotations SET edited_at = updated_at;

-- The trigger goes back to stamping now() unconditionally, because that is
-- what updated_at now means again, on every table it fires for.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose Down

ALTER TABLE annotations DROP COLUMN edited_at;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    IF NEW.updated_at IS NOT DISTINCT FROM OLD.updated_at THEN
        NEW.updated_at = now();
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

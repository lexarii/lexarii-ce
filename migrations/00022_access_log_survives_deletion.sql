-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Let the audit log outlive the account it is about.
--
-- Migration 19 declared access_log.user_id as ON DELETE SET NULL, and said why:
-- "deleting an account must not erase the record of what it read. That is
-- exactly the moment the record matters." It could not work. user_id is part of
-- the primary key, and a primary key column is NOT NULL - so the cascade could
-- never set it, and deleting an account that had read anything failed outright:
--
--   null value in column "user_id" of relation "access_log"
--     violates not-null constraint
--
-- Nothing noticed because nothing deleted accounts. The account deletion this
-- migration accompanies is what found it.
--
-- The fix is a surrogate key. The old tuple stays as a UNIQUE index, so the
-- aggregation still works exactly as before - one row per person, book, day and
-- kind, and RecordAccess still resolves ON CONFLICT against it. What changes is
-- that user_id may now be NULL, which is what "a deleted account" looks like:
-- the reading is still on the owner's record, and no longer points at a person.
--
-- NULLs being distinct in a unique index is harmless here. Rows only become
-- NULL after the account is gone, and at that point there is nobody left to
-- aggregate further reading for.

-- +goose Up

ALTER TABLE access_log DROP CONSTRAINT access_log_pkey;

-- Dropping the primary key does NOT drop the NOT NULL that came with it.
-- Postgres keeps the constraint behind, so without this line the column is
-- still not nullable and the cascade still fails - which is exactly what
-- happened on the first attempt at this migration.
ALTER TABLE access_log ALTER COLUMN user_id DROP NOT NULL;

ALTER TABLE access_log
    ADD COLUMN id uuid PRIMARY KEY DEFAULT gen_random_uuid();

CREATE UNIQUE INDEX access_log_entry_key
    ON access_log (book_file_id, user_id, day, kind);

-- +goose Down

DROP INDEX access_log_entry_key;

ALTER TABLE access_log DROP COLUMN id;

-- Rows whose account has since been deleted cannot go back into a primary key
-- that forbids NULL. They are the records this migration existed to keep, and
-- rolling back is not a reason to lose them - but the old shape has nowhere to
-- put them, so they are the price of going back.
DELETE FROM access_log WHERE user_id IS NULL;

ALTER TABLE access_log ALTER COLUMN user_id SET NOT NULL;

ALTER TABLE access_log
    ADD CONSTRAINT access_log_pkey PRIMARY KEY (book_file_id, user_id, day, kind);

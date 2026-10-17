-- SPDX-License-Identifier: AGPL-3.0-or-later

-- What happens to somebody's books when their account goes.
--
-- Deleting an account already did the right thing for a BORROWER: book_access
-- cascades, so their access ends and the book is simply the owner's again. The
-- owner's own side was the gap - book_files.owner_id became NULL and the books
-- belonged to nobody, visible to nobody, recoverable only with SQL.
--
-- The policy these columns implement:
--
--   The account goes, and everything personal goes with it - annotations,
--   reading position, conversations, collections all cascade. That is what
--   deletion has to mean.
--
--   The BOOKS are not personal data, they are files. They are archived rather
--   than destroyed, and remembered against the name that owned them, because
--   somebody may come back. An administrator can hand them to a new owner.
--
--   After a retention period - six months by default - the archived ROW may be
--   deleted. The file is kept.
--
-- The file itself is only moved when Lexarii owns the storage. A book scanned
-- from the library directory stays exactly where it is: that directory is the
-- operator's and is read-only to us, which is the first invariant in this
-- project. Archiving such a file means recording that it is archived, not
-- touching it. Only uploads - which live in Lexarii's own directory, are
-- content-addressed, and have no other home - are moved into the archive.

-- +goose Up

ALTER TABLE book_files
    -- When the owning account was deleted. NULL for every ordinary book.
    ADD COLUMN archived_at timestamptz,
    -- Who owned it, by name, captured at deletion.
    --
    -- Text and not a foreign key, deliberately: the row it would point at is
    -- the one being deleted. This is a label for a person deciding what to do
    -- with the book, not a reference - and after the account is gone the name
    -- is the only thing left that connects the book to whoever added it.
    ADD COLUMN archived_owner text,
    -- Where the file went, when Lexarii moved it. NULL when the file stayed
    -- where it was, which is every book scanned from the library directory.
    ADD COLUMN archived_path text;

-- Archived books are a small minority and are always queried as a set - "what
-- is waiting to be claimed", "what has been waiting long enough to expire" - so
-- a partial index is both smaller and the one that gets used.
CREATE INDEX book_files_archived_idx ON book_files (archived_at)
    WHERE archived_at IS NOT NULL;

-- +goose Down

DROP INDEX book_files_archived_idx;
ALTER TABLE book_files
    DROP COLUMN archived_at,
    DROP COLUMN archived_owner,
    DROP COLUMN archived_path;

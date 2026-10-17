-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Books become per-person: a file is visible to whoever added it and to the
-- people they grant it to, and to nobody else.
--
-- This retires the shared-library premise the schema has carried since
-- migration 3. Until now `works`, `book_files`, `tags` and `chunks` had no
-- owner, and everything with an account could read everything - which was a
-- deliberate design for one household sharing one shelf. It no longer is.
--
-- Ownership lives on `book_files` rather than `works` because a file is the
-- thing a person actually added. Two people can own different files of the same
-- work - one the EPUB, one the PDF - and the work is visible to each of them
-- through their own copy. A work is visible when at least one of its files is.
--
-- `granted` carries `can_download` because reading and taking away are
-- different permissions, and the second is the one with consequences. Note what
-- that permission can and cannot do: withholding it means the original file is
-- never sent, so a reader receives sanitised chapter HTML rather than an EPUB.
-- It does NOT stop a determined person keeping what they can see. Nothing does.
-- Screenshots and OCR exist. What this buys is the difference between one click
-- and several hundred, plus a record of who was given what.

-- +goose Up

ALTER TABLE book_files
    -- ON DELETE SET NULL rather than CASCADE: deleting an account must never
    -- delete books. An ownerless file is visible to nobody until an
    -- administrator claims it, which is the safe direction to fail.
    ADD COLUMN owner_id uuid REFERENCES users (id) ON DELETE SET NULL;

-- Everything already here was added by the person running the installation.
-- The earliest administrator is the best available answer, and on a
-- single-account install it is the only one.
-- +goose StatementBegin
DO $$
DECLARE
    first_admin uuid;
BEGIN
    SELECT id INTO first_admin FROM users
     WHERE is_admin ORDER BY created_at LIMIT 1;

    IF first_admin IS NULL THEN
        SELECT id INTO first_admin FROM users ORDER BY created_at LIMIT 1;
    END IF;

    IF first_admin IS NOT NULL THEN
        UPDATE book_files SET owner_id = first_admin WHERE owner_id IS NULL;
        RAISE NOTICE 'assigned % existing files to the first account',
            (SELECT count(*) FROM book_files WHERE owner_id = first_admin);
    END IF;
END;
$$;
-- +goose StatementEnd

CREATE INDEX book_files_owner_idx ON book_files (owner_id);

-- Who may read someone else's book.
CREATE TABLE book_access (
    book_file_id uuid NOT NULL REFERENCES book_files (id) ON DELETE CASCADE,
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- Recorded so a grant can be explained later: "why can they see this?"
    granted_by uuid REFERENCES users (id) ON DELETE SET NULL,
    -- Reading and taking a copy are different permissions. Default false: a
    -- grant that silently allowed downloading would be the wrong way round.
    can_download boolean     NOT NULL DEFAULT false,
    created_at   timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (book_file_id, user_id)
);

CREATE INDEX book_access_user_idx ON book_access (user_id);

-- Who may read someone else's annotations on a book.
--
-- Separate from book_access because they are separate decisions: giving someone
-- a book does not hand them your marginalia, and the common case is sharing a
-- book without it.
CREATE TABLE annotation_access (
    book_file_id uuid NOT NULL REFERENCES book_files (id) ON DELETE CASCADE,
    -- Whose annotations are being shared.
    owner_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- Who may see them.
    viewer_id  uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (book_file_id, owner_id, viewer_id),

    -- Sharing your notes with yourself is not a thing.
    CONSTRAINT annotation_access_not_self CHECK (owner_id <> viewer_id)
);

CREATE INDEX annotation_access_viewer_idx ON annotation_access (viewer_id, book_file_id);

-- Every access decision goes through this, so there is exactly one definition
-- of "may read" and no handler can quietly disagree with another.
--
-- A view rather than a function because it composes into a join: the library
-- listing, the reader, search and retrieval all need "the files this person may
-- see" as a set, not as a per-row test.
CREATE VIEW readable_book_files AS
    SELECT bf.id AS book_file_id, bf.owner_id AS user_id, true AS can_download
      FROM book_files bf
     WHERE bf.owner_id IS NOT NULL
    UNION
    SELECT ba.book_file_id, ba.user_id, ba.can_download
      FROM book_access ba;

-- +goose Down

DROP VIEW readable_book_files;
DROP TABLE annotation_access;
DROP TABLE book_access;
DROP INDEX book_files_owner_idx;
ALTER TABLE book_files DROP COLUMN owner_id;

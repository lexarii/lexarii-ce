-- SPDX-License-Identifier: AGPL-3.0-or-later

-- A file can belong to more than one person.
--
-- Until now a book_file had exactly one owner_id and content_hash was unique
-- install-wide. That holds while one person adds books. The moment two people
-- on one installation add the same EPUB - which in a family is the ordinary
-- case, not the edge case - it breaks in three ways at once:
--
--   - The second person's upload was answered "duplicate" and their ownership
--     was never recorded, so they could not see the book they had just added.
--     And the answer itself told them somebody else here has that exact file.
--   - If the first person's extraction was still running, the second upload was
--     folded into the first person's job and simply dropped.
--   - Whichever ingest ran last overwrote original_filename, so one person's
--     upload renamed the other's book.
--
-- The file stays one row - its bytes, text, chunks, embeddings and enrichment
-- are about the CONTENT and are never duplicated - and who holds it moves to a
-- table of its own. Every holder has full owner rights to the file.
--
-- owner_id is retired in this same migration rather than kept as a "primary
-- holder". Two columns answering one question is exactly the defect migration
-- 29 had to repair, and keeping it in step would take a trigger racing the
-- users foreign key's own SET NULL. Every query that read it is rewritten in
-- the same change, and the Down below restores it, from the earliest holder,
-- without loss for an installation that never had a second holder.

-- +goose Up

CREATE TABLE book_file_holders (
    book_file_id uuid NOT NULL REFERENCES book_files (id) ON DELETE CASCADE,
    -- CASCADE: deleting an account ends that person's holding. Whether the
    -- FILE then needs archiving is decided before the delete, by whoever
    -- deletes the account, because only it knows whether anybody else holds it.
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- The name this person gave the file. Per holder, so one person's upload
    -- never renames another's book. book_files.original_filename stays as the
    -- name the file first arrived with, which background work uses to merge
    -- formats by stem and which no person is shown.
    original_filename text        NOT NULL DEFAULT '',
    created_at        timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (book_file_id, user_id)
);

-- "What does this person hold" - the shelf, storage per person, account deletion.
CREATE INDEX book_file_holders_user_idx ON book_file_holders (user_id);

-- A file nobody owns and that is not archived was readable by nobody but the
-- people it had been lent to, and manageable by nobody at all. From here on a
-- file nobody holds is ALWAYS archived, so an administrator can see it and hand
-- it to somebody; these are archived now, under a marker rather than a name,
-- since there is no name to give. Their grants go with every other grant
-- nobody could answer for, below: they were made by somebody who is gone.
UPDATE book_files
   SET archived_at = now(), archived_owner = 'nobody (no owner was recorded)'
 WHERE owner_id IS NULL AND archived_at IS NULL;

-- Everyone who owned a file holds it. An archived file has no owner by
-- construction - that is what archiving meant - and so no holder.
INSERT INTO book_file_holders (book_file_id, user_id, original_filename, created_at)
SELECT id, owner_id, original_filename, created_at
  FROM book_files
 WHERE owner_id IS NOT NULL AND archived_at IS NULL;

-- An upload whose file is not on the shelf yet.
--
-- Extraction is a job, and a holding needs a book_file row to point at, which
-- does not exist until the job has run. A second person uploading the same
-- bytes while the first person's job is still queued has nothing to attach
-- to. They leave a claim keyed on the content hash instead, and the ingest
-- that creates or refreshes the file turns every claim into a holding in the
-- same transaction. A short-lived table: rows live from upload to extraction.
CREATE TABLE holding_claims (
    content_hash      text        NOT NULL,
    user_id           uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    original_filename text        NOT NULL DEFAULT '',
    created_at        timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (content_hash, user_id),
    CONSTRAINT holding_claims_hash_shape CHECK (content_hash ~ '^[0-9a-f]{64}$')
);

-- Grants belong to the holder who made them.
--
-- Two people holding one file each lend it as they see fit. Neither sees or
-- revokes the other's grants: that would tell one of them who the other lends
-- books to. So a grant is keyed on who made it as well as who received it, and
-- the same person can hold a grant from each holder at once.
--
-- A grant with nobody to answer for it is attributed to whoever owns the file
-- now. That is the person who could see and revoke it until today, so nothing
-- changes for them. The only ones left after that are on archived files, whose
-- owner is gone: they were dormant, and they are dropped rather than revived
-- for whoever claims the file next, who never made them and could not see them.
UPDATE book_access ba
   SET granted_by = bf.owner_id
  FROM book_files bf
 WHERE bf.id = ba.book_file_id
   AND bf.owner_id IS NOT NULL
   AND bf.archived_at IS NULL
   AND ba.granted_by IS DISTINCT FROM bf.owner_id;

DELETE FROM book_access ba
 WHERE NOT EXISTS (SELECT 1 FROM book_file_holders h
                    WHERE h.book_file_id = ba.book_file_id
                      AND h.user_id = ba.granted_by);

ALTER TABLE book_access DROP CONSTRAINT book_access_pkey;
ALTER TABLE book_access DROP CONSTRAINT book_access_granted_by_fkey;
ALTER TABLE book_access ALTER COLUMN granted_by SET NOT NULL;
ALTER TABLE book_access ADD PRIMARY KEY (book_file_id, user_id, granted_by);
-- A grant is made BY A HOLDING and ends with it. Whether the holder deletes
-- their account or later lets the file go, the books they lent stop being lent
-- by them, without any caller having to remember to do it. It also makes "only
-- a holder may grant" a fact of the schema rather than of one INSERT.
ALTER TABLE book_access
    ADD CONSTRAINT book_access_grantor_holds
    FOREIGN KEY (book_file_id, granted_by)
    REFERENCES book_file_holders (book_file_id, user_id) ON DELETE CASCADE;

-- Who lent the book that was read, recorded at the time it was read.
--
-- A holder sees who read the file through THEIR grants, and nothing about the
-- other holder's lending. Taken from the live grants it would vanish the moment
-- a grant was revoked - which is exactly when the record is wanted - so it is
-- kept on the row. Existing rows were all lent by the owner of the day.
ALTER TABLE access_log ADD COLUMN lent_by uuid[] NOT NULL DEFAULT '{}';

UPDATE access_log al
   SET lent_by = ARRAY[bf.owner_id]
  FROM book_files bf
 WHERE bf.id = al.book_file_id AND bf.owner_id IS NOT NULL;

-- The single definition of "may read", with the owner branch now the holders.
--
-- Shaped as the union of the two kinds of reader, joined to book_files, rather
-- than migration 29's lateral over every file. A filter on user_id - "this
-- person's shelf" - now reaches the indexes on holders and grants directly
-- instead of probing them once per file in the installation; a filter on
-- book_file_id reaches their primary keys. Still one row per (file, reader),
-- can_download true when any route allows it, archived files excluded.
CREATE OR REPLACE VIEW readable_book_files AS
    SELECT reader.book_file_id,
           reader.user_id,
           bool_or(reader.can_download) AS can_download
      FROM (
               SELECT h.book_file_id, h.user_id, true AS can_download
                 FROM book_file_holders h
               UNION ALL
               SELECT ba.book_file_id, ba.user_id, ba.can_download
                 FROM book_access ba
           ) AS reader
      JOIN book_files bf ON bf.id = reader.book_file_id
     -- An archived book belongs to nobody until somebody claims it, and while
     -- it belongs to nobody it is readable by nobody.
     WHERE bf.archived_at IS NULL
     GROUP BY reader.book_file_id, reader.user_id;

-- The single definition of who may change a work's metadata or merge it:
-- somebody who holds EVERY file of it.
--
-- A work is shared: its title is what everybody holding any of its files sees,
-- and merging moves every file of it. "Holds one of its files" was the rule
-- while one person owned every copy, and stops being safe the moment two
-- people can hold the same bytes: somebody lent one EPUB could upload it,
-- hold it, and retitle - or merge away - a book whose other files, including
-- ones never lent to anybody, are somebody else's. Holding all of them means
-- the work is entirely yours, or entirely shared with people holding exactly
-- the same files; then any of you may change it, and it changes for all of you.
CREATE VIEW work_editors AS
    SELECT e.work_id, h.user_id
      FROM book_file_holders h
      JOIN book_files bf ON bf.id = h.book_file_id
      JOIN editions e ON e.id = bf.edition_id
     WHERE bf.archived_at IS NULL
     GROUP BY e.work_id, h.user_id
    HAVING count(*) = (SELECT count(*)
                         FROM book_files bf2
                         JOIN editions e2 ON e2.id = bf2.edition_id
                        WHERE e2.work_id = e.work_id
                          AND bf2.archived_at IS NULL);

DROP INDEX book_files_owner_idx;
ALTER TABLE book_files DROP COLUMN owner_id;

-- +goose Down

ALTER TABLE book_files
    ADD COLUMN owner_id uuid REFERENCES users (id) ON DELETE SET NULL;

-- Files archived on the way up because nobody owned them stay archived: their
-- grants were dropped then, and an administrator can still claim them.

-- The earliest holder was the owner, and is again.
UPDATE book_files bf
   SET owner_id = (SELECT h.user_id FROM book_file_holders h
                    WHERE h.book_file_id = bf.id
                    ORDER BY h.created_at, h.user_id
                    LIMIT 1);

CREATE INDEX book_files_owner_idx ON book_files (owner_id);

DROP VIEW work_editors;

CREATE OR REPLACE VIEW readable_book_files AS
    SELECT bf.id AS book_file_id,
           reader.user_id,
           bool_or(reader.can_download) AS can_download
      FROM book_files bf
      JOIN LATERAL (
               SELECT bf.owner_id AS user_id, true AS can_download
                WHERE bf.owner_id IS NOT NULL
               UNION ALL
               SELECT ba.user_id, ba.can_download
                 FROM book_access ba
                WHERE ba.book_file_id = bf.id
           ) AS reader ON true
     WHERE bf.archived_at IS NULL
     GROUP BY bf.id, reader.user_id;

ALTER TABLE access_log DROP COLUMN lent_by;

-- Back to one grant per (file, reader). A second holder had full rights to the
-- file; after this they keep reading and downloading it through a grant from
-- the owner, rather than losing a book they added.
ALTER TABLE book_access DROP CONSTRAINT book_access_grantor_holds;

INSERT INTO book_access (book_file_id, user_id, granted_by, can_download, created_at)
SELECT h.book_file_id, h.user_id, bf.owner_id, true, h.created_at
  FROM book_file_holders h
  JOIN book_files bf ON bf.id = h.book_file_id
 WHERE h.user_id <> bf.owner_id
ON CONFLICT (book_file_id, user_id, granted_by) DO UPDATE SET can_download = true;

UPDATE book_access ba
   SET can_download = true
 WHERE NOT ba.can_download
   AND EXISTS (SELECT 1 FROM book_access other
                WHERE other.book_file_id = ba.book_file_id
                  AND other.user_id = ba.user_id
                  AND other.can_download);

DELETE FROM book_access ba
 USING book_access keep
 WHERE keep.book_file_id = ba.book_file_id
   AND keep.user_id = ba.user_id
   AND (keep.created_at, keep.granted_by) < (ba.created_at, ba.granted_by);

ALTER TABLE book_access DROP CONSTRAINT book_access_pkey;
ALTER TABLE book_access ALTER COLUMN granted_by DROP NOT NULL;
ALTER TABLE book_access ADD PRIMARY KEY (book_file_id, user_id);
ALTER TABLE book_access
    ADD CONSTRAINT book_access_granted_by_fkey
    FOREIGN KEY (granted_by) REFERENCES users (id) ON DELETE SET NULL;

DROP TABLE holding_claims;
DROP TABLE book_file_holders;

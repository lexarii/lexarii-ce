-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Rebuild readable_book_files as a single scan.
--
-- Two defects, one cause: it was a UNION of "files you own" and "files granted
-- to you", and the second branch never looked at book_files at all.
--
-- FIRST, a grant outlived the account that made it. Deleting an owner archives
-- their books and NULLs owner_id, and DOMAIN.md says an archived book "is
-- visible to nobody meanwhile, because readable_book_files requires an owner" -
-- which only the first branch did. So somebody the book had been lent to kept
-- reading and downloading it after the lender's account was gone, and nobody
-- could revoke it: UnshareBook needs an owner to act as.
--
-- SECOND, the two branches disagree about can_download, and UNION only folds
-- rows that match completely. An owner who also held a grant appeared TWICE,
-- once with true and once with false. Roughly fifteen queries join this view as
-- though it were unique, so every one of them doubled: a reader saw every
-- highlight painted twice, and every count was wrong. ShareBook refuses to
-- grant to an owner, but ClaimArchived hands a book to somebody without
-- clearing the grant they already had - which is exactly the person from the
-- first defect.
--
-- One row per (file, reader), with can_download true when either route allows
-- it. A LEFT JOIN rather than a UNION, so an archived file is excluded once,
-- for everybody, in one place.

-- +goose Up

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
     -- An archived book belongs to nobody until somebody claims it, and while
     -- it belongs to nobody it is readable by nobody.
     WHERE bf.archived_at IS NULL
     GROUP BY bf.id, reader.user_id;

-- +goose Down

CREATE OR REPLACE VIEW readable_book_files AS
    SELECT bf.id AS book_file_id, bf.owner_id AS user_id, true AS can_download
      FROM book_files bf
     WHERE bf.owner_id IS NOT NULL
    UNION
    SELECT ba.book_file_id, ba.user_id, ba.can_download
      FROM book_access ba;

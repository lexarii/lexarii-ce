-- SPDX-License-Identifier: AGPL-3.0-or-later

-- An ISBN no longer has to be unique across the installation.
--
-- editions_isbn13_key made an ISBN name exactly one edition. With books
-- belonging to people, and an upload only joining an edition its uploader
-- already holds a file of, two people who each add the same book from
-- different files each have an edition of their own - legitimately carrying
-- the same ISBN. The unique index refused the second, and the refusal was an
-- answer to a question nobody should be able to ask: give a book an ISBN, by
-- uploading it or by editing it, and whether that failed said whether anybody
-- on the installation has that book.
--
-- The lookup index stays, without the uniqueness.

-- +goose Up

DROP INDEX editions_isbn13_key;
CREATE INDEX editions_isbn13_idx ON editions (isbn13) WHERE isbn13 IS NOT NULL;

-- +goose Down

-- Back to one edition per ISBN. Later editions carrying an ISBN an earlier one
-- already has lose it: the edition and its files stay, only the duplicate
-- identifier goes.
UPDATE editions e
   SET isbn13 = NULL
 WHERE e.isbn13 IS NOT NULL
   AND EXISTS (SELECT 1 FROM editions o
                WHERE o.isbn13 = e.isbn13
                  AND (o.created_at, o.id) < (e.created_at, e.id));

DROP INDEX editions_isbn13_idx;
CREATE UNIQUE INDEX editions_isbn13_key ON editions (isbn13) WHERE isbn13 IS NOT NULL;

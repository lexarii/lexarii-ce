-- SPDX-License-Identifier: AGPL-3.0-or-later

-- The work-level companion to readable_book_files.
--
-- Access is decided per FILE, because a file is the thing a person added and
-- the thing that gets granted. Most questions are asked about WORKS: how many
-- books do I have, which subjects do I own, who is my most collected author.
-- Every one of those needs "the works at least one of whose files this person
-- may read", and writing that join out by hand at each site is how one of them
-- eventually gets it wrong.
--
-- Separate migration rather than an edit to 00017 because that one has already
-- run: a migration that has been applied somewhere is history, and rewriting it
-- means two installations disagree about what version 17 was.

-- +goose Up

CREATE VIEW readable_works AS
    SELECT DISTINCT rbf.user_id, e.work_id
      FROM readable_book_files rbf
      JOIN book_files bf ON bf.id = rbf.book_file_id
      JOIN editions e ON e.id = bf.edition_id;

-- +goose Down

DROP VIEW readable_works;

-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Who read someone else's book, and how much of it.
--
-- `book_access` records who was GRANTED what. That is the permission, not the
-- fact. For a feature justified by legal caution the more useful half is the
-- fact: whether the person who was given a book read three pages of it or
-- pulled all 558 in four minutes.
--
-- Two decisions shape this table, and both are deliberate:
--
-- ONLY SOMEBODY ELSE'S BOOKS ARE LOGGED. Reading your own library is not
-- recorded at all - the owner check is in the statement, not in a handler, so
-- it cannot be forgotten. An audit trail over a person's own reading is not an
-- audit trail, it is surveillance, and it would bury the entries that matter
-- under everything else they read that week.
--
-- IT AGGREGATES. A row per page view would mean 558 rows for one reading of one
-- book. Instead there is one row per person, book, day and kind, carrying which
-- distinct units were seen. `cardinality(units)` against the book's page count
-- is the question anyone actually asks: how much of it did they take?

-- +goose Up

CREATE TABLE access_log (
    book_file_id uuid NOT NULL REFERENCES book_files (id) ON DELETE CASCADE,
    -- SET NULL rather than CASCADE: deleting an account must not erase the
    -- record of what it read. That is exactly the moment the record matters.
    user_id uuid REFERENCES users (id) ON DELETE SET NULL,
    day     date NOT NULL DEFAULT current_date,
    -- What was served: a PDF page, an EPUB chapter, or the whole file.
    kind text NOT NULL,

    -- views counts requests; units lists the distinct pages or chapters seen.
    -- They differ when someone re-reads, and the gap is itself informative.
    views int   NOT NULL DEFAULT 0,
    units int[] NOT NULL DEFAULT '{}',

    first_at timestamptz NOT NULL DEFAULT now(),
    last_at  timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (book_file_id, user_id, day, kind),

    CONSTRAINT access_log_kind CHECK (kind IN ('page', 'chapter', 'file'))
);

-- "What has this person been reading" and "who has read this book" are both
-- asked; the primary key serves the second, this serves the first.
CREATE INDEX access_log_user_idx ON access_log (user_id, last_at DESC);

-- +goose Down

DROP TABLE access_log;

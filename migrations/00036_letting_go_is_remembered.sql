-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Somebody who took a book off their shelf is not handed it back by a scan.
--
-- A scan makes the administrator who ran it a holder of what it finds - their
-- library directory is theirs, even where somebody else uploaded the same file
-- first. That made "remove from my shelf" undone by the next scan. The first
-- answer, giving scans only files nobody held, broke the other half: an
-- administrator's own files stopped reaching their shelf whenever somebody
-- else had uploaded them first, and the difference said who had.
--
-- So a release is remembered, per person and per file, and only a deliberate
-- act - uploading the file again, or an administrator handing it to them -
-- forgets it. A scan, or an extraction that was already queued when they let
-- go, respects it.

-- +goose Up

CREATE TABLE released_holdings (
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- The bytes, not the row: the row can be archived, expire and come back
    -- as a new row for the same file, and letting go of it should still hold.
    content_hash text        NOT NULL,
    released_at  timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (user_id, content_hash),
    CONSTRAINT released_holdings_hash_shape CHECK (content_hash ~ '^[0-9a-f]{64}$')
);

-- +goose Down

DROP TABLE released_holdings;

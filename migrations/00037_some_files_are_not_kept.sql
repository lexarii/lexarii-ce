-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Some files must not be kept on an installation at all.
--
-- An administrator of a library several people add to may need to take one
-- file off it for everybody - something this library must not keep - and keep it
-- from coming straight back with the next upload or scan. Taking it off
-- every shelf is not enough on its own: the same bytes are one upload, one
-- rescan of the library directory, or one queued extraction away from
-- returning. So the bytes are remembered, by content hash, and every way a
-- file arrives checks this table under the file's lock.
--
-- Keyed on the bytes rather than the book_files row for the same reason as
-- released_holdings: the row can be archived, expire and come back as a new
-- row for the same file, and the block must still hold.

-- +goose Up

CREATE TABLE blocked_content (
    content_hash text        PRIMARY KEY,
    -- Why, in the administrator's words. Shown to administrators only; it is
    -- the record of a decision somebody may later need to revisit.
    reason       text        NOT NULL,
    -- SET NULL: the block outlives the administrator who made it. It is the
    -- installation's decision, and lifting it is any administrator's.
    blocked_by   uuid        REFERENCES users (id) ON DELETE SET NULL,
    blocked_at   timestamptz NOT NULL DEFAULT now(),
    -- When every copy Lexarii itself stored of these bytes (uploads, the
    -- archive, the cover) was last confirmed deleted. NULL while some may
    -- remain: deleting them happens after the removal commits and can fail,
    -- or never run if the server stops first, so the blocklist offers to
    -- finish and startup tries again. A library-directory file is never
    -- deleted and does not count.
    copies_discarded_at timestamptz,

    CONSTRAINT blocked_content_hash_shape CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT blocked_content_reason_not_blank CHECK (length(trim(reason)) > 0)
);

-- +goose Down

DROP TABLE blocked_content;

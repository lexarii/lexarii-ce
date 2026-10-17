-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Stored copies an erasure owes the disk.
--
-- Erasing a book (LEXARII_DELETED_ACCOUNT_FILES=erase, or an administrator
-- erasing somebody's books) deletes its record in one transaction and the
-- copies Lexarii stored afterwards. Deleting files inside the transaction
-- would make them the one thing a rollback cannot undo; deleting them after
-- it, with nothing written down, would lose them to a crash or a failed
-- disk. So the erasure records what it owes here, in the same transaction,
-- and a sweep pays it: right after the erasure, at startup, and periodically,
-- each under the bytes' lock and only while nothing has come to want them.
-- An archive record that expires in erase mode is owed the same way.
--
-- paths are where the erased records said their bytes were; the sweep also
-- finds every copy Lexarii stored under the hash itself. covers are stored
-- cover names. Neither is ever a file the sweep may delete unless it lies in
-- Lexarii's own upload directory or archive (or, for covers, its cover
-- directory): the library directory is read-only.
--
-- The table starts empty, and nothing writes to it unless an installation
-- erases.

-- +goose Up

CREATE TABLE pending_discards (
    content_hash text        PRIMARY KEY,
    paths        text[]      NOT NULL DEFAULT '{}',
    covers       text[]      NOT NULL DEFAULT '{}',
    queued_at    timestamptz NOT NULL DEFAULT now(),
    attempts     integer     NOT NULL DEFAULT 0,
    last_error   text,
    CONSTRAINT pending_discards_hash_shape CHECK (content_hash ~ '^[0-9a-f]{64}$')
);

-- +goose Down

DROP TABLE pending_discards;

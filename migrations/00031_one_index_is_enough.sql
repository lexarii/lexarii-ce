-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Drop a duplicate index on annotations.
--
-- Migration 23 created annotations_user_recent_idx on (user_id, created_at
-- DESC), which migration 5 had already created as annotations_user_created_idx
-- with a byte-identical definition. Two identical indexes cost two writes on
-- every highlight and two rebuilds on every vacuum, and answer the same
-- question.
--
-- The older name is dropped, because the newer one is what migration 23's
-- queries were written against and is the one somebody reading that file will
-- look for.

-- +goose Up

DROP INDEX IF EXISTS annotations_user_created_idx;

-- +goose Down

CREATE INDEX IF NOT EXISTS annotations_user_created_idx
    ON annotations (user_id, created_at DESC);

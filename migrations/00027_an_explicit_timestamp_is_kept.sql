-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Let a writer say when a row was changed.
--
-- set_updated_at stamps now() on every UPDATE, unconditionally, on eleven
-- tables. That is right for the ordinary case and wrong for exactly one: a
-- change that happened somewhere else, at a time the server did not witness.
--
-- Offline sync is that case. A phone edits a highlight on Monday and pushes it
-- on Friday; the push carries Monday, the row is written with Monday, and the
-- trigger replaces it with Friday. Everything downstream then reasons about
-- the wrong moment:
--
--   * Last-write-wins compares arrival instead of authorship. A laptop edit
--     made on Wednesday loses to the phone's Monday edit, because the phone's
--     landed stamped Friday - and the laptop is told its own newer edit was
--     stale.
--   * A pull asks for everything after a moment. A mark written with Monday's
--     timestamp is already older than every other device's cursor, so it is
--     never sent to any of them and stays invisible until a full resync.
--
-- The rule now: if the statement set updated_at itself, that is the answer. If
-- it did not, the trigger stamps now() exactly as before. Every existing
-- caller falls in the second case, so nothing changes for them.

-- +goose Up

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    -- IS NOT DISTINCT FROM, not =, because either side may be NULL.
    IF NEW.updated_at IS NOT DISTINCT FROM OLD.updated_at THEN
        NEW.updated_at = now();
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

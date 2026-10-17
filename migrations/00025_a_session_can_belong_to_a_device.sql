-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Name the thing a session was issued to.
--
-- Until now every session was a browser's, carried in a cookie, and the
-- user-agent string was enough to tell them apart. A native client is
-- different in two ways that matter: it holds the token itself rather than
-- letting a cookie jar hold it, and it is expected to keep it for months
-- while the phone it lives on is lost, replaced, or sold.
--
-- So the person has to be able to look at the list and know which line is
-- which phone, and revoke exactly that one. A user-agent of
-- "Lexarii-Android/0.1 (okhttp/4.12)" is the same string on every device
-- they own; the name they chose is not.
--
-- NULL means a browser session, which is every session that exists today.
-- Presence of a device is also what says a token was handed to a client
-- rather than set as a cookie, which the session list shows plainly: somebody
-- reading it deserves to know which of their sessions is a bearer token.

-- +goose Up

ALTER TABLE sessions ADD COLUMN device text;

-- +goose Down

ALTER TABLE sessions DROP COLUMN device;

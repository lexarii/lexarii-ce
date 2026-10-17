-- SPDX-License-Identifier: AGPL-3.0-or-later

-- A job is visible to whoever it was queued for, and to administrators.
--
-- GET /api/v1/jobs/{id} and its event stream answered anybody signed in who
-- had the id, and a job's progress note and last error carry file paths and
-- parser messages about somebody's book. Ids are not secrets: until now the
-- upload endpoint itself handed one person the id of another person's job.
--
-- NULL for work nobody in particular asked for - enrichment, indexing, the
-- embedding pass - which only administrators start, and only administrators see.

-- +goose Up

ALTER TABLE jobs
    -- SET NULL: a job outlives the account that asked for it. It is still
    -- the installation's work, and administrators can still see it.
    ADD COLUMN requested_by uuid REFERENCES users (id) ON DELETE SET NULL;

-- Uploads and scans already said who they were for, in the payload.
-- Compared as text, so a payload holding anything other than a uuid simply
-- matches nobody instead of failing the migration on a cast.
UPDATE jobs j
   SET requested_by = u.id
  FROM users u
 WHERE u.id::text = j.payload ->> 'ownerId';

-- +goose Down

ALTER TABLE jobs DROP COLUMN requested_by;

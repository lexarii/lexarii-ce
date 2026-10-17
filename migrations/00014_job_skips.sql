-- +goose Up

-- Skips need counting, separately from attempts.
--
-- A skipped job is rescheduled WITHOUT consuming an attempt, so that a model
-- server switched off overnight does not permanently fail a whole library.
-- That is right, and it was also unbounded: a book the model consistently fails
-- to answer - it times out on every single request - skipped, waited thirty
-- seconds, and ran again, forever, holding a worker each time. Two such books
-- occupied two of four workers indefinitely and nothing in the queue said so,
-- because a skip is not a failure.
--
-- Counting them separately keeps both properties: an outage still costs no
-- attempts, and a job that will never succeed still stops eventually.
ALTER TABLE jobs
    ADD COLUMN skips int NOT NULL DEFAULT 0;

COMMENT ON COLUMN jobs.skips IS
    'Times this job was rescheduled without consuming an attempt, because the '
    'model server was unavailable. Used to back off and, eventually, to give up.';

-- +goose Down

ALTER TABLE jobs DROP COLUMN skips;

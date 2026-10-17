-- +goose Up

CREATE TYPE job_type AS ENUM ('scan', 'extract', 'enrich', 'chunk', 'embed');

CREATE TYPE job_status AS ENUM ('pending', 'running', 'succeeded', 'failed', 'cancelled');

-- Asynchronous work. Every job is idempotent and resumable: a 5000-book library
-- is hours of GPU time, and a restart must not start over.
CREATE TABLE jobs (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    type          job_type    NOT NULL,
    status        job_status  NOT NULL DEFAULT 'pending',
    -- Target is loose on purpose: a scan targets a directory, an embed targets
    -- a book_file. Typed FKs would need one nullable column per job type.
    target_id     uuid,
    payload       jsonb       NOT NULL DEFAULT '{}',
    priority      int         NOT NULL DEFAULT 0,
    progress      real        NOT NULL DEFAULT 0,
    progress_note text,
    attempts      int         NOT NULL DEFAULT 0,
    max_attempts  int         NOT NULL DEFAULT 3,
    last_error    text,
    -- Set when a worker claims the row; lets a supervisor reclaim jobs abandoned
    -- by a worker that died mid-run.
    locked_by     text,
    locked_at     timestamptz,
    run_after     timestamptz NOT NULL DEFAULT now(), -- backoff between retries
    started_at    timestamptz,
    finished_at   timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT jobs_progress_range CHECK (progress >= 0 AND progress <= 1),
    CONSTRAINT jobs_attempts_within_max CHECK (attempts <= max_attempts),
    CONSTRAINT jobs_lock_is_paired CHECK ((locked_by IS NULL) = (locked_at IS NULL)),
    CONSTRAINT jobs_finished_has_terminal_status
        CHECK (finished_at IS NULL OR status IN ('succeeded', 'failed', 'cancelled'))
);

-- The claim query: WHERE status = 'pending' AND run_after <= now()
-- ORDER BY priority DESC, created_at FOR UPDATE SKIP LOCKED.
CREATE INDEX jobs_claim_idx ON jobs (priority DESC, created_at)
    WHERE status = 'pending';

CREATE INDEX jobs_run_after_idx ON jobs (run_after) WHERE status = 'pending';
CREATE INDEX jobs_target_idx ON jobs (target_id) WHERE target_id IS NOT NULL;
CREATE INDEX jobs_status_idx ON jobs (status);

-- One live job per (type, target). Enqueueing a second embed for a book that is
-- already queued must be a no-op, enforced here rather than in a racy read-then-write.
CREATE UNIQUE INDEX jobs_one_active_per_target_idx ON jobs (type, target_id)
    WHERE status IN ('pending', 'running') AND target_id IS NOT NULL;

CREATE TRIGGER jobs_set_updated_at
    BEFORE UPDATE ON jobs
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose Down
DROP TABLE jobs;
DROP TYPE job_status;
DROP TYPE job_type;

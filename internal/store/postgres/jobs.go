// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
)

// Job is a unit of asynchronous work.
type Job struct {
	ID           uuid.UUID
	Type         domain.JobType
	Status       domain.JobStatus
	TargetID     *uuid.UUID
	Payload      json.RawMessage
	Priority     int
	Progress     float32
	ProgressNote string
	Attempts     int
	MaxAttempts  int
	// Skips counts reschedules that did not consume an attempt, because the
	// model server was unavailable.
	Skips     int
	LastError string
	RunAfter  time.Time
	CreatedAt time.Time
	// RequestedBy is the person the job was queued for, or zero for the
	// installation's own work. A wrapper around the handlers can decide by
	// it - deferring one person's work, say (see worker.Defer).
	RequestedBy uuid.UUID
}

// deref0 reads a nullable id, zero when NULL.
func deref0(id *uuid.UUID) uuid.UUID {
	if id == nil {
		return uuid.Nil
	}
	return *id
}

// EnqueueOptions configures a job at creation.
type EnqueueOptions struct {
	Type     domain.JobType
	TargetID *uuid.UUID
	Payload  any
	// Priority overrides the type's place in the queue
	// (domain.JobType.Priority) when non-zero. Nothing in Lexarii sets it;
	// it is for tests and for an installation with a reason of its own.
	Priority int
	// MaxAttempts defaults to 3 when zero.
	MaxAttempts int
	// RunAfter delays the job. Zero means run as soon as a worker is free.
	RunAfter time.Time
	// RequestedBy is the person this job is for, who may follow it alongside
	// administrators. Zero for the installation's own work, which only
	// administrators see. When the enqueue finds a job already live for the
	// target, that job keeps its own requester: a job id is only handed to
	// somebody who may read it if targets are chosen so that never happens
	// across people.
	RequestedBy uuid.UUID
}

// scrubFinishedJobs takes a person out of their finished jobs: the name the
// uploaded file had and whom it was for (payload originalFilename and
// ownerId), and who asked for it (requested_by). Finished jobs are never run
// again and nothing reads those fields from them, so what is left is the
// installation's record that the work was done. Jobs still pending or
// running need theirs, and are left alone.
//
// Finished rows are written by nothing else but this, account deletion's
// cascade (requested_by set null) and the retention sweep
// (DeleteFinishedJobs). This and the sweep both lock many of them, so both
// take them in id order, FOR UPDATE, before writing: two sets of rows locked
// in one order cannot wait on each other in a circle, which an UPDATE and a
// DELETE each locking in whatever order its scan met the rows could. The
// cascade then finds this person's finished rows already held by the
// transaction it runs in. Nothing holding these rows waits on anything
// else, so the scrub can run at any point of a transaction.
func scrubFinishedJobs(ctx context.Context, tx pgx.Tx, user uuid.UUID) error {
	if _, err := tx.Exec(ctx, `
		UPDATE jobs
		   SET payload = payload - 'originalFilename' - 'ownerId',
		       requested_by = NULL
		 WHERE id IN (
		    SELECT id FROM jobs
		     WHERE finished_at IS NOT NULL
		       AND (requested_by = $1::uuid OR payload ->> 'ownerId' = $1::uuid::text)
		     ORDER BY id
		       FOR UPDATE)`, user); err != nil {
		return fmt.Errorf("scrubbing finished jobs: %w", mapError(err))
	}
	return nil
}

// deleteFinishedBatch bounds how many finished jobs one statement of
// DeleteFinishedJobs deletes, so a first sweep over a long history never
// holds a large set of row locks at once. Smaller in a test.
var deleteFinishedBatch = 5000

// DeleteFinishedJobs deletes the jobs that finished before the given time,
// and reports how many. Pending and running jobs are never touched, and
// neither is one that finished since: the retention sweep
// (LEXARII_JOB_RETENTION). Each batch locks its rows in id order first, as
// scrubFinishedJobs does, so the two never deadlock.
func (p *Pool) DeleteFinishedJobs(ctx context.Context, before time.Time) (int64, error) {
	var total int64
	for {
		tag, err := p.Exec(ctx, `
			DELETE FROM jobs WHERE id IN (
			    SELECT id FROM jobs
			     WHERE status IN ('succeeded', 'failed', 'cancelled')
			       AND finished_at < $1
			     ORDER BY id
			     LIMIT $2
			       FOR UPDATE)`, before, deleteFinishedBatch)
		if err != nil {
			return total, fmt.Errorf("deleting finished jobs: %w", mapError(err))
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < int64(deleteFinishedBatch) {
			return total, nil
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}

// Enqueue adds a job, or returns the existing one when work for the same target
// is already queued or running.
//
// Deduplication is enforced by a partial unique index rather than by a
// read-then-write in Go, which would race between two workers enqueueing at the
// same moment. Enqueueing an embed for a book already queued is a no-op.
func (p *Pool) Enqueue(ctx context.Context, opts EnqueueOptions) (uuid.UUID, bool, error) {
	payload, maxAttempts, runAfter, err := jobValues(opts)
	if err != nil {
		return uuid.Nil, false, err
	}

	var id uuid.UUID
	err = p.QueryRow(ctx, `
		INSERT INTO jobs (type, target_id, payload, priority, max_attempts, run_after, requested_by)
		-- A requester whose account has gone is nobody in particular: the
		-- work still needs doing - a scan an administrator started before
		-- leaving, say - and only administrators see it now.
		VALUES ($1, $2, $3, $4, $5, $6, (SELECT id FROM users WHERE id = $7))
		ON CONFLICT (type, target_id)
			WHERE status IN ('pending', 'running') AND target_id IS NOT NULL
			DO NOTHING
		RETURNING id`,
		opts.Type, opts.TargetID, payload, priorityOf(opts), maxAttempts, runAfter,
		nullableUUID(opts.RequestedBy),
	).Scan(&id)

	if err == nil {
		return id, true, nil
	}
	if !isNotFound(mapError(err)) {
		return uuid.Nil, false, fmt.Errorf("enqueuing %s: %w", opts.Type, mapError(err))
	}

	// DO NOTHING returned no row: a live job for this target already exists.
	if opts.TargetID == nil {
		return uuid.Nil, false, fmt.Errorf("enqueuing %s: insert returned no row", opts.Type)
	}
	err = p.QueryRow(ctx, `
		SELECT id FROM jobs
		WHERE type = $1 AND target_id = $2 AND status IN ('pending', 'running')`,
		opts.Type, opts.TargetID,
	).Scan(&id)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("finding existing %s job: %w", opts.Type, mapError(err))
	}
	return id, false, nil
}

// jobValues applies EnqueueOptions' defaults, for every statement that adds a
// job.
// priorityOf is a job's place in the queue: its type's, unless overridden.
func priorityOf(opts EnqueueOptions) int {
	if opts.Priority != 0 {
		return opts.Priority
	}
	return opts.Type.Priority()
}

func jobValues(opts EnqueueOptions) (json.RawMessage, int, time.Time, error) {
	payload := json.RawMessage("{}")
	if opts.Payload != nil {
		encoded, err := json.Marshal(opts.Payload)
		if err != nil {
			return nil, 0, time.Time{}, fmt.Errorf("encoding payload: %w", err)
		}
		payload = encoded
	}

	maxAttempts := opts.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	runAfter := opts.RunAfter
	if runAfter.IsZero() {
		runAfter = time.Now()
	}
	return payload, maxAttempts, runAfter, nil
}

// Claim takes the next runnable job for a worker, or returns ErrNotFound when
// there is nothing to do.
//
// FOR UPDATE SKIP LOCKED is what lets several workers drain the queue
// concurrently without coordinating: each transaction locks the row it takes and
// every other worker steps straight over it instead of blocking.
func (p *Pool) Claim(ctx context.Context, workerID string) (Job, error) {
	var j Job
	var requester *uuid.UUID
	err := p.QueryRow(ctx, `
		UPDATE jobs SET
			status = 'running',
			attempts = attempts + 1,
			locked_by = $1,
			locked_at = now(),
			started_at = COALESCE(started_at, now())
		WHERE id = (
			SELECT id FROM jobs
			WHERE status = 'pending' AND run_after <= now()
			  -- A job with no attempts left must never be selected. Claiming
			  -- increments attempts, so picking one at its limit violates
			  -- jobs_attempts_within_max, the claim fails, and the worker tries
			  -- again immediately - forever. One such row wedges every worker
			  -- in a hot loop against the database. Seen in practice: 518
			  -- failed claims in a few minutes.
			  AND attempts < max_attempts
			ORDER BY priority DESC, created_at
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING id, type, status, target_id, payload, priority, progress,
		          COALESCE(progress_note, ''), attempts, max_attempts, skips,
		          COALESCE(last_error, ''), run_after, created_at, requested_by`,
		workerID,
	).Scan(&j.ID, &j.Type, &j.Status, &j.TargetID, &j.Payload, &j.Priority,
		&j.Progress, &j.ProgressNote, &j.Attempts, &j.MaxAttempts, &j.Skips,
		&j.LastError, &j.RunAfter, &j.CreatedAt, &requester)
	j.RequestedBy = deref0(requester)
	if err != nil {
		return Job{}, mapError(err)
	}
	return j, nil
}

// Progress updates a running job's progress. Fraction is clamped to [0,1].
// Only by the worker holding it: see Succeed.
func (p *Pool) Progress(ctx context.Context, id uuid.UUID, worker string, fraction float32, note string) error {
	if fraction < 0 {
		fraction = 0
	}
	if fraction > 1 {
		fraction = 1
	}
	_, err := p.Exec(ctx, `
		UPDATE jobs SET progress = $2, progress_note = NULLIF($3, ''), locked_at = now()
		WHERE id = $1 AND status = 'running' AND locked_by = $4`, id, fraction, note, worker)
	if err != nil {
		return fmt.Errorf("updating progress: %w", mapError(err))
	}
	return nil
}

// Succeed marks a job finished.
//
// Only a job that is still RUNNING, and only by the worker that holds it -
// which is not fussiness. Nothing interrupts a handler that is already
// executing, so two things can be true when it returns: a person cancelled
// it, or it stopped reporting progress long enough for ReclaimStale to hand
// it to another worker. Guarding on status alone caught the first and only
// part of the second: a job reclaimed and claimed again is running again, by
// somebody else, and a stale worker finishing late would have written over
// that worker's run - cleared its lock, so a third could claim the same
// work, or put it back to pending while it ran. Every change a worker makes
// to a job it claimed (Progress, Succeed, Fail, RescheduleSkipped, Defer) is
// guarded on locked_by for that reason.
func (p *Pool) Succeed(ctx context.Context, id uuid.UUID, worker string) error {
	_, err := p.Exec(ctx, `
		UPDATE jobs SET status = 'succeeded', progress = 1, finished_at = now(),
		                locked_by = NULL, locked_at = NULL, last_error = NULL
		WHERE id = $1 AND status = 'running' AND locked_by = $2`, id, worker)
	if err != nil {
		return fmt.Errorf("completing job: %w", mapError(err))
	}
	return nil
}

// Fail records a failure, rescheduling the job when attempts remain.
//
// Backoff is exponential in the attempt count. A model server that is switched
// off must not be hammered once a second by a queue of enrichment jobs.
func (p *Pool) Fail(ctx context.Context, id uuid.UUID, worker string, cause error) error {
	_, err := p.Exec(ctx, `
		UPDATE jobs SET
			last_error = $2,
			status = CASE WHEN attempts >= max_attempts THEN 'failed'::job_status
			              ELSE 'pending'::job_status END,
			finished_at = CASE WHEN attempts >= max_attempts THEN now() ELSE NULL END,
			run_after = CASE WHEN attempts >= max_attempts THEN run_after
			                 ELSE now() + (interval '10 seconds' * power(2, attempts)) END,
			locked_by = NULL,
			locked_at = NULL
		-- Running, and by this worker, for the same reasons as Succeed: a
		-- cancelled job stays cancelled, and one reclaimed and running
		-- elsewhere is that worker's.
		WHERE id = $1 AND status = 'running' AND locked_by = $3`, id, errText(cause), worker)
	if err != nil {
		return fmt.Errorf("failing job: %w", mapError(err))
	}
	return nil
}

// Reschedule returns a job to the queue WITHOUT consuming an attempt.
//
// This is for expected, temporary conditions - most often the model server
// being switched off. A library of 5000 books must not end up with 5000
// permanently failed enrichment jobs because the GPU box was off overnight,
// which is exactly what would happen if this were a normal failure.
func (p *Pool) Reschedule(ctx context.Context, id uuid.UUID, worker string, after time.Time, note string) error {
	return p.RescheduleSkipped(ctx, id, worker, after, note, 0)
}

// RescheduleSkipped returns a job to the queue without consuming an attempt,
// giving up once it has been skipped maxSkips times.
//
// The skip count is what stops "do not penalise a book for the model being
// off" from becoming "retry this book forever". A limit of zero means no limit,
// which is the old behaviour and is only correct where a caller has some other
// bound.
func (p *Pool) RescheduleSkipped(ctx context.Context, id uuid.UUID, worker string, after time.Time, note string, maxSkips int) error {
	_, err := p.Exec(ctx, `
		UPDATE jobs SET
			-- Given up on only when a limit is set and this skip reaches it.
			status = (CASE
				WHEN $4 > 0 AND skips + 1 >= $4 THEN 'failed'
				ELSE 'pending'
			END)::job_status,
			skips = skips + 1,
			attempts = GREATEST(attempts - 1, 0),
			run_after = $2,
			finished_at = CASE
				WHEN $4 > 0 AND skips + 1 >= $4 THEN now()
				ELSE finished_at
			END,
			last_error = CASE
				WHEN $4 > 0 AND skips + 1 >= $4
				THEN 'gave up after ' || (skips + 1) || ' skips: ' || NULLIF($3, '')
				ELSE NULLIF($3, '')
			END,
			locked_by = NULL,
			locked_at = NULL
		WHERE id = $1 AND status = 'running' AND locked_by = $5`, id, after, note, maxSkips, worker)
	if err != nil {
		return fmt.Errorf("rescheduling job: %w", mapError(err))
	}
	return nil
}

// Defer returns a running job to the queue until a given time, without
// consuming an attempt and without counting as a skip: it is waiting, not
// failing, and it starts again from nothing - no progress, not started. The
// note replaces the progress note, so whoever follows the job sees why; the
// last error, if any, is left as it was.
//
// Only a RUNNING job held by this worker, for the same reasons as Succeed: a
// job cancelled, or reclaimed and running elsewhere, while its handler ran is
// not put back by that handler.
func (p *Pool) Defer(ctx context.Context, id uuid.UUID, worker string, until time.Time, note string) error {
	_, err := p.Exec(ctx, `
		UPDATE jobs SET
			status = 'pending',
			attempts = GREATEST(attempts - 1, 0),
			run_after = $2,
			progress = 0,
			progress_note = NULLIF($3, ''),
			started_at = NULL,
			locked_by = NULL,
			locked_at = NULL
		WHERE id = $1 AND status = 'running' AND locked_by = $4`, id, until, note, worker)
	if err != nil {
		return fmt.Errorf("deferring job: %w", mapError(err))
	}
	return nil
}

// Cancel stops a pending or running job.
func (p *Pool) Cancel(ctx context.Context, id uuid.UUID) error {
	tag, err := p.Exec(ctx, `
		UPDATE jobs SET status = 'cancelled', finished_at = now(),
		                locked_by = NULL, locked_at = NULL
		WHERE id = $1 AND status IN ('pending', 'running')`, id)
	if err != nil {
		return fmt.Errorf("cancelling job: %w", mapError(err))
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("cancelling job: %w", errNotFound)
	}
	return nil
}

// ReclaimStale returns jobs abandoned by a dead worker to the queue.
//
// A worker that is killed mid-job leaves its row marked running forever. Any
// job whose lock has not been refreshed within the timeout is presumed orphaned
// and made runnable again - which is safe precisely because every job is
// idempotent and keyed on content hash rather than on having run exactly once.
func (p *Pool) ReclaimStale(ctx context.Context, olderThan time.Duration) (int64, error) {
	// A job is only returned to the queue if it has an attempt left to make.
	//
	// Without that condition a job that went stale on its last attempt came
	// back as pending and unclaimable: claiming increments attempts, which its
	// limit forbids, so every worker picked it up, failed, and tried again
	// immediately. Ending it as failed is both the honest outcome and the one
	// that keeps the queue moving.
	tag, err := p.Exec(ctx, `
		UPDATE jobs SET
			status = (CASE WHEN attempts < max_attempts THEN 'pending' ELSE 'failed' END)::job_status,
			locked_by = NULL,
			locked_at = NULL,
			finished_at = CASE WHEN attempts < max_attempts THEN finished_at ELSE now() END,
			last_error = CASE
				WHEN attempts < max_attempts
				THEN 'reclaimed after the worker stopped reporting'
				ELSE 'gave up after ' || attempts || ' attempts; the worker stopped reporting each time'
			END
		WHERE status = 'running' AND locked_at < now() - $1::interval`,
		olderThan.String())
	if err != nil {
		return 0, fmt.Errorf("reclaiming stale jobs: %w", mapError(err))
	}
	return tag.RowsAffected(), nil
}

// GetJob reads one job by id.
func (p *Pool) GetJob(ctx context.Context, id uuid.UUID) (Job, error) {
	var j Job
	var requester *uuid.UUID
	err := p.QueryRow(ctx, `
		SELECT id, type, status, target_id, payload, priority, progress,
		       COALESCE(progress_note, ''), attempts, max_attempts, skips,
		       COALESCE(last_error, ''), run_after, created_at, requested_by
		FROM jobs WHERE id = $1`, id,
	).Scan(&j.ID, &j.Type, &j.Status, &j.TargetID, &j.Payload, &j.Priority,
		&j.Progress, &j.ProgressNote, &j.Attempts, &j.MaxAttempts, &j.Skips,
		&j.LastError, &j.RunAfter, &j.CreatedAt, &requester)
	j.RequestedBy = deref0(requester)
	if err != nil {
		return Job{}, mapError(err)
	}
	return j, nil
}

// GetJobFor reads a job on somebody's behalf: the person it was queued for, or
// an administrator. Anybody else is told it does not exist.
//
// A job's progress note and last error talk about a file - its path, what the
// parser made of it - and the file may be somebody else's. Holding an id is not
// permission: ids get logged, pasted and guessed at, and this endpoint once
// handed one person the id of another's job.
//
// The administrator check is a read in the same statement, so revoking the
// right takes effect at the next poll of an open stream.
func (p *Pool) GetJobFor(ctx context.Context, id, viewer uuid.UUID) (Job, error) {
	if viewer == uuid.Nil {
		return Job{}, errNotFound
	}
	var j Job
	var requester *uuid.UUID
	err := p.QueryRow(ctx, `
		SELECT id, type, status, target_id, payload, priority, progress,
		       COALESCE(progress_note, ''), attempts, max_attempts, skips,
		       COALESCE(last_error, ''), run_after, created_at, requested_by
		FROM jobs
		WHERE id = $1
		  AND (requested_by = $2
		       OR EXISTS (SELECT 1 FROM users WHERE id = $2 AND is_admin))`, id, viewer,
	).Scan(&j.ID, &j.Type, &j.Status, &j.TargetID, &j.Payload, &j.Priority,
		&j.Progress, &j.ProgressNote, &j.Attempts, &j.MaxAttempts, &j.Skips,
		&j.LastError, &j.RunAfter, &j.CreatedAt, &requester)
	j.RequestedBy = deref0(requester)
	if err != nil {
		return Job{}, mapError(err)
	}
	return j, nil
}

// QueueDepth counts pending and running jobs by type.
func (p *Pool) QueueDepth(ctx context.Context) (map[domain.JobType]int, error) {
	rows, err := p.Query(ctx, `
		SELECT type, count(*) FROM jobs
		WHERE status IN ('pending', 'running')
		GROUP BY type`)
	if err != nil {
		return nil, fmt.Errorf("reading queue depth: %w", mapError(err))
	}
	defer rows.Close()

	out := make(map[domain.JobType]int)
	for rows.Next() {
		var t domain.JobType
		var n int
		if err := rows.Scan(&t, &n); err != nil {
			return nil, fmt.Errorf("scanning queue depth: %w", err)
		}
		out[t] = n
	}
	return out, rows.Err()
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	const maxLen = 2000 // a stack-trace-sized error should not bloat the row
	s := err.Error()
	if len(s) > maxLen {
		return s[:maxLen] + "…"
	}
	return s
}

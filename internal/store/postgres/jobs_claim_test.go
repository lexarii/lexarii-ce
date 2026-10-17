// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"errors"
	"testing"
	"time"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

func clearJobs(t *testing.T, p *Pool) {
	t.Helper()
	if _, err := p.Exec(t.Context(), `DELETE FROM jobs`); err != nil {
		t.Fatalf("clearing jobs: %v", err)
	}
}

// The poison pill.
//
// A job that went stale on its last attempt used to come back as pending with
// attempts already at its limit. Claiming increments attempts, which the
// constraint forbids, so the claim failed - and the worker tried again
// immediately, forever. Four workers produced 518 failed claims in minutes
// against a real database, and no other job could make progress.
func TestClaimSkipsJobsWithNoAttemptsLeft(t *testing.T) {
	p := testPool(t)
	clearJobs(t, p)

	id, _, err := p.Enqueue(t.Context(), EnqueueOptions{Type: domain.JobEnrich, MaxAttempts: 3})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := p.Exec(t.Context(),
		`UPDATE jobs SET attempts = max_attempts WHERE id = $1`, id); err != nil {
		t.Fatalf("exhausting attempts: %v", err)
	}

	// It must be skipped, not attempted and rejected.
	_, err = p.Claim(t.Context(), "worker-1")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Claim returned %v, want ErrNotFound: an exhausted job was selected", err)
	}
}

// A job with attempts left is still claimed normally.
func TestClaimTakesJobsWithAttemptsLeft(t *testing.T) {
	p := testPool(t)
	clearJobs(t, p)

	if _, _, err := p.Enqueue(t.Context(), EnqueueOptions{Type: domain.JobScan, MaxAttempts: 3}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	job, err := p.Claim(t.Context(), "worker-1")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if job.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", job.Attempts)
	}
}

// Reclaiming is for jobs that can still run. One that has used its last attempt
// is finished, and saying so is what keeps it out of the claim loop.
func TestReclaimFailsJobsWithNoAttemptsLeft(t *testing.T) {
	p := testPool(t)
	clearJobs(t, p)

	id, _, err := p.Enqueue(t.Context(), EnqueueOptions{Type: domain.JobEnrich, MaxAttempts: 2})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// Running, on its last attempt, and long since silent.
	if _, err := p.Exec(t.Context(), `
		UPDATE jobs SET status = 'running', attempts = max_attempts,
		                locked_by = 'worker-1', locked_at = now() - interval '1 hour'
		WHERE id = $1`, id); err != nil {
		t.Fatalf("staging a stale job: %v", err)
	}

	if _, err := p.ReclaimStale(t.Context(), time.Minute); err != nil {
		t.Fatalf("ReclaimStale: %v", err)
	}

	job, err := p.GetJob(t.Context(), id)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Status != domain.JobFailed {
		t.Errorf("status = %q, want failed: an exhausted job went back to the queue", job.Status)
	}
	// The reason has to say what happened, or a failed job is a mystery.
	if job.LastError == "" {
		t.Error("the job failed with no explanation")
	}

	// And it stays out of the claim loop.
	if _, err := p.Claim(t.Context(), "worker-1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Claim returned %v, want ErrNotFound", err)
	}
}

// A stale job that still has attempts left goes back to the queue, which is
// what reclaiming is for.
func TestReclaimRequeuesJobsWithAttemptsLeft(t *testing.T) {
	p := testPool(t)
	clearJobs(t, p)

	id, _, err := p.Enqueue(t.Context(), EnqueueOptions{Type: domain.JobEnrich, MaxAttempts: 3})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := p.Exec(t.Context(), `
		UPDATE jobs SET status = 'running', attempts = 1,
		                locked_by = 'worker-1', locked_at = now() - interval '1 hour'
		WHERE id = $1`, id); err != nil {
		t.Fatalf("staging a stale job: %v", err)
	}

	if _, err := p.ReclaimStale(t.Context(), time.Minute); err != nil {
		t.Fatalf("ReclaimStale: %v", err)
	}

	job, err := p.GetJob(t.Context(), id)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Status != domain.JobPending {
		t.Fatalf("status = %q, want pending", job.Status)
	}
	if _, err := p.Claim(t.Context(), "worker-2"); err != nil {
		t.Errorf("a requeued job could not be claimed: %v", err)
	}
}

// The sequence that actually happened: a slow job goes stale on every attempt
// until it runs out. The queue must end up quiet, not spinning.
func TestRepeatedStalingEndsInFailureNotAHotLoop(t *testing.T) {
	p := testPool(t)
	clearJobs(t, p)

	id, _, err := p.Enqueue(t.Context(), EnqueueOptions{Type: domain.JobEnrich, MaxAttempts: 3})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	for attempt := 1; attempt <= 4; attempt++ {
		job, err := p.Claim(t.Context(), "worker-1")
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				break // nothing claimable, which is the correct end state
			}
			t.Fatalf("Claim on attempt %d: %v", attempt, err)
		}
		if _, err := p.Exec(t.Context(),
			`UPDATE jobs SET locked_at = now() - interval '1 hour' WHERE id = $1`, job.ID); err != nil {
			t.Fatalf("ageing the lock: %v", err)
		}
		if _, err := p.ReclaimStale(t.Context(), time.Minute); err != nil {
			t.Fatalf("ReclaimStale: %v", err)
		}
	}

	job, err := p.GetJob(t.Context(), id)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Status != domain.JobFailed {
		t.Errorf("status = %q, want failed after exhausting its attempts", job.Status)
	}
	if job.Attempts > job.MaxAttempts {
		t.Errorf("attempts = %d exceeded max %d", job.Attempts, job.MaxAttempts)
	}
	// The decisive check: claiming is quiet, not an error every time round.
	if _, err := p.Claim(t.Context(), "worker-1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Claim returned %v, want ErrNotFound; this is the hot loop", err)
	}
}

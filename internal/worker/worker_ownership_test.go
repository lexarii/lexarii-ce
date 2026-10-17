// SPDX-License-Identifier: AGPL-3.0-or-later

package worker_test

import (
	"context"
	"testing"
	"time"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
	"scm.poniatowski.dev/adam/lexarii/internal/worker"
)

// A worker whose job was reclaimed and handed to another while it ran cannot
// record anything over that other worker's run when it finally finishes: not
// its progress, and not its success.
func TestAWorkerThatLostItsJobCannotFinishIt(t *testing.T) {
	p := testPool(t)

	jobID, _, err := p.Enqueue(t.Context(), postgres.EnqueueOptions{Type: domain.JobScan})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	const intruder = "another-process/worker-0"
	handed := make(chan struct{})
	handler := func(ctx context.Context, job postgres.Job, report worker.ProgressFunc) error {
		// The job goes quiet long enough to be reclaimed, and another worker
		// claims it - all while this handler is still running.
		if _, err := p.Exec(ctx, `UPDATE jobs SET locked_at = now() - interval '1 hour' WHERE id = $1`, job.ID); err != nil {
			t.Errorf("ageing the lock: %v", err)
		}
		if n, err := p.ReclaimStale(ctx, time.Minute); err != nil || n != 1 {
			t.Errorf("ReclaimStale = %d, %v; want the job returned", n, err)
		}
		if again, err := p.Claim(ctx, intruder); err != nil || again.ID != job.ID {
			t.Errorf("Claim by another worker = %v, %v", again.ID, err)
		}
		close(handed)

		report(0.9, "late progress from the worker that lost the job")
		return nil // and a late success
	}

	pool := worker.New(p, map[domain.JobType]worker.Handler{domain.JobScan: handler}, worker.Config{
		Workers: 1, PollInterval: 5 * time.Millisecond, ReclaimInterval: time.Hour, Logger: quietTestLogger(),
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- pool.Run(ctx) }()

	select {
	case <-handed:
	case <-ctx.Done():
		t.Fatal("the handler never ran")
	}
	// Wait until the pool has recorded its outcome and let go of the job.
	for len(pool.Running()) != 0 {
		select {
		case <-ctx.Done():
			t.Fatal("the pool never finished the job")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	var status, lockedBy string
	var progress float32
	if err := p.QueryRow(t.Context(),
		`SELECT status, COALESCE(locked_by, ''), progress FROM jobs WHERE id = $1`, jobID,
	).Scan(&status, &lockedBy, &progress); err != nil {
		t.Fatalf("reading the job: %v", err)
	}
	if status != string(domain.JobRunning) || lockedBy != intruder {
		t.Errorf("job is %s held by %q, want still running for %q", status, lockedBy, intruder)
	}
	if progress != 0 {
		t.Errorf("progress = %v, want the late report ignored", progress)
	}
}

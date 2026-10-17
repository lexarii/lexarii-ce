// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
)

// claimOnlyJobForReschedule enqueues one job and claims it as worker.
func claimOnlyJobForReschedule(t *testing.T, p *Pool, worker string) Job {
	t.Helper()
	cleanJobs(t, p)
	id, _, err := p.Enqueue(t.Context(), EnqueueOptions{Type: domain.JobEnrich, TargetID: newTarget(), MaxAttempts: 2})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	j, err := p.Claim(t.Context(), worker)
	if err != nil || j.ID != id {
		t.Fatalf("Claim = %+v, %v", j, err)
	}
	return j
}

// releaseRunAfterForReschedule makes a rescheduled job runnable now, so a
// test does not have to wait for the time it was put back until.
func releaseRunAfterForReschedule(t *testing.T, p *Pool, j Job) {
	t.Helper()
	if _, err := p.Exec(t.Context(), `UPDATE jobs SET run_after = now() - interval '1 second' WHERE id = $1`, j.ID); err != nil {
		t.Fatalf("releasing run_after: %v", err)
	}
}

// The model being off overnight must not fail a library's worth of
// enrichment: an unlimited reschedule never gives up and never spends an
// attempt, however many times it happens.
func TestRescheduleWithoutALimitNeverGivesUp(t *testing.T) {
	p := testPool(t)
	ctx := t.Context()
	j := claimOnlyJobForReschedule(t, p, "w")

	for i := 0; i < 5; i++ {
		later := time.Now().Add(time.Hour)
		if err := p.Reschedule(ctx, j.ID, "w", later, "the model is off"); err != nil {
			t.Fatalf("Reschedule %d: %v", i, err)
		}
		got, err := p.GetJob(ctx, j.ID)
		if err != nil {
			t.Fatalf("GetJob: %v", err)
		}
		if got.Status != domain.JobPending || got.Attempts != 0 || got.Skips != i+1 || got.LastError != "the model is off" {
			t.Fatalf("after %d skips: %+v", i+1, got)
		}
		if got.RunAfter.Before(later.Add(-time.Second)) {
			t.Fatalf("run_after %v, want %v", got.RunAfter, later)
		}
		// Not runnable until then.
		if _, err := p.Claim(ctx, "w"); !isNotFound(err) {
			t.Fatalf("a rescheduled job was claimed before its time: %v", err)
		}
		releaseRunAfterForReschedule(t, p, got)
		if again, err := p.Claim(ctx, "w"); err != nil || again.ID != j.ID {
			t.Fatalf("re-claim = %+v, %v", again, err)
		}
	}
}

// With a limit, the skip that reaches it ends the job as failed and says why.
func TestRescheduleSkippedGivesUpAtItsLimit(t *testing.T) {
	p := testPool(t)
	ctx := t.Context()
	j := claimOnlyJobForReschedule(t, p, "w")

	if err := p.RescheduleSkipped(ctx, j.ID, "w", time.Now(), "the model is off", 2); err != nil {
		t.Fatalf("first skip: %v", err)
	}
	got, _ := p.GetJob(ctx, j.ID)
	if got.Status != domain.JobPending || got.Skips != 1 {
		t.Fatalf("after one skip of two: %+v", got)
	}

	releaseRunAfterForReschedule(t, p, got)
	if _, err := p.Claim(ctx, "w"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := p.RescheduleSkipped(ctx, j.ID, "w", time.Now(), "the model is off", 2); err != nil {
		t.Fatalf("second skip: %v", err)
	}
	got, _ = p.GetJob(ctx, j.ID)
	if got.Status != domain.JobFailed || got.Skips != 2 {
		t.Fatalf("after reaching the limit: %+v, want failed with 2 skips", got)
	}
	if got.LastError != "gave up after 2 skips: the model is off" {
		t.Errorf("last error = %q", got.LastError)
	}
}

// A failure's message is kept, but a stack-trace-sized one is cut to size.
func TestFailKeepsTheErrorWithinBounds(t *testing.T) {
	p := testPool(t)
	ctx := t.Context()

	t.Run("a huge error is truncated", func(t *testing.T) {
		j := claimOnlyJobForReschedule(t, p, "w")
		if err := p.Fail(ctx, j.ID, "w", errors.New(strings.Repeat("e", 5000))); err != nil {
			t.Fatalf("Fail: %v", err)
		}
		got, _ := p.GetJob(ctx, j.ID)
		if utf8.RuneCountInString(got.LastError) != 2001 || !strings.HasSuffix(got.LastError, "…") {
			t.Errorf("last error is %d runes, want 2000 and an ellipsis", utf8.RuneCountInString(got.LastError))
		}
		if got.Status != domain.JobPending {
			t.Errorf("status = %s, want pending with an attempt left", got.Status)
		}
	})

	t.Run("a failure with no error records none", func(t *testing.T) {
		j := claimOnlyJobForReschedule(t, p, "w")
		if err := p.Fail(ctx, j.ID, "w", nil); err != nil {
			t.Fatalf("Fail: %v", err)
		}
		got, _ := p.GetJob(ctx, j.ID)
		if got.LastError != "" {
			t.Errorf("last error = %q, want none", got.LastError)
		}
	})
}

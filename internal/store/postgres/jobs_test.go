// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
)

func cleanJobs(t *testing.T, p *Pool) {
	t.Helper()
	if _, err := p.Exec(context.Background(), "DELETE FROM jobs"); err != nil {
		t.Fatalf("cleaning jobs: %v", err)
	}
}

func newTarget() *uuid.UUID {
	id := uuid.New()
	return &id
}

func TestEnqueueAndClaim(t *testing.T) {
	p := testPool(t)
	cleanJobs(t, p)
	ctx := t.Context()

	target := newTarget()
	id, created, err := p.Enqueue(ctx, EnqueueOptions{
		Type:     domain.JobExtract,
		TargetID: target,
		Payload:  map[string]string{"path": "/library/book.epub"},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if !created {
		t.Error("created = false on a first enqueue")
	}

	job, err := p.Claim(ctx, "worker-1")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if job.ID != id {
		t.Errorf("claimed %s, want %s", job.ID, id)
	}
	if job.Status != domain.JobRunning {
		t.Errorf("status = %q, want running", job.Status)
	}
	if job.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", job.Attempts)
	}
	if string(job.Payload) == "" || string(job.Payload) == "{}" {
		t.Errorf("payload = %q, want the enqueued value", job.Payload)
	}

	// The queue is now empty.
	if _, err := p.Claim(ctx, "worker-2"); !isNotFound(err) {
		t.Fatalf("second Claim = %v, want ErrNotFound", err)
	}
}

// Enqueueing work for a target that already has a live job must be a no-op,
// enforced by the database rather than by a racy read-then-write.
func TestEnqueueDeduplicatesLiveJobs(t *testing.T) {
	p := testPool(t)
	cleanJobs(t, p)
	ctx := t.Context()

	target := newTarget()
	first, created, err := p.Enqueue(ctx, EnqueueOptions{Type: domain.JobEmbed, TargetID: target})
	if err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	if !created {
		t.Fatal("first enqueue did not create")
	}

	second, created, err := p.Enqueue(ctx, EnqueueOptions{Type: domain.JobEmbed, TargetID: target})
	if err != nil {
		t.Fatalf("second enqueue: %v", err)
	}
	if created {
		t.Error("a duplicate enqueue reported creating a new job")
	}
	if second != first {
		t.Errorf("duplicate enqueue returned %s, want the existing %s", second, first)
	}

	var n int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM jobs`).Scan(&n); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if n != 1 {
		t.Errorf("jobs = %d, want 1", n)
	}

	// A different job type for the same target is legitimate: extract and embed
	// are different work.
	if _, created, err := p.Enqueue(ctx, EnqueueOptions{Type: domain.JobChunk, TargetID: target}); err != nil {
		t.Fatalf("different type: %v", err)
	} else if !created {
		t.Error("a different job type for the same target should be allowed")
	}
}

// Once a job finishes, the same work may be queued again - re-embedding after a
// model change, for instance.
func TestEnqueueAllowedAfterCompletion(t *testing.T) {
	p := testPool(t)
	cleanJobs(t, p)
	ctx := t.Context()

	target := newTarget()
	first, _, err := p.Enqueue(ctx, EnqueueOptions{Type: domain.JobEmbed, TargetID: target})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := p.Claim(ctx, "w"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := p.Succeed(ctx, first, "w"); err != nil {
		t.Fatalf("succeed: %v", err)
	}

	second, created, err := p.Enqueue(ctx, EnqueueOptions{Type: domain.JobEmbed, TargetID: target})
	if err != nil {
		t.Fatalf("re-enqueue: %v", err)
	}
	if !created || second == first {
		t.Error("re-enqueueing after completion should create a new job")
	}
}

// The reason SKIP LOCKED is there: many workers, no coordination, no job run twice.
func TestConcurrentWorkersEachClaimDistinctJobs(t *testing.T) {
	p := testPool(t)
	cleanJobs(t, p)
	ctx := t.Context()

	const jobCount = 60
	for i := 0; i < jobCount; i++ {
		if _, _, err := p.Enqueue(ctx, EnqueueOptions{
			Type:     domain.JobExtract,
			TargetID: newTarget(),
			Payload:  map[string]int{"n": i},
		}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	const workers = 8
	var mu sync.Mutex
	claimed := make(map[uuid.UUID]string)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			name := fmt.Sprintf("worker-%d", worker)
			for {
				job, err := p.Claim(ctx, name)
				if isNotFound(err) {
					return
				}
				if err != nil {
					t.Errorf("%s claim: %v", name, err)
					return
				}
				mu.Lock()
				if other, dup := claimed[job.ID]; dup {
					t.Errorf("job %s claimed by both %s and %s", job.ID, other, name)
				}
				claimed[job.ID] = name
				mu.Unlock()

				if err := p.Succeed(ctx, job.ID, name); err != nil {
					t.Errorf("%s succeed: %v", name, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	if len(claimed) != jobCount {
		t.Errorf("claimed %d distinct jobs, want %d", len(claimed), jobCount)
	}

	var remaining int
	if err := p.QueryRow(ctx,
		`SELECT count(*) FROM jobs WHERE status IN ('pending','running')`).Scan(&remaining); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if remaining != 0 {
		t.Errorf("%d jobs left unfinished", remaining)
	}
}

func TestClaimRespectsPriorityThenAge(t *testing.T) {
	p := testPool(t)
	cleanJobs(t, p)
	ctx := t.Context()

	low, _, err := p.Enqueue(ctx, EnqueueOptions{Type: domain.JobExtract, TargetID: newTarget(), Priority: 0})
	if err != nil {
		t.Fatalf("enqueue low: %v", err)
	}
	high, _, err := p.Enqueue(ctx, EnqueueOptions{Type: domain.JobExtract, TargetID: newTarget(), Priority: 50})
	if err != nil {
		t.Fatalf("enqueue high: %v", err)
	}

	first, err := p.Claim(ctx, "w")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if first.ID != high {
		t.Errorf("claimed %s first, want the higher-priority job %s", first.ID, high)
	}

	second, err := p.Claim(ctx, "w")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if second.ID != low {
		t.Errorf("claimed %s second, want %s", second.ID, low)
	}
}

// The queue's order is the types' order, whatever order the work was asked
// for in: discovery, then extraction, then making books searchable, and the
// model's description last. Enriching a first scan as it goes must not take
// the workers from extraction.
func TestClaimTakesWorkInTheOrderOfItsTypes(t *testing.T) {
	p := testPool(t)
	cleanJobs(t, p)
	ctx := t.Context()

	// Queued in the reverse of the order they should run in.
	order := []domain.JobType{domain.JobScan, domain.JobExtract, domain.JobChunk, domain.JobEmbed, domain.JobEnrich}
	for i := len(order) - 1; i >= 0; i-- {
		if _, _, err := p.Enqueue(ctx, EnqueueOptions{Type: order[i], TargetID: newTarget()}); err != nil {
			t.Fatalf("enqueue %s: %v", order[i], err)
		}
	}
	for _, want := range order {
		got, err := p.Claim(ctx, "w")
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if got.Type != want {
			t.Errorf("claimed %s, want %s", got.Type, want)
		}
	}
}

func TestClaimSkipsDelayedJobs(t *testing.T) {
	p := testPool(t)
	cleanJobs(t, p)
	ctx := t.Context()

	if _, _, err := p.Enqueue(ctx, EnqueueOptions{
		Type:     domain.JobEnrich,
		TargetID: newTarget(),
		RunAfter: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	if _, err := p.Claim(ctx, "w"); !isNotFound(err) {
		t.Fatalf("Claim = %v, want ErrNotFound: a delayed job is not yet runnable", err)
	}
}

// A failure that has attempts left goes back to the queue with a delay; one that
// has exhausted them is terminal.
func TestFailRetriesThenGivesUp(t *testing.T) {
	p := testPool(t)
	cleanJobs(t, p)
	ctx := t.Context()

	id, _, err := p.Enqueue(ctx, EnqueueOptions{
		Type: domain.JobEnrich, TargetID: newTarget(), MaxAttempts: 2,
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// First attempt fails: back to pending, with backoff.
	if _, err := p.Claim(ctx, "w"); err != nil {
		t.Fatalf("claim 1: %v", err)
	}
	if err := p.Fail(ctx, id, "w", errors.New("model server unreachable")); err != nil {
		t.Fatalf("fail 1: %v", err)
	}

	job, err := p.GetJob(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if job.Status != domain.JobPending {
		t.Errorf("status = %q after a retryable failure, want pending", job.Status)
	}
	if !job.RunAfter.After(time.Now()) {
		t.Error("a retried job should be delayed by backoff, not runnable immediately")
	}
	if job.LastError == "" {
		t.Error("the failure cause was not recorded")
	}

	// Make it runnable, then exhaust the attempts.
	if _, err := p.Exec(ctx, `UPDATE jobs SET run_after = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("clearing backoff: %v", err)
	}
	if _, err := p.Claim(ctx, "w"); err != nil {
		t.Fatalf("claim 2: %v", err)
	}
	if err := p.Fail(ctx, id, "w", errors.New("still unreachable")); err != nil {
		t.Fatalf("fail 2: %v", err)
	}

	job, err = p.GetJob(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if job.Status != domain.JobFailed {
		t.Errorf("status = %q after exhausting attempts, want failed", job.Status)
	}
	if !job.Status.Terminal() {
		t.Error("a failed job should be terminal")
	}
}

// A worker killed mid-job leaves its row marked running forever. Reclaiming is
// safe because every job is idempotent.
func TestReclaimStaleReturnsAbandonedJobs(t *testing.T) {
	p := testPool(t)
	cleanJobs(t, p)
	ctx := t.Context()

	id, _, err := p.Enqueue(ctx, EnqueueOptions{Type: domain.JobChunk, TargetID: newTarget()})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := p.Claim(ctx, "worker-that-dies"); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Nothing is stale yet.
	if n, err := p.ReclaimStale(ctx, time.Hour); err != nil || n != 0 {
		t.Fatalf("ReclaimStale = (%d, %v), want (0, nil)", n, err)
	}

	// Simulate the worker dying: its lock stops being refreshed.
	if _, err := p.Exec(ctx,
		`UPDATE jobs SET locked_at = now() - interval '10 minutes' WHERE id = $1`, id); err != nil {
		t.Fatalf("ageing the lock: %v", err)
	}

	n, err := p.ReclaimStale(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("ReclaimStale: %v", err)
	}
	if n != 1 {
		t.Fatalf("reclaimed %d jobs, want 1", n)
	}

	job, err := p.GetJob(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if job.Status != domain.JobPending {
		t.Errorf("status = %q after reclaim, want pending", job.Status)
	}

	// And it can be picked up again.
	if _, err := p.Claim(ctx, "worker-that-lives"); err != nil {
		t.Fatalf("re-claim: %v", err)
	}
}

func TestProgressIsClampedAndRecorded(t *testing.T) {
	p := testPool(t)
	cleanJobs(t, p)
	ctx := t.Context()

	id, _, err := p.Enqueue(ctx, EnqueueOptions{Type: domain.JobScan})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := p.Claim(ctx, "w"); err != nil {
		t.Fatalf("claim: %v", err)
	}

	if err := p.Progress(ctx, id, "w", 0.42, "scanned 42 of 100 files"); err != nil {
		t.Fatalf("Progress: %v", err)
	}
	job, err := p.GetJob(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if job.Progress < 0.41 || job.Progress > 0.43 {
		t.Errorf("progress = %v, want ~0.42", job.Progress)
	}
	if job.ProgressNote != "scanned 42 of 100 files" {
		t.Errorf("note = %q", job.ProgressNote)
	}

	// Out-of-range values must be clamped, not rejected by the CHECK constraint.
	for _, v := range []float32{-1, 2, 99} {
		if err := p.Progress(ctx, id, "w", v, ""); err != nil {
			t.Fatalf("Progress(%v): %v", v, err)
		}
	}
	job, err = p.GetJob(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if job.Progress != 1 {
		t.Errorf("progress = %v after clamping, want 1", job.Progress)
	}
}

func TestCancel(t *testing.T) {
	p := testPool(t)
	cleanJobs(t, p)
	ctx := t.Context()

	id, _, err := p.Enqueue(ctx, EnqueueOptions{Type: domain.JobEnrich, TargetID: newTarget()})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := p.Cancel(ctx, id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	job, err := p.GetJob(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if job.Status != domain.JobCancelled {
		t.Errorf("status = %q, want cancelled", job.Status)
	}

	// A cancelled job is not claimable, and cancelling it again is not found.
	if _, err := p.Claim(ctx, "w"); !isNotFound(err) {
		t.Errorf("a cancelled job was claimable")
	}
	if err := p.Cancel(ctx, id); !isNotFound(err) {
		t.Errorf("second Cancel = %v, want ErrNotFound", err)
	}
}

func TestQueueDepth(t *testing.T) {
	p := testPool(t)
	cleanJobs(t, p)
	ctx := t.Context()

	for i := 0; i < 3; i++ {
		if _, _, err := p.Enqueue(ctx, EnqueueOptions{Type: domain.JobExtract, TargetID: newTarget()}); err != nil {
			t.Fatalf("enqueue extract: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, _, err := p.Enqueue(ctx, EnqueueOptions{Type: domain.JobEmbed, TargetID: newTarget()}); err != nil {
			t.Fatalf("enqueue embed: %v", err)
		}
	}

	depth, err := p.QueueDepth(ctx)
	if err != nil {
		t.Fatalf("QueueDepth: %v", err)
	}
	if depth[domain.JobExtract] != 3 {
		t.Errorf("extract depth = %d, want 3", depth[domain.JobExtract])
	}
	if depth[domain.JobEmbed] != 2 {
		t.Errorf("embed depth = %d, want 2", depth[domain.JobEmbed])
	}
}

func TestJobTypeNeedsLLM(t *testing.T) {
	needs := map[domain.JobType]bool{
		domain.JobScan:    false,
		domain.JobExtract: false,
		domain.JobChunk:   false,
		domain.JobEnrich:  true,
		domain.JobEmbed:   true,
	}
	for jobType, want := range needs {
		if got := jobType.NeedsLLM(); got != want {
			t.Errorf("%s.NeedsLLM() = %v, want %v", jobType, got, want)
		}
	}
}

// Cancelling a running job has to stick.
//
// Nothing interrupts a handler that is already executing, so it finishes and
// reports its outcome afterwards. Unguarded, that report wrote over the
// cancellation: a job somebody stopped came back as succeeded, or - worse -
// Fail returned it to the queue and it ran all over again, which for an
// enrichment job is the minutes of model time they cancelled it to save.
func TestAFinishedHandlerCannotUndoACancellation(t *testing.T) {
	p := testPool(t)
	ctx := t.Context()
	cleanJobs(t, p)

	for _, report := range []struct {
		name string
		call func(uuid.UUID) error
	}{
		{"succeed", func(id uuid.UUID) error { return p.Succeed(ctx, id, "worker-1") }},
		{"fail", func(id uuid.UUID) error { return p.Fail(ctx, id, "worker-1", errors.New("too late")) }},
	} {
		t.Run(report.name, func(t *testing.T) {
			id, _, err := p.Enqueue(ctx, EnqueueOptions{Type: domain.JobScan})
			if err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			if _, err := p.Claim(ctx, "worker-1"); err != nil {
				t.Fatalf("Claim: %v", err)
			}
			if err := p.Cancel(ctx, id); err != nil {
				t.Fatalf("Cancel: %v", err)
			}

			// The handler, which never noticed, reports its outcome.
			if err := report.call(id); err != nil {
				t.Fatalf("%s: %v", report.name, err)
			}

			var status string
			if err := p.QueryRow(ctx,
				`SELECT status FROM jobs WHERE id = $1`, id).Scan(&status); err != nil {
				t.Fatalf("reading the job: %v", err)
			}
			if status != "cancelled" {
				t.Errorf("status = %q after %s, want it to stay cancelled", status, report.name)
			}
		})
	}
}

// A deferred job is pending again, runnable only from the time given, with
// the attempt its claim took given back, no skip counted, and the reason as
// its note.
func TestDeferPutsAJobBackWithoutAnAttempt(t *testing.T) {
	p := testPool(t)
	cleanJobs(t, p)
	ctx := t.Context()

	owner := testOwner(t, p)
	id, _, err := p.Enqueue(ctx, EnqueueOptions{Type: domain.JobEnrich, TargetID: newTarget(), RequestedBy: owner})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claimed, err := p.Claim(ctx, "w")
	if err != nil || claimed.ID != id || claimed.Attempts != 1 || claimed.RequestedBy != owner {
		t.Fatalf("Claim = %+v, %v", claimed, err)
	}
	until := time.Now().Add(time.Hour)
	if err := p.Defer(ctx, id, "w", until, "waiting its turn"); err != nil {
		t.Fatalf("Defer: %v", err)
	}

	got, err := p.GetJob(ctx, id)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != domain.JobPending || got.Attempts != 0 || got.Skips != 0 ||
		got.ProgressNote != "waiting its turn" || got.RunAfter.Before(until.Add(-time.Second)) {
		t.Errorf("after Defer: %+v", got)
	}
	if _, err := p.Claim(ctx, "w"); !isNotFound(err) {
		t.Errorf("Claim before the time = %v, want nothing runnable", err)
	}

	// Once the time has come it runs, with its attempts intact.
	if _, err := p.Exec(ctx, `UPDATE jobs SET run_after = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("moving the clock: %v", err)
	}
	again, err := p.Claim(ctx, "w")
	if err != nil || again.ID != id || again.Attempts != 1 {
		t.Errorf("Claim after the time = %+v, %v; want the job on its first attempt", again, err)
	}

	// Deferring a job that is no longer running changes nothing.
	if err := p.Cancel(ctx, id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if err := p.Defer(ctx, id, "w", time.Now(), "late"); err != nil {
		t.Fatalf("Defer: %v", err)
	}
	if got, _ := p.GetJob(ctx, id); got.Status != domain.JobCancelled {
		t.Errorf("a cancelled job was put back: %+v", got)
	}
}

// A job reclaimed from a worker that went quiet, and claimed again, is the
// new worker's. The first one, finishing late, changes nothing about it -
// neither defers it back to the queue, nor fails, nor finishes it - and the
// second one's own changes still land.
func TestOnlyTheWorkerHoldingAJobChangesIt(t *testing.T) {
	p := testPool(t)
	cleanJobs(t, p)
	ctx := t.Context()

	id, _, err := p.Enqueue(ctx, EnqueueOptions{Type: domain.JobEnrich, TargetID: newTarget()})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	reclaim := func(worker string) Job {
		t.Helper()
		if _, err := p.Exec(ctx, `UPDATE jobs SET locked_at = now() - interval '1 hour' WHERE id = $1`, id); err != nil {
			t.Fatalf("ageing the lock: %v", err)
		}
		if _, err := p.ReclaimStale(ctx, time.Minute); err != nil {
			t.Fatalf("ReclaimStale: %v", err)
		}
		j, err := p.Claim(ctx, worker)
		if err != nil || j.ID != id {
			t.Fatalf("Claim by %s = %+v, %v", worker, j, err)
		}
		return j
	}
	if _, err := p.Claim(ctx, "a"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	reclaim("b")
	reclaim("c")
	before, _ := p.GetJob(ctx, id)

	// Worker a, long gone quiet, comes back and tries everything.
	for name, stale := range map[string]func() error{
		"defer":    func() error { return p.Defer(ctx, id, "a", time.Now(), "late") },
		"skip":     func() error { return p.RescheduleSkipped(ctx, id, "a", time.Now(), "late", 40) },
		"fail":     func() error { return p.Fail(ctx, id, "a", errors.New("late")) },
		"succeed":  func() error { return p.Succeed(ctx, id, "a") },
		"progress": func() error { return p.Progress(ctx, id, "a", 0.9, "late") },
	} {
		if err := stale(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	after, _ := p.GetJob(ctx, id)
	if after.Status != domain.JobRunning || after.Attempts != before.Attempts ||
		after.Skips != before.Skips || after.Progress != before.Progress || after.ProgressNote == "late" {
		t.Errorf("a stale worker changed a job another holds: before %+v, after %+v", before, after)
	}

	// Worker c, which holds it, defers it as it should.
	if err := p.Defer(ctx, id, "c", time.Now().Add(time.Hour), "waiting"); err != nil {
		t.Fatalf("Defer: %v", err)
	}
	if got, _ := p.GetJob(ctx, id); got.Status != domain.JobPending || got.ProgressNote != "waiting" {
		t.Errorf("the holder's deferral did not land: %+v", got)
	}
}

// insertJob writes a job row directly, finished or not, for tests about what
// happens to old ones.
func insertJob(t *testing.T, p *Pool, status string, finishedAgo time.Duration, requestedBy *uuid.UUID,
	payload map[string]any) uuid.UUID {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	var finished *time.Time
	if finishedAgo > 0 {
		at := time.Now().Add(-finishedAgo)
		finished = &at
	}
	var id uuid.UUID
	if err := p.QueryRow(t.Context(), `
		INSERT INTO jobs (type, status, payload, requested_by, finished_at, target_id)
		VALUES ('extract', $1::job_status, $2, $3, $4, gen_random_uuid()) RETURNING id`,
		status, body, requestedBy, finished).Scan(&id); err != nil {
		t.Fatalf("inserting a job: %v", err)
	}
	return id
}

// jobRow reads what a job still says: its payload and who asked, or that it
// is gone.
func jobRow(t *testing.T, p *Pool, id uuid.UUID) (payload map[string]any, requestedBy *uuid.UUID, exists bool) {
	t.Helper()
	var body []byte
	err := p.QueryRow(t.Context(), `SELECT payload, requested_by FROM jobs WHERE id = $1`, id).Scan(&body, &requestedBy)
	if isNotFound(mapError(err)) {
		return nil, nil, false
	}
	if err != nil {
		t.Fatalf("reading a job: %v", err)
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decoding a payload: %v", err)
	}
	return payload, requestedBy, true
}

// Deleting an account, either way, and erasing somebody's books take them
// out of their finished jobs: the uploaded file's name, whom it was for, who
// asked. Unfinished jobs keep what they need to run; nobody else's change.
func TestFinishedJobsForgetWhoUploadedWhat(t *testing.T) {
	for _, tc := range []struct {
		name string
		do   func(p *Pool, admin, x uuid.UUID) error
	}{
		{"deleting the account", func(p *Pool, admin, x uuid.UUID) error {
			_, err := p.DeleteAccount(context.Background(), admin, x)
			return err
		}},
		{"deleting the account, erasing", func(p *Pool, admin, x uuid.UUID) error {
			_, err := p.DeleteAccountErasing(context.Background(), admin, x)
			return err
		}},
		{"erasing their books", func(p *Pool, admin, x uuid.UUID) error {
			_, err := p.EraseSoleHoldings(context.Background(), admin, x)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testPool(t)
			cleanLibrary(t, p)
			admin, x, y := testOwner(t, p), secondUser(t, p), secondUser(t, p)
			mine := func(hash string) map[string]any {
				return map[string]any{"contentHash": hash, "uploaded": true,
					"originalFilename": "my diary.epub", "ownerId": x.String()}
			}
			done := insertJob(t, p, "succeeded", time.Hour, &x, mine(fmt.Sprintf("%064x", 0xc1)))
			failed := insertJob(t, p, "failed", time.Hour, &x, mine(fmt.Sprintf("%064x", 0xc2)))
			// Queued by somebody else for them - a scan's extraction, say.
			forThem := insertJob(t, p, "succeeded", time.Hour, &admin,
				map[string]any{"contentHash": fmt.Sprintf("%064x", 0xc3), "ownerId": x.String()})
			// Running: it still needs to know whose it is.
			running := insertJob(t, p, "running", 0, &x, mine(fmt.Sprintf("%064x", 0xc4)))
			theirs := insertJob(t, p, "succeeded", time.Hour, &y, map[string]any{
				"contentHash": fmt.Sprintf("%064x", 0xc5), "uploaded": true,
				"originalFilename": "theirs.epub", "ownerId": y.String()})
			t.Cleanup(func() {
				_, _ = p.Exec(context.Background(), `DELETE FROM jobs WHERE id = ANY($1)`,
					[]uuid.UUID{done, failed, forThem, running, theirs})
			})

			if err := tc.do(p, admin, x); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}

			for _, id := range []uuid.UUID{done, failed, forThem} {
				payload, by, ok := jobRow(t, p, id)
				if !ok {
					t.Fatal("a finished job was deleted; it should only forget the person")
				}
				_, named := payload["originalFilename"]
				_, owned := payload["ownerId"]
				if named || owned || by != nil {
					t.Errorf("a finished job still says %v, asked by %v", payload, by)
				}
				if payload["contentHash"] == nil {
					t.Errorf("a finished job lost what it was about: %v", payload)
				}
			}
			if payload, _, _ := jobRow(t, p, running); payload["originalFilename"] != "my diary.epub" ||
				payload["ownerId"] != x.String() {
				t.Errorf("a running job lost what it needs: %v", payload)
			}
			if payload, by, _ := jobRow(t, p, theirs); payload["originalFilename"] != "theirs.epub" || by == nil || *by != y {
				t.Errorf("somebody else's finished job changed: %v, asked by %v", payload, by)
			}
		})
	}
}

// The retention sweep deletes finished jobs older than the cut-off, in
// batches, and nothing pending or running however old, nor anything that
// finished since.
func TestDeletingFinishedJobs(t *testing.T) {
	p := testPool(t)
	deleteFinishedBatch = 2
	t.Cleanup(func() { deleteFinishedBatch = 5000 })
	ctx := t.Context()
	marker := map[string]any{"test": uuid.NewString()}
	old := []uuid.UUID{
		insertJob(t, p, "succeeded", 40*24*time.Hour, nil, marker),
		insertJob(t, p, "failed", 40*24*time.Hour, nil, marker),
		insertJob(t, p, "cancelled", 40*24*time.Hour, nil, marker),
	}
	recent := insertJob(t, p, "succeeded", 24*time.Hour, nil, marker)
	pending := insertJob(t, p, "pending", 0, nil, marker)
	if _, err := p.Exec(ctx, `UPDATE jobs SET created_at = now() - interval '90 days' WHERE id = $1`, pending); err != nil {
		t.Fatalf("ageing: %v", err)
	}
	t.Cleanup(func() {
		_, _ = p.Exec(context.Background(), `DELETE FROM jobs WHERE payload ->> 'test' = $1`, marker["test"])
	})

	n, err := p.DeleteFinishedJobs(ctx, time.Now().Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("DeleteFinishedJobs: %v", err)
	}
	if n < int64(len(old)) {
		t.Errorf("deleted %d, want at least the %d old ones", n, len(old))
	}
	for _, id := range old {
		if _, _, ok := jobRow(t, p, id); ok {
			t.Error("an old finished job survived the sweep")
		}
	}
	for _, id := range []uuid.UUID{recent, pending} {
		if _, _, ok := jobRow(t, p, id); !ok {
			t.Error("the sweep deleted a job that finished since, or never finished")
		}
	}
}

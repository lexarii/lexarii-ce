// SPDX-License-Identifier: AGPL-3.0-or-later

package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/llm"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// fakeQueue is an in-memory Queue. Hand-written rather than generated: the
// interface is five methods and a fake keeps the test readable.
type fakeQueue struct {
	mu sync.Mutex

	pending []postgres.Job

	succeeded        []uuid.UUID
	failed           map[uuid.UUID]error
	rescheduled      map[uuid.UUID]string
	rescheduledAfter map[uuid.UUID]time.Time
	rescheduleLimit  int
	deferred         map[uuid.UUID]string
	deferFailures    int
	deferredUntil    map[uuid.UUID]time.Time
	progress         map[uuid.UUID]float32
	reclaimCalls     int
}

func newFakeQueue(jobs ...postgres.Job) *fakeQueue {
	return &fakeQueue{
		pending:          jobs,
		failed:           make(map[uuid.UUID]error),
		rescheduled:      make(map[uuid.UUID]string),
		rescheduledAfter: make(map[uuid.UUID]time.Time),
		deferred:         make(map[uuid.UUID]string),
		deferredUntil:    make(map[uuid.UUID]time.Time),
		progress:         make(map[uuid.UUID]float32),
	}
}

func (q *fakeQueue) Claim(context.Context, string) (postgres.Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) == 0 {
		return postgres.Job{}, store.ErrNotFound
	}
	job := q.pending[0]
	q.pending = q.pending[1:]
	return job, nil
}

func (q *fakeQueue) Progress(_ context.Context, id uuid.UUID, _ string, fraction float32, _ string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.progress[id] = fraction
	return nil
}

func (q *fakeQueue) Succeed(_ context.Context, id uuid.UUID, _ string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.succeeded = append(q.succeeded, id)
	return nil
}

func (q *fakeQueue) Fail(_ context.Context, id uuid.UUID, _ string, cause error) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.failed[id] = cause
	return nil
}

func (q *fakeQueue) RescheduleSkipped(_ context.Context, id uuid.UUID, _ string, after time.Time, note string, maxSkips int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.rescheduled[id] = note
	q.rescheduledAfter[id] = after
	q.rescheduleLimit = maxSkips
	return nil
}

func (q *fakeQueue) Defer(_ context.Context, id uuid.UUID, _ string, until time.Time, note string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.deferFailures > 0 {
		q.deferFailures--
		return errors.New("database hiccup")
	}
	q.deferred[id] = note
	q.deferredUntil[id] = until
	return nil
}

func (q *fakeQueue) ReclaimStale(context.Context, time.Duration) (int64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.reclaimCalls++
	return 0, nil
}

func (q *fakeQueue) outcome(id uuid.UUID) (succeeded bool, reschedule string, failure error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, s := range q.succeeded {
		if s == id {
			succeeded = true
		}
	}
	return succeeded, q.rescheduled[id], q.failed[id]
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// runPool runs the pool until every job has an outcome, or the timeout expires.
func runPool(t *testing.T, q *fakeQueue, handlers map[domain.JobType]Handler, expect int) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	pool := New(q, handlers, Config{
		Workers:         2,
		PollInterval:    5 * time.Millisecond,
		ReclaimInterval: 10 * time.Millisecond,
		Logger:          quietLogger(),
	})

	done := make(chan error, 1)
	go func() { done <- pool.Run(ctx) }()

	deadline := time.After(4 * time.Second)
	for {
		q.mu.Lock()
		settled := len(q.succeeded) + len(q.failed) + len(q.rescheduled) + len(q.deferred)
		q.mu.Unlock()
		if settled >= expect {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out: %d of %d jobs settled", settled, expect)
		case <-time.After(5 * time.Millisecond):
		}
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("pool returned %v, want nil on cancellation", err)
	}
}

func job(jobType domain.JobType) postgres.Job {
	return postgres.Job{
		ID: uuid.New(), Type: jobType, Status: domain.JobRunning,
		Attempts: 1, MaxAttempts: 3, Payload: []byte("{}"),
	}
}

func TestPoolRunsHandlerAndRecordsSuccess(t *testing.T) {
	j := job(domain.JobScan)
	q := newFakeQueue(j)

	var ran bool
	handlers := map[domain.JobType]Handler{
		domain.JobScan: func(context.Context, postgres.Job, ProgressFunc) error {
			ran = true
			return nil
		},
	}

	runPool(t, q, handlers, 1)

	if !ran {
		t.Error("the handler did not run")
	}
	if ok, _, _ := q.outcome(j.ID); !ok {
		t.Error("the job was not recorded as succeeded")
	}
}

func TestPoolRecordsHandlerFailure(t *testing.T) {
	j := job(domain.JobExtract)
	q := newFakeQueue(j)

	boom := errors.New("the file is corrupt")
	handlers := map[domain.JobType]Handler{
		domain.JobExtract: func(context.Context, postgres.Job, ProgressFunc) error { return boom },
	}

	runPool(t, q, handlers, 1)

	_, _, failure := q.outcome(j.ID)
	if !errors.Is(failure, boom) {
		t.Fatalf("failure = %v, want %v", failure, boom)
	}
}

// A switched-off model server must not permanently fail a queue of enrichment
// jobs. Skipping reschedules without consuming an attempt.
func TestSkippedJobIsRescheduledNotFailed(t *testing.T) {
	j := job(domain.JobEnrich)
	q := newFakeQueue(j)

	handlers := map[domain.JobType]Handler{
		domain.JobEnrich: func(context.Context, postgres.Job, ProgressFunc) error {
			return errors.Join(ErrSkipped, errors.New("model server unreachable"))
		},
	}

	runPool(t, q, handlers, 1)

	succeeded, note, failure := q.outcome(j.ID)
	if succeeded {
		t.Error("a skipped job was recorded as succeeded")
	}
	if failure != nil {
		t.Errorf("a skipped job was recorded as failed: %v", failure)
	}
	if note == "" {
		t.Error("a skipped job was not rescheduled")
	}
}

// One bad book must not take down a worker.
func TestHandlerPanicBecomesAFailure(t *testing.T) {
	j := job(domain.JobExtract)
	q := newFakeQueue(j)

	handlers := map[domain.JobType]Handler{
		domain.JobExtract: func(context.Context, postgres.Job, ProgressFunc) error {
			panic("index out of range in a third-party parser")
		},
	}

	runPool(t, q, handlers, 1)

	_, _, failure := q.outcome(j.ID)
	if failure == nil {
		t.Fatal("a panicking handler did not produce a failure")
	}
	if got := failure.Error(); got == "" {
		t.Error("the panic value was not recorded")
	}
}

// An unregistered job type is a wiring bug, not a transient fault.
func TestUnknownJobTypeFails(t *testing.T) {
	j := job(domain.JobChunk)
	q := newFakeQueue(j)

	runPool(t, q, map[domain.JobType]Handler{}, 1)

	_, _, failure := q.outcome(j.ID)
	if failure == nil {
		t.Fatal("a job with no handler did not fail")
	}
}

func TestProgressIsForwarded(t *testing.T) {
	j := job(domain.JobScan)
	q := newFakeQueue(j)

	handlers := map[domain.JobType]Handler{
		domain.JobScan: func(_ context.Context, _ postgres.Job, report ProgressFunc) error {
			report(0.5, "halfway")
			return nil
		},
	}

	runPool(t, q, handlers, 1)

	q.mu.Lock()
	got := q.progress[j.ID]
	q.mu.Unlock()
	if got != 0.5 {
		t.Errorf("progress = %v, want 0.5", got)
	}
}

func TestPoolDrainsManyJobsConcurrently(t *testing.T) {
	const count = 40
	jobs := make([]postgres.Job, 0, count)
	for i := 0; i < count; i++ {
		jobs = append(jobs, job(domain.JobExtract))
	}
	q := newFakeQueue(jobs...)

	var mu sync.Mutex
	seen := make(map[uuid.UUID]int)
	handlers := map[domain.JobType]Handler{
		domain.JobExtract: func(_ context.Context, j postgres.Job, _ ProgressFunc) error {
			mu.Lock()
			seen[j.ID]++
			mu.Unlock()
			return nil
		},
	}

	runPool(t, q, handlers, count)

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != count {
		t.Errorf("ran %d distinct jobs, want %d", len(seen), count)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("job %s ran %d times, want once", id, n)
		}
	}
}

// Cancellation is how the pool is stopped, not an error.
func TestPoolStopsCleanlyOnCancellation(t *testing.T) {
	q := newFakeQueue()
	pool := New(q, map[domain.JobType]Handler{}, Config{
		Workers: 2, PollInterval: time.Millisecond, ReclaimInterval: time.Millisecond,
		Logger: quietLogger(),
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- pool.Run(ctx) }()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

// A job interrupted by shutdown is left running so the reclaimer can return it
// to the queue. Failing it would consume an attempt for no reason.
func TestShutdownDoesNotFailInterruptedJobs(t *testing.T) {
	j := job(domain.JobExtract)
	q := newFakeQueue(j)

	started := make(chan struct{})
	handlers := map[domain.JobType]Handler{
		domain.JobExtract: func(ctx context.Context, _ postgres.Job, _ ProgressFunc) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		},
	}

	pool := New(q, handlers, Config{
		Workers: 1, PollInterval: time.Millisecond, ReclaimInterval: time.Hour,
		Logger: quietLogger(),
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- pool.Run(ctx) }()

	<-started
	cancel()
	<-done

	succeeded, note, failure := q.outcome(j.ID)
	if succeeded {
		t.Error("an interrupted job was marked succeeded")
	}
	if failure != nil {
		t.Errorf("an interrupted job was marked failed: %v", failure)
	}
	if note != "" {
		t.Errorf("an interrupted job was rescheduled: %q", note)
	}
}

func TestReclaimerRuns(t *testing.T) {
	q := newFakeQueue()
	pool := New(q, map[domain.JobType]Handler{}, Config{
		Workers: 1, PollInterval: time.Hour, ReclaimInterval: 5 * time.Millisecond,
		Logger: quietLogger(),
	})

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_ = pool.Run(ctx)

	q.mu.Lock()
	calls := q.reclaimCalls
	q.mu.Unlock()
	if calls == 0 {
		t.Error("the reclaimer never ran")
	}
}

// Deduplication of extract jobs depends on this being stable across processes
// and restarts.
func TestTargetForHashIsStableAndDistinct(t *testing.T) {
	a := TargetForHash("abc123")
	b := TargetForHash("abc123")
	c := TargetForHash("def456")

	if a != b {
		t.Errorf("the same hash produced different targets: %s vs %s", a, b)
	}
	if a == c {
		t.Error("different hashes produced the same target")
	}
	if a == uuid.Nil {
		t.Error("target is the nil UUID")
	}
}

func TestIsSettled(t *testing.T) {
	settled := map[domain.IngestStatus]bool{
		domain.IngestReady: true,
		// Re-parsing a scan produces the same absence of text every time; OCR is
		// a separate, explicit step.
		domain.IngestNeedsOCR: true,
		// NOT settled. "Unsupported" says what this version of Lexarii can
		// read, not what the file is, so it has to be retried: the day MOBI
		// became readable, every MOBI already shelved would otherwise have
		// stayed unreadable forever.
		domain.IngestUnsupported: false,
		domain.IngestPending:     false,
		domain.IngestExtracting:  false,
		// A failure is worth retrying: the file may have been repaired or
		// replaced since.
		domain.IngestFailed: false,
	}
	for status, want := range settled {
		if got := isSettled(status); got != want {
			t.Errorf("isSettled(%q) = %v, want %v", status, got, want)
		}
	}
}

// A skipped job must not retry forever.
//
// Skips deliberately do not consume an attempt, so that a model server off
// overnight does not fail a library. Unbounded, that meant a book which always
// failed skipped, waited, and ran again indefinitely - holding a worker each
// time. Two of them held half the pool with nothing in the queue saying so.
func TestSkippedJobsBackOffAndAreBounded(t *testing.T) {
	q := newFakeQueue(job(domain.JobScan))
	handlers := map[domain.JobType]Handler{
		domain.JobScan: func(context.Context, postgres.Job, ProgressFunc) error {
			return fmt.Errorf("%w: the model is off", ErrSkipped)
		},
	}
	runPool(t, q, handlers, 1)

	q.mu.Lock()
	defer q.mu.Unlock()
	if q.rescheduleLimit != maxSkips {
		t.Errorf("reschedule limit = %d, want %d: skips would be unbounded", q.rescheduleLimit, maxSkips)
	}
	if len(q.rescheduled) != 1 {
		t.Fatalf("rescheduled %d jobs, want 1", len(q.rescheduled))
	}
}

// A provider that says when to come back is believed, as a floor: it never
// shortens our own backoff, and without one nothing changes.
func TestRescheduleUsesRetryAfterAsAFloor(t *testing.T) {
	pool := New(newFakeQueue(), map[domain.JobType]Handler{}, Config{PollInterval: time.Second})

	delayFor := func(skips int, cause error) time.Duration {
		q := newFakeQueue()
		pool.queue = q
		j := job(domain.JobScan)
		j.Skips = skips
		if err := pool.reschedule(context.Background(), j, "w", cause); err != nil {
			t.Fatalf("reschedule: %v", err)
		}
		q.mu.Lock()
		defer q.mu.Unlock()
		return time.Until(q.rescheduledAfter[j.ID])
	}
	within := func(got, want time.Duration) bool {
		return got > want-2*time.Second && got <= want+time.Second
	}

	// Longer than our backoff (15s on the first skip): the server's word wins,
	// even past the usual 15-minute cap, and through the wrapping the enricher
	// adds.
	hour := fmt.Errorf("%w: %w", ErrSkipped, &llm.HTTPError{Status: 429, RetryAfter: time.Hour})
	if got := delayFor(0, hour); !within(got, time.Hour) {
		t.Errorf("delay = %v, want about an hour", got)
	}
	// Shorter than our backoff: ours stands.
	short := fmt.Errorf("%w: %w", ErrSkipped, &llm.HTTPError{Status: 429, RetryAfter: 2 * time.Second})
	if got := delayFor(0, short); !within(got, 15*time.Second) {
		t.Errorf("delay = %v, want our 15s backoff", got)
	}
	// Absent: unchanged.
	if got := delayFor(0, errors.New("model off")); !within(got, 15*time.Second) {
		t.Errorf("delay = %v, want our 15s backoff", got)
	}
}

// The delay grows with the number of skips, so a book that keeps failing stops
// occupying a worker every thirty seconds.
func TestSkipBackoffGrowsWithSkips(t *testing.T) {
	pool := New(newFakeQueue(), map[domain.JobType]Handler{}, Config{PollInterval: time.Second})

	previous := time.Duration(0)
	for _, skips := range []int{0, 1, 2, 3, 8} {
		q := newFakeQueue()
		pool.queue = q

		j := job(domain.JobScan)
		j.Skips = skips
		if err := pool.reschedule(context.Background(), j, "w", errors.New("model off")); err != nil {
			t.Fatalf("reschedule: %v", err)
		}

		q.mu.Lock()
		delay := time.Until(q.rescheduledAfter[j.ID])
		q.mu.Unlock()

		if skips > 0 && delay <= previous && previous < maxSkipBackoff {
			t.Errorf("skips=%d gave a delay of %v, not longer than the previous %v", skips, delay, previous)
		}
		if delay > maxSkipBackoff+time.Second {
			t.Errorf("skips=%d gave %v, above the %v cap", skips, delay, maxSkipBackoff)
		}
		previous = delay
	}
}

// A deferred job goes back to the queue until the time it was given - neither
// failed nor skipped - with its reason as the note, and a wrapper around a
// handler can defer without the handler knowing.
func TestADeferredJobWaitsWithoutFailing(t *testing.T) {
	j := job(domain.JobEnrich)
	q := newFakeQueue(j)
	until := time.Now().Add(time.Hour).Truncate(time.Second)
	ran := false
	inner := func(context.Context, postgres.Job, ProgressFunc) error {
		ran = true
		return nil
	}
	// An installation's own rule, decided before the handler runs.
	mustWait := func(postgres.Job) bool { return true }
	wrap := func(h Handler) Handler {
		return func(ctx context.Context, job postgres.Job, report ProgressFunc) error {
			if mustWait(job) {
				return fmt.Errorf("checking turns: %w", Defer(until, "waiting its turn"))
			}
			return h(ctx, job, report)
		}
	}

	runPool(t, q, map[domain.JobType]Handler{domain.JobEnrich: wrap(inner)}, 1)

	if ran {
		t.Error("the wrapped handler ran")
	}
	succeeded, rescheduled, failure := q.outcome(j.ID)
	if succeeded || rescheduled != "" || failure != nil {
		t.Errorf("succeeded %v, skipped %q, failed %v; want only deferred", succeeded, rescheduled, failure)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.deferred[j.ID] != "waiting its turn" || !q.deferredUntil[j.ID].Equal(until) {
		t.Errorf("deferred %q until %v, want the reason and %v", q.deferred[j.ID], q.deferredUntil[j.ID], until)
	}
}

// A deferral is held between a second and two months from now: a time
// already past would spin the job through the queue, and a mistaken far one
// would park it where nobody sees it run.
func TestADeferralIsBounded(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct{ until, want time.Time }{
		"zero":        {time.Time{}, now.Add(minDeferral)},
		"past":        {now.Add(-time.Hour), now.Add(minDeferral)},
		"now":         {now, now.Add(minDeferral)},
		"an hour":     {now.Add(time.Hour), now.Add(time.Hour)},
		"far future":  {now.AddDate(10, 0, 0), now.Add(maxDeferral)},
		"just inside": {now.Add(maxDeferral - time.Minute), now.Add(maxDeferral - time.Minute)},
	} {
		if got := deferralUntil(now, tc.until); !got.Equal(tc.want) {
			t.Errorf("%s: deferralUntil = %v, want %v", name, got, tc.want)
		}
	}
}

// A deferral that cannot be recorded at once is tried again, so the job is
// not left running for the reclaimer to count against it - and never as a
// skip, which would count toward a limit meant for a model that is not there.
func TestADeferralThatCannotBeRecordedIsTriedAgain(t *testing.T) {
	j := job(domain.JobEnrich)
	q := newFakeQueue(j)
	q.deferFailures = deferAttempts - 1
	until := time.Now().Add(time.Hour)
	runPool(t, q, map[domain.JobType]Handler{
		domain.JobEnrich: func(context.Context, postgres.Job, ProgressFunc) error {
			return Defer(until, "waiting its turn")
		},
	}, 1)
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.deferred[j.ID] != "waiting its turn" || !q.deferredUntil[j.ID].Equal(until) {
		t.Errorf("deferred %q until %v; want the reason, until %v", q.deferred[j.ID], q.deferredUntil[j.ID], until)
	}
	if len(q.rescheduled) != 0 || len(q.failed) != 0 {
		t.Errorf("skipped %v, failed %v; want neither", q.rescheduled, q.failed)
	}
}

// Every pool names its workers apart from every other, so a worker of one
// process can never pass for a worker of another sharing the database.
func TestTwoPoolsNeverShareWorkerIDs(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		pool := New(newFakeQueue(), nil, Config{Workers: 4, Logger: quietLogger()})
		for _, id := range pool.workerIDs() {
			if seen[id] {
				t.Fatalf("worker id %q used twice", id)
			}
			seen[id] = true
		}
	}
}

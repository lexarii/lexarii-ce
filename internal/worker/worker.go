// SPDX-License-Identifier: AGPL-3.0-or-later

// Package worker drains the job queue.
//
// Workers are deliberately dumb: claim a job, run its handler, record the
// outcome. All the resumability lives in the queue and in the handlers being
// idempotent, which is what lets a worker be killed at any moment without
// losing or duplicating work.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/llm"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// Queue is the slice of the job store a worker pool needs.
//
// Declared here, by the consumer, rather than in the store: it keeps the pool
// testable with a small fake and stops the worker package depending on more of
// the store than it actually uses.
type Queue interface {
	Claim(ctx context.Context, workerID string) (postgres.Job, error)
	// Each change to a claimed job names the worker that claimed it, and
	// does nothing when that worker no longer holds it.
	Progress(ctx context.Context, id uuid.UUID, worker string, fraction float32, note string) error
	Succeed(ctx context.Context, id uuid.UUID, worker string) error
	Fail(ctx context.Context, id uuid.UUID, worker string, cause error) error
	// RescheduleSkipped returns a job to the queue without consuming an
	// attempt, giving up once it has been skipped maxSkips times.
	RescheduleSkipped(ctx context.Context, id uuid.UUID, worker string, after time.Time, note string, maxSkips int) error
	// Defer returns a job to the queue until a given time, without consuming
	// an attempt or counting as a skip.
	Defer(ctx context.Context, id uuid.UUID, worker string, until time.Time, note string) error
	ReclaimStale(ctx context.Context, olderThan time.Duration) (int64, error)
}

// Handler runs one job. It must be idempotent: a handler can be interrupted at
// any point and run again from the start.
type Handler func(ctx context.Context, job postgres.Job, report ProgressFunc) error

// ProgressFunc reports how far a job has got. Calling it is optional but keeps
// the UI honest on long work, and it refreshes the job's lock so a slow job is
// not mistaken for an abandoned one.
type ProgressFunc func(fraction float32, note string)

// maxSkips is how many times a job may be skipped before it is given up on.
//
// Generous, because the case it exists for is a model server that is off for a
// weekend: with the backoff below, this is several days of trying. What it
// stops is the other case - a job that will never succeed, skipping forever and
// occupying a worker every time.
const maxSkips = 40

// maxSkipBackoff caps the wait between skips.
const maxSkipBackoff = 15 * time.Minute

// ErrSkipped tells the pool a job could not run for an expected, temporary
// reason - most often that the model server is switched off.
//
// A skipped job is rescheduled without consuming an attempt. A library of 5000
// books must not end up with 5000 permanently failed enrichment jobs because
// the GPU box was off overnight.
var ErrSkipped = errors.New("job skipped")

// DeferredError tells the pool a job is not to run yet: it goes back to the
// queue, pending, until Until, without consuming an attempt and without
// counting as a skip. Nothing is wrong with it - it simply has to wait.
//
// For a decision made outside the job itself. An installation can wrap the
// handlers it passes to New and have the wrapper return one - work for one
// person waiting until their share of the model's time comes round again,
// say - with no change to any handler. A skip is the wrong tool for that: it
// backs off on its own clock and gives up after maxSkips, while a deferral
// waits exactly as long as it is told and never gives up by itself.
//
// Reason becomes the job's progress note, which whoever may follow the job
// sees, so it should be neutral and say nothing about anybody else's work.
type DeferredError struct {
	Until  time.Time
	Reason string
}

func (e *DeferredError) Error() string {
	if e.Reason == "" {
		return "job deferred until " + e.Until.Format(time.RFC3339)
	}
	return "job deferred until " + e.Until.Format(time.RFC3339) + ": " + e.Reason
}

// Defer is the error a handler returns to put its job back until a given
// time. A time already past puts it back to run as soon as a worker is free.
func Defer(until time.Time, reason string) error {
	return &DeferredError{Until: until, Reason: reason}
}

// Config configures a Pool.
type Config struct {
	// Workers is the number of concurrent workers. Defaults to 4.
	Workers int
	// PollInterval is how long a worker waits after finding an empty queue.
	// Defaults to 2 seconds.
	PollInterval time.Duration
	// StaleAfter is how long a running job may go without a progress report
	// before it is presumed abandoned. Defaults to 5 minutes.
	StaleAfter time.Duration
	// ReclaimInterval is how often to sweep for abandoned jobs. Defaults to 1 minute.
	ReclaimInterval time.Duration

	// Archive expires the records of books whose owner deleted their account.
	//
	// Optional: with no archive keeper the sweep does not run, which is the
	// right behaviour for anything embedding the pool without that policy.
	Archive ArchiveKeeper
	// ArchiveRetention is how long an archived book waits to be claimed before
	// its record is deleted. The FILE is not deleted by this, unless
	// EraseExpired.
	ArchiveRetention time.Duration
	// EraseExpired owes the disk an expired record's stored copies, for an
	// installation that erases what departed people leave
	// (LEXARII_DELETED_ACCOUNT_FILES=erase). Never a file in the library
	// directory.
	EraseExpired bool
	// ArchiveSweepInterval is how often to look. Defaults to 24 hours: this is
	// a six-month deadline, and checking it more often than daily is noise.
	ArchiveSweepInterval time.Duration

	// Jobs deletes finished jobs once they are JobRetention old. A finished
	// job is never run again, and an upload's still names the file somebody
	// uploaded. Optional: with none the sweep does not run.
	Jobs JobExpirer
	// JobRetention is how long a finished job is kept. Zero means
	// domain.DefaultJobRetention; anything shorter than
	// domain.MinJobRetention is raised to it.
	JobRetention time.Duration
	// JobSweepInterval is how often to look, starting at once. Defaults to an
	// hour.
	JobSweepInterval time.Duration

	// Authors normalises author rows: prunes names that are not people, merges
	// the ones that are the same person. Optional; with none configured the
	// sweep does not run.
	Authors AuthorNormaliser
	// AuthorSweepInterval is how often to normalise. Defaults to 15 minutes.
	AuthorSweepInterval time.Duration
	// AuthorThreshold is the trigram similarity below which two names are not
	// even considered. It does not decide anything - see AuthorNormaliser.
	AuthorThreshold float64

	Logger *slog.Logger
}

// AuthorNormaliser tidies the author rows a scan produced.
//
// Deliberately narrow. The rules behind it are deterministic - a name is a
// person or it is not, two names are the same person or they are not, and two
// people who co-wrote a book are never merged - which is what makes running it
// unattended reasonable. Nothing here consults a model.
type AuthorNormaliser interface {
	NormaliseAuthors(ctx context.Context, threshold float64, apply bool) (postgres.AuthorNormalisation, error)
	CountAuthors(ctx context.Context) (int, error)
}

// ArchiveKeeper expires the records of books left behind by deleted accounts.
type ArchiveKeeper interface {
	// ExpireArchived deletes the ROWS of books archived before this time, and
	// returns where their files are. The files are left alone, unless erase:
	// then their stored copies are owed the disk, to be deleted.
	ExpireArchived(ctx context.Context, olderThan time.Time, erase bool) ([]string, error)
}

// JobExpirer deletes finished jobs.
type JobExpirer interface {
	// DeleteFinishedJobs deletes the jobs that finished before this time and
	// reports how many. Pending and running jobs are never touched.
	DeleteFinishedJobs(ctx context.Context, before time.Time) (int64, error)
}

// Pool runs handlers against the job queue.
type Pool struct {
	queue    Queue
	handlers map[domain.JobType]Handler
	cfg      Config
	log      *slog.Logger

	// instance names this pool among every pool on every machine sharing
	// the database: see workerIDs.
	instance string

	mu      sync.RWMutex
	running map[uuid.UUID]string // job id -> worker, for diagnostics
}

// instanceName is the host, the process and a random part. A worker's id is
// what the queue's guards check a job's holder by (locked_by), so two
// processes sharing one database - two containers, or the old and new
// binaries of a rolling restart - must never both have a "worker-0": the one
// coming back late from a reclaimed job would pass for the one now running
// it. Host and pid say where a job is when somebody reads the queue; the
// random part makes two pools collide only by astronomical chance, even in
// one process.
func instanceName() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "host"
	}
	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), uuid.NewString()[:8])
}

// workerIDs names a pool's workers, each unique to this pool.
func (p *Pool) workerIDs() []string {
	ids := make([]string, p.cfg.Workers)
	for i := range ids {
		ids[i] = fmt.Sprintf("%s/worker-%d", p.instance, i)
	}
	return ids
}

// New builds a worker pool.
func New(queue Queue, handlers map[domain.JobType]Handler, cfg Config) *Pool {
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = 5 * time.Minute
	}
	if cfg.ReclaimInterval <= 0 {
		cfg.ReclaimInterval = time.Minute
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}

	return &Pool{
		queue:    queue,
		handlers: handlers,
		cfg:      cfg,
		log:      log,
		instance: instanceName(),
		running:  make(map[uuid.UUID]string),
	}
}

// Run drains the queue until the context is cancelled.
//
// It returns nil on a clean shutdown: cancellation is how the pool is stopped,
// not an error.
func (p *Pool) Run(ctx context.Context) error {
	g, ctx := errgroup.WithContext(ctx)

	for _, id := range p.workerIDs() {
		g.Go(func() error { return p.work(ctx, id) })
	}
	g.Go(func() error { return p.reclaim(ctx) })
	if p.cfg.Archive != nil {
		g.Go(func() error { return p.expireArchives(ctx) })
	}
	if p.cfg.Authors != nil {
		g.Go(func() error { return p.normaliseAuthors(ctx) })
	}
	if p.cfg.Jobs != nil {
		g.Go(func() error { return p.expireJobs(ctx) })
	}

	err := g.Wait()
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}

// work is one worker's claim-run-record loop.
func (p *Pool) work(ctx context.Context, workerID string) error {
	log := p.log.With("worker", workerID)
	timer := time.NewTimer(p.cfg.PollInterval)
	defer timer.Stop()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		job, err := p.queue.Claim(ctx, workerID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			// Empty queue. Wait, then look again.
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(p.cfg.PollInterval)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				continue
			}
		case err != nil:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// A database blip must not kill the worker; back off and retry.
			log.Warn("claiming job failed", "error", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(p.cfg.PollInterval):
				continue
			}
		}

		p.runJob(ctx, workerID, job)
	}
}

// runJob executes one job and records the outcome.
func (p *Pool) runJob(ctx context.Context, workerID string, job postgres.Job) {
	log := p.log.With("worker", workerID, "job", job.ID, "type", job.Type)

	p.mu.Lock()
	p.running[job.ID] = workerID
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.running, job.ID)
		p.mu.Unlock()
	}()

	handler, ok := p.handlers[job.Type]
	if !ok {
		// An unregistered type is a wiring bug, not a transient fault, so it is
		// failed rather than retried forever.
		log.Error("no handler registered")
		p.fail(ctx, job.ID, workerID, fmt.Errorf("no handler for job type %q", job.Type))
		return
	}

	report := func(fraction float32, note string) {
		// Progress failures are not worth failing a job over, but they are worth
		// knowing about: without them the job looks abandoned to the reclaimer.
		if err := p.queue.Progress(ctx, job.ID, workerID, fraction, note); err != nil && ctx.Err() == nil {
			log.Warn("reporting progress failed", "error", err)
		}
	}

	start := time.Now()
	err := runHandler(ctx, handler, job, report)
	elapsed := time.Since(start)

	var deferred *DeferredError
	switch {
	case err == nil:
		if err := p.queue.Succeed(ctx, job.ID, workerID); err != nil && ctx.Err() == nil {
			log.Error("recording success failed", "error", err)
		}
		log.Info("job finished", "durationMs", elapsed.Milliseconds())

	case errors.As(err, &deferred):
		// Waiting is not failing: back to the queue until the time given,
		// with the attempt it took given back.
		if ferr := p.deferJob(ctx, job, workerID, deferred); ferr != nil && ctx.Err() == nil {
			log.Error("deferring job failed", "error", ferr)
		}
		log.Info("job deferred", "until", deferred.Until, "reason", deferred.Reason)

	case errors.Is(err, ErrSkipped):
		// Rescheduled without consuming an attempt, so a model server that is
		// off overnight does not permanently fail the whole queue.
		if ferr := p.reschedule(ctx, job, workerID, err); ferr != nil && ctx.Err() == nil {
			log.Error("rescheduling skipped job failed", "error", ferr)
		}
		log.Info("job skipped", "reason", err)

	case ctx.Err() != nil:
		// Shutting down. Leave the job running: the reclaimer will return it to
		// the queue, and handlers are idempotent so re-running is safe.
		log.Info("job interrupted by shutdown")

	default:
		log.Warn("job failed", "error", err, "attempt", job.Attempts, "maxAttempts", job.MaxAttempts)
		p.fail(ctx, job.ID, workerID, err)
	}
}

// runHandler isolates a handler panic so one bad book cannot take down a worker.
func runHandler(ctx context.Context, h Handler, job postgres.Job, report ProgressFunc) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("handler panicked: %v", v)
		}
	}()
	return h(ctx, job, report)
}

func (p *Pool) fail(ctx context.Context, id uuid.UUID, workerID string, cause error) {
	// Use a detached context: the job context may already be cancelled, and the
	// failure still needs recording.
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := p.queue.Fail(recordCtx, id, workerID, cause); err != nil {
		p.log.Error("recording failure failed", "job", id, "error", err)
	}
}

// A deferral waits at least minDeferral - a time already past would have the
// job claimed again at once, in a loop as fast as the queue can turn - and at
// most maxDeferral, so a wrapper's mistake (a zero year, a time far in the
// future) cannot park a job where nobody will ever see it run. Two months is
// longer than any per-person schedule should need.
const (
	minDeferral = time.Second
	maxDeferral = 62 * 24 * time.Hour
)

// deferralUntil is when a deferred job may run again: the time asked for,
// held between minDeferral and maxDeferral from now.
func deferralUntil(now, until time.Time) time.Time {
	if earliest := now.Add(minDeferral); until.Before(earliest) {
		return earliest
	}
	if latest := now.Add(maxDeferral); until.After(latest) {
		return latest
	}
	return until
}

// deferAttempts is how many times a deferral is tried before the job is left
// to the reclaimer.
const deferAttempts = 3

// deferJob returns a deferred job to the queue until the time it was given.
//
// A deferral that cannot be recorded is tried again, briefly backing off:
// left running, the job would be returned by the reclaimer, which counts the
// attempt and, on the last one, fails a job nothing was wrong with.
// Recording it as a skip instead was the earlier fallback, and it was no
// better: it is a write to the same table, so it fails when this does, and
// when it succeeds it counts toward maxSkips, a limit for a model that is
// not there, not for waiting one's turn. Only when every try fails - the
// database itself is unreachable - is the job left to the reclaimer.
func (p *Pool) deferJob(ctx context.Context, job postgres.Job, workerID string, d *DeferredError) error {
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	until := deferralUntil(time.Now(), d.Until)
	var errs []error
	for try := 0; try < deferAttempts; try++ {
		if try > 0 {
			select {
			case <-recordCtx.Done():
				return errors.Join(append(errs, recordCtx.Err())...)
			case <-time.After(time.Duration(try*try) * 100 * time.Millisecond):
			}
		}
		err := p.queue.Defer(recordCtx, job.ID, workerID, until, d.Reason)
		if err == nil {
			return nil
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// reschedule returns a skipped job to the queue without consuming an attempt.
func (p *Pool) reschedule(ctx context.Context, job postgres.Job, workerID string, cause error) error {
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	// Backing off on each skip, and giving up eventually.
	//
	// A fixed delay meant a book the model consistently could not answer -
	// every request timing out - skipped, waited thirty seconds, and ran again,
	// forever, holding a worker each time. Two such books held two of four
	// workers indefinitely, and nothing said so, because a skip is not a
	// failure.
	//
	// The backoff is capped so that an overnight outage still retries at a
	// sensible interval rather than drifting into hours.
	delay := p.cfg.PollInterval * 15
	for i := 0; i < job.Skips && delay < maxSkipBackoff; i++ {
		delay *= 2
	}
	if delay > maxSkipBackoff {
		delay = maxSkipBackoff
	}

	// A server that said when to come back is a better clock than ours. It is a
	// floor, not a replacement: our backoff still applies when it is longer, and
	// when it is not given nothing changes. Not subject to maxSkipBackoff - a
	// provider asking for an hour should get an hour.
	var httpErr *llm.HTTPError
	if errors.As(cause, &httpErr) && httpErr.RetryAfter > delay {
		delay = httpErr.RetryAfter
	}

	return p.queue.RescheduleSkipped(recordCtx, job.ID, workerID,
		time.Now().Add(delay), cause.Error(), maxSkips)
}

// reclaim periodically returns abandoned jobs to the queue.
func (p *Pool) reclaim(ctx context.Context) error {
	ticker := time.NewTicker(p.cfg.ReclaimInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			n, err := p.queue.ReclaimStale(ctx, p.cfg.StaleAfter)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				p.log.Warn("reclaiming stale jobs failed", "error", err)
				continue
			}
			if n > 0 {
				p.log.Info("returned abandoned jobs to the queue", "count", n)
			}
		}
	}
}

// expireArchives deletes the records of books nobody claimed.
//
// The row goes and the file stays. A catalogue entry naming somebody who
// deleted their account is a record about a person and should not be kept
// forever; the book is a file on a disk that costs nothing to leave alone and
// cannot be recovered once deleted. Where those files are is logged, because
// once the row is gone nothing in the database knows.
func (p *Pool) expireArchives(ctx context.Context) error {
	interval := p.cfg.ArchiveSweepInterval
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	retention := p.cfg.ArchiveRetention
	if retention <= 0 {
		// Belt and braces: a zero retention would expire everything the moment
		// it was archived, which is the opposite of the point.
		retention = 183 * 24 * time.Hour
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			paths, err := p.cfg.Archive.ExpireArchived(ctx, time.Now().Add(-retention), p.cfg.EraseExpired)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				p.log.Warn("expiring archived books failed", "error", err)
				continue
			}
			for _, path := range paths {
				if p.cfg.EraseExpired {
					p.log.Info("archived book expired and erased",
						"path", path, "retention", retention,
						"note", "its stored copies are deleted; a file in the library directory is kept")
					continue
				}
				p.log.Info("archived book expired from the catalogue",
					"path", path, "retention", retention,
					"note", "the file is kept; only the record was deleted")
			}
		}
	}
}

// expireJobs deletes finished jobs older than the retention: once at start,
// so a server restarted more often than the interval still sweeps, and then
// every interval.
func (p *Pool) expireJobs(ctx context.Context) error {
	interval := p.cfg.JobSweepInterval
	if interval <= 0 {
		interval = time.Hour
	}
	retention := jobRetention(p.cfg.JobRetention)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		n, err := p.cfg.Jobs.DeleteFinishedJobs(ctx, time.Now().Add(-retention))
		switch {
		case err != nil && ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			p.log.Warn("deleting finished jobs failed", "error", err)
		case n > 0:
			p.log.Info("deleted finished jobs", "count", n, "retention", retention)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// jobRetention applies JobRetention's default and floor.
func jobRetention(configured time.Duration) time.Duration {
	switch {
	case configured <= 0:
		return domain.DefaultJobRetention
	case configured < domain.MinJobRetention:
		return domain.MinJobRetention
	}
	return configured
}

// normaliseAuthors tidies author rows as books arrive.
//
// A sweep rather than a step at the end of a scan, because a scan does not
// produce authors: it enqueues extraction, and the authors appear as those jobs
// run, minutes later. Hanging this off the scan would normalise the catalogue
// as it was BEFORE the new books landed - correct-looking, and always one scan
// behind.
//
// It also covers books that arrive by upload, which never involve a scan at
// all, and it is idempotent: with nothing to do it is two queries that return
// nothing.
func (p *Pool) normaliseAuthors(ctx context.Context) error {
	interval := p.cfg.AuthorSweepInterval
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	threshold := p.cfg.AuthorThreshold
	if threshold <= 0 {
		threshold = 0.35
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			// The sweep compares every author with every other, which is fine
			// at the size of a personal library and quadratic above it. It
			// declines rather than spending tens of seconds every quarter of
			// an hour on a machine that is also serving reads; `make authors`
			// still does the whole thing for somebody who asked.
			count, err := p.cfg.Authors.CountAuthors(ctx)
			if err == nil && count > postgres.MaxSweepAuthors {
				p.log.Warn("skipping the author sweep: too many to compare pairwise",
					"authors", count, "limit", postgres.MaxSweepAuthors,
					"run", "make authors")
				continue
			}

			out, err := p.cfg.Authors.NormaliseAuthors(ctx, threshold, true)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				p.log.Warn("normalising authors failed", "error", err)
				continue
			}
			for _, im := range out.Pruned {
				p.log.Info("author was not a person",
					"name", im.Name, "books", im.Books, "action", "detached")
			}
			for _, pair := range out.Merged {
				p.log.Info("authors merged",
					"kept", pair.KeepName, "merged", pair.DropName,
					"similarity", pair.Similarity)
			}
		}
	}
}

// Running reports the jobs currently being executed by this pool.
func (p *Pool) Running() map[uuid.UUID]string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	out := make(map[uuid.UUID]string, len(p.running))
	for id, worker := range p.running {
		out[id] = worker
	}
	return out
}

// SPDX-License-Identifier: AGPL-3.0-or-later

package worker

import (
	"context"
	"errors"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// lifecycleLogs is a slog handler that keeps every record, so a test can tell
// that a failure the pool deliberately survives was at least reported.
type lifecycleLogs struct {
	mu      *sync.Mutex
	records *[]lifecycleLogRecord
	attrs   []slog.Attr
}

type lifecycleLogRecord struct {
	level slog.Level
	msg   string
	attrs map[string]any
}

func newLifecycleLogs() *lifecycleLogs {
	return &lifecycleLogs{mu: &sync.Mutex{}, records: &[]lifecycleLogRecord{}}
}

func (h *lifecycleLogs) Enabled(context.Context, slog.Level) bool { return true }

func (h *lifecycleLogs) Handle(_ context.Context, r slog.Record) error {
	attrs := map[string]any{}
	for _, a := range h.attrs {
		attrs[a.Key] = a.Value.Any()
	}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.records = append(*h.records, lifecycleLogRecord{level: r.Level, msg: r.Message, attrs: attrs})
	return nil
}

func (h *lifecycleLogs) WithAttrs(as []slog.Attr) slog.Handler {
	return &lifecycleLogs{mu: h.mu, records: h.records, attrs: append(append([]slog.Attr{}, h.attrs...), as...)}
}

func (h *lifecycleLogs) WithGroup(string) slog.Handler { return h }

func (h *lifecycleLogs) logger() *slog.Logger { return slog.New(h) }

// find returns the records with this message.
func (h *lifecycleLogs) find(msg string) []lifecycleLogRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []lifecycleLogRecord
	for _, r := range *h.records {
		if r.msg == msg {
			out = append(out, r)
		}
	}
	return out
}

// waitForLifecycle polls until cond holds, failing the test after a generous
// deadline. Polling on an event rather than sleeping a fixed time.
func waitForLifecycle(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// flakyQueue is fakeQueue with a database that fails on demand.
type flakyQueue struct {
	*fakeQueue

	fmu sync.Mutex

	claimErrs  int
	claimHook  func()
	claimCalls int

	progressErr   error
	succeedErr    error
	failErr       error
	rescheduleErr error

	succeedCalls    int
	failCalls       int
	rescheduleCalls int

	reclaimResults []reclaimResult
	reclaimAges    []time.Duration
	reclaimHook    func()
}

type reclaimResult struct {
	n   int64
	err error
}

var errLifecycleDB = errors.New("database unavailable")

func (q *flakyQueue) Claim(ctx context.Context, worker string) (postgres.Job, error) {
	q.fmu.Lock()
	q.claimCalls++
	hook := q.claimHook
	fail := q.claimErrs > 0
	if fail {
		q.claimErrs--
	}
	q.fmu.Unlock()
	if hook != nil {
		hook()
	}
	if fail {
		return postgres.Job{}, errLifecycleDB
	}
	return q.fakeQueue.Claim(ctx, worker)
}

func (q *flakyQueue) Progress(ctx context.Context, id uuid.UUID, worker string, fraction float32, note string) error {
	if q.progressErr != nil {
		return q.progressErr
	}
	return q.fakeQueue.Progress(ctx, id, worker, fraction, note)
}

func (q *flakyQueue) Succeed(ctx context.Context, id uuid.UUID, worker string) error {
	q.fmu.Lock()
	q.succeedCalls++
	q.fmu.Unlock()
	if q.succeedErr != nil {
		return q.succeedErr
	}
	return q.fakeQueue.Succeed(ctx, id, worker)
}

func (q *flakyQueue) Fail(ctx context.Context, id uuid.UUID, worker string, cause error) error {
	q.fmu.Lock()
	q.failCalls++
	q.fmu.Unlock()
	if q.failErr != nil {
		return q.failErr
	}
	return q.fakeQueue.Fail(ctx, id, worker, cause)
}

func (q *flakyQueue) RescheduleSkipped(ctx context.Context, id uuid.UUID, worker string, after time.Time, note string, maxSkips int) error {
	q.fmu.Lock()
	q.rescheduleCalls++
	q.fmu.Unlock()
	if q.rescheduleErr != nil {
		return q.rescheduleErr
	}
	return q.fakeQueue.RescheduleSkipped(ctx, id, worker, after, note, maxSkips)
}

func (q *flakyQueue) ReclaimStale(_ context.Context, olderThan time.Duration) (int64, error) {
	q.fmu.Lock()
	defer q.fmu.Unlock()
	q.reclaimAges = append(q.reclaimAges, olderThan)
	if q.reclaimHook != nil {
		q.reclaimHook()
	}
	if len(q.reclaimResults) == 0 {
		return 0, nil
	}
	r := q.reclaimResults[0]
	q.reclaimResults = q.reclaimResults[1:]
	return r.n, r.err
}

func (q *flakyQueue) counts() (claims, succeeds, fails, reschedules, reclaims int) {
	q.fmu.Lock()
	defer q.fmu.Unlock()
	return q.claimCalls, q.succeedCalls, q.failCalls, q.rescheduleCalls, len(q.reclaimAges)
}

// startLifecyclePool runs a pool in the background and returns a stop
// function that cancels it and asserts a clean shutdown.
func startLifecyclePool(t *testing.T, q Queue, handlers map[domain.JobType]Handler, cfg Config) (*Pool, func()) {
	t.Helper()
	if cfg.PollInterval == 0 {
		cfg.PollInterval = time.Millisecond
	}
	if cfg.ReclaimInterval == 0 {
		cfg.ReclaimInterval = time.Hour
	}
	pool := New(q, handlers, cfg)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- pool.Run(ctx) }()

	stopped := false
	stop := func() {
		t.Helper()
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run = %v, want nil on cancellation", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the pool did not stop after cancellation")
		}
	}
	t.Cleanup(stop)
	return pool, stop
}

func TestAClaimFailureIsRiddenOut(t *testing.T) {
	j := job(domain.JobScan)
	q := &flakyQueue{fakeQueue: newFakeQueue(j), claimErrs: 2}
	logs := newLifecycleLogs()

	_, stop := startLifecyclePool(t, q, map[domain.JobType]Handler{
		domain.JobScan: func(context.Context, postgres.Job, ProgressFunc) error { return nil },
	}, Config{Workers: 1, Logger: logs.logger()})

	waitForLifecycle(t, "the job to succeed after the database came back", func() bool {
		ok, _, _ := q.outcome(j.ID)
		return ok
	})
	stop()

	if n := len(logs.find("claiming job failed")); n != 2 {
		t.Errorf("claim failures logged = %d, want 2", n)
	}
}

// A claim that fails because the pool is shutting down is shutdown, not a
// database fault worth a warning.
func TestAClaimFailingDuringShutdownIsQuiet(t *testing.T) {
	q := &flakyQueue{fakeQueue: newFakeQueue(), claimErrs: 1}
	logs := newLifecycleLogs()

	pool := New(q, nil, Config{Workers: 1, PollInterval: time.Millisecond, ReclaimInterval: time.Hour, Logger: logs.logger()})
	ctx, cancel := context.WithCancel(t.Context())
	q.claimHook = cancel

	if err := pool.Run(ctx); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if n := len(logs.find("claiming job failed")); n != 0 {
		t.Errorf("a claim cut short by shutdown was logged as a failure %d times", n)
	}
}

// When the queue cannot record an outcome the pool says so and carries on;
// the job is left for the reclaimer, never recorded as something else.
func TestRecordingFailuresAreReportedAndSurvived(t *testing.T) {
	tests := []struct {
		name    string
		break_  func(q *flakyQueue)
		handler Handler
		logged  string
		check   func(t *testing.T, q *flakyQueue, id uuid.UUID)
	}{
		{
			name:    "success",
			break_:  func(q *flakyQueue) { q.succeedErr = errLifecycleDB },
			handler: func(context.Context, postgres.Job, ProgressFunc) error { return nil },
			logged:  "recording success failed",
			check: func(t *testing.T, q *flakyQueue, id uuid.UUID) {
				if _, _, failure := q.outcome(id); failure != nil {
					t.Errorf("a job that succeeded was failed: %v", failure)
				}
			},
		},
		{
			name:   "progress",
			break_: func(q *flakyQueue) { q.progressErr = errLifecycleDB },
			handler: func(_ context.Context, _ postgres.Job, report ProgressFunc) error {
				report(0.5, "half way")
				return nil
			},
			logged: "reporting progress failed",
			check: func(t *testing.T, q *flakyQueue, id uuid.UUID) {
				waitForLifecycle(t, "the job to succeed", func() bool {
					ok, _, _ := q.outcome(id)
					return ok
				})
			},
		},
		{
			name:    "a skip",
			break_:  func(q *flakyQueue) { q.rescheduleErr = errLifecycleDB },
			handler: func(context.Context, postgres.Job, ProgressFunc) error { return ErrSkipped },
			logged:  "rescheduling skipped job failed",
			check: func(t *testing.T, q *flakyQueue, id uuid.UUID) {
				if _, _, failure := q.outcome(id); failure != nil {
					t.Errorf("a skipped job was failed: %v", failure)
				}
			},
		},
		{
			name:    "a failure",
			break_:  func(q *flakyQueue) { q.failErr = errLifecycleDB },
			handler: func(context.Context, postgres.Job, ProgressFunc) error { return errors.New("bad file") },
			logged:  "recording failure failed",
			check: func(t *testing.T, q *flakyQueue, id uuid.UUID) {
				if _, note, _ := q.outcome(id); note != "" {
					t.Errorf("a failed job was rescheduled as a skip: %q", note)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			j := job(domain.JobScan)
			q := &flakyQueue{fakeQueue: newFakeQueue(j)}
			tc.break_(q)
			logs := newLifecycleLogs()

			_, stop := startLifecyclePool(t, q, map[domain.JobType]Handler{domain.JobScan: tc.handler},
				Config{Workers: 1, Logger: logs.logger()})

			waitForLifecycle(t, "the failure to be logged", func() bool { return len(logs.find(tc.logged)) > 0 })
			tc.check(t, q, j.ID)
			stop()

			for _, r := range logs.find(tc.logged) {
				if err, _ := r.attrs["error"].(error); !errors.Is(err, errLifecycleDB) {
					t.Errorf("logged error = %v, want the database's", r.attrs["error"])
				}
			}
		})
	}
}

// Every try at recording a deferral failing - the database is gone - leaves
// the job to the reclaimer: not failed, not counted as a skip.
func TestADeferralNobodyCanRecordIsLeftToTheReclaimer(t *testing.T) {
	j := job(domain.JobEnrich)
	fq := newFakeQueue(j)
	fq.deferFailures = deferAttempts
	q := &flakyQueue{fakeQueue: fq}
	logs := newLifecycleLogs()

	_, stop := startLifecyclePool(t, q, map[domain.JobType]Handler{
		domain.JobEnrich: func(context.Context, postgres.Job, ProgressFunc) error {
			return Defer(time.Now().Add(time.Hour), "waiting its turn")
		},
	}, Config{Workers: 1, Logger: logs.logger()})

	waitForLifecycle(t, "the deferral to be given up on", func() bool {
		return len(logs.find("deferring job failed")) > 0
	})
	stop()

	q.mu.Lock()
	defer q.mu.Unlock()
	if q.deferFailures != 0 {
		t.Errorf("%d tries were left untried", q.deferFailures)
	}
	if _, ok := q.deferred[j.ID]; ok {
		t.Error("the deferral was recorded")
	}
	if _, ok := q.failed[j.ID]; ok {
		t.Error("a job that was only waiting was failed")
	}
	if _, ok := q.rescheduled[j.ID]; ok {
		t.Error("a deferral fell back to a skip, which counts toward maxSkips")
	}
}

func TestDeferredErrorIsFoundAndSaysWhen(t *testing.T) {
	until := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	var d *DeferredError
	if err := Defer(until, "your turn comes round at noon"); !errors.As(err, &d) {
		t.Fatalf("Defer returned %T, want *DeferredError", err)
	}
	if !d.Until.Equal(until) || d.Reason != "your turn comes round at noon" {
		t.Errorf("deferral = %+v", d)
	}
	for _, err := range []error{Defer(until, "a reason"), Defer(until, "")} {
		if !strings.Contains(err.Error(), until.Format(time.RFC3339)) {
			t.Errorf("Error() = %q, want the time it waits until", err.Error())
		}
	}
	if msg := Defer(until, "").Error(); strings.HasSuffix(msg, ": ") || strings.HasSuffix(msg, ":") {
		t.Errorf("Error() = %q dangles a separator with no reason", msg)
	}
}

func TestTheReclaimerReportsAndSurvivesErrors(t *testing.T) {
	q := &flakyQueue{fakeQueue: newFakeQueue(), reclaimResults: []reclaimResult{
		{n: 2},
		{err: errLifecycleDB},
		{n: 0},
	}}
	logs := newLifecycleLogs()

	_, stop := startLifecyclePool(t, q, nil, Config{
		Workers: 1, ReclaimInterval: time.Millisecond, StaleAfter: 42 * time.Second, Logger: logs.logger(),
	})
	waitForLifecycle(t, "the reclaimer to keep going after an error", func() bool {
		_, _, _, _, reclaims := q.counts()
		return reclaims >= 3
	})
	stop()

	returned := logs.find("returned abandoned jobs to the queue")
	if len(returned) != 1 || returned[0].attrs["count"] != int64(2) {
		t.Errorf("reclaim reports = %+v, want one naming 2 jobs", returned)
	}
	if len(logs.find("reclaiming stale jobs failed")) != 1 {
		t.Error("the reclaim failure was not reported")
	}
	q.fmu.Lock()
	defer q.fmu.Unlock()
	for _, age := range q.reclaimAges {
		if age != 42*time.Second {
			t.Errorf("reclaimed jobs older than %v, want StaleAfter", age)
		}
	}
}

func TestStaleAfterDefaultsToFiveMinutes(t *testing.T) {
	q := &flakyQueue{fakeQueue: newFakeQueue()}
	_, stop := startLifecyclePool(t, q, nil, Config{Workers: 1, ReclaimInterval: time.Millisecond, Logger: quietLogger()})
	waitForLifecycle(t, "a reclaim", func() bool {
		_, _, _, _, reclaims := q.counts()
		return reclaims >= 1
	})
	stop()
	q.fmu.Lock()
	defer q.fmu.Unlock()
	if q.reclaimAges[0] != 5*time.Minute {
		t.Errorf("stale after %v, want 5m", q.reclaimAges[0])
	}
}

// fakeArchiveKeeper records the cut-offs it is asked about.
type fakeArchiveKeeper struct {
	mu      sync.Mutex
	hook    func()
	asked   []time.Time
	erased  []bool
	results []archiveResult
}

type archiveResult struct {
	paths []string
	err   error
}

func (k *fakeArchiveKeeper) ExpireArchived(_ context.Context, olderThan time.Time, erase bool) ([]string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.asked = append(k.asked, olderThan)
	k.erased = append(k.erased, erase)
	if k.hook != nil {
		k.hook()
	}
	if len(k.results) == 0 {
		return nil, nil
	}
	r := k.results[0]
	k.results = k.results[1:]
	return r.paths, r.err
}

func (k *fakeArchiveKeeper) calls() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.asked)
}

func TestArchiveSweepExpiresRecordsAfterTheRetention(t *testing.T) {
	tests := []struct {
		name      string
		retention time.Duration
		want      time.Duration
	}{
		{name: "default is about six months", retention: 0, want: 183 * 24 * time.Hour},
		{name: "configured", retention: 48 * time.Hour, want: 48 * time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			keeper := &fakeArchiveKeeper{results: []archiveResult{
				{paths: []string{"/data/archive/one.epub", "/data/archive/two.pdf"}},
				{err: errLifecycleDB},
			}}
			logs := newLifecycleLogs()

			_, stop := startLifecyclePool(t, newFakeQueue(), nil, Config{
				Workers: 1, Archive: keeper, ArchiveRetention: tc.retention,
				ArchiveSweepInterval: time.Millisecond, Logger: logs.logger(),
			})
			waitForLifecycle(t, "the sweep to run past an error", func() bool { return keeper.calls() >= 3 })
			stop()

			keeper.mu.Lock()
			first := keeper.asked[0]
			keeper.mu.Unlock()
			want := time.Now().Add(-tc.want)
			if d := first.Sub(want); d < -time.Minute || d > time.Minute {
				t.Errorf("expired records archived before %v, want about %v", first, want)
			}

			// Each expired file is named, because once the row is gone nothing
			// else knows where it is. The file itself is never touched.
			var named []string
			for _, r := range logs.find("archived book expired from the catalogue") {
				named = append(named, r.attrs["path"].(string))
			}
			if len(named) != 2 || named[0] != "/data/archive/one.epub" || named[1] != "/data/archive/two.pdf" {
				t.Errorf("logged paths = %v, want both expired files", named)
			}
			if len(logs.find("expiring archived books failed")) != 1 {
				t.Error("the sweep failure was not reported")
			}
		})
	}
}

// On an installation that erases, the sweep says so, and the store owes the
// expired records' stored copies; otherwise it keeps the files.
func TestArchiveSweepErasesWhenTheInstallationSaysSo(t *testing.T) {
	for _, erase := range []bool{false, true} {
		keeper := &fakeArchiveKeeper{results: []archiveResult{{paths: []string{"/data/archive/one.epub"}}}}
		logs := newLifecycleLogs()
		_, stop := startLifecyclePool(t, newFakeQueue(), nil, Config{
			Workers: 1, Archive: keeper, EraseExpired: erase,
			ArchiveSweepInterval: time.Millisecond, Logger: logs.logger(),
		})
		waitForLifecycle(t, "the sweep to run", func() bool { return keeper.calls() >= 2 })
		stop()

		keeper.mu.Lock()
		asked := keeper.erased[0]
		keeper.mu.Unlock()
		if asked != erase {
			t.Errorf("EraseExpired=%v: the store was asked to erase = %v", erase, asked)
		}
		erasedLogs := len(logs.find("archived book expired and erased"))
		if (erasedLogs == 1) != erase {
			t.Errorf("EraseExpired=%v: %d erased-expiry log lines", erase, erasedLogs)
		}
	}
}

// fakeAuthorNormaliser records how it was asked to normalise.
type fakeAuthorNormaliser struct {
	mu         sync.Mutex
	count      int
	countErr   error
	countCalls int
	thresholds []float64
	applied    []bool
	hook       func()
	results    []authorResult
}

type authorResult struct {
	out postgres.AuthorNormalisation
	err error
}

func (a *fakeAuthorNormaliser) CountAuthors(context.Context) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.countCalls++
	return a.count, a.countErr
}

func (a *fakeAuthorNormaliser) NormaliseAuthors(_ context.Context, threshold float64, apply bool) (postgres.AuthorNormalisation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.thresholds = append(a.thresholds, threshold)
	a.applied = append(a.applied, apply)
	if a.hook != nil {
		a.hook()
	}
	if len(a.results) == 0 {
		return postgres.AuthorNormalisation{}, nil
	}
	r := a.results[0]
	a.results = a.results[1:]
	return r.out, r.err
}

func (a *fakeAuthorNormaliser) calls() (counts, normalises int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.countCalls, len(a.thresholds)
}

func TestAuthorSweepNormalisesAndApplies(t *testing.T) {
	tests := []struct {
		name      string
		threshold float64
		countErr  error
		want      float64
	}{
		{name: "default threshold", want: 0.35},
		{name: "configured threshold", threshold: 0.6, want: 0.6},
		// Not knowing how many there are is not a reason to skip the sweep.
		{name: "an uncountable catalogue is still swept", countErr: errLifecycleDB, want: 0.35},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			authors := &fakeAuthorNormaliser{count: 10, countErr: tc.countErr, results: []authorResult{
				{out: postgres.AuthorNormalisation{
					Pruned: []postgres.AuthorImposter{{Name: "Unknown"}},
					Merged: []postgres.AuthorPair{{KeepName: "Dennis Ritchie", DropName: "D. Ritchie"}},
				}},
				{err: errLifecycleDB},
			}}
			logs := newLifecycleLogs()

			_, stop := startLifecyclePool(t, newFakeQueue(), nil, Config{
				Workers: 1, Authors: authors, AuthorThreshold: tc.threshold,
				AuthorSweepInterval: time.Millisecond, Logger: logs.logger(),
			})
			waitForLifecycle(t, "the sweep to run past an error", func() bool {
				_, n := authors.calls()
				return n >= 3
			})
			stop()

			authors.mu.Lock()
			for n := range authors.thresholds {
				if authors.thresholds[n] != tc.want || !authors.applied[n] {
					t.Errorf("normalised with threshold %v apply %v, want %v and applied",
						authors.thresholds[n], authors.applied[n], tc.want)
				}
			}
			authors.mu.Unlock()

			if r := logs.find("author was not a person"); len(r) != 1 || r[0].attrs["name"] != "Unknown" {
				t.Errorf("pruned reports = %+v", r)
			}
			if r := logs.find("authors merged"); len(r) != 1 || r[0].attrs["kept"] != "Dennis Ritchie" {
				t.Errorf("merge reports = %+v", r)
			}
			if len(logs.find("normalising authors failed")) != 1 {
				t.Error("the sweep failure was not reported")
			}
		})
	}
}

// Above the pairwise limit the unattended sweep declines rather than spending
// tens of seconds of a serving machine's time every quarter of an hour.
func TestAuthorSweepDeclinesAHugeCatalogue(t *testing.T) {
	authors := &fakeAuthorNormaliser{count: postgres.MaxSweepAuthors + 1}
	logs := newLifecycleLogs()

	_, stop := startLifecyclePool(t, newFakeQueue(), nil, Config{
		Workers: 1, Authors: authors, AuthorSweepInterval: time.Millisecond, Logger: logs.logger(),
	})
	waitForLifecycle(t, "several sweeps", func() bool {
		counts, _ := authors.calls()
		return counts >= 3
	})
	stop()

	if _, n := authors.calls(); n != 0 {
		t.Errorf("normalised %d times above the limit, want never", n)
	}
	if len(logs.find("skipping the author sweep: too many to compare pairwise")) == 0 {
		t.Error("declining was not reported")
	}
}

func TestRunningShowsWhoIsDoingWhat(t *testing.T) {
	j := job(domain.JobScan)
	q := newFakeQueue(j)
	started := make(chan struct{})
	release := make(chan struct{})

	pool, stop := startLifecyclePool(t, q, map[domain.JobType]Handler{
		domain.JobScan: func(context.Context, postgres.Job, ProgressFunc) error {
			close(started)
			<-release
			return nil
		},
	}, Config{Workers: 2, Logger: quietLogger()})

	<-started
	running := pool.Running()
	worker, ok := running[j.ID]
	if !ok || len(running) != 1 {
		t.Fatalf("Running = %v, want just the job", running)
	}
	if !strings.HasPrefix(worker, pool.instance+"/worker-") {
		t.Errorf("worker id %q does not name this pool", worker)
	}

	// The map handed out is a copy.
	delete(running, j.ID)
	if _, ok := pool.Running()[j.ID]; !ok {
		t.Error("changing the returned map changed the pool's record")
	}

	close(release)
	waitForLifecycle(t, "the job to finish", func() bool { return len(pool.Running()) == 0 })
	stop()
}

// Stopping the pool stops everything it started: workers, the reclaimer and
// both sweeps, with a handler mid-job told to stop too.
func TestShutdownLeavesNoGoroutinesBehind(t *testing.T) {
	before := runtime.NumGoroutine()

	j := job(domain.JobScan)
	started := make(chan struct{})
	sawCancel := make(chan struct{})
	_, stop := startLifecyclePool(t, newFakeQueue(j), map[domain.JobType]Handler{
		domain.JobScan: func(ctx context.Context, _ postgres.Job, _ ProgressFunc) error {
			close(started)
			<-ctx.Done()
			close(sawCancel)
			return ctx.Err()
		},
	}, Config{
		// The sweeps on their default intervals, a day and a quarter of an
		// hour: neither fires during the test, and both must still stop.
		Workers: 3, ReclaimInterval: time.Hour,
		Archive: &fakeArchiveKeeper{}, Authors: &fakeAuthorNormaliser{},
		Logger: quietLogger(),
	})

	<-started
	stop()

	select {
	case <-sawCancel:
	default:
		t.Error("Run returned before the running handler saw the cancellation")
	}
	waitForLifecycle(t, "every pool goroutine to exit", func() bool {
		return runtime.NumGoroutine() <= before
	})
}

// A claim failing while the worker backs off from an earlier one: shutdown
// arriving during the wait ends the worker at once.
func TestShutdownDuringAClaimBackoff(t *testing.T) {
	q := &flakyQueue{fakeQueue: newFakeQueue(), claimErrs: 1}
	logs := newLifecycleLogs()

	_, stop := startLifecyclePool(t, q, nil, Config{Workers: 1, PollInterval: time.Hour, Logger: logs.logger()})
	waitForLifecycle(t, "the claim failure", func() bool { return len(logs.find("claiming job failed")) == 1 })
	stop() // fails the test if the hour-long backoff holds the worker
}

// A sweep whose query fails because the pool is stopping has nothing to
// report: it is shutdown, not a fault.
func TestSweepsFailingAtShutdownAreQuiet(t *testing.T) {
	tests := []struct {
		name   string
		logged string
		config func(cancel func()) (Queue, Config)
	}{
		{
			name:   "reclaimer",
			logged: "reclaiming stale jobs failed",
			config: func(cancel func()) (Queue, Config) {
				q := &flakyQueue{fakeQueue: newFakeQueue(), reclaimHook: cancel,
					reclaimResults: []reclaimResult{{err: errLifecycleDB}}}
				return q, Config{ReclaimInterval: time.Millisecond}
			},
		},
		{
			name:   "archive",
			logged: "expiring archived books failed",
			config: func(cancel func()) (Queue, Config) {
				keeper := &fakeArchiveKeeper{hook: cancel, results: []archiveResult{{err: errLifecycleDB}}}
				return newFakeQueue(), Config{Archive: keeper, ArchiveSweepInterval: time.Millisecond}
			},
		},
		{
			name:   "authors",
			logged: "normalising authors failed",
			config: func(cancel func()) (Queue, Config) {
				authors := &fakeAuthorNormaliser{hook: cancel, results: []authorResult{{err: errLifecycleDB}}}
				return newFakeQueue(), Config{Authors: authors, AuthorSweepInterval: time.Millisecond}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			q, cfg := tc.config(cancel)
			logs := newLifecycleLogs()
			cfg.Workers = 1
			cfg.PollInterval = time.Hour
			if cfg.ReclaimInterval == 0 {
				cfg.ReclaimInterval = time.Hour
			}
			cfg.Logger = logs.logger()

			if err := New(q, nil, cfg).Run(ctx); err != nil {
				t.Fatalf("Run = %v, want nil", err)
			}
			if n := len(logs.find(tc.logged)); n != 0 {
				t.Errorf("a failure caused by shutdown was reported %d times", n)
			}
		})
	}
}

// fakeJobExpirer records the cut-offs it is asked to delete finished jobs
// before.
type fakeJobExpirer struct {
	mu     sync.Mutex
	before []time.Time
	errs   []error
}

func (e *fakeJobExpirer) DeleteFinishedJobs(_ context.Context, before time.Time) (int64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.before = append(e.before, before)
	if len(e.errs) > 0 {
		err := e.errs[0]
		e.errs = e.errs[1:]
		return 0, err
	}
	return 3, nil
}

func (e *fakeJobExpirer) calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.before)
}

// Finished jobs are deleted once they are older than the retention - 30 days
// unless configured, never under the week enrichment reads back - starting
// at once and going on past a failure.
func TestJobSweepDeletesFinishedJobsAfterTheRetention(t *testing.T) {
	for _, tc := range []struct {
		name      string
		retention time.Duration
		want      time.Duration
	}{
		{"default is thirty days", 0, 30 * 24 * time.Hour},
		{"configured", 90 * 24 * time.Hour, 90 * 24 * time.Hour},
		{"never under a week", time.Hour, 7 * 24 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jobs := &fakeJobExpirer{errs: []error{errLifecycleDB}}
			logs := newLifecycleLogs()
			_, stop := startLifecyclePool(t, newFakeQueue(), nil, Config{
				Workers: 1, Jobs: jobs, JobRetention: tc.retention,
				JobSweepInterval: time.Millisecond, Logger: logs.logger(),
			})
			waitForLifecycle(t, "the sweep to run past an error", func() bool { return jobs.calls() >= 3 })
			stop()

			jobs.mu.Lock()
			first := jobs.before[0]
			jobs.mu.Unlock()
			want := time.Now().Add(-tc.want)
			if d := first.Sub(want); d < -time.Minute || d > time.Minute {
				t.Errorf("deleted jobs finished before %v, want about %v", first, want)
			}
			if len(logs.find("deleting finished jobs failed")) != 1 {
				t.Error("the sweep failure was not reported")
			}
			if len(logs.find("deleted finished jobs")) == 0 {
				t.Error("what the sweep deleted was not reported")
			}
		})
	}
}

// The sweep runs at start, not only after a first interval: a server
// restarted more often than that would otherwise never sweep.
func TestJobSweepRunsAtStart(t *testing.T) {
	jobs := &fakeJobExpirer{}
	_, stop := startLifecyclePool(t, newFakeQueue(), nil, Config{
		Workers: 1, Jobs: jobs, JobSweepInterval: time.Hour, Logger: newLifecycleLogs().logger(),
	})
	waitForLifecycle(t, "the first sweep", func() bool { return jobs.calls() >= 1 })
	stop()
}

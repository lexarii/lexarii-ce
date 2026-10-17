// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// fakeQueue serves a scripted sequence of job states, one per GetJobFor call, so a
// test can drive a job through progress to completion deterministically.
type fakeQueue struct {
	mu     sync.Mutex
	states []postgres.Job
	calls  int

	enqueueID      uuid.UUID
	enqueueCreated bool
	enqueueErr     error
	getErr         error
	cancelErr      error
	cancelled      []uuid.UUID
	lastEnqueue    postgres.EnqueueOptions
	// viewers is who each read was made for. Whether a job is theirs to see
	// is the store's decision; the handler's part is to ask on behalf of the
	// person signed in, every time.
	viewers []uuid.UUID
}

func (q *fakeQueue) Enqueue(_ context.Context, opts postgres.EnqueueOptions) (uuid.UUID, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.lastEnqueue = opts
	return q.enqueueID, q.enqueueCreated, q.enqueueErr
}

func (q *fakeQueue) GetJobFor(_ context.Context, _, viewer uuid.UUID) (postgres.Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.viewers = append(q.viewers, viewer)
	if q.getErr != nil {
		return postgres.Job{}, q.getErr
	}
	if len(q.states) == 0 {
		return postgres.Job{}, store.ErrNotFound
	}
	i := q.calls
	if i >= len(q.states) {
		i = len(q.states) - 1 // hold on the final state
	}
	q.calls++
	return q.states[i], nil
}

func (q *fakeQueue) Cancel(_ context.Context, id uuid.UUID) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.cancelled = append(q.cancelled, id)
	return q.cancelErr
}

func job(status domain.JobStatus, progress float32, note string) postgres.Job {
	return postgres.Job{
		ID: uuid.New(), Type: domain.JobScan, Status: status,
		Progress: progress, ProgressNote: note,
		Attempts: 1, MaxAttempts: 3, CreatedAt: time.Now(),
	}
}

func TestStartScanReturns202WithJobID(t *testing.T) {
	id := uuid.New()
	q := &fakeQueue{enqueueID: id, enqueueCreated: true}
	rt := &Router{Health: &HealthHandler{}, Jobs: &JobHandler{Queue: q, Accounts: anAdmin}}

	rec := serve(t, rt, http.MethodPost, "/api/v1/scan")

	// 202, not 200: scanning thousands of books takes minutes, and the work
	// outlives the request.
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", rec.Code, rec.Body)
	}

	var got dto.JobAccepted
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if got.JobID != id.String() {
		t.Errorf("jobId = %q, want %q", got.JobID, id)
	}
	if !got.Created {
		t.Error("created = false on a first scan")
	}
	if q.lastEnqueue.Type != domain.JobScan {
		t.Errorf("enqueued %q, want scan", q.lastEnqueue.Type)
	}
	// Discovery is cheap and unblocks extraction, so it jumps the queue - by
	// its type's priority, not a number of its own.
	if q.lastEnqueue.Priority != 0 || domain.JobScan.Priority() <= domain.JobExtract.Priority() {
		t.Errorf("scan priority = %d (type %d), want the type's, ahead of extraction",
			q.lastEnqueue.Priority, domain.JobScan.Priority())
	}
}

// Asking twice while a scan is already running is not an error.
func TestStartScanReportsExistingJob(t *testing.T) {
	id := uuid.New()
	q := &fakeQueue{enqueueID: id, enqueueCreated: false}
	rt := &Router{Health: &HealthHandler{}, Jobs: &JobHandler{Queue: q, Accounts: anAdmin}}

	rec := serve(t, rt, http.MethodPost, "/api/v1/scan")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}

	var got dto.JobAccepted
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if got.Created {
		t.Error("created = true for a scan that was already queued")
	}
	if got.JobID != id.String() {
		t.Errorf("jobId = %q, want the existing job", got.JobID)
	}
}

func TestGetJob(t *testing.T) {
	j := job(domain.JobRunning, 0.42, "scanned 42 of 100 files")
	q := &fakeQueue{states: []postgres.Job{j}}
	rt := &Router{Health: &HealthHandler{}, Jobs: &JobHandler{Queue: q, Accounts: anAdmin}}

	rec := serve(t, rt, http.MethodGet, "/api/v1/jobs/"+j.ID.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	var got dto.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if got.Status != "running" || got.Progress != 0.42 {
		t.Errorf("job = %+v", got)
	}
	if got.ProgressNote != "scanned 42 of 100 files" {
		t.Errorf("progressNote = %q", got.ProgressNote)
	}
}

func TestGetJobNotFound(t *testing.T) {
	rt := &Router{Health: &HealthHandler{}, Jobs: &JobHandler{Queue: &fakeQueue{}, Accounts: anAdmin}}
	rec := serve(t, rt, http.MethodGet, "/api/v1/jobs/"+uuid.New().String())
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestCancelJob(t *testing.T) {
	id := uuid.New()
	q := &fakeQueue{}
	rt := &Router{Health: &HealthHandler{}, Jobs: &JobHandler{Queue: q, Accounts: anAdmin}}

	rec := serve(t, rt, http.MethodDelete, "/api/v1/jobs/"+id.String())
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body)
	}
	if len(q.cancelled) != 1 || q.cancelled[0] != id {
		t.Errorf("cancelled = %v, want %s", q.cancelled, id)
	}

	// Cancelling something already finished is "nothing to cancel".
	q2 := &fakeQueue{cancelErr: store.ErrNotFound}
	rt2 := &Router{Health: &HealthHandler{}, Jobs: &JobHandler{Queue: q2, Accounts: anAdmin}}
	if rec := serve(t, rt2, http.MethodDelete, "/api/v1/jobs/"+id.String()); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// parseSSE splits a stream body into (event, data) pairs.
func parseSSE(t *testing.T, body string) [][2]string {
	t.Helper()

	var out [][2]string
	for _, block := range strings.Split(body, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" || strings.HasPrefix(block, ":") {
			continue // blank or heartbeat comment
		}
		var event, data string
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if event != "" {
			out = append(out, [2]string{event, data})
		}
	}
	return out
}

func TestEventsStreamsProgressThenTerminates(t *testing.T) {
	id := uuid.New()
	states := []postgres.Job{
		{ID: id, Type: domain.JobScan, Status: domain.JobRunning, Progress: 0, ProgressNote: "starting"},
		{ID: id, Type: domain.JobScan, Status: domain.JobRunning, Progress: 0.5, ProgressNote: "halfway"},
		{ID: id, Type: domain.JobScan, Status: domain.JobSucceeded, Progress: 1, ProgressNote: "167 books found"},
	}
	q := &fakeQueue{states: states}
	h := &JobHandler{Queue: q, PollInterval: time.Millisecond, Accounts: anAdmin}
	rt := &Router{Health: &HealthHandler{}, Jobs: h}

	rec := httptest.NewRecorder()
	req := asUser(httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+id.String()+"/events", nil))

	done := make(chan struct{})
	go func() {
		defer close(done)
		rt.Handler().ServeHTTP(rec, req)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not terminate; a UI would spin forever")
	}

	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	// Nginx buffers proxied responses by default, holding every event until the
	// stream closes.
	if rec.Header().Get("X-Accel-Buffering") != "no" {
		t.Error("X-Accel-Buffering is not disabled; events would be buffered by a proxy")
	}

	events := parseSSE(t, rec.Body.String())
	if len(events) < 2 {
		t.Fatalf("got %d events: %v", len(events), events)
	}

	// The stream MUST end with a terminal event.
	last := events[len(events)-1]
	if last[0] != "done" {
		t.Fatalf("last event = %q, want done; all events = %v", last[0], events)
	}

	var final dto.Job
	if err := json.Unmarshal([]byte(last[1]), &final); err != nil {
		t.Fatalf("decoding the done event: %v", err)
	}
	if final.Status != "succeeded" || final.Progress != 1 {
		t.Errorf("final job = %+v", final)
	}
	if final.ProgressNote != "167 books found" {
		t.Errorf("final note = %q", final.ProgressNote)
	}
}

// A job that has already finished must still produce a terminal event, not an
// empty stream that hangs.
func TestEventsOnAlreadyFinishedJobTerminatesImmediately(t *testing.T) {
	id := uuid.New()
	q := &fakeQueue{states: []postgres.Job{
		{ID: id, Type: domain.JobScan, Status: domain.JobSucceeded, Progress: 1},
	}}
	rt := &Router{Health: &HealthHandler{}, Jobs: &JobHandler{Queue: q, PollInterval: time.Millisecond, Accounts: anAdmin}}

	rec := httptest.NewRecorder()
	req := asUser(httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+id.String()+"/events", nil))

	done := make(chan struct{})
	go func() {
		defer close(done)
		rt.Handler().ServeHTTP(rec, req)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the stream did not terminate for a finished job")
	}

	events := parseSSE(t, rec.Body.String())
	if len(events) == 0 || events[len(events)-1][0] != "done" {
		t.Fatalf("events = %v, want a terminal done event", events)
	}
}

// A failed job is a terminal state too: the stream ends rather than polling on.
func TestEventsTerminatesOnFailure(t *testing.T) {
	id := uuid.New()
	q := &fakeQueue{states: []postgres.Job{
		{ID: id, Type: domain.JobExtract, Status: domain.JobRunning},
		{ID: id, Type: domain.JobExtract, Status: domain.JobFailed, LastError: "the file is corrupt"},
	}}
	rt := &Router{Health: &HealthHandler{}, Jobs: &JobHandler{Queue: q, PollInterval: time.Millisecond, Accounts: anAdmin}}

	rec := httptest.NewRecorder()
	req := asUser(httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+id.String()+"/events", nil))

	done := make(chan struct{})
	go func() {
		defer close(done)
		rt.Handler().ServeHTTP(rec, req)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not terminate on failure")
	}

	events := parseSSE(t, rec.Body.String())
	last := events[len(events)-1]
	if last[0] != "done" {
		t.Fatalf("last event = %q, want done", last[0])
	}

	var final dto.Job
	if err := json.Unmarshal([]byte(last[1]), &final); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if final.Status != "failed" {
		t.Errorf("status = %q, want failed", final.Status)
	}
	// The client needs to be able to show why.
	if final.LastError == "" {
		t.Error("the failure reason was not reported")
	}
}

// A 404 must be reported as a 404, not as an event stream that says nothing.
// Once headers are sent, the status can no longer be corrected.
func TestEventsOnUnknownJobIs404(t *testing.T) {
	rt := &Router{Health: &HealthHandler{}, Jobs: &JobHandler{Queue: &fakeQueue{}, Accounts: anAdmin}}
	rec := serve(t, rt, http.MethodGet, "/api/v1/jobs/"+uuid.New().String()+"/events")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
		t.Error("a missing job opened an event stream instead of reporting 404")
	}
}

// When the client disconnects the handler must return promptly; the job itself
// carries on.
func TestEventsStopsWhenClientDisconnects(t *testing.T) {
	id := uuid.New()
	q := &fakeQueue{states: []postgres.Job{
		{ID: id, Type: domain.JobScan, Status: domain.JobRunning, Progress: 0.1},
	}}
	rt := &Router{Health: &HealthHandler{}, Jobs: &JobHandler{Queue: q, PollInterval: 5 * time.Millisecond}}

	ctx, cancel := context.WithCancel(context.Background())
	req := asUser(httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+id.String()+"/events", nil).WithContext(ctx))
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		rt.Handler().ServeHTTP(rec, req)
	}()

	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the handler did not return after the client disconnected")
	}
}

func TestEventsRejectsBadID(t *testing.T) {
	rt := &Router{Health: &HealthHandler{}, Jobs: &JobHandler{Queue: &fakeQueue{}, Accounts: anAdmin}}
	if rec := serve(t, rt, http.MethodGet, "/api/v1/jobs/not-a-uuid/events"); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// A job is read on behalf of whoever is signed in, every time - the initial
// read and every poll of an open stream. Whether it is theirs is the store's
// rule (GetJobFor); the handler's part is never to ask as anybody else, and to
// answer "not yours" exactly as "does not exist".
func TestJobsAreReadForTheSignedInPerson(t *testing.T) {
	viewer := uuid.MustParse("c0ffee00-0000-4000-8000-000000000003")
	running := job(domain.JobRunning, 0.5, "")
	finished := job(domain.JobSucceeded, 1, "")
	finished.ID = running.ID

	for _, tc := range []struct {
		name  string
		path  string
		queue *fakeQueue
		want  int
	}{
		{"get, theirs", "/api/v1/jobs/" + running.ID.String(),
			&fakeQueue{states: []postgres.Job{running}}, http.StatusOK},
		{"get, somebody else's", "/api/v1/jobs/" + running.ID.String(),
			&fakeQueue{getErr: store.ErrNotFound}, http.StatusNotFound},
		{"events, theirs", "/api/v1/jobs/" + running.ID.String() + "/events",
			&fakeQueue{states: []postgres.Job{running, finished}}, http.StatusOK},
		{"events, somebody else's", "/api/v1/jobs/" + running.ID.String() + "/events",
			&fakeQueue{getErr: store.ErrNotFound}, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := &Router{Health: &HealthHandler{}, Jobs: &JobHandler{
				Queue: tc.queue, Accounts: aReader, PollInterval: time.Millisecond,
			}}
			rec := httptest.NewRecorder()
			rt.Handler().ServeHTTP(rec, asUserID(httptest.NewRequest(http.MethodGet, tc.path, nil), viewer))

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.want, rec.Body)
			}
			if len(tc.queue.viewers) == 0 {
				t.Fatal("the job was never read")
			}
			for _, v := range tc.queue.viewers {
				if v != viewer {
					t.Fatalf("read on behalf of %s, want the signed-in %s", v, viewer)
				}
			}
		})
	}
}
